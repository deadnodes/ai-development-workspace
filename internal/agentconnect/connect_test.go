package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/agentguide"
	"strings"
	"testing"
)

func kitServer(t *testing.T, products int, token string) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "get_state"}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Limit int `json:"limit"`
	}) (*mcp.CallToolResult, any, error) {
		if in.Limit != 2 {
			t.Errorf("discovery must request limit2, got%d", in.Limit)
		}
		rows := []map[string]string{}
		for i := 0; i < products && i < 2; i++ {
			rows = append(rows, map[string]string{"id": fmt.Sprintf("p%d", i)})
		}
		page := map[string]any{"projection": "compact_navigation", "scope": map[string]any{"collection": "products"}, "items": rows, "pagination": map[string]any{"returned": len(rows), "has_more": products > 2}}
		if products > 2 {
			page["pagination"].(map[string]any)["next_cursor"] = "fixture-cursor"
		}
		return &mcp.CallToolResult{StructuredContent: page}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/agent-kit":
			json.NewEncoder(w).Encode(agentguide.Get())
		case "/mcp":
			if r.Method == http.MethodGet {
				t.Error("discovery opened standalone SSE")
			}
			mcpHandler.ServeHTTP(w, r)
		case "/api/state":
			t.Error("connect fetched full State")
			w.WriteHeader(500)
		case "/api/products/p0/context":
			json.NewEncoder(w).Encode(map[string]any{"product": map[string]string{"id": "p0"}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
func writeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
}
func readFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestConnectPreservesUnrelatedContentAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".codex/config.toml", "# My settings\nmodel = \"example\"\n[mcp_servers.other]\nurl = \"https://other.test/mcp\"\n")
	writeFile(t, root, "AGENTS.md", "# Existing project instructions\nKeep these exactly.\n")
	writeFile(t, root, ".claude/settings.json", "{\"existing\":true}\n")
	writeFile(t, root, ".mcp.json", "{\"existing\":true}\n")
	t.Setenv("RCP_CONNECT_TEST_TOKEN", "secret-not-written")
	server := kitServer(t, 1, "secret-not-written")
	options := Options{URL: server.URL, Workspace: root, TokenEnv: "RCP_CONNECT_TEST_TOKEN"}
	binding, err := Install(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ProductID != "p0" {
		t.Fatal("single Product not selected")
	}
	files := installPaths()
	snapshots := map[string][]byte{}
	for _, path := range files {
		snapshots[path] = readFile(t, root, path)
		if bytes.Contains(snapshots[path], []byte("secret-not-written")) {
			t.Fatal("persisted token")
		}
	}
	if !bytes.HasPrefix(snapshots[files[0]], []byte("# My settings\nmodel = \"example\"")) || !bytes.Contains(snapshots[files[0]], []byte("[mcp_servers.other]")) {
		t.Fatal("unrelated TOML changed")
	}
	if !bytes.HasPrefix(snapshots[files[2]], []byte("# Existing project instructions\nKeep these exactly.\n")) {
		t.Fatal("AGENTS overwritten")
	}
	if !bytes.HasPrefix(snapshots[files[1]], []byte("---\nname:")) {
		t.Fatal("skill frontmatter no longer first")
	}
	if _, err = Install(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if !bytes.Equal(snapshots[path], readFile(t, root, path)) {
			t.Fatalf("non-idempotent %s", path)
		}
	}
	info, _ := os.Stat(filepath.Join(root, "AGENTS.md"))
	if info.Mode().Perm() != 0640 {
		t.Fatal("existing file mode changed")
	}
	for _, path := range []string{".claude/settings.json", ".mcp.json"} {
		if string(readFile(t, root, path)) != "{\"existing\":true}\n" {
			t.Fatal("unrelated Claude/MCP config changed", path)
		}
	}
}
func TestConnectRejectsConflictsWithoutWriting(t *testing.T) {
	server := kitServer(t, 1, "")
	for _, c := range []struct{ name, path, content string }{
		{"unmanaged MCP", ".codex/config.toml", "[mcp_servers.\"release-control\"]\nurl=\"https://owned.test\"\n"},
		{"invalid TOML", ".codex/config.toml", "not = [ valid"},
		{"unmanaged skill", ".agents/skills/rcp-handoff/SKILL.md", "User skill"},
		{"invalid binding", ".release-control.json", "{\"password\":\"do-not-touch\"}"},
		{"broken markers", "AGENTS.md", agentsStart + "\nunfinished"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, c.path, c.content)
			_, err := Install(context.Background(), Options{URL: server.URL, Workspace: root})
			if err == nil {
				t.Fatal("conflict accepted")
			}
			if string(readFile(t, root, c.path)) != c.content {
				t.Fatal("conflicting file changed")
			}
			if c.path != ".release-control.json" {
				if _, err = os.Stat(filepath.Join(root, ".release-control.json")); !os.IsNotExist(err) {
					t.Fatal("partial install persisted")
				}
			}
		})
	}
}
func TestConnectRejectsSymlinkEscapes(t *testing.T) {
	server := kitServer(t, 1, "")
	for _, relative := range []string{".codex", ".agents", ".claude", "AGENTS.md"} {
		t.Run(relative, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := outside
			if relative == "AGENTS.md" {
				target = filepath.Join(outside, "notes")
				os.WriteFile(target, []byte("untouched"), 0600)
			}
			if err := os.Symlink(target, filepath.Join(root, relative)); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root}); err == nil {
				t.Fatal("symlink accepted")
			}
			entries, _ := os.ReadDir(outside)
			want := 0
			if relative == "AGENTS.md" {
				want = 1
			}
			if len(entries) != want {
				t.Fatal("escaped write")
			}
		})
	}
}
func TestConnectRequiresExplicitProductWhenAmbiguousAndChecksServer(t *testing.T) {
	for _, count := range []int{0, 2} {
		server := kitServer(t, count, "")
		if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: t.TempDir()}); err == nil {
			t.Fatal("ambiguous Product selected")
		}
		if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: t.TempDir(), ProductID: "p0"}); err != nil {
			t.Fatal(err)
		}
	}
	server := kitServer(t, 1, "protected")
	if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: t.TempDir()}); err == nil {
		t.Fatal("server auth ignored")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"skill": map[string]string{"name": "rcp-handoff", "content": "x", "sha256": "wrong"}, "instructions": "x"})
	}))
	defer bad.Close()
	if _, err := Install(context.Background(), Options{URL: bad.URL, Workspace: t.TempDir()}); err == nil {
		t.Fatal("bad checksum accepted")
	}
}

