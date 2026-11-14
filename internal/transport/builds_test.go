package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/providers/forgejo"
	"strings"
	"testing"
)

type transportForgejo struct{}

func (transportForgejo) BranchHead(context.Context, buildconfig.Connection, string, string) (string, error) {
	return strings.Repeat("a", 40), nil
}
func (transportForgejo) CommitExists(context.Context, buildconfig.Connection, string, string) error {
	return nil
}
func (transportForgejo) Dispatch(context.Context, buildconfig.Connection, string, string, string, string, string) (forgejo.Run, error) {
	return forgejo.Run{ID: 1}, nil
}
func (transportForgejo) GetRun(context.Context, buildconfig.Connection, string, int64) (forgejo.Run, error) {
	return forgejo.Run{}, nil
}
func (transportForgejo) ListRuns(context.Context, buildconfig.Connection, string, string, forgejo.Filter) (forgejo.Page, error) {
	return forgejo.Page{}, nil
}

type transportRegistry struct{}

func (transportRegistry) Resolve(context.Context, buildconfig.Registry, string, string) (buildregistry.Artifact, error) {
	return buildregistry.Artifact{}, buildregistry.ErrMissing
}
func TestBuildHTTPMCPContract(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "build.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = store.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	s := application.New(store)
	c := &buildconfig.Config{Connections: []buildconfig.Connection{{ID: "f", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "secret-ref"}}, Registries: []buildconfig.Registry{{ID: "r", Origin: "https://registry.example"}}, Pools: []buildconfig.Pool{{ID: "pool", MaxInFlight: 1}}, Profiles: []buildconfig.Profile{{ID: "app", ProductID: "p", ConnectionID: "f", Repository: "o/r", Workflow: "build.yml", DefaultBranch: "main", PoolID: "pool", RegistryID: "r", Images: []buildconfig.Image{{ID: "app", Repository: "o/r"}}, DispatchRef: "main", TagCandidates: []string{"main-{sha8}", "br-{sha8}"}}}}
	if e = s.ConfigureBuilds(c, transportForgejo{}, transportRegistry{}); e != nil {
		t.Fatal(e)
	}
	handler := New(s, Options{Token: "test-token"})
	callHTTP := func(method, path, body string, authenticated bool) (int, []byte) {
		req, e := http.NewRequest(method, "http://localhost"+path, bytes.NewBufferString(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer test-token")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	if status, _ := callHTTP("GET", "/api/builds?product_id=p", "", false); status != 401 {
		t.Fatal(status)
	}
	status, raw := callHTTP("POST", "/api/builds", `{"product_id":"p","profile_id":"app"}`, true)
	var b domain.Build
	if status != 200 || json.Unmarshal(raw, &b) != nil || b.ID == "" {
		t.Fatal(status, string(raw))
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "build-test", Version: "1"}, nil)
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer test-token")
		handler.ServeHTTP(w, r)
	})
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://localhost/mcp", HTTPClient: &http.Client{Transport: deploymentHandlerTransport{authHandler}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	tools, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	required := map[string]bool{"queue_build": false, "get_build": false, "list_builds": false, "cancel_build": false, "close_build": false, "list_build_profiles": false}
	for _, tool := range tools.Tools {
		if _, ok := required[tool.Name]; ok {
			required[tool.Name] = true
			if tool.InputSchema == nil {
				t.Fatal("missing schema")
			}
			if tool.Name == "queue_build" && !strings.Contains(tool.Description, "image-only") {
				t.Fatal("missing workflow effect")
			}
		}
	}
	for name, found := range required {
		if !found {
			t.Fatal("missing tool", name)
		}
	}
	for _, name := range []string{"queue_build", "get_build", "list_builds", "list_build_profiles", "cancel_build"} {
		args := map[string]any{"product_id": "p"}
		switch name {
		case "queue_build":
			args["profile_id"] = "app"
		case "get_build", "cancel_build":
			args["build_id"] = b.ID
		}
		res, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || res.IsError {
			t.Fatal(name, res, e)
		}
		j, _ := json.Marshal(res.StructuredContent)
		if bytes.Contains(j, []byte("secret-ref")) {
			t.Fatal("credential reference leaked")
		}
		if name == "queue_build" || name == "get_build" {
			var got domain.Build
			if json.Unmarshal(j, &got) != nil || got.ID != b.ID {
				t.Fatal("HTTP/MCP handle differs", string(j))
			}
		}
	}
	status, raw = callHTTP("GET", "/api/builds/"+b.ID+"?product_id=p", "", true)
	if status != 200 || !bytes.Contains(raw, []byte("CANCELLED")) {
		t.Fatal(status, string(raw))
	}

	// Exercise both transports against the same active durable handle.
	if e := store.Update(ctx, func(st *domain.State) error {
		st.Builds[0].Status = "RUNNING"
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	status, raw = callHTTP("POST", "/api/builds/"+b.ID+"/close", `{"product_id":"p","reason":"operator abandoned build","request_key":"close-http"}`, true)
	if status != 200 || !bytes.Contains(raw, []byte("OPERATOR_CLOSED")) {
		t.Fatal(status, string(raw))
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "close_build", Arguments: map[string]any{
		"product_id": "p", "build_id": b.ID, "reason": "operator abandoned build", "request_key": "close-http",
	}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	status, _ = callHTTP("POST", "/api/builds/"+b.ID+"/close", `{"product_id":"p","reason":"operator abandoned build","images":[]}`, true)
	if status != 400 {
		t.Fatal("artifact input accepted", status)
	}
	for _, body := range []string{`{"product_id":"p","profile_id":"app","target":"dev"}`, `{"product_id":"p","profile_id":"app","branch":"main","sha":"` + strings.Repeat("a", 40) + `"}`} {
		if status, _ := callHTTP("POST", "/api/builds", body, true); status < 400 || status >= 500 {
			t.Fatal("obsolete or ambiguous selector accepted", status)
		}
	}
	for _, path := range []string{"/api/builds?product_id=p&limit=101", "/api/builds?product_id=p&unknown=1", "/api/builds?product_id=p&product_id=x"} {
		if status, _ := callHTTP("GET", path, "", true); status < 400 {
			t.Fatal("invalid query accepted", path)
		}
	}
	if status, _ := callHTTP("POST", "/api/builds", `{"product_id":"p","profile_id":"app","unknown":1}`, true); status != 400 {
		t.Fatal(status)
	}
}

type buildAuthTransport struct{}

func (buildAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer test-token")
	return http.DefaultTransport.RoundTrip(r)
}

func TestBuildToolSchemas(t *testing.T) {
	for _, name := range []string{"queue_build", "get_build", "list_builds", "cancel_build", "close_build", "list_build_profiles"} {
		tool := buildTool(name, "description")
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Additional bool                      `json:"additionalProperties"`
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		}
		if json.Unmarshal(raw, &schema) != nil || schema.Additional || schema.Properties["product_id"]["maxLength"] != float64(128) {
			t.Fatal(name, string(raw))
		}
		want := []string{"product_id"}
		switch name {
		case "queue_build":
			want = append(want, "profile_id")
			if schema.Properties["target"] != nil || !bytes.Contains(raw, []byte(`"not":{"required":["branch","sha"]}`)) || schema.Properties["sha"]["pattern"] != "^[0-9a-f]{40}$" {
				t.Fatal("SHA schema")
			}
		case "close_build":
			want = append(want, "build_id", "reason")
			if schema.Properties["reason"]["minLength"] != float64(8) || schema.Properties["reason"]["maxLength"] != float64(500) {
				t.Fatal("reason bounds")
			}
		case "get_build", "cancel_build":
			want = append(want, "build_id")
		case "list_builds":
			if schema.Properties["limit"]["maximum"] != float64(100) || schema.Properties["limit"]["minimum"] != float64(1) || schema.Properties["cursor"]["maxLength"] != float64(2048) {
				t.Fatal("list bounds")
			}
		}
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(schema.Required)
		if string(a) != string(b) {
			t.Fatal(name, string(b))
		}
	}
}
