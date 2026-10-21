package workerrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

func TestRealWorkerFailureCancellationExpiryKillAndRestart(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real Docker lifecycle tests require explicit opt-in")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "worker")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/worker")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture worker: %v %s", err, b)
	}
	for _, scenario := range []string{"failure", "timeout", "cancel", "expiry", "kill"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			repo := filepath.Join(dir, "fixture")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				cmd.Env = cleanEnv()
				b, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatal("fixture Git", err)
				}
				return stringTrim(b)
			}
			git("init")
			git("add", "input.txt")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Fixture")
			sha := git("rev-parse", "HEAD")
			check := []string{"/bin/sleep", "30"}
			seconds := 60
			if scenario == "failure" {
				check = []string{"/bin/false"}
			}
			if scenario == "timeout" {
				seconds = 2
			}
			e := &domain.WorkerExecution{Kind: "source_check", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", Platform: "linux/arm64", CheckArgv: check, Network: "none", TimeoutSeconds: seconds, MemoryMB: 256, StorageMB: 32, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 4096}
			recipe := workerconfig.Recipe{ID: "fixture", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/fixture/source", Capability: "source_check", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: e}
			recipe.Hash = workerconfig.RecipeHash(recipe)
			token := "fixture-worker-token-000000000000000001"
			authority, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{recipe}, Credentials: []workerconfig.Credential{{ID: "fixture", Principal: "worker/fixture", TokenEnv: "RCP_FIXTURE_WORKER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"source_check"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "control-fixture", func(string) string { return token })
			if err != nil {
				t.Fatal(err)
			}
			store, err := persistence.OpenLocal(ctx, filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service := application.New(store)
			service.ConfigureWorkers(authority)
			for _, command := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "p"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "fixture", "goal": "check"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "fixture", "objective": "check"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "r", "url": recipe.RepositoryURL}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "fixture", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}} {
				command.Actor = "fixture/operator"
				if _, err := service.Execute(ctx, command); err != nil {
					t.Fatal(err)
				}
			}
			base := transport.New(service, transport.Options{Token: "control-fixture", Workers: authority})
			releaseBlockedHeartbeat := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "expiry" && r.URL.Path == "/api/worker/heartbeat" {
					select {
					case <-r.Context().Done():
					case <-releaseBlockedHeartbeat:
					}
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			defer close(releaseBlockedHeartbeat)
			home, _ := os.UserHomeDir()
			cfg := Config{Endpoint: server.URL, TokenEnv: "RCP_FIXTURE_WORKER_TOKEN", ProductID: "p", RepositoryID: "r", ConfigRevision: "cfg1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: filepath.Join(dir, "state"), DockerContext: "orbstack", DockerConfig: filepath.Join(home, ".docker")}
			configFile := filepath.Join(dir, "worker.json")
			b, _ := json.Marshal(cfg)
			if err = os.WriteFile(configFile, b, 0600); err != nil {
				t.Fatal(err)
			}
			v, err := service.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "fixture/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "fixture-lifecycle-" + scenario, "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": sha, "parameters": map[string]any{}, "deadline": time.Now().Add(time.Minute), "max_attempts": 1}})
			if err != nil {
				t.Fatal(err)
			}
			job := v.(domain.WorkerJob)
			run := exec.Command(binary, "run-once", configFile)
			run.Env = append(os.Environ(), "RCP_FIXTURE_WORKER_TOKEN="+token)
			var output bytes.Buffer
			run.Stdout = &output
			run.Stderr = &output
			if err = run.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- run.Wait() }()
			if scenario == "cancel" || scenario == "kill" || scenario == "expiry" {
				waitForOwnedRunning(t, cfg)
				if scenario == "cancel" {
					if _, err = service.Execute(ctx, domain.Command{Action: "cancel_worker_job", Actor: "fixture/operator", ProductID: "p", ID: job.ID}); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "kill" {
					if err = run.Process.Kill(); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case err = <-waited:
				if err == nil {
					t.Fatal("negative execution unexpectedly succeeded")
				}
			case <-time.After(15 * time.Second):
				_ = run.Process.Kill()
				<-waited
				t.Fatalf("worker failed to stop: %s", output.String())
			}
			// Watchdog is a separate process; after SIGKILL it, not worker defers,
			// must stop the exact resources while the supervisor is dead.
			deadline := time.Now().Add(8 * time.Second)
			for {
				if ownedResourcesGone(cfg) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("watchdog left owned resources running")
				}
				time.Sleep(100 * time.Millisecond)
			}
			principal, err := authority.Authenticate(token, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "failure" {
				state, _ := store.Read(ctx)
				if wait := time.Until(state.WorkerAttempts[0].LeaseUntil) + 20*time.Millisecond; wait > 0 {
					time.Sleep(wait)
				}
				if claim, err := service.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"}); err != nil || claim != nil {
					t.Fatal("uncertain execution automatically reclaimed", err)
				}
			}
			before, _ := store.Read(ctx)
			if scenario == "failure" {
				if before.WorkerJobs[0].Status != "FAILED" || before.WorkerAttempts[0].Report == nil || before.WorkerAttempts[0].Report.Result != "CHECK_FAILED" {
					t.Fatalf("actual failure not recorded: %+v %s", before.WorkerJobs, output.String())
				}
			} else if before.WorkerJobs[0].Status != "RECONCILIATION_REQUIRED" {
				t.Fatalf("uncertain lifecycle not reconciled: %s %s", before.WorkerJobs[0].Status, output.String())
			}
			restart := exec.Command(binary, "run-once", configFile)
			restart.Env = run.Env
			if b, err = restart.CombinedOutput(); err != nil {
				t.Fatalf("orphan recovery/restart failed %v %s", err, b)
			}
			after, _ := store.Read(ctx)
			if len(after.WorkerAttempts) != 1 || after.WorkerJobs[0].Fence != 1 || len(after.DeliveryArtifacts) != 0 {
				t.Fatal("restart repeated uncertain execution")
			}
			assertOwnedResourcesGone(t, cfg)
			t.Logf("actual %s: exact resources removed; state=%s; restart attempts=1", scenario, after.WorkerJobs[0].Status)
		})
	}
}
func waitForOwnedRunning(t *testing.T, c Config) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	d := docker{context: c.DockerContext, config: c.DockerConfig}
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(c.StateDir)
		for _, entry := range entries {
			if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
				continue
			}
			j, err := readJournal(filepath.Join(c.StateDir, entry.Name(), "journal.json"))
			if err != nil {
				continue
			}
			for _, resource := range j.Resources {
				if resource.ID == "" {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				b, err := d.output(ctx, nil, 128, "inspect", "--format", "{{.State.Running}}", resource.ID)
				cancel()
				if err == nil && stringTrim(b) == "true" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("execution container never became running")
}
func ownedResourcesGone(c Config) bool {
	entries, _ := os.ReadDir(c.StateDir)
	d := docker{context: c.DockerContext, config: c.DockerConfig}
	for _, entry := range entries {
		if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
			continue
		}
		j, err := readJournal(filepath.Join(c.StateDir, entry.Name(), "journal.json"))
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		b, err := d.output(ctx, nil, 1024, "container", "ls", "--all", "--filter", "label=rcp.worker.owner="+j.Owner, "--format", "{{.ID}}")
		cancel()
		if err != nil || stringTrim(b) != "" {
			return false
		}
	}
	return true
}
