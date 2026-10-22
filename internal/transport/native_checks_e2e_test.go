package transport_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/persistence"
	githubprovider "releasecontrol/internal/providers/github"
	"releasecontrol/internal/publishrunner"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerclient"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
)

type nativeChecksProvider struct {
	*nativePRFixture
	adapter *githubprovider.Provider
}

func (p *nativeChecksProvider) FindCheckRun(c context.Context, x delivery.Connection, i delivery.CheckRunIdentity) (delivery.CheckRunObservation, error) {
	return p.adapter.FindCheckRun(c, x, i)
}
func (p *nativeChecksProvider) ReadCheckRun(c context.Context, x delivery.Connection, i delivery.CheckRunIdentity, id int64) (delivery.CheckRunObservation, error) {
	return p.adapter.ReadCheckRun(c, x, i, id)
}
func (p *nativeChecksProvider) CreateCheckRun(c context.Context, x delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	return p.adapter.CreateCheckRun(c, x, w)
}
func (p *nativeChecksProvider) UpdateCheckRun(c context.Context, x delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	return p.adapter.UpdateCheckRun(c, x, w)
}

type nativeChecksHTTP struct {
	mu                             sync.Mutex
	runs                           map[int64]map[string]any
	creates, patches, reads, lists int
	lostCreate, lostPatch          bool
	statuses                       []string
}

func newNativeChecksHTTP(t *testing.T) (*nativeChecksHTTP, *httptest.Server) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("RCP_NATIVE_CHECK_APP_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	f := &nativeChecksHTTP{runs: map[int64]map[string]any{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/2/access_tokens" {
			var scope struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&scope)
			if len(scope.Repositories) != 1 || scope.Repositories[0] != "source" || len(scope.Permissions) != 1 || scope.Permissions["checks"] != "write" {
				t.Error("check token widened source scope")
				w.WriteHeader(403)
				return
			}
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				w.WriteHeader(401)
				return
			}
			signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature) != nil {
				t.Error("invalid App JWT")
				w.WriteHeader(401)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"token": "native-check-fixture-token", "expires_at": time.Now().Add(time.Hour), "permissions": map[string]string{"checks": "write", "metadata": "read"}, "repositories": []map[string]any{{"id": 11, "full_name": "team/source"}}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer native-check-fixture-token" || !strings.HasPrefix(r.URL.Path, "/repos/team/source/") {
			t.Error("wrong source check endpoint/token")
			w.WriteHeader(403)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		lost := false
		var result any
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/team/source/check-runs":
			f.creates++
			var v map[string]any
			if json.NewDecoder(r.Body).Decode(&v) != nil {
				w.WriteHeader(400)
				return
			}
			id := int64(len(f.runs) + 1)
			v["id"] = id
			v["app"] = map[string]any{"id": 1}
			f.runs[id] = v
			f.statuses = append(f.statuses, fmt.Sprint(v["status"]))
			result = v
			if !f.lostCreate {
				f.lostCreate = true
				lost = true
			}
		case r.Method == "PATCH":
			f.patches++
			var id int64
			fmt.Sscanf(r.URL.Path, "/repos/team/source/check-runs/%d", &id)
			v := f.runs[id]
			if v == nil {
				w.WriteHeader(404)
				return
			}
			var patch map[string]any
			json.NewDecoder(r.Body).Decode(&patch)
			for key, value := range patch {
				v[key] = value
			}
			f.statuses = append(f.statuses, fmt.Sprint(v["status"])+":"+fmt.Sprint(v["conclusion"]))
			result = v
			if !f.lostPatch && v["status"] == "in_progress" {
				f.lostPatch = true
				lost = true
			}
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/commits/"):
			f.lists++
			q := r.URL.Query()
			if q.Get("check_name") != "RCP / checks and build" || q.Get("app_id") != "1" || q.Get("filter") != "all" {
				t.Error("unscoped recovery listing")
			}
			runs := []any{map[string]any{"external_id": "unrelated", "app": map[string]int{"id": 999}}}
			for _, v := range f.runs {
				if strings.Contains(r.URL.Path, "/commits/"+fmt.Sprint(v["head_sha"])+"/") {
					runs = append(runs, v)
				}
			}
			result = map[string]any{"total_count": len(runs), "check_runs": runs}
		case r.Method == "GET":
			f.reads++
			var id int64
			fmt.Sscanf(r.URL.Path, "/repos/team/source/check-runs/%d", &id)
			result = f.runs[id]
			if result == nil {
				w.WriteHeader(404)
				return
			}
		default:
			t.Error("unsupported check fixture request")
			w.WriteHeader(400)
			return
		}
		if lost {
			connection, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				connection.Close()
			}
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(server.Close)
	t.Setenv("RCP_GITHUB_API_URL", server.URL)
	return f, server
}

