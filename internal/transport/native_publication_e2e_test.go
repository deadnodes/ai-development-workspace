package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/publishrunner"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type nativePRFixture struct {
	*persistedDeliveryFixture
	sha                    string
	proposals, inspections atomic.Int32
}

func (f *nativePRFixture) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	return []delivery.BranchInfo{{Name: "main", SHA: f.sha}, {Name: "feature/work", SHA: f.sha}}, nil
}
func (f *nativePRFixture) Compare(context.Context, delivery.Connection, string, string, string) (delivery.CompareResult, error) {
	return delivery.CompareResult{Status: "identical"}, nil
}
func (f *nativePRFixture) InspectArtifact(context.Context, delivery.Connection, delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	f.inspections.Add(1)
	return delivery.ArtifactResult{}, fmt.Errorf("GitHub fallback forbidden")
}
func (f *nativePRFixture) ProposeGitOps(_ context.Context, _ delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	if a.Digest == "" || a.ExpectedHeadSHA == "" || a.ExpectedBlobSHA == "" {
		return delivery.GitOpsPullRequest{}, fmt.Errorf("unpinned GitOps")
	}
	f.proposals.Add(1)
	return delivery.GitOpsPullRequest{Number: 9, URL: "https://github.test/fixture/pull/9", HeadSHA: f.sha, State: "open"}, nil
}
func (f *nativePRFixture) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, p delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	return p, nil
}

