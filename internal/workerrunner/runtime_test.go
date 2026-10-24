package workerrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"releasecontrol/internal/artifactspool"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

// Opt-in: creates only labelled per-attempt containers and a temporary Git
// fixture/database. Operator images must already be cached. No registry writes.
func TestRealWorkerTwoCleanBuilds(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real isolated Docker execution requires explicit opt-in")
	}
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "fixture")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	source := `package main
import "fmt"
func main(){fmt.Println("rcp-worker-fixture")}
`
	check := `package main
import("net";"os";"testing";"time")
func TestBoundaries(t *testing.T){if os.Getenv("RCP_FIXTURE_WORKER_TOKEN")!=""{t.Fatal("credential leak")};for _,path:=range []string{"/source/.git","/var/run/docker.sock"}{if _,err:=os.Stat(path);!os.IsNotExist(err){t.Fatal("unexpected secret or socket",path)}};if err:=os.WriteFile("/source/main.go",[]byte("tamper"),0600);err==nil{t.Fatal("source writable")};conn,err:=net.DialTimeout("tcp","192.0.2.1:80",200*time.Millisecond);if err==nil{conn.Close();t.Fatal("network egress")}}
`
	for name, data := range map[string]string{"main.go": source, "main_test.go": check} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = cleanEnv()
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git failed: %v", err)
		}
		return stringTrim(b)
	}
	git("init")
	git("add", "main.go", "main_test.go")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Fixture")
	sha := git("rev-parse", "HEAD")
	workerBinary := filepath.Join(root, "worker")
	build := exec.Command("go", "build", "-o", workerBinary, "../../cmd/worker")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("worker build %v %s", err, b)
	}
	store, err := persistence.OpenLocal(ctx, filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := application.New(store)
	e := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441", Platform: "linux/arm64", CheckArgv: []string{"go", "test", "/source/main.go", "/source/main_test.go"}, BuildArgv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20}
	recipe := workerconfig.Recipe{ID: "fixture", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/fixture/source", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: e}
	recipe.Hash = workerconfig.RecipeHash(recipe)
	token := "fixture-worker-token-000000000000000001"
	authority, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{recipe}, Credentials: []workerconfig.Credential{{ID: "fixture", Principal: "worker/fixture", TokenEnv: "RCP_FIXTURE_WORKER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "control-fixture", func(string) string { return token })
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureWorkers(authority)
	for _, command := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "p"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "fixture", "goal": "build"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "fixture", "objective": "build"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "r", "url": recipe.RepositoryURL}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "fixture", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}} {
		command.Actor = "fixture/operator"
		if _, err := service.Execute(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	var starts, reports atomic.Int32
	baseHandler := transport.New(service, transport.Options{Token: "control-fixture", Workers: authority})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lose := r.URL.Path == "/api/worker/start" && starts.Add(1) == 1 || r.URL.Path == "/api/worker/report" && reports.Add(1) == 1
		if lose {
			recorded := httptest.NewRecorder()
			baseHandler.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("lost response was not accepted: %s status %d", r.URL.Path, recorded.Code)
			}
			if r.URL.Path == "/api/worker/report" {
				// Span the ordinary heartbeat period AFTER real server admission.
				// A still-running supervisor would receive the correct terminal
				// HTTP422 and cancel this request before its retry can run.
				select {
				case <-time.After(1200 * time.Millisecond):
				case <-r.Context().Done():
					t.Error("accepted REPORT response canceled by terminal heartbeat")
				}
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		baseHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	stateDir := filepath.Join(root, "worker-state")
	home, _ := os.UserHomeDir()
	spoolRoot := filepath.Join(root, "spool")
	if err := os.Mkdir(spoolRoot, 0700); err != nil {
		t.Fatal(err)
	}
	spoolRoot, _ = filepath.EvalSymlinks(spoolRoot)
	cfg := Config{ArtifactSpool: &artifactspool.Config{Root: spoolRoot, MaxArchiveBytes: 16777216, MaxTotalBytes: 67108864, MaxEntries: 4}, Endpoint: server.URL, TokenEnv: "RCP_FIXTURE_WORKER_TOKEN", ProductID: "p", RepositoryID: "r", ConfigRevision: "cfg1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: stateDir, DockerContext: "orbstack", DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true}
	configFile := filepath.Join(root, "worker.json")
	b, _ := json.Marshal(cfg)
	if err = os.WriteFile(configFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	var manifests []string
	for n := 0; n < 3; n++ {
		revisionID := "rev"
		if n == 2 {
			previous := sha
			if err = os.WriteFile(filepath.Join(repo, "main.go"), []byte(strings.ReplaceAll(source, "rcp-worker-fixture", "rcp-worker-changed")), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "main.go")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Changed fixture")
			sha = git("rev-parse", "HEAD")
			revisionID = "changed"
			if _, err = service.Execute(ctx, domain.Command{Action: "record_integration_revision", Actor: "fixture/operator", ID: revisionID, IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "fixture", "base_commit": previous, "head_commit": sha, "commits": []string{sha}}}); err != nil {
				t.Fatal(err)
			}
		}
		command := domain.Command{Action: "queue_worker_job", Actor: "fixture/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "fixture-clean-build-" + strings.Repeat("x", n+1), "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "cfg1", "revision_id": revisionID, "source_sha": sha, "parameters": map[string]any{}, "deadline": time.Now().Add(10 * time.Minute), "max_attempts": 1}}
		if _, err = service.Execute(ctx, command); err != nil {
			t.Fatal(err)
		}
		run := exec.Command(workerBinary, "run-once", configFile)
		run.Env = append(os.Environ(), "RCP_FIXTURE_WORKER_TOKEN="+token)
		out, err := run.CombinedOutput()
		if err != nil {
			t.Fatalf("real worker failed %v: %s; fixture=%s", err, out, root)
		}
		var report domain.WorkerReport
		if json.Unmarshal(out, &report) != nil || report.Result != "PASSED" || report.LocalOCI == nil {
			t.Fatalf("worker receipt invalid: %s", out)
		}

		// Explicit test-only export for the separate publisher integration probe.
		if export := os.Getenv("RCP_TEST_OCI_EXPORT"); export != "" && n == 0 {
			if !filepath.IsAbs(export) {
				t.Fatal("fixture export must be absolute")
			}
			if err := os.Mkdir(export, 0700); err != nil {
				t.Fatal(err)
			}
			matches, err := filepath.Glob(filepath.Join(stateDir, "*", "image.oci.tar"))
			if err != nil || len(matches) != 1 {
				t.Fatal("expected exact worker archive", matches, err)
			}
			data, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(export, "image.oci.tar"), data, 0600); err != nil {
				t.Fatal(err)
			}
			data, err = json.Marshal(report.LocalOCI)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(export, "receipt.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := artifactspool.Resolve(*cfg.ArtifactSpool, *report.LocalOCI); err != nil {
			t.Fatalf("report preceded durable spool: %v", err)
		}
		manifests = append(manifests, report.LocalOCI.ManifestDigest)
		if report.LocalOCI.SourceSHA != sha {
			t.Fatal("compiled source provenance mismatch")
		}
		assertOwnedResourcesGone(t, cfg)
	}
	if manifests[0] != manifests[1] {
		t.Fatal("clean fixture builds not deterministic", manifests)
	}
	const expectedManifest = "sha256:b53328e30a220c8a754ee29df1dfefd23280c11f0c310418d81796c8729058e9"
	if manifests[0] != expectedManifest {
		t.Fatal("pinned fixture manifest differs from verified golden identity", manifests[0])
	}
	if manifests[2] == manifests[0] {
		t.Fatal("changed source did not change output identity")
	}
	state, err := store.Read(ctx)
	if err != nil || len(state.WorkerJobs) != 3 || len(state.WorkerAttempts) != 3 || len(state.DeliveryArtifacts) != 0 || len(state.DeliveryBuildRuns) != 0 {
		t.Fatal("local output incorrectly became published build/artifact", err)
	}
	for _, a := range state.WorkerAttempts {
		if a.Status != "SUCCEEDED" || a.Report == nil || a.Report.LocalOCI == nil {
			t.Fatal("server lost typed result")
		}
	}
	startEvents, reportEvents := 0, 0
	for _, event := range state.Events {
		if event.Action == "worker_start" {
			startEvents++
		}
		if event.Action == "worker_report" {
			reportEvents++
		}
	}
	if starts.Load() < 4 || reports.Load() < 4 || startEvents != 3 || reportEvents != 3 {
		t.Fatal("lost responses caused missing replay or duplicate transitions", starts.Load(), reports.Load(), startEvents, reportEvents)
	}
	t.Logf("real worker: two clean manifests=%s; changed source manifest=%s; lost start/report responses recovered; no registry publication", manifests[0], manifests[2])
}

func assertOwnedResourcesGone(t *testing.T, cfg Config) {
	t.Helper()
	entries, err := os.ReadDir(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	d := docker{context: cfg.DockerContext, config: cfg.DockerConfig}
	for _, entry := range entries {
		if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
			continue
		}
		j, err := readJournal(filepath.Join(cfg.StateDir, entry.Name(), "journal.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !j.Done {
			t.Fatal("successful execution did not close its resource journal")
		}
		for _, resource := range j.Resources {
			if !containerIDPattern.MatchString(resource.ID) || resource.CreateState != "REMOVED" {
				t.Fatal("cleanup missing exact durable container identity", resource)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		b, err := d.output(ctx, nil, 1024, "container", "ls", "--all", "--filter", "label=rcp.worker.owner="+j.Owner, "--format", "{{.ID}}")
		cancel()
		if err != nil || stringTrim(b) != "" {
			t.Fatalf("owned resources remain owner=%s: %s %v", j.Owner, b, err)
		}
	}
}
