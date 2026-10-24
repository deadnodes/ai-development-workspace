package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerclient"
)

func TestOIDCAndWorkerCredentialsRemainIsolated(t *testing.T) {
	s, authority, _ := workerTransportFixture(t)
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer issuer.Close()
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, issuer.Client())
	const bootstrap = "fixture-bootstrap-token-at-least-32-characters"
	manager, err := auth.New(ctx, auth.Config{Mode: "oidc", IssuerURL: issuer.URL, PublicURL: "https://rcp.example", ClientID: "test", AdminRoles: []string{"admin"}, EncryptionKey: []byte(strings.Repeat("k", 32)), FilePath: filepath.Join(t.TempDir(), "auth.db"), BootstrapToken: bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server := httptest.NewServer(New(s, Options{Auth: manager, Token: "control-only", Workers: authority}))
	defer server.Close()
	for _, path := range []string{"/api/state", "/api/commands", "/api/backups/export", "/mcp", "/auth/me", "/", "/api/worker/unknown", "/api/worker/claim/", "/api/worker/../commands", "/api/worker/%2e%2e/commands"} {
		if code := workerHTTP(t, server, path, testWorkerCredential, `{}`); code != 401 {
			t.Fatalf("worker escaped %s: %d", path, code)
		}
	}
	for _, token := range []string{"", "control-only", bootstrap, "actor/human"} {
		if code := workerHTTP(t, server, "/api/worker/claim", token, `{"product_id":"p","repository_id":"r","capability":"source_check"}`); code != 401 {
			t.Fatalf("control credential entered machine protocol: %d", code)
		}
	}
	for _, token := range []string{"", "control-only"} {
		if code := workerHTTP(t, server, "/api/commands", token, `{}`); code != 401 {
			t.Fatalf("OIDC bypass: %d", code)
		}
	}
	request, _ := http.NewRequest("GET", server.URL+"/api/state", nil)
	request.Header.Set("Authorization", "Bearer "+bootstrap)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("verified control credential rejected", response.StatusCode)
	}
	for _, body := range []string{`{"product_id":"foreign","repository_id":"r","capability":"source_check"}`, `{"product_id":"p","repository_id":"foreign","capability":"source_check"}`, `{"product_id":"p","repository_id":"r","capability":"deploy"}`} {
		if code := workerHTTP(t, server, "/api/worker/claim", testWorkerCredential, body); code != 422 {
			t.Fatal("worker scope bypass", code)
		}
	}
	request, _ = http.NewRequest("POST", server.URL+"/api/worker/claim", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+testWorkerCredential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://foreign.example")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("worker skipped origin guard", response.StatusCode)
	}
	client, err := workerclient.New(server.URL, testWorkerCredential)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Claim(context.Background(), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"})
	if err != nil || lease == nil {
		t.Fatalf("worker blocked by OIDC: %v", err)
	}
	input := domain.WorkerAttemptRequest{JobID: lease.Job.ID, AttemptID: lease.Attempt.ID, Fence: lease.Attempt.Fence, InputHash: lease.Job.InputHash}
	if _, err := client.Start(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	result, err := client.Report(context.Background(), domain.WorkerReport{WorkerAttemptRequest: input, Result: "PASSED"})
	if err != nil || result.Job.Status != "SUCCEEDED" {
		t.Fatalf("worker failed under OIDC: %v", err)
	}
}
