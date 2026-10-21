package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/workerclient"
	"releasecontrol/internal/workerconfig"
)

const testWorkerCredential = "fixture-worker-token-000000000000000000001"

func workerTransportFixture(t *testing.T) (*application.Service, *workerconfig.Authority, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "worker.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	s := application.New(store)
	for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "p"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "f", "goal": "g"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "i", "objective": "o"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "r", "url": "https://github.com/test/repo"}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "feature", "base_commit": strings.Repeat("a", 40), "head_commit": strings.Repeat("b", 40), "commits": []string{strings.Repeat("b", 40)}}}} {
		c.Actor = "fixture/operator"
		if _, e := s.Execute(ctx, c); e != nil {
			t.Fatal(e)
		}
	}
	r := workerconfig.Recipe{ID: "check", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/test/repo", Capability: "source_check", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{"race": {Type: "boolean"}}}
	r.Hash = workerconfig.RecipeHash(r)
	a, e := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{r}, Credentials: []workerconfig.Credential{{ID: "runner", Principal: "worker/test", TokenEnv: "WORKER_FIXTURE", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"source_check"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "control-only", func(string) string { return testWorkerCredential })
	if e != nil {
		t.Fatal(e)
	}
	s.ConfigureWorkers(a)
	if _, e = s.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "human/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "transport-initial-fixture", "recipe_id": "check", "recipe_revision": "v1", "recipe_hash": r.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": strings.Repeat("b", 40), "parameters": map[string]any{"race": true}, "deadline": time.Now().Add(time.Minute), "max_attempts": 2}}); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(New(s, Options{Token: "control-only", Workers: a}))
	t.Cleanup(server.Close)
	return s, a, server
}
func workerHTTP(t *testing.T, server *httptest.Server, path, token, body string) int {
	t.Helper()
	r, e := http.NewRequest("POST", server.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, e := server.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	return res.StatusCode
}
func TestWorkerChannelIsolationAndTypedClient(t *testing.T) {
	_, _, server := workerTransportFixture(t)
	for _, path := range []string{"/api/state", "/api/commands", "/api/backups/export", "/api/backups/restore", "/api/schema", "/mcp", "/"} {
		if code := workerHTTP(t, server, path, testWorkerCredential, `{}`); code != 401 {
			t.Fatalf("worker escaped %s: %d", path, code)
		}
	}
	body := `{"product_id":"p","repository_id":"r","capability":"source_check"}`
	for _, token := range []string{"", "control-only", "actor/worker"} {
		if code := workerHTTP(t, server, "/api/worker/claim", token, body); code != 401 {
			t.Fatalf("unauthorized claim: %d", code)
		}
	}
	for _, body := range []string{`{"product_id":"p","repository_id":"r","capability":"source_check","actor":"human/admin"}`, `{"product_id":"p","repository_id":"r","capability":"source_check"} {}`, `{"product_id":"p","repository_id":"r","capability":"source_check","command":"unapproved"}`} {
		if code := workerHTTP(t, server, "/api/worker/claim", testWorkerCredential, body); code != 400 {
			t.Fatalf("unknown fields accepted: %d", code)
		}
	}
	for _, body := range []string{`{"product_id":"other","repository_id":"r","capability":"source_check"}`, `{"product_id":"p","repository_id":"other","capability":"source_check"}`, `{"product_id":"p","repository_id":"r","capability":"deploy"}`} {
		if code := workerHTTP(t, server, "/api/worker/claim", testWorkerCredential, body); code != 422 {
			t.Fatalf("scope escape: %d", code)
		}
	}
	client, e := workerclient.New(server.URL, testWorkerCredential)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := client.Claim(context.Background(), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"})
	if e != nil || lease == nil {
		t.Fatal(e)
	}
	req := domain.WorkerAttemptRequest{JobID: lease.Job.ID, AttemptID: lease.Attempt.ID, Fence: lease.Attempt.Fence, InputHash: lease.Job.InputHash}
	if _, e = client.Start(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Heartbeat(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	report := domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED"}
	result, e := client.Report(context.Background(), report)
	if e != nil || result.Job.Status != "SUCCEEDED" {
		t.Fatal(e)
	}
	if _, e = client.Report(context.Background(), report); e != nil {
		t.Fatal("lost response replay", e)
	}
	report.Result = "CHECK_FAILED"
	if _, e = client.Report(context.Background(), report); e == nil {
		t.Fatal("conflicting replay accepted")
	}
}
func TestWorkerRoutesDisabledByDefaultAndWithoutControlProtection(t *testing.T) {
	s, a, _ := workerTransportFixture(t)
	for _, o := range []Options{{Token: "control-only"}, {Workers: a}} {
		server := httptest.NewServer(New(s, o))
		code := workerHTTP(t, server, "/api/worker/claim", testWorkerCredential, `{}`)
		server.Close()
		if code != 503 {
			t.Fatal("worker route enabled without protected authority", code)
		}
	}
}
func TestQueueWorkerStrictImmutableInputs(t *testing.T) {
	s, _, server := workerTransportFixture(t)
	st, e := s.State(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	j := st.WorkerJobs[0]
	good := map[string]any{"request_key": "transport-strict-fixture", "recipe_id": j.Inputs.RecipeID, "recipe_revision": j.Inputs.RecipeRevision, "recipe_hash": j.Inputs.RecipeHash, "config_revision": j.Inputs.ConfigRevision, "revision_id": j.Inputs.RevisionID, "source_sha": j.Inputs.SourceSHA, "parameters": j.Inputs.Parameters, "deadline": time.Now().Add(time.Minute), "max_attempts": 2}
	for _, change := range []map[string]any{{"request_key": ""}, {"request_key": "short"}, {"recipe_hash": "sha256:" + strings.Repeat("c", 64)}, {"recipe_revision": "different"}, {"config_revision": "different"}, {"source_sha": strings.Repeat("c", 40)}, {"revision_id": "missing"}, {"command": "unapproved"}, {"repository_url": "https://evil.test/repo"}, {"parameters": map[string]any{"race": "secret"}}, {"deadline": time.Now().Add(-time.Second)}, {"max_attempts": 11}} {
		data := map[string]any{}
		for k, v := range good {
			data[k] = v
		}
		for k, v := range change {
			data[k] = v
		}
		b, _ := json.Marshal(domain.Command{Action: "queue_worker_job", ProductID: "p", IntegrationID: "i", Actor: "human/operator", Data: data})
		if code := workerHTTP(t, server, "/api/commands", "control-only", string(b)); code != 422 {
			t.Fatalf("immutable/strict field escape: %d", code)
		}
	}
}
