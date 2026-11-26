package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func codeServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	store, err := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := httptest.NewServer(transport.New(application.New(store), transport.Options{Token: token}))
	t.Cleanup(server.Close)
	return server
}
func codeRequest(t *testing.T, server *httptest.Server, method, path, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}
func TestCodeModeCalls(t *testing.T) {
	server := codeServer(t, "")
	status, raw := codeRequest(t, server, "POST", "/api/tools/get_status_schema", "", "")
	if status != 200 {
		t.Fatalf("stateless call without initialize: %d %s", status, raw)
	}
	_, expected := codeRequest(t, server, "GET", "/api/statuses", "", "")
	var got, want any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(expected, &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("status mismatch: %s", raw)
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"does_not_exist", "{}", 404}, {"execute", `{"action":"invalid_action"}`, 422}, {"resume", `{"feature_id":"missing"}`, 422},
		{"get_status_schema", "null", 400}, {"get_status_schema", "[]", 400}, {"get_status_schema", "{} {}", 400},
	} {
		status, raw := codeRequest(t, server, "POST", "/api/tools/"+tc.path, tc.body, "")
		if status != tc.status || !bytes.Contains(raw, []byte(`"error"`)) {
			t.Errorf("%s: %d %s", tc.path, status, raw)
		}
	}
	status, _ = codeRequest(t, server, "POST", "/api/tools/get_status_schema", strings.Repeat(" ", 12<<20)+"{}", "")
	if status != 413 {
		t.Fatalf("oversized: %d", status)
	}
}
func TestCodeModeToken(t *testing.T) {
	server := codeServer(t, "secret")
	for _, path := range []string{"/api/tools/get_status_schema", "/api/tools", "/api/code-sdk/rcp.ts"} {
		method := "GET"
		if strings.Contains(path, "get_status_schema") {
			method = "POST"
		}
		for _, token := range []string{"", "secret"} {
			status, raw := codeRequest(t, server, method, path, "{}", token)
			want := 200
			if token == "" {
				want = 401
			}
			if status != want {
				t.Fatalf("%s: %d %s", path, status, raw)
			}
		}
	}
}
func TestCodeModeRegistrySDKAndDiscovery(t *testing.T) {
	server := codeServer(t, "")
	status, raw := codeRequest(t, server, "GET", "/api/tools", "", "")
	var registry struct {
		Tools []struct {
			Name  string         `json:"name"`
			Input map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if status != 200 || json.Unmarshal(raw, &registry) != nil {
		t.Fatalf("registry: %d %s", status, raw)
	}
	found := map[string]bool{}
	last := ""
	for _, tool := range registry.Tools {
		if tool.Name < last || tool.Input == nil {
			t.Fatalf("invalid registry: %+v", tool)
		}
		last = tool.Name
		found[tool.Name] = true
	}
	for _, name := range []string{"get_code_api", "execute", "resume", "search_context"} {
		if !found[name] {
			t.Fatal("missing", name)
		}
	}
	status, sdk := codeRequest(t, server, "GET", "/api/code-sdk/rcp.ts", "", "")
	for _, text := range []string{"export const rcp", "search_context", "export interface SearchContextInput", server.URL} {
		if status != 200 || !strings.Contains(string(sdk), text) {
			t.Fatal("SDK missing", text)
		}
	}
	_, sdk2 := codeRequest(t, server, "GET", "/api/code-sdk/rcp.ts", "", "")
	if !bytes.Equal(sdk, sdk2) {
		t.Fatal("SDK is not deterministic")
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_code_api", Arguments: map[string]any{"query": "ReSuMe"}})
	if err != nil || result.IsError {
		t.Fatalf("discovery: %v %+v", err, result)
	}
	data, _ := json.Marshal(result.StructuredContent)
	var discovery struct {
		Declarations string   `json:"declarations"`
		Tools        []string `json:"tools"`
	}
	if json.Unmarshal(data, &discovery) != nil || !strings.Contains(discovery.Declarations, "resume") || len(discovery.Tools) == 0 {
		t.Fatalf("discovery: %s", data)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node missing; skipping SDK execution only")
	}
	dir := t.TempDir()
	for name, data := range map[string][]byte{"rcp.ts": sdk, "script.ts": []byte(`import rcp from "./rcp.ts"; console.log(JSON.stringify(await rcp.get_status_schema({})));`)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, filepath.Join(dir, "script.ts"))
	cmd.Env = append(os.Environ(), "RCP_URL="+server.URL, "RCP_TOKEN=", "RCP_TOKEN_FILE=")
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v %s", err, e.Stderr)
		}
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(out)) {
		t.Fatalf("node output: %s", out)
	}
}
func TestCodeModeSchemaConverter(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(`{"type":"object","required":["nested"],"properties":{"nested":{"type":"object","properties":{"bad-key":{"enum":["a","b"],"description":"safe */ comment"}}},"items":{"type":"array","items":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	got := transport.SchemaTypeScriptForTest(schema, 0)
	for _, part := range []string{`nested: {`, `"bad-key"?: "a" | "b"`, `items?: Array<(number) | (null)>`, `safe * / comment`} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in %s", part, got)
		}
	}
	if transport.SchemaTypeScriptForTest(schema, 8) != "any" {
		t.Fatal("depth bound")
	}
	// Discriminated variants keep the shared base fields and add required keys.
	var variants any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"action":{"type":"string"},"feature_id":{"type":"string"},"data":{"type":"object","properties":{"title":{"type":"string"}}}},"oneOf":[{"properties":{"action":{"const":"create_feature"},"data":{"required":["title"]}},"required":["action","data","feature_id"]}]}`), &variants); err != nil {
		t.Fatal(err)
	}
	got = transport.SchemaTypeScriptForTest(variants, 0)
	for _, part := range []string{`) & ((`, `feature_id?: string;`, `action: "create_feature";`, `feature_id: unknown;`, `title: unknown;`} {
		if !strings.Contains(got, part) {
			t.Errorf("variant: missing %q in %s", part, got)
		}
	}
}

// The recorder path also verifies dispatch where the host forbids TCP listeners.
func TestCodeModeInProcess(t *testing.T) {
	store, err := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := transport.New(application.New(store), transport.Options{})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		handler.ServeHTTP(rec, req)
		return rec
	}
	expected := request("GET", "/api/statuses", "")
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"get_status_schema", "", 200}, {"does_not_exist", "{}", 404}, {"execute", `{"action":"invalid"}`, 422}, {"resume", `{"feature_id":"missing"}`, 422}, {"get_code_api", `{"query":"resume"}`, 200},
	} {
		rec := request("POST", "/api/tools/"+tc.path, tc.body)
		if rec.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		if tc.path == "get_status_schema" && rec.Body.String() != expected.Body.String() {
			t.Fatal("status content mismatch")
		}
		if tc.path == "get_code_api" && !strings.Contains(rec.Body.String(), "ResumeInput") {
			t.Fatal("live discovery missing resume")
		}
	}
	registry := request("GET", "/api/tools", "")
	if registry.Code != 200 || !strings.Contains(registry.Body.String(), `"get_code_api"`) {
		t.Fatal(registry.Body.String())
	}
	sdk := request("GET", "/api/code-sdk/rcp.ts", "")
	if sdk.Code != 200 {
		t.Fatal(sdk.Body.String())
	}
	if node, err := exec.LookPath("node"); err == nil {
		path := filepath.Join(t.TempDir(), "rcp.ts")
		if err := os.WriteFile(path, sdk.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(node, "--check", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("SDK syntax: %v %s", err, out)
		}
	}
}

// Observe the principal in a service method reached through the nested MCP request.
type codePrincipalService struct {
	transport.Service
	principal auth.Principal
}

func (s *codePrincipalService) ResumeSummary(ctx context.Context, _ string) (any, error) {
	s.principal, _ = auth.FromContext(ctx)
	return map[string]bool{"ok": true}, nil
}
func TestCodeModePrincipalAndTokenInProcess(t *testing.T) {
	store, err := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &codePrincipalService{Service: application.New(store)}
	handler := transport.New(service, transport.Options{Token: "secret"})
	for _, token := range []string{"", "secret"} {
		req := httptest.NewRequest("POST", "http://localhost/api/tools/resume", strings.NewReader(`{"feature_id":"f"}`))
		principal := auth.Principal{ID: "agent/code-mode", Kind: "agent", Products: []string{"p"}}
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := 200
		if token == "" {
			want = 401
		}
		if rec.Code != want {
			t.Fatalf("token request: %d %s", rec.Code, rec.Body.String())
		}
		if token != "" && !reflect.DeepEqual(service.principal, principal) {
			t.Fatalf("lost principal: %+v", service.principal)
		}
	}
}