// Explicit opt-in owns one exact-ID tmpfs registry and per-attempt build
// containers. No live RCP, registry credentials or application targets are used.
func TestRealNativeAPIWorkerPublisherDistributionPR(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("explicit Docker fixture opt-in required")
	}
	ctx := context.Background()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	binaries := map[string]string{}
	for _, name := range []string{"worker", "publisher"} {
		file := filepath.Join(root, name)
		cmd := exec.Command("go", "build", "-o", file, "../../cmd/"+name)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build %s: %v %s", name, e, b)
		}
		binaries[name] = file
	}
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("owned Docker fixture: %v %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	owner := fmt.Sprintf("native-e2e-%d", time.Now().UnixNano())
	id := docker("run", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Fatal("invalid owned container identity")
	}
	t.Cleanup(func() { docker("rm", "--force", id) })
	origin := "http://" + docker("port", id, "5000/tcp")
	t.Logf("owned Distribution fixture owner=%s id=%s origin=%s", owner, id, origin)
	ready := false
	for range 50 {
		res, e := http.Get(origin + "/v2/")
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Distribution not ready")
	}
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			repo := filepath.Join(dir, "source")
			os.Mkdir(repo, 0700)
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main(){}\n"), 0600)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				b, e := cmd.CombinedOutput()
				if e != nil {
					t.Fatalf("fixture Git %v %s", e, b)
				}
				return strings.TrimSpace(string(b))
			}
			git("init")
			git("add", "main.go")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Fixture")
			sha := git("rev-parse", "HEAD")
			var store interface {
				application.Store
				Close()
			}
			var err error
			if kind == "local" {
				store, err = persistence.OpenLocal(ctx, filepath.Join(dir, "rcp.db"))
			} else {
				store, err = persistence.Open(ctx, isolatedDatabase(t))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			e := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441", Platform: "linux/arm64", CheckArgv: []string{"go", "test", "/source/main.go"}, BuildArgv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20}
			recipe := workerconfig.Recipe{ID: "build", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/team/source", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: e}
			recipe.Hash = workerconfig.RecipeHash(recipe)
			publication := workerconfig.PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "native-test", BuildRecipeHash: recipe.Hash, Origin: origin, Repository: "fixture/image", MaxBytes: 16 << 20, TimeoutSeconds: 30, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
			publication.Hash = workerconfig.PublicationHash(publication)
			buildToken := "native-e2e-build-token-000000000000000001"
			publishToken := "native-e2e-publish-token-000000000000001"
			control := "native-e2e-control-token-000000000000001"
			t.Setenv("NATIVE_E2E_BUILD", buildToken)
			t.Setenv("NATIVE_E2E_PUBLISH", publishToken)
			authority, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{recipe}, Publications: []workerconfig.PublicationRecipe{publication}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_E2E_BUILD", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_E2E_PUBLISH", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			provider := &nativePRFixture{persistedDeliveryFixture: &persistedDeliveryFixture{}, sha: sha}
			service := application.NewWithProvider(store, provider)
			service.ConfigureWorkers(authority)
			var starts, reports atomic.Int32
			base := transport.New(service, transport.Options{Token: control, Workers: authority})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer "+publishToken {
					lose := r.URL.Path == "/api/worker/start" && starts.Add(1) == 1 || r.URL.Path == "/api/worker/report" && reports.Add(1) == 1
					if lose {
						recorded := httptest.NewRecorder()
						base.ServeHTTP(recorded, r)
						if recorded.Code != 200 {
							t.Errorf("lost response action rejected: %d %s", recorded.Code, recorded.Body.String())
						}
						conn, _, e := w.(http.Hijacker).Hijack()
						if e == nil {
							conn.Close()
						}
						return
					}
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			post := func(c domain.Command, out any) {
				t.Helper()
				c.Actor = "fixture/operator"
				if c.Data == nil {
					c.Data = map[string]any{}
				}
				b, _ := json.Marshal(c)
				req, _ := http.NewRequest("POST", server.URL+"/api/commands", bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+control)
				res, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				defer res.Body.Close()
				b, _ = io.ReadAll(res.Body)
				if res.StatusCode != 200 {
					t.Fatalf("%s: %d %s", c.Action, res.StatusCode, b)
				}
				if out != nil && json.Unmarshal(b, out) != nil {
					t.Fatal("invalid command response")
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "native fixture"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "native fixture", "goal": "test"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "native fixture", "objective": "test"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:UNUSED_FIXTURE_KEY", "owner": "team"}}} {
				post(c, nil)
			}
			if _, err = service.Query(ctx, "discover_repositories", "conn"); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "team/" + r.id, "role": r.role, "default_branch": "main"}}, nil)
			}
			for _, c := range []domain.Command{{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "fixture", "repository_id": "source"}}, {Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE_EXISTING_ARTIFACT", "profile": publication.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/fixture/image"}}, {Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "fixture Dev"}}, {Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}} {
				post(c, nil)
			}
			var buildJob domain.WorkerJob
			post(domain.Command{Action: "queue_worker_job", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "native-e2e-build-request", "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": sha, "parameters": map[string]any{}, "deadline": time.Now().Add(10 * time.Minute), "max_attempts": 1}}, &buildJob)
			home, _ := os.UserHomeDir()
			workerCfg := workerrunner.Config{Endpoint: server.URL, TokenEnv: "NATIVE_E2E_BUILD", ProductID: "p", RepositoryID: "source", ConfigRevision: "cfg1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: filepath.Join(dir, "worker-state"), DockerContext: "orbstack", DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true}
			workerFile := filepath.Join(dir, "worker.json")
			b, _ := json.Marshal(workerCfg)
			os.WriteFile(workerFile, b, 0600)
			run := func(binary, file string) error {
				cmd := exec.Command(binary, "run-once", file)
				b, e := cmd.CombinedOutput()
				if e != nil {
					t.Logf("native subprocess result: %v %s", e, b)
				}
				return e
			}
			if err = run(binaries["worker"], workerFile); err != nil {
				t.Fatal("real worker failed", err)
			}
			st, _ := store.Read(ctx)
			if len(st.WorkerAttempts) != 1 || st.WorkerAttempts[0].Report == nil || st.WorkerAttempts[0].Report.LocalOCI == nil || st.WorkerJobs[0].Status != "SUCCEEDED" {
				t.Fatal("real accepted build missing")
			}
			receipt := *st.WorkerAttempts[0].Report.LocalOCI
			archives, _ := filepath.Glob(filepath.Join(workerCfg.StateDir, "*", "image.oci.tar"))
			if len(archives) != 1 {
				t.Fatal("exact owned archive missing")
			}
			pubDir := filepath.Join(dir, "publisher-state")
			os.Mkdir(pubDir, 0700)
			publisherCfg := publishrunner.Config{Endpoint: server.URL, TokenEnv: "NATIVE_E2E_PUBLISH", ConfigRevision: "cfg1", Recipe: publication, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: publication.Repository, Archive: archives[0], StateDir: pubDir, MaxArchiveBytes: publication.MaxBytes, TimeoutSeconds: publication.TimeoutSeconds, FixtureLoopbackHTTP: true}}
			pubFile := filepath.Join(dir, "publisher.json")
			b, _ = json.Marshal(publisherCfg)
			os.WriteFile(pubFile, b, 0600)
			queue := func(key string) {
				post(domain.Command{Action: "queue_worker_publication", ProductID: "p", Data: map[string]any{"build_job_id": buildJob.ID, "component_id": "component", "recipe_id": publication.ID, "recipe_hash": publication.Hash, "recipe_revision": publication.Revision, "config_revision": "cfg1", "request_key": key, "deadline": time.Now().Add(time.Minute)}}, nil)
			}
			queue("native-e2e-publication-one")
			if run(binaries["publisher"], pubFile) == nil {
				t.Fatal("lost START incorrectly reported success")
			}
			if run(binaries["publisher"], pubFile) == nil {
				t.Fatal("lost REPORT incorrectly reported success")
			}
			if err = run(binaries["publisher"], pubFile); err != nil {
				t.Fatal("pending report replay failed", err)
			}
			queue("native-e2e-publication-two")
			if err = run(binaries["publisher"], pubFile); err != nil {
				t.Fatal("second profile job failed", err)
			}
			st, _ = store.Read(ctx)
			if len(st.WorkerJobs) != 3 || len(st.WorkerAttempts) != 3 || len(st.DeliveryArtifacts) != 2 || len(st.DeliveryBuildRuns) != 0 {
				t.Fatalf("duplicate/missing chain evidence jobs=%d attempts=%d artifacts=%d gha=%d", len(st.WorkerJobs), len(st.WorkerAttempts), len(st.DeliveryArtifacts), len(st.DeliveryBuildRuns))
			}
			artifact := st.DeliveryArtifacts[1]
			if artifact.Native == nil || artifact.BuildRunID != 0 || artifact.Native.Receipt != receipt || artifact.Native.Observation.ManifestDigest != receipt.ManifestDigest {
				t.Fatal("dishonest native provenance")
			}
			post(domain.Command{Action: "deploy_existing_artifact", ID: "deploy", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "rev", "artifact_id": artifact.ID}}, nil)
			for range 6 {
				store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						st.Operations[i].NextAttemptAt = time.Time{}
					}
					return nil
				})
				if _, err = service.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			st, _ = store.Read(ctx)
			found := false
			for _, op := range st.Operations {
				if op.ID == "deploy" {
					found = op.Phase == "WAIT_GITOPS_PR" && op.Status == "RUNNING" && op.GitOpsResult == nil
				}
			}
			if !found || provider.dispatches.Load() != 0 || provider.applies.Load() != 0 || provider.inspections.Load() != 0 || provider.proposals.Load() != 1 {
				t.Fatal("native path did not stop at one GitOps PR without Actions")
			}
			t.Logf("actual protected API→worker→publisher subprocess(restart/lost START+REPORT, 2 jobs)→pinned Distribution→server GET/hash→one GitOps PR: source=%s digest=%s; zero GHA/direct writes", sha, receipt.ManifestDigest)
		})
	}
}