type cancellingContext struct {
	context.Context
	calls int
}

func (c *cancellingContext) Err() error {
	c.calls++
	if c.calls >= 10 {
		return context.Canceled
	}
	return nil
}
func TestConnectRollsBackAlreadyReplacedFiles(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plans := []filePlan{}
	for i := range 7 {
		path := fmt.Sprintf("file%d", i)
		writeFile(t, directory, path, "original")
		plan, err := readPlan(root, path)
		if err != nil {
			t.Fatal(err)
		}
		plan.after = []byte("replacement")
		plans = append(plans, plan)
	}
	ctx := &cancellingContext{Context: context.Background()}
	if err = commitPlans(ctx, root, plans); err == nil {
		t.Fatal("expected cancellation")
	}
	for _, plan := range plans {
		if string(readFile(t, directory, plan.relative)) != "original" {
			t.Fatal("partial mutation survived rollback")
		}
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 7 {
		t.Fatal("temporary files remained")
	}
}
func TestRunHelpAndCLI(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, &out); err != nil || !strings.Contains(out.String(), "product-id") {
		t.Fatal("help failed", err)
	}
	server := kitServer(t, 1, "")
	out.Reset()
	if err := Run(context.Background(), []string{"--url", server.URL, "--workspace", t.TempDir()}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new chat") {
		t.Fatal("reload requirement omitted")
	}
}
