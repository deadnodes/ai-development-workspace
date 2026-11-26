package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/coreos/go-oidc/v3/oidc"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	rr "releasecontrol/internal/runtimereport"
	"releasecontrol/internal/transport"
	"strings"
	"testing"
	"time"
)

func TestPrivateRuntimeReporterTokenScope(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	svc := application.New(store)
	owner := auth.WithPrincipal(ctx, auth.Principal{ID: "human/owner", Kind: "human", Permissions: []string{"write"}, Products: []string{"p"}})
	if _, err := svc.Execute(owner, domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "Runtime reports", "visibility": "private"}}); err != nil {
		t.Fatal(err)
	}
	handler := transport.New(svc, transport.Options{Token: "control", RuntimeReporterToken: "reporter", RuntimeReporterScope: "p:prod:cluster"})
	at := time.Now().UTC()
	report := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "cluster", ReportedAt: at, Workloads: []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api", Containers: []rr.Container{{Name: "api", Image: "v1"}}}}}
	raw, _ := json.Marshal(report)
	call := func(method, path, token string, body []byte) int {
		req := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, tc := range []struct {
		method, path, token string
		body                []byte
		want                int
	}{{"GET", "/api/state", "reporter", nil, 403}, {"GET", "/healthz", "reporter", nil, 403}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod", "reporter", nil, 403}, {"POST", "/api/commands", "reporter", []byte(`{}`), 403}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod&limit=1001", "control", nil, 422}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod&limit=0", "control", nil, 422}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod&cursor=bad", "control", nil, 422}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod&limit=1", "control", nil, 200}, {"GET", "/api/state", "control", nil, 200}, {"GET", "/api/state", "bad", nil, 401}, {"POST", "/api/runtime-reports", "reporter", bytes.Replace(raw, []byte(`"prod"`), []byte(`"dev"`), 1), 403}, {"POST", "/api/runtime-reports", "reporter", []byte(`{"product_id":"p","environment_id":"prod","cluster":"bad!"}`), 403}, {"POST", "/api/runtime-reports", "reporter", bytes.Replace(raw, []byte(`"cluster":"cluster"`), []byte(`"cluster":"other"`), 1), 403}, {"POST", "/api/runtime-reports", "reporter", raw, 200}, {"POST", "/api/runtime-reports", "reporter", raw, 429}, {"POST", "/api/runtime-reports", "reporter", []byte(strings.Repeat(" ", 1<<20) + `{}`), 400}, {"GET", "/api/runtime-reports?product_id=p&environment_id=prod", "control", nil, 200}} {
		if got := call(tc.method, tc.path, tc.token, tc.body); got != tc.want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, got, tc.want)
		}
	}
	state, e := store.Read(ctx)
	if e != nil || len(state.Events) != 2 || state.Events[1].Actor != "service/prod-runtime-reporter" {
		t.Fatal(state.Events, e)
	}
	// Exercise the real MCP handler without opening a socket.
	req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_runtime_report","arguments":{"product_id":"p","environment_id":"prod","limit":1}}}`))
	req.Header.Set("Authorization", "Bearer control")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "cluster") || !strings.Contains(rec.Body.String(), "pagination") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

// OIDC discovery is served in-process; no network sockets are opened.
type reporterRoundTrip func(*http.Request) (*http.Response, error)

func (f reporterRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRuntimeReporterOIDC(t *testing.T) {
	client := &http.Client{Transport: reporterRoundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteString(`{"issuer":"https://issuer","authorization_endpoint":"https://issuer/auth","token_endpoint":"https://issuer/token","jwks_uri":"https://issuer/keys","id_token_signing_alg_values_supported":["RS256"]}`)
		return rec.Result(), nil
	})}
	ctx := oidc.ClientContext(context.Background(), client)
	manager, err := auth.New(ctx, auth.Config{Mode: "oidc", PublicURL: "https://rcp", IssuerURL: "https://issuer", ClientID: "rcp", EncryptionKey: make([]byte, 32), AllowedRoles: []string{"member"}, FilePath: filepath.Join(t.TempDir(), "auth.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	if _, err := svc.Execute(ctx, domain.Command{Action: "create_product", ID: "p", Actor: "test", Data: map[string]any{"name": "Runtime reports"}}); err != nil {
		t.Fatal(err)
	}
	handler := transport.New(svc, transport.Options{Auth: manager, RuntimeReporterToken: "reporter", RuntimeReporterScope: "p:prod:c"})
	report := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", ReportedAt: time.Now()}
	body, _ := json.Marshal(report)
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{{"POST", "/api/runtime-reports", "reporter", 200}, {"GET", "/api/state", "reporter", 403}, {"POST", "/api/commands", "reporter", 403}, {"POST", "/api/runtime-reports", "unknown", 401}} {
		req := httptest.NewRequest(tc.method, "http://localhost"+tc.path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatal(tc, rec.Code, rec.Body.String())
		}
	}
}
