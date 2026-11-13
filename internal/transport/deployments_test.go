package transport

import (
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
	"releasecontrol/internal/providers/github"
	"strings"
	"testing"
)

type deploymentContractRegistry struct{}

func (deploymentContractRegistry) Resolve(context.Context, buildconfig.Registry, string, string) (buildregistry.Artifact, error) {
	return buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("a", 64), Revision: strings.Repeat("a", 40)}, nil
}

type deploymentHandlerTransport struct{ handler http.Handler }

func (d deploymentHandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	d.handler.ServeHTTP(w, r)
	return w.Result(), nil
}
func TestDeploymentHTTPMCPWithoutSockets(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	store.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
		return nil
	})
	c := &buildconfig.Config{Connections: []buildconfig.Connection{{ID: "forgejo", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "token"}}, Registries: []buildconfig.Registry{{ID: "registry", Origin: "https://registry.example"}}, Pools: []buildconfig.Pool{{ID: "pool"}}, GitOps: []buildconfig.GitOps{{ID: "flux", Kind: "github", APIURL: "https://api.github.com", AppID: 1, InstallationID: 2, PrivateKeyRef: "github.pem", Repository: "o/flux", Branch: "main"}}, Environments: []buildconfig.Environment{{ID: "dev", GitOpsID: "flux", AllowAutoMerge: true}}, Profiles: []buildconfig.Profile{{ID: "app", ProductID: "p", ConnectionID: "forgejo", Repository: "o/r", Workflow: "build.yml", DefaultBranch: "main", DispatchRef: "main", PoolID: "pool", RegistryID: "registry", TagCandidates: []string{"main-{sha8}"}, Images: []buildconfig.Image{{ID: "app", Repository: "o/r"}}, Deployments: map[string]buildconfig.DeploymentTarget{"dev": {Files: []buildconfig.DeploymentFile{{Path: "dev.yaml", ImageID: "app", ImageRepository: "registry.example/o/r", TagFormat: "{tag}@{digest}", MinMatches: 1}}}}}}}
	s := application.New(store)
	if e = s.ConfigureBuilds(c, transportForgejo{}, deploymentContractRegistry{}); e != nil {
		t.Fatal(e)
	}
	s.ConfigureDeployments(forgejo.New(), buildregistry.New(), github.New())
	b, e := s.QueueBuild(ctx, domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DispatchBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	h := New(s, Options{Token: "test-token"})
	call := func(method, path, body string, authorized bool) (int, []byte) {
		req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authorized {
			req.Header.Set("Authorization", "Bearer test-token")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	if code, _ := call("GET", "/api/deployments?product_id=p", "", false); code != 401 {
		t.Fatal(code)
	}
	payload := `{"product_id":"p","build_id":"` + b.ID + `","environment_id":"dev"}`
	code, raw := call("POST", "/api/deployments", payload, true)
	if code != 200 {
		t.Fatal(code, string(raw))
	}
	var d domain.Deployment
	if json.Unmarshal(raw, &d) != nil || d.Status != "PENDING_CHECKS" {
		t.Fatal(string(raw))
	}
	for _, body := range []string{strings.Replace(payload, "dev", "prod", 1), strings.TrimSuffix(payload, "}") + `,"unknown":true}`, payload + `{}`} {
		if code, _ := call("POST", "/api/deployments", body, true); code < 400 {
			t.Fatal(body, code)
		}
	}
	for _, path := range []string{"/api/deployments?product_id=p&limit=101", "/api/deployments?product_id=p&unknown=x", "/api/deployments?product_id=p&product_id=x"} {
		if code, _ := call("GET", path, "", true); code < 400 {
			t.Fatal(path, code)
		}
	}
	if code, _ := call("GET", "/api/deployments/"+d.ID+"?product_id=p", "", true); code != 200 {
		t.Fatal(code)
	}
	// MCP uses the same HTTP handler through an in-memory RoundTripper.
	client := mcp.NewClient(&mcp.Implementation{Name: "deployment-test", Version: "1"}, nil)
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer test-token")
		h.ServeHTTP(w, r)
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
	found := map[string]bool{}
	for _, tool := range tools.Tools {
		found[tool.Name] = true
	}
	for _, name := range []string{"deploy_build", "get_deployment", "list_deployments"} {
		if !found[name] {
			t.Fatal("missing", name)
		}
	}
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_build", Arguments: map[string]any{"product_id": "p", "build_id": b.ID, "environment_id": "dev"}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_build", Arguments: map[string]any{"product_id": "p", "build_id": b.ID, "environment_id": "prod"}})
	if e == nil && !result.IsError {
		t.Fatal("MCP prod accepted")
	}
}
