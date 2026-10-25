package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"releasecontrol/internal/testdocker"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/registryobserver"
	"releasecontrol/internal/registryobserver/fixture"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

// Setup writes go directly to the exact owned Distribution fixture. The observer
// sees a separate GET-only proxy; its writes are counted and rejected.
func TestRegistryObservationDistributionProtectedAPI(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("requires explicit isolated Docker fixture opt-in")
	}
	fixtureDocker := testdocker.Load(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("docker", append([]string{"--context", fixtureDocker.Context}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("Docker fixture: %v %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	owner := fmt.Sprintf("rcp011-observe-%d", time.Now().UnixNano())
	id := docker("run", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("invalid owned container ID")
	}
	t.Logf("owned registry %s owner %s", id, owner)
	t.Cleanup(func() {
		if docker("inspect", "--format", `{{.Id}} {{index .Config.Labels "rcp.fixture.owner"}}`, id) != id+" "+owner {
			t.Error("fixture ownership mismatch")
			return
		}
		docker("rm", "--force", id)
	})
	origin := "http://" + docker("port", id, "5000/tcp")
	for i := 0; i < 50; i++ {
		r, e := http.Get(origin + "/v2/")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		if i == 49 {
			t.Fatal("registry not ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	f := fixture.New()
	request := func(method, path, media string, b []byte) *http.Response {
		t.Helper()
		q, e := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		q.Header.Set("Content-Type", media)
		r, e := http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		if r.StatusCode >= 300 {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			t.Fatalf("fixture setup %s %s: %d %s", method, path, r.StatusCode, body)
		}
		return r
	}
	for digest, b := range f.Blobs {
		r := request("POST", "/v2/"+f.Repository+"/blobs/uploads/", "", nil)
		location := r.Header.Get("Location")
		r.Body.Close()
		u, e := url.Parse(location)
		if e != nil {
			t.Fatal(e)
		}
		v := u.Query()
		v.Set("digest", digest)
		u.RawQuery = v.Encode()
		q, e := http.NewRequestWithContext(ctx, "PUT", u.String(), bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		q.Header.Set("Content-Type", "application/octet-stream")
		r, e = http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 201 {
			t.Fatalf("upload blob %d %s", r.StatusCode, body)
		}
	}
	putManifest := func(ref string, b []byte) {
		t.Helper()
		var v struct {
			MediaType string `json:"mediaType"`
		}
		if json.Unmarshal(b, &v) != nil {
			t.Fatal("fixture manifest")
		}
		r := request("PUT", "/v2/"+f.Repository+"/manifests/"+ref, v.MediaType, b)
		r.Body.Close()
	}
	for _, d := range []string{f.ARM, f.AMD, f.UntTagged, f.Nested, f.Index, f.Referrer} {
		putManifest(d, f.Manifests[d])
	}
	putManifest("latest", f.Manifests[f.Index])
	fallback, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": fixture.IndexMedia, "manifests": f.Referrers[f.Index]})
	putManifest("sha256-"+strings.TrimPrefix(f.Index, "sha256:"), fallback)
	target, _ := url.Parse(origin)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var gets, writes atomic.Int64
	readServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(405)
			return
		}
		gets.Add(1)
		proxy.ServeHTTP(w, r)
	}))
	defer readServer.Close()
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var store retentionTestStore
			var e error
			if kind == "local" {
				store, e = persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
			} else {
				store, e = persistence.Open(ctx, isolatedDatabase(t))
			}
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			observerToken := "registry-e2e-observer-token-0000000000000000001"
			control := "registry-e2e-control-token-00000000000000000001"
			t.Setenv("REGISTRY_E2E_OBSERVER", observerToken)
			recipe := workerconfig.RegistryObserveRecipe{ID: "observe", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "a", Origin: readServer.URL, Repository: f.Repository, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}, Limits: domain.RegistryObserveLimits{MaxResponseBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxNodes: 128, MaxEdges: 256, MaxDepth: 16, MaxPages: 32, MaxRequests: 512, TimeoutSeconds: 60}}
			recipe.Hash = workerconfig.ObservationHash(recipe)
			authority, e := workerconfig.New(workerconfig.Config{Revision: "observation1", LeaseSeconds: 30, Observations: []workerconfig.RegistryObserveRecipe{recipe}, Credentials: []workerconfig.Credential{{ID: "observer", Principal: "worker/observer", TokenEnv: "REGISTRY_E2E_OBSERVER", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"registry_observe"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if e != nil {
				t.Fatal(e)
			}
			svc := application.NewWithProvider(store, &retentionNoIOProvider{})
			svc.ConfigureWorkers(authority)
			var dropStart, dropReport atomic.Bool
			handler := transport.New(svc, transport.Options{Token: control, Workers: authority})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/worker/start" && !dropStart.Swap(true) || r.URL.Path == "/api/worker/report" && !dropReport.Swap(true) {
					rr := httptest.NewRecorder()
					handler.ServeHTTP(rr, r)
					if rr.Code != 200 {
						t.Logf("before dropped response %d %s", rr.Code, rr.Body.String())
					}
					w.WriteHeader(503)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			post := func(c domain.Command, out any) {
				t.Helper()
				c.Actor = "fixture/operator"
				b, _ := json.Marshal(c)
				r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/commands", bytes.NewReader(b))
				r.Header.Set("Authorization", "Bearer "+control)
				r.Header.Set("Content-Type", "application/json")
				resp, e := http.DefaultClient.Do(r)
				if e != nil {
					t.Fatal(e)
				}
				defer resp.Body.Close()
				b, _ = io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					t.Fatalf("%s %d %s", c.Action, resp.StatusCode, b)
				}
				if out != nil && json.Unmarshal(b, out) != nil {
					t.Fatal("invalid command response")
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "Observer fixture"}}, {Action: "create_repository", ProductID: "p", ID: "r", Data: map[string]any{"name": "Source", "url": "https://github.com/fixture/observer"}}, {Action: "create_application", ProductID: "p", ID: "a", Data: map[string]any{"name": "Component", "repository_id": "r"}}, {Action: "configure_artifact_retention", ProductID: "p", Data: map[string]any{"application_id": "a", "keep_last": 3}}} {
				post(c, nil)
			}
			image := strings.TrimPrefix(readServer.URL, "http://") + "/" + f.Repository
			for i, d := range f.Roots {
				post(domain.Command{Action: "pin_artifact", ProductID: "p", Data: map[string]any{"application_id": "a", "image_repository": image, "digest": d, "purpose": "manual", "request_key": fmt.Sprintf("observation-known-root-%d", i)}}, nil)
			}
			command := domain.Command{Action: "refresh_registry_inventory", ProductID: "p", Data: map[string]any{"application_id": "a", "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "observation1", "request_key": "observation-real-registry-1", "deadline": time.Now().Add(time.Minute)}}
			var job domain.WorkerJob
			post(command, &job)
			post(command, nil)
			stateDir, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Chmod(stateDir, 0700); e != nil {
				t.Fatal(e)
			}
			config := registryobserver.Config{Endpoint: server.URL, TokenEnv: "REGISTRY_E2E_OBSERVER", ConfigRevision: "observation1", StateDir: stateDir, Recipe: recipe}
			if _, e = registryobserver.RunOnce(ctx, config); e == nil {
				t.Fatal("lost START response not surfaced")
			}
			if _, e = registryobserver.RunOnce(ctx, config); e == nil {
				t.Fatal("lost REPORT response not surfaced")
			}
			before := gets.Load()
			if _, e = registryobserver.RunOnce(ctx, config); e != nil {
				t.Fatal("durable report replay", e)
			}
			if gets.Load() != before {
				t.Fatal("known report retry rescanned registry")
			}
			st, e := store.Read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if len(st.RegistryInventories) != 1 {
				t.Fatalf("durable inventory count %d", len(st.RegistryInventories))
			}
			inv := st.RegistryInventories[0]
			if inv.JobID != job.ID || job.Inputs.SourceSHA != "" || job.Inputs.RevisionID != "" {
				t.Fatal("wrong operation/source identity")
			}
			query := func() domain.RetentionReport {
				t.Helper()
				r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/products/p/retention", nil)
				r.Header.Set("Authorization", "Bearer "+control)
				resp, e := http.DefaultClient.Do(r)
				if e != nil {
					t.Fatal(e)
				}
				defer resp.Body.Close()
				var v domain.RetentionReport
				if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&v) != nil {
					t.Fatal("retention query")
				}
				return v
			}
			report := query()
			if len(report.Registry) != 1 || report.Registry[0].InventoryID != inv.ID || len(report.ProtectedObjects) < 10 || report.DeletionAllowed || report.GlobalInventoryComplete {
				t.Fatalf("admitted projection %+v", report.Registry)
			}
			foundUntagged, foundShared := false, false
			for _, o := range report.ProtectedObjects {
				foundUntagged = foundUntagged || o.Digest == f.UntTagged
				foundShared = foundShared || o.Digest == f.SharedBlob
			}
			if !foundUntagged || !foundShared {
				t.Fatal("untagged root or shared layers lost")
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "observer-fixture", Version: "1"}, nil)
			session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: retentionAuthTransport{control}}, DisableStandaloneSSE: true}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_artifact_retention", Arguments: map[string]any{"product_id": "p"}})
			if e != nil || result.IsError {
				t.Fatal("MCP retention", e)
			}
			b, _ := json.Marshal(result.StructuredContent)
			var viaMCP domain.RetentionReport
			if json.Unmarshal(b, &viaMCP) != nil || viaMCP.InputHash != report.InputHash {
				t.Fatal("MCP/HTTP evidence disagreement")
			}
			after, e := store.Read(ctx)
			if e != nil || len(after.Events) != len(st.Events) {
				t.Fatal("query mutated state")
			}
			if writes.Load() != 0 {
				t.Fatal("observer attempted registry write")
			}
			t.Logf("%s protected API -> durable observer -> Distribution GET -> inventory -> HTTP/MCP: nodes=%d edges=%d roots=%d status=%s discovery=%s registry_writes=%d", kind, len(inv.Report.Graph.Nodes), len(inv.Report.Graph.Edges), len(inv.Report.Graph.Roots), report.Registry[0].Status, inv.Report.Graph.Discovery, writes.Load())
		})
	}
}
