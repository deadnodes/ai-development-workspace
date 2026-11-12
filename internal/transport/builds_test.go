package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
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
func (transportForgejo) Dispatch(context.Context, buildconfig.Connection, string, string, string) (forgejo.Run, error) {
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
	c := &buildconfig.Config{Connections: []buildconfig.Connection{{ID: "f", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "secret-ref"}}, Registries: []buildconfig.Registry{{ID: "r", Origin: "https://registry.example"}}, Pools: []buildconfig.Pool{{ID: "pool", MaxInFlight: 1}}, Profiles: []buildconfig.Profile{{ID: "app", ProductID: "p", ConnectionID: "f", Repository: "o/r", Workflow: "dev.yml", DefaultBranch: "main", PoolID: "pool", RegistryID: "r", ImageRepository: "o/r", TagTemplate: "forgejo-{sha8}", Targets: map[string]buildconfig.Target{"dev": {Branch: "main"}}}}}
	if e = s.ConfigureBuilds(c, transportForgejo{}, transportRegistry{}); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(New(s, Options{Token: "test-token"}))
	defer server.Close()
	callHTTP := func(method, path, body string, authenticated bool) (int, []byte) {
		req, e := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer test-token")
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}
	if status, _ := callHTTP("GET", "/api/builds?product_id=p", "", false); status != 401 {
		t.Fatal(status)
	}
	status, raw := callHTTP("POST", "/api/builds", `{"product_id":"p","profile_id":"app","target":"dev"}`, true)
	var b domain.Build
	if status != 200 || json.Unmarshal(raw, &b) != nil || b.ID == "" {
		t.Fatal(status, string(raw))
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "build-test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: buildAuthTransport{}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	tools, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	required := map[string]bool{"queue_build": false, "get_build": false, "list_builds": false, "cancel_build": false, "list_build_profiles": false}
	for _, tool := range tools.Tools {
		if _, ok := required[tool.Name]; ok {
			required[tool.Name] = true
			if tool.InputSchema == nil {
				t.Fatal("missing schema")
			}
			if tool.Name == "queue_build" && !strings.Contains(tool.Description, "dev deploys") {
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
			args["target"] = "dev"
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
	for _, path := range []string{"/api/builds?product_id=p&limit=101", "/api/builds?product_id=p&unknown=1", "/api/builds?product_id=p&product_id=x"} {
		if status, _ := callHTTP("GET", path, "", true); status < 400 {
			t.Fatal("invalid query accepted", path)
		}
	}
	if status, _ := callHTTP("POST", "/api/builds", `{"product_id":"p","profile_id":"app","target":"dev","unknown":1}`, true); status != 400 {
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
	for _, name := range []string{"queue_build", "get_build", "list_builds", "cancel_build", "list_build_profiles"} {
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
			want = append(want, "profile_id", "target")
			if schema.Properties["sha"]["pattern"] != "^[0-9a-f]{40}$" {
				t.Fatal("SHA schema")
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
