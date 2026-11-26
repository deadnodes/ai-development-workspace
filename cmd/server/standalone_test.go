package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestStandaloneProcess(t *testing.T) {
	if os.Getenv("RCP_TEST_STANDALONE_CHILD") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		os.Exit(2)
	}
	os.Args = append(os.Args[:1], os.Args[separator+1:]...)
	main()
	os.Exit(0)
}

func standaloneCommand(t *testing.T, ctx context.Context, args ...string) *exec.Cmd {
	t.Helper()
	binary := os.Getenv("RCP_SMOKE_BINARY")
	if binary == "" {
		var err error
		binary, err = os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		args = append([]string{"-test.run=^TestStandaloneProcess$", "--"}, args...)
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = t.TempDir()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "RCP_") && !slices.Contains([]string{"DATABASE_URL", "RC_TOKEN", "LISTEN_ADDR", "ALLOWED_HOSTS"}, strings.ToUpper(key)) {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "RCP_TEST_STANDALONE_CHILD=1")
	return command
}

func TestStandaloneSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "state #1.sqlite")
	store, err := persistence.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	owner := auth.WithPrincipal(ctx, auth.Principal{ID: "other-user", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
	_, err = application.New(store).Execute(owner, domain.Command{Action: "create_product", ID: "already-private", Data: map[string]any{"name": "Other user's project", "visibility": "private"}})
	store.Close()
	if err != nil {
		t.Fatalf("seed private fixture: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	server := standaloneCommand(t, ctx, "serve", "--data", path, "--listen", addr)
	var logs bytes.Buffer
	server.Stderr = &logs
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	httpClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{}}
	for {
		response, err := httpClient.Get("http://" + addr + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("standalone HTTP did not become healthy")
		case <-time.After(40 * time.Millisecond):
		}
	}
	command := standaloneCommand(t, ctx, "mcp", "--data", path)
	command.Stderr = os.Stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "standalone-smoke", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	instructions := session.InitializeResult().Instructions
	for _, name := range []string{"get_tool_guide", "execute"} {
		if !strings.Contains(instructions, name) || !slices.ContainsFunc(listed.Tools, func(tool *mcp.Tool) bool { return tool.Name == name }) {
			t.Fatalf("startup instructions or stdio tools are missing %s", name)
		}
	}
	if strings.Contains(instructions, "get_code_api") {
		t.Fatal("stdio instructions advertise the HTTP-only SDK tool")
	}
	call := func(name string, args map[string]any, expected ...string) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		value, err := json.Marshal(result.StructuredContent)
		if err != nil || result.IsError {
			content, _ := json.Marshal(result.Content)
			t.Fatalf("%s %v: %s (%v)", name, args, content, err)
		}
		if string(value) == "null" {
			value, _ = json.Marshal(result.Content)
		}
		for _, text := range expected {
			if !strings.Contains(string(value), text) {
				t.Fatalf("%s: missing %q in %s", name, text, value)
			}
		}
	}
	call("get_state", map[string]any{}, "already-private")
	call("execute", map[string]any{"action": "add_product_member", "product_id": "already-private", "data": map[string]any{"user_id": "new-member"}}, "new-member")
	call("execute", map[string]any{"action": "create_product", "id": "fixture-private", "data": map[string]any{"name": "Fixture board", "visibility": "private"}}, "fixture-private")
	call("execute", map[string]any{"action": "create_feature", "id": "fixture-feature", "product_id": "fixture-private", "data": map[string]any{"title": "Portable SQLite feature", "problem": "Context must survive restart", "goal": "Searchable local notes"}}, "fixture-feature")
	call("get_state", map[string]any{}, "fixture-private")
	call("search_context", map[string]any{"product_id": "fixture-private", "query": "portable"}, "fixture-feature")
	call("execute", map[string]any{"action": "create_integration", "id": "fixture-integration", "feature_id": "fixture-feature", "data": map[string]any{"title": "Fixture integration", "objective": "Track locally"}}, "fixture-integration")
	for _, request := range []*mcp.CallToolParams{
		{Name: "execute", Arguments: map[string]any{"action": "refresh_integration_git", "integration_id": "fixture-integration", "data": map[string]any{}}},
		{Name: "queue_build", Arguments: map[string]any{"product_id": "fixture-private", "profile_id": "app"}},
		{Name: "deploy_build", Arguments: map[string]any{"product_id": "fixture-private", "environment_id": "dev", "build_id": "fixture-build"}},
	} {
		result, err := session.CallTool(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		content, err := json.Marshal(result.Content)
		if err != nil || !result.IsError || !bytes.Contains(content, []byte("mcp --data does not run workers")) {
			t.Fatalf("%s did not refuse worker-backed work: %s (%v)", request.Name, content, err)
		}
	}
	store, err = persistence.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	state, err := store.Read(ctx)
	if err != nil || len(state.Operations) != 0 || len(state.Builds) != 0 || len(state.Deployments) != 0 {
		t.Fatalf("worker requests left durable work or the store could not be read: %v", err)
	}
	response, err := httpClient.Get("http://" + addr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("fixture-private")) || !bytes.Contains(body, []byte("already-private")) {
		t.Fatalf("HTTP did not see private products: %d %s (%v)", response.StatusCode, body, err)
	}
}

func TestStandaloneRefusesUnauthenticatedLAN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	command := standaloneCommand(t, ctx, "serve", "--data", path, "--listen", "0.0.0.0:0")
	output, err := command.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("loopback")) || !bytes.Contains(output, []byte("RC_TOKEN")) {
		t.Fatal(fmt.Errorf("unsafe listen was not rejected: %s: %w", output, err))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused startup touched data file: %v", err)
	}
}

func TestStandaloneWorkspaceBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "bootstrap.sqlite")
	for _, name := range []string{"My Local Workspace", "Other Workspace"} {
		workspace := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		command := standaloneCommand(t, ctx, "mcp", "--data", path, "--workspace", workspace)
		command.Stderr = os.Stderr
		session, err := mcp.NewClient(&mcp.Implementation{Name: "bootstrap-smoke", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	store, err := persistence.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Read(ctx)
	if err != nil || len(state.Products) != 1 || state.Products[0].Name != "My Local Workspace" {
		t.Fatalf("expected workspace bootstrap to run only once, got %+v (%v)", state.Products, err)
	}
}