// Real small-source worker/publisher processes use the ordinary protected API.
// Only Git/PR ownership is simulated; checks exercise the concrete App HTTP
// adapter, narrow tokens, uncertain reply observation, and durable core gate.
func TestRealNativeChecksWorkerPublisherPR(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("explicit owned Docker fixture opt-in required")
	}
	ctx := context.Background()
	root := t.TempDir()
	binaries := map[string]string{}
	for _, name := range []string{"worker", "publisher"} {
		file := filepath.Join(root, name)
		if out, e := exec.Command("go", "build", "-o", file, "../../cmd/"+name).CombinedOutput(); e != nil {
			t.Fatalf("executor build:%v %s", e, out)
		}
		binaries[name] = file
	}
	docker := func(args ...string) string {
		t.Helper()
		out, e := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("owned checks Docker:%v %s", e, out)
		}
		return strings.TrimSpace(string(out))
	}
	owner := fmt.Sprintf("native-checks-%d", time.Now().UnixNano())
	registry := docker("run", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(registry) {
		t.Fatal("exact registry ID required")
	}
	t.Cleanup(func() {
		if docker("inspect", "--format", `{{.Id}} {{index .Config.Labels "rcp.fixture.owner"}}`, registry) != registry+" "+owner {
			t.Error("registry ownership changed")
			return
		}
		docker("rm", "--force", registry)
	})
	origin := "http://" + docker("port", registry, "5000/tcp")
	t.Logf("owned check registry owner=%s id=%s", owner, registry)
	for range 50 {
		res, e := http.Get(origin + "/v2/")
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			repo := filepath.Join(dir, "source")
			os.Mkdir(repo, 0700)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				out, e := cmd.CombinedOutput()
				if e != nil {
					t.Fatalf("fixture Git:%v %s", e, out)
				}
				return strings.TrimSpace(string(out))
			}
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main(){}\n"), 0600)
			git("init")
			git("add", "main.go")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Checks fixture")
			sha := git("rev-parse", "HEAD")
			var store interface {
				application.Store
				Close()
			}
			var err error
			if kind == "local" {
				store, err = persistence.OpenLocal(ctx, filepath.Join(dir, "state.db"))
			} else {
				store, err = persistence.Open(ctx, isolatedDatabase(t))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			execution := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441", Platform: "linux/arm64", CheckArgv: []string{"go", "test", "/source/main.go"}, BuildArgv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20}
			recipe := workerconfig.Recipe{ID: "build", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/team/source", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: execution, Check: &domain.WorkerCheckBinding{Name: "RCP / checks and build", ExpectedAppID: 1, FinalizeSeconds: 300}}
			recipe.Hash = workerconfig.RecipeHash(recipe)
			publication := workerconfig.PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "checks-fixture", BuildRecipeHash: recipe.Hash, Origin: origin, Repository: "fixture/checks", MaxBytes: 16 << 20, TimeoutSeconds: 30, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
			publication.Hash = workerconfig.PublicationHash(publication)
			control := "native-checks-control-fixture-000000000001"
			buildToken := "native-checks-build-fixture-000000000001"
			publishToken := "native-checks-publish-fixture-000000000001"
			t.Setenv("CHECKS_BUILD_TOKEN", buildToken)
			t.Setenv("CHECKS_PUBLISH_TOKEN", publishToken)
			authority, err := workerconfig.New(workerconfig.Config{Revision: "checks1", LeaseSeconds: 60, Recipes: []workerconfig.Recipe{recipe}, Publications: []workerconfig.PublicationRecipe{publication}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "CHECKS_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "CHECKS_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			checks, checkServer := newNativeChecksHTTP(t)
			provider := &nativeChecksProvider{nativePRFixture: &nativePRFixture{persistedDeliveryFixture: &persistedDeliveryFixture{}, sha: sha}, adapter: githubprovider.NewWithClient(checkServer.Client())}
			service := application.NewWithProvider(store, provider)
			service.ConfigureWorkers(authority)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			base := transport.New(service, transport.Options{Token: control, Workers: authority})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/worker/start" && r.Header.Get("Authorization") == "Bearer "+buildToken {
					rec := httptest.NewRecorder()
					base.ServeHTTP(rec, r)
					if rec.Code == 200 {
						once.Do(func() { close(started) })
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					w.WriteHeader(rec.Code)
					w.Write(rec.Body.Bytes())
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			post := func(c domain.Command, out any) { realApplicationCommand(t, server.URL, control, c, out) }
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "checks fixture"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "checks", "goal": "exact source"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "checks", "objective": "test"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "local checks fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:RCP_NATIVE_CHECK_APP_KEY", "owner": "team", "api_url": checkServer.URL}}} {
				post(c, nil)
			}
			if _, err = service.Query(ctx, "discover_repositories", "conn"); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "team/" + r.id, "role": r.role, "default_branch": "main"}}, nil)
			}
			for _, c := range []domain.Command{{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "fixture", "repository_id": "source"}}, {Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "native": map[string]any{"build_recipe_id": recipe.ID, "publication_recipe_id": publication.ID, "parameters": map[string]any{}}, "profile": publication.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/fixture/checks"}}, {Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "private fixture"}}, {Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}} {
				post(c, nil)
			}
			tick := func() {
				t.Helper()
				if err := store.Update(ctx, func(st *domain.State) error {
					now := time.Now().UTC()
					for i := range st.Operations {
						if st.Operations[i].NextAttemptAt.After(now) {
							st.Operations[i].NextAttemptAt = now
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			until := func(predicate func(domain.State) bool) {
				t.Helper()
				for range 35 {
					st, err := store.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if predicate(st) {
						return
					}
					tick()
				}
				st, _ := store.Read(ctx)
				t.Fatalf("checks graph did not advance: %+v", st.Operations)
			}
			spoolRoot := filepath.Join(dir, "spool")
			os.Mkdir(spoolRoot, 0700)
			spool := &artifactspool.Config{Root: spoolRoot, MaxArchiveBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxEntries: 8}
			home, _ := os.UserHomeDir()
			workerFile := filepath.Join(dir, "worker.json")
			publisherFile := filepath.Join(dir, "publisher.json")
			pubDir := filepath.Join(dir, "publisher")
			os.Mkdir(pubDir, 0700)
			for file, v := range map[string]any{workerFile: workerrunner.Config{Endpoint: server.URL, TokenEnv: "CHECKS_BUILD_TOKEN", ProductID: "p", RepositoryID: "source", ConfigRevision: "checks1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: filepath.Join(dir, "worker"), DockerContext: "orbstack", DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true, ArtifactSpool: spool}, publisherFile: publishrunner.Config{Endpoint: server.URL, TokenEnv: "CHECKS_PUBLISH_TOKEN", ConfigRevision: "checks1", Recipe: publication, ArtifactSpool: spool, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: publication.Repository, StateDir: pubDir, MaxArchiveBytes: publication.MaxBytes, TimeoutSeconds: publication.TimeoutSeconds, FixtureLoopbackHTTP: true}}} {
				b, _ := json.Marshal(v)
				os.WriteFile(file, b, 0600)
			}
			var deploy domain.ExternalOperation
			post(domain.Command{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "rev", "request_key": "checks-managed-request-0001"}}, &deploy)
			until(func(st domain.State) bool { return len(st.WorkerJobs) == 1 })
			client, err := workerclient.New(server.URL, buildToken)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := client.Claim(ctx, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
			if err != nil || lease != nil {
				t.Fatalf("claim before observed queued check:%+v %v", lease, err)
			}
			observed := func(st domain.State, status, conclusion string) bool {
				for _, op := range st.Operations {
					if op.Kind == "CHECK_PUBLICATION" && op.CheckPublication != nil && op.CheckPublication.Observed != nil && op.CheckPublication.Observed.Status == status && op.CheckPublication.Observed.Conclusion == conclusion {
						return true
					}
				}
				return false
			}
			until(func(st domain.State) bool { return observed(st, "queued", "") })
			cmd := exec.Command(binaries["worker"], "run-once", workerFile)
			type workerResult struct {
				out []byte
				err error
			}
			done := make(chan workerResult, 1)
			go func() { out, err := cmd.CombinedOutput(); done <- workerResult{out, err} }()
			defer func() {
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
			}()
			select {
			case <-started:
			case result := <-done:
				t.Fatalf("worker did not start:%v %s", result.err, result.out)
			case <-time.After(time.Minute):
				t.Fatal("worker start barrier timed out")
			}
			until(func(st domain.State) bool { return observed(st, "in_progress", "") })
			unblock()
			result := <-done
			if result.err != nil {
				t.Fatalf("real worker:%v %s", result.err, result.out)
			}
			until(func(st domain.State) bool { return observed(st, "completed", "success") })
			until(func(st domain.State) bool { return len(st.WorkerJobs) == 2 })
			if out, err := exec.Command(binaries["publisher"], "run-once", publisherFile).CombinedOutput(); err != nil {
				t.Fatalf("real publisher:%v %s", err, out)
			}
			until(func(st domain.State) bool {
				for _, op := range st.Operations {
					if op.ID == deploy.ID && op.Phase == "WAIT_GITOPS_PR" {
						return true
					}
				}
				return false
			})
			checks.mu.Lock()
			if checks.creates != 1 || checks.lists < 1 || checks.reads < 1 || checks.patches != 2 {
				t.Errorf("uncertain write repeated: creates%d patches%d lists%d reads%d", checks.creates, checks.patches, checks.lists, checks.reads)
			}
			checks.mu.Unlock()
			if provider.proposals.Load() != 1 || provider.dispatches.Load() != 0 || provider.inspections.Load() != 0 || provider.applies.Load() != 0 {
				t.Fatal("checks native path dispatched/fell back/direct/duplicated PR")
			}
			provider.merged = true
			tick()
			// Same fixed recipe with an actual syntax error exercises a failed real
			// source check; it must publish failure and never queue a registry job/PR.
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nthis is invalid Go\n"), 0600)
			git("add", "main.go")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Failing check fixture")
			sha = git("rev-parse", "HEAD")
			provider.sha = sha
			provider.merged = false
			post(domain.Command{Action: "record_integration_revision", ID: "bad-rev", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}, nil)
			var failed domain.ExternalOperation
			post(domain.Command{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "bad-rev", "request_key": "checks-failed-request-0001"}}, &failed)
			until(func(st domain.State) bool {
				for _, j := range st.WorkerJobs {
					if j.Inputs.SourceSHA == sha && j.CheckOperationID != "" {
						for _, op := range st.Operations {
							if op.ID == j.CheckOperationID && op.CheckPublication.Observed != nil {
								return true
							}
						}
					}
				}
				return false
			})
			if out, err := exec.Command(binaries["worker"], "run-once", workerFile).CombinedOutput(); err == nil {
				t.Fatalf("invalid Go checks passed:%s", out)
			}
			until(func(st domain.State) bool {
				for _, j := range st.WorkerJobs {
					if j.Inputs.SourceSHA == sha {
						for _, op := range st.Operations {
							if op.ID == j.CheckOperationID && op.CheckPublication.Observed != nil && op.CheckPublication.Observed.Status == "completed" && op.CheckPublication.Observed.Conclusion == "failure" {
								return true
							}
						}
					}
				}
				return false
			})
			st, _ := store.Read(ctx)
			if len(st.WorkerJobs) != 3 || len(st.DeliveryArtifacts) != 1 || provider.proposals.Load() != 1 {
				t.Fatal("failed check reached publication/artifact/PR")
			}
			// Lightweight protected standalone API cases share the real adapter;
			// they never execute or manufacture a PASSED report. Only the lease
			// clock is advanced to simulate an executor disappearing after START.
			for _, mode := range []string{"cancelled", "expired", "unknown"} {
				deadline := time.Now().Add(time.Minute)
				if mode == "expired" {
					deadline = time.Now().Add(500 * time.Millisecond)
				}
				var job domain.WorkerJob
				post(domain.Command{Action: "queue_worker_job", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "checks-protected-api-" + mode, "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "checks1", "revision_id": "bad-rev", "source_sha": sha, "parameters": map[string]any{}, "deadline": deadline, "max_attempts": 1}}, &job)
				checkFor := func(st domain.State, status, conclusion string) bool {
					for _, op := range st.Operations {
						if op.ID == job.CheckOperationID && op.CheckPublication != nil && op.CheckPublication.Observed != nil {
							return op.CheckPublication.Observed.Status == status && op.CheckPublication.Observed.Conclusion == conclusion
						}
					}
					return false
				}
				until(func(st domain.State) bool { return checkFor(st, "queued", "") })
				switch mode {
				case "cancelled":
					post(domain.Command{Action: "cancel_worker_job", ProductID: "p", ID: job.ID}, nil)
					until(func(st domain.State) bool { return checkFor(st, "completed", "cancelled") })
				case "expired":
					time.Sleep(550 * time.Millisecond)
					until(func(st domain.State) bool { return checkFor(st, "completed", "timed_out") })
				case "unknown":
					lease, err := client.Claim(ctx, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
					if err != nil || lease == nil || lease.Job.ID != job.ID {
						t.Fatalf("unknown fixture claim:%+v %v", lease, err)
					}
					request := domain.WorkerAttemptRequest{JobID: lease.Job.ID, AttemptID: lease.Attempt.ID, Fence: lease.Attempt.Fence, InputHash: lease.Job.InputHash}
					if _, err = client.Start(ctx, request); err != nil {
						t.Fatal(err)
					}
					until(func(st domain.State) bool { return checkFor(st, "in_progress", "") })
					if err = store.Update(ctx, func(st *domain.State) error {
						for i := range st.WorkerAttempts {
							if st.WorkerAttempts[i].ID == lease.Attempt.ID {
								st.WorkerAttempts[i].LeaseUntil = time.Now().Add(-time.Second)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					tick()
					tick()
					current, _ := store.Read(ctx)
					if !checkFor(current, "in_progress", "") {
						t.Fatal("unknown executor became terminal/green")
					}
					for _, j := range current.WorkerJobs {
						if j.ID == job.ID && j.Status != "RECONCILIATION_REQUIRED" {
							t.Fatal("unknown executor was replayed/resolved")
						}
					}
					post(domain.Command{Action: "cancel_worker_job", ProductID: "p", ID: job.ID}, nil)
					until(func(st domain.State) bool { return checkFor(st, "completed", "cancelled") })
				}
				current, _ := store.Read(ctx)
				if len(current.DeliveryArtifacts) != 1 || provider.proposals.Load() != 1 {
					t.Fatal("non-success standalone check reached artifact/PR")
				}
			}
			t.Logf("%s exact-SHA queued/inprogress/success, lost CREATE/PATCH observation, real failed checks/no publication PASS", kind)
		})
	}
}
