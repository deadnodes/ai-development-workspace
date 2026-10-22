package workerrunner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	githubprovider "releasecontrol/internal/providers/github"
	"releasecontrol/internal/publishrunner"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
	"time"
)

func TestRealMainCandidateHistoricalGitProdProfile(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("owned Docker fixture requires opt-in")
	}
	ctx := context.Background()
	docker := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("owned Docker: %v %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	owner := "main-candidate-" + nonce()
	id := docker("run", "--pull", "never", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Fatal("invalid registry identity")
	}
	t.Cleanup(func() {
		if docker("inspect", "--format", `{{index .Config.Labels "rcp.fixture.owner"}}`, id) != owner {
			t.Error("registry owner changed")
			return
		}
		docker("rm", "--force", id)
	})
	origin := "http://" + docker("port", id, "5000/tcp")
	ready := false
	for range 50 {
		r, e := http.Get(origin + "/v2/")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("registry not ready")
	}
	t.Logf("owned Distribution id=%s owner=%s", id, owner)
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("postgres=%t", pg), func(t *testing.T) {
			open := openSourceFixtureStore(t, pg)
			store := open()
			defer func() { store.Close() }()
			f := newMainCandidateGitFixture(t)
			key, e := rsa.GenerateKey(rand.Reader, 2048)
			if e != nil {
				t.Fatal(e)
			}
			t.Setenv("RCP_MAIN_FIXTURE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
			gitops := newCandidateGitOpsHTTPFixture(t)
			client := &http.Client{Transport: sourceRoundTrip(func(r *http.Request) (*http.Response, error) {
				if gitops.handles(r) {
					return gitops.roundTrip(r)
				}
				return f.roundTrip(r)
			})}
			provider := &mainCandidateActivationProvider{composedPipelineProvider: &composedPipelineProvider{Provider: githubprovider.NewWithClient(client), t: t}, gitops: gitops}
			cfg := sourceFetchConfig(t)
			cfg.ProductID = "p"
			cfg.RepositoryID = "source"
			cfg.ConfigRevision = "main-v1"
			x := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441", Platform: "linux/arm64", CheckArgv: []string{"go", "-C", "/source", "test", "./..."}, BuildArgv: []string{"go", "-C", "/source", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "."}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20, SourceFetch: cfg.Recipe.Execution.SourceFetch}
			dev := workerconfig.Recipe{ID: "dev-build", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/fixture/source", Capability: "local_oci_build", Trust: "trusted_source", Execution: x, Parameters: map[string]workerconfig.Parameter{}}
			dev.Hash = workerconfig.RecipeHash(dev)
			prod := dev
			px := *x
			prod.Execution = &px
			prod.ID = "prod-build"
			px.CheckArgv = []string{"go", "-C", "/source", "test", "-tags=prod", "-ldflags=-X=fixture/candidate.Profile=prod", "./..."}
			px.BuildArgv = []string{"go", "-C", "/source", "build", "-tags=prod", "-trimpath", "-buildvcs=false", "-ldflags=-buildid= -X=main.Profile=prod", "-o", "/output/app", "."}
			prod.Hash = workerconfig.RecipeHash(prod)
			cfg.Recipe = prod
			devPub := workerconfig.PublicationRecipe{ID: "dev-publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "dev", BuildRecipeHash: dev.Hash, Origin: origin, Repository: fmt.Sprintf("fixture/main-dev-%t", pg), MaxBytes: 16 << 20, TimeoutSeconds: 30, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
			devPub.Hash = workerconfig.PublicationHash(devPub)
			prodPub := devPub
			prodPub.ID = "prod-publish"
			prodPub.Profile = "prod"
			prodPub.BuildRecipeHash = prod.Hash
			prodPub.Repository = fmt.Sprintf("fixture/main-prod-%t", pg)
			prodPub.Hash = workerconfig.PublicationHash(prodPub)
			gitops.configure(strings.TrimPrefix(origin, "http://") + "/" + prodPub.Repository)
			control := "main-candidate-control-token-000000001"
			t.Setenv("RC_TOKEN", control)
			t.Setenv(cfg.TokenEnv, "main-candidate-build-token-000000001")
			t.Setenv(cfg.SourceGitHub.TokenEnv, "fixture-source-only")
			t.Setenv("MAIN_CANDIDATE_PUBLISH_TOKEN", "main-candidate-publish-token-000001")
			auth, e := workerconfig.New(workerconfig.Config{Revision: cfg.ConfigRevision, LeaseSeconds: 20, Recipes: []workerconfig.Recipe{dev, prod}, Publications: []workerconfig.PublicationRecipe{devPub, prodPub}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: cfg.TokenEnv, ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "MAIN_CANDIDATE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if e != nil {
				t.Fatal(e)
			}
			service := application.NewWithProvider(store, provider)
			service.ConfigureWorkers(auth)
			server := httptest.NewServer(transport.New(service, transport.Options{Token: control, Workers: auth}))
			defer func() { server.Close() }()
			post := func(c domain.Command, out any, wantOK bool) {
				t.Helper()
				c.Actor = "fixture/operator"
				if c.Data == nil {
					c.Data = map[string]any{}
				}
				b, _ := json.Marshal(c)
				req, _ := http.NewRequest("POST", server.URL+"/api/commands", bytes.NewReader(b))
				req.Header.Set("Authorization", "Bearer "+control)
				req.Header.Set("Content-Type", "application/json")
				r, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				defer r.Body.Close()
				b, _ = io.ReadAll(r.Body)
				if wantOK && r.StatusCode != 200 {
					t.Fatalf("%s: %d %s", c.Action, r.StatusCode, b)
				}
				if !wantOK && (r.StatusCode < 400 || r.StatusCode >= 500) {
					t.Fatalf("expected refusal %s: %d %s", c.Action, r.StatusCode, b)
				}
				if out != nil && wantOK {
					if e := json.Unmarshal(b, out); e != nil {
						t.Fatal(e)
					}
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "fixture"}}, {Action: "create_feature", ID: "fa", ProductID: "p", Data: map[string]any{"title": "A", "goal": "A only"}}, {Action: "create_integration", ID: "ia", FeatureID: "fa", Data: map[string]any{"title": "A", "objective": "A only"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:RCP_MAIN_FIXTURE_KEY", "owner": "fixture"}}} {
				post(c, nil, true)
			}
			if _, e := service.Query(ctx, "discover_repositories", "conn"); e != nil {
				t.Fatal(e)
			}
			for _, r := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "fixture/" + r.id, "role": r.role, "default_branch": "main"}}, nil, true)
			}
			for _, c := range []domain.Command{{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "fixture", "repository_id": "source"}}, {Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "native": domain.NativeBinding{BuildRecipeID: dev.ID, PublicationRecipeID: devPub.ID, Parameters: map[string]any{}}, "profile": devPub.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/" + devPub.Repository}}, {Action: "update_integration", IntegrationID: "ia", Data: map[string]any{"repositories": []string{"source"}, "commits": []map[string]string{{"repository_id": "source", "sha": f.aHead}}}}, {Action: "record_integration_revision", ID: "a1", IntegrationID: "ia", Data: map[string]any{"repository_id": "source", "branch": "feature/a", "base_commit": f.base, "head_commit": f.aHead, "commits": []string{f.aHead}}}, {Action: "create_gate", ID: "source-gate", FeatureID: "fa", Data: map[string]any{"title": "Source checks and independent review", "reason": "fixture attributed verification", "integration_ids": []string{"ia"}, "blocking": true, "commit": f.aHead}}, {Action: "add_check", ID: "source-check", GateID: "source-gate", Data: map[string]any{"title": "Source verification", "mechanism": "fixture external executor", "instructions": "checks and independent source review"}}} {
				post(c, nil, true)
			}
			var sourceResult domain.CheckResult
			post(domain.Command{Action: "record_check_result", CheckID: "source-check", Data: map[string]any{"result": "passed", "commit": f.aHead, "observations": "Fixture attributed source checks/review, not provider human approval"}}, &sourceResult, true)
			post(domain.Command{Action: "start_integration", IntegrationID: "ia"}, nil, true)
			post(domain.Command{Action: "complete_integration", IntegrationID: "ia"}, nil, true)
			for _, env := range []struct{ id, purpose string }{{"dev", "DEV"}, {"prod", "PROD"}} {
				post(domain.Command{Action: "create_environment", ID: env.id, ProductID: "p", Data: map[string]any{"name": env.id, "cluster": "fixture-dev", "namespace": env.id}}, nil, true)
				post(domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": env.id, "application_id": "component", "purpose": env.purpose, "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": env.id + ".yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, nil, true)
			}
			read := func() domain.State {
				t.Helper()
				s, e := store.Read(ctx)
				if e != nil {
					t.Fatal(e)
				}
				return s
			}
			operation := func(id string) domain.ExternalOperation {
				t.Helper()
				for _, o := range read().Operations {
					if o.ID == id {
						return o
					}
				}
				t.Fatal("operation missing", id)
				return domain.ExternalOperation{}
			}
			tick := func() {
				t.Helper()
				if e := store.Update(ctx, func(s *domain.State) error {
					for i := range s.Operations {
						s.Operations[i].NextAttemptAt = s.Operations[i].UpdatedAt
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				if _, e := service.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			ticks := func() {
				for range 12 {
					tick()
				}
			}
			data := func(key, result string) map[string]any {
				return map[string]any{"name": "A main candidate", "environment_id": "prod", "idempotency_key": "fixture-main-candidate:" + key, "main_sources": []domain.MainCandidateSource{{ApplicationID: "component", MainSHA: f.mainA, Native: domain.NativeBinding{BuildRecipeID: prod.ID, PublicationRecipeID: prodPub.ID, Parameters: map[string]any{}}, Revisions: []domain.MainCandidateRevision{{RevisionID: "a1", PullRequestNumber: 1, VerificationResultIDs: []string{result}}}}}}
			}
			before := read()
			baseline, _ := json.Marshal(before.ComponentBuilds)
			environments, _ := json.Marshal(before.Environments)
			var parent domain.ExternalOperation
			post(domain.Command{Action: "prepare_release_candidate", ProductID: "p", Data: data("main-a", sourceResult.ID)}, &parent, true)
			if parent.MainCandidate == nil || parent.MainCandidate.EvidenceKind != "ATTRIBUTED_RCP_VERIFICATION" || parent.Kind != "RELEASE_CANDIDATE" {
				t.Fatal("missing truthful main candidate proof")
			}
			server.Close()
			store.Close()
			store = open()
			service = application.NewWithProvider(store, provider)
			service.ConfigureWorkers(auth)
			server = httptest.NewServer(transport.New(service, transport.Options{Token: control, Workers: auth}))
			var replay domain.ExternalOperation
			old := read()
			post(domain.Command{Action: "prepare_release_candidate", ProductID: "p", Data: data("main-a", sourceResult.ID)}, &replay, true)
			if replay.ID != parent.ID || len(read().Operations) != len(old.Operations) || len(read().Events) != len(old.Events) {
				t.Fatal("replay created work")
			}
			ticks()
			if len(read().WorkerJobs) != 1 {
				t.Fatalf("native build not queued %+v", read().Operations)
			}
			job := read().WorkerJobs[0]
			if job.Inputs.SourceSHA != f.mainA || job.Inputs.RecipeHash != prod.Hash || job.Inputs.SourceProof == nil || job.Inputs.SourceProof.ParentID != parent.ID || job.Inputs.Execution.SourceFetch == nil {
				t.Fatal("candidate source/profile proof not bound")
			}
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			spool := filepath.Join(dir, "spool")
			if e := os.Mkdir(spool, 0700); e != nil {
				t.Fatal(e)
			}
			cfg.Endpoint = server.URL
			cfg.ArtifactSpool = &artifactspool.Config{Root: spool, MaxArchiveBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxEntries: 8}
			cfg.DockerContext = "orbstack"
			home, _ := os.UserHomeDir()
			cfg.DockerConfig = filepath.Join(home, ".docker")
			cfg.AllowRootlessUnconfined = true
			pubState := filepath.Join(dir, "publisher")
			if e := os.Mkdir(pubState, 0700); e != nil {
				t.Fatal(e)
			}
			pub := publishrunner.Config{ArtifactSpool: cfg.ArtifactSpool, Endpoint: server.URL, TokenEnv: "MAIN_CANDIDATE_PUBLISH_TOKEN", ConfigRevision: cfg.ConfigRevision, Recipe: prodPub, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: prodPub.Repository, StateDir: pubState, MaxArchiveBytes: prodPub.MaxBytes, TimeoutSeconds: 30, FixtureLoopbackHTTP: true}}
			report, e := runOne(ctx, cfg, client)
			if e != nil || report == nil || report.Result != "PASSED" || report.LocalOCI == nil || report.LocalOCI.SourceSHA != f.mainA {
				t.Fatalf("real main worker %+v %v", report, e)
			}
			assertOwnedResourcesGone(t, cfg)
			ticks()
			if _, e := publishrunner.RunOnce(ctx, pub); e != nil {
				t.Fatal(e)
			}
			ticks()
			parent = operation(parent.ID)
			if parent.Status != "SUCCEEDED" || parent.DeploymentState != "READY_FOR_VERIFICATION" || len(parent.ChildIDs) != 1 {
				t.Fatalf("candidate not ready %+v", parent)
			}
			child := operation(parent.ChildIDs[0])
			s := read()
			if !child.BuildOnly || child.Status != "SUCCEEDED" || child.DeploymentState != "ARTIFACT_READY" || child.Artifact == nil || len(s.DeliveryArtifacts) != 1 {
				t.Fatal("candidate did not stop before deployment", child)
			}
			a := s.DeliveryArtifacts[0]
			if a.Native == nil || a.Native.Profile != "prod" || a.SourceCommit != f.mainA || a.Digest != report.LocalOCI.ManifestDigest || a.Native.Receipt.RecipeHash != prod.Hash {
				t.Fatal("main artifact/profile mismatch")
			}
			if strings.Contains(string(f.git("show", f.mainA+":b.go")), "B1") || !strings.Contains(string(f.git("show", f.mainB+":b.go")), "B1") {
				t.Fatal("fixture did not exercise historical A-only")
			}
			post(domain.Command{Action: "create_test_scenario", ID: "candidate-scope", ProductID: "p", Data: map[string]any{"title": "A-only source/profile", "objective": "offline candidate scope", "integration_ids": []string{"ia"}, "steps": []string{"offline source/profile checks"}, "expected_outcomes": []string{"A1+B0 prod"}, "mechanism": "offline compiler fixture", "blocking": true}}, nil, true)
			post(domain.Command{Action: "record_scenario_run", ProductID: "p", Data: map[string]any{"scenario_version_id": "candidate-scope", "candidate_operation_id": parent.ID, "components": []domain.ScenarioComponentEvidence{{ApplicationID: "component", OperationID: child.ID, SourceSHA: f.mainA, ArtifactDigest: a.Digest}}, "result": "passed", "observations": []string{"Actual isolated compiler checked A-only source and prod profile; no runtime/Dev acceptance assertion"}}}, nil, true)
			post(domain.Command{Action: "promote_release_candidate", ID: parent.ID, ProductID: "p", Data: map[string]any{"approve": true}}, nil, false)
			post(domain.Command{Action: "prepare_release_candidate", ProductID: "p", Data: map[string]any{"name": "legacy", "environment_id": "prod", "approve_main_update": true, "components": []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: f.base, TargetBranch: "generated/legacy", RevisionIDs: []string{"a1"}}}}}, nil, false)
			beforeReservation, _ := json.Marshal(read().Environments)
			if !bytes.Equal(environments, beforeReservation) {
				t.Fatal("build-only candidate changed Dev environment before explicit reservation")
			}
			environments = assertRealCandidateDevReservation(t, server.URL, control, parent.ID, f.mainB, read, post)
			var stale domain.ExternalOperation
			post(domain.Command{Action: "prepare_release_candidate", ProductID: "p", Data: data("stale-before-publish", sourceResult.ID)}, &stale, true)
			ticks()
			report, e = runOne(ctx, cfg, client)
			if e != nil || report == nil || report.Result != "PASSED" {
				t.Fatalf("negative setup build %+v %v", report, e)
			}
			assertOwnedResourcesGone(t, cfg)
			ticks()
			var failed domain.CheckResult
			runtimePeer, runtimeObservers := newCandidateRuntimeHTTPFixture(t, a.ImageRepository, a.Digest, gitops)
			service.SetRuntimeObservers(runtimeObservers)
			// Run the informational control through a production read-only worker.
			// The accelerated candidate tick helper makes all candidate retries due
			// on every tick, which would starve lower-priority inventory reads.
			inventoryWorker := application.New(store)
			inventoryWorker.SetRuntimeObservers(runtimeObservers)
			inventoryTick := func() {
				if worked, err := inventoryWorker.Tick(ctx); err != nil || !worked {
					t.Fatalf("informational worker tick: worked=%v err=%v", worked, err)
				}
			}
			environments = assertRealCandidateDevActivation(t, server.URL, control, parent.ID, provider, runtimePeer, ticks, inventoryTick, read, post)
			post(domain.Command{Action: "record_check_result", CheckID: "source-check", Data: map[string]any{"result": "failed", "commit": f.aHead, "observations": "later causal failure"}}, &failed, true)
			if e := store.Update(ctx, func(s *domain.State) error {
				for i := range s.Results {
					if s.Results[i].ID == failed.ID {
						s.Results[i].CreatedAt = sourceResult.CreatedAt.Add(-time.Hour)
					}
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			ticks()
			if _, e := publishrunner.RunOnce(ctx, pub); e != nil {
				t.Fatal(e)
			}
			ticks()
			if len(read().DeliveryArtifacts) != 1 || operation(stale.ID).Status != "FAILED" {
				t.Fatal("old-clock failure admitted publication")
			}
			final := read()
			builds, _ := json.Marshal(final.ComponentBuilds)
			envs, _ := json.Marshal(final.Environments)
			if !bytes.Equal(baseline, builds) || !bytes.Equal(environments, envs) || len(final.RuntimeObservations) != 0 || len(final.Releases) != 0 || len(final.DeliveryBuildRuns) != 0 || len(provider.proposals) != 1 || provider.forbidden != 0 || f.writes.Load() != 0 || f.merges != 0 || f.pulls.Load() == 0 || string(f.git("rev-parse", "main")) != f.mainB {
				t.Fatal("candidate changed Dev config, desired/runtime, main or GitOps")
			}
			t.Logf("readonly PR/main(%s ancestor of %s)→SourceGitHub→actual prod recipe→OCI %s→build-only READY→reserved Dev exact-artifact PR; A-only excludes B, deleted source branch, durable replay; later old-clock FAIL blocks second publication; no promotion/main/runtime", f.mainA, f.mainB, a.Digest)
		})
	}
}
