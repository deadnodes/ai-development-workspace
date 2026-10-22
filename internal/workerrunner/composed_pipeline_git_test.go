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

func TestRealComposedGitSourceWorkerPublication(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("owned Docker fixture requires opt-in")
	}
	ctx := context.Background()
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("owned Docker %v: %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	owner := "composed-pipeline-" + nonce()
	id := docker("run", "--pull", "never", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Fatal("invalid fixture ID")
	}
	t.Cleanup(func() {
		if docker("inspect", "--format", `{{index .Config.Labels "rcp.fixture.owner"}}`, id) != owner {
			t.Error("fixture owner changed")
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
			f := &composedGitAPI{sourceGitFixture: newSourceGitFixture(t)}
			a1 := string(f.git("rev-parse", f.shas[0]+"^1"))
			a2 := string(f.git("rev-parse", f.shas[1]+"^1"))
			b1 := string(f.git("rev-parse", f.shas[0]+"^2"))
			base := string(f.git("rev-parse", a1+"^"))
			f.git("branch", "main", base)
			f.git("branch", "feature/a", a2)
			f.git("branch", "feature/b", b1)
			f.git("checkout", "-b", "feature/bad", a2)
			if e := os.WriteFile(filepath.Join(f.root, "a.go"), []byte("package main\nconst A=\"A_BAD\"\n"), 0600); e != nil {
				t.Fatal(e)
			}
			f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-am", "Semantic failure")
			bad := string(f.git("rev-parse", "HEAD"))
			protected := string(f.git("show-ref", "--heads", "main", "feature/a", "feature/b", "feature/bad"))
			key, e := rsa.GenerateKey(rand.Reader, 2048)
			if e != nil {
				t.Fatal(e)
			}
			t.Setenv("RCP_COMPOSE_FIXTURE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
			client := &http.Client{Transport: sourceRoundTrip(f.response)}
			provider := &composedPipelineProvider{Provider: githubprovider.NewWithClient(client), t: t}
			cfg := sourceFetchConfig(t)
			cfg.Recipe.RepositoryID = "source"
			cfg.RepositoryID = "source"
			x := cfg.Recipe.Execution
			x.Kind = "local_oci_build"
			cfg.Recipe.Capability = x.Kind
			x.ToolchainImage = "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b"
			x.BuildKitImage = "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441"
			x.CheckArgv = []string{"go", "test", "/source/main.go", "/source/a.go", "/source/b.go", "/source/main_test.go"}
			x.BuildArgv = []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go", "/source/a.go", "/source/b.go"}
			x.OutputFile = "app"
			x.ImagePath = "app"
			x.Entrypoint = []string{"/app"}
			x.TimeoutSeconds = 300
			x.MemoryMB = 1536
			x.StorageMB = 512
			x.CPUs = 2
			x.PIDs = 128
			x.OutputBytes = 16 << 20
			x.LogBytes = 1 << 20
			cfg.Recipe.Hash = workerconfig.RecipeHash(cfg.Recipe)
			publication := workerconfig.PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "composed-test", BuildRecipeHash: cfg.Recipe.Hash, Origin: origin, Repository: fmt.Sprintf("fixture/composed-%t", pg), MaxBytes: 16 << 20, TimeoutSeconds: 30, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
			publication.Hash = workerconfig.PublicationHash(publication)
			buildToken := "composed-fixture-worker-token-00000001"
			publishToken := "composed-fixture-publish-token-000001"
			control := "composed-fixture-control-token-000001"
			t.Setenv(cfg.TokenEnv, buildToken)
			t.Setenv(cfg.SourceGitHub.TokenEnv, "fixture-source-only")
			t.Setenv("COMPOSE_PUBLISH_TOKEN", publishToken)
			t.Setenv("RC_TOKEN", control)
			auth, e := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{cfg.Recipe}, Publications: []workerconfig.PublicationRecipe{publication}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: cfg.TokenEnv, ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "COMPOSE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if e != nil {
				t.Fatal(e)
			}
			service := application.NewWithProvider(store, provider)
			service.ConfigureWorkers(auth)
			server := httptest.NewServer(transport.New(service, transport.Options{Token: control, Workers: auth}))
			defer server.Close()
			cfg.Endpoint = server.URL
			post := func(c domain.Command, out any) {
				t.Helper()
				c.Actor = "fixture/operator"
				if c.Data == nil {
					c.Data = map[string]any{}
				}
				b, _ := json.Marshal(c)
				r, _ := http.NewRequest("POST", server.URL+"/api/commands", bytes.NewReader(b))
				r.Header.Set("Authorization", "Bearer "+control)
				r.Header.Set("Content-Type", "application/json")
				res, e := http.DefaultClient.Do(r)
				if e != nil {
					t.Fatal(e)
				}
				defer res.Body.Close()
				b, _ = io.ReadAll(res.Body)
				if res.StatusCode != 200 {
					t.Fatalf("%s: %d %s", c.Action, res.StatusCode, b)
				}
				if out != nil {
					if e := json.Unmarshal(b, out); e != nil {
						t.Fatal(e)
					}
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "fixture"}}, {Action: "create_feature", ID: "fa", ProductID: "p", Data: map[string]any{"title": "A", "goal": "A"}}, {Action: "create_feature", ID: "fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "B"}}, {Action: "create_integration", ID: "ia", FeatureID: "fa", Data: map[string]any{"title": "A", "objective": "A"}}, {Action: "create_integration", ID: "ib", FeatureID: "fb", Data: map[string]any{"title": "B", "objective": "B"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:RCP_COMPOSE_FIXTURE_KEY", "owner": "fixture"}}} {
				post(c, nil)
			}
			if _, e := service.Query(ctx, "discover_repositories", "conn"); e != nil {
				t.Fatal(e)
			}
			for _, r := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "fixture/" + r.id, "role": r.role, "default_branch": "main"}}, nil)
			}
			for _, c := range []domain.Command{{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "fixture", "repository_id": "source"}}, {Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "native": map[string]any{"build_recipe_id": cfg.Recipe.ID, "publication_recipe_id": publication.ID, "parameters": map[string]any{}}, "profile": publication.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/" + publication.Repository}}, {Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "fixture Dev"}}, {Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}} {
				post(c, nil)
			}
			for _, r := range []struct{ id, integration, branch, sha string }{{"a1", "ia", "feature/a", a1}, {"a2", "ia", "feature/a", a2}, {"b1", "ib", "feature/b", b1}, {"bad", "ia", "feature/bad", bad}} {
				post(domain.Command{Action: "record_integration_revision", ID: r.id, IntegrationID: r.integration, Data: map[string]any{"repository_id": "source", "branch": r.branch, "base_commit": base, "head_commit": r.sha, "commits": strings.Fields(string(f.git("rev-list", base+".."+r.sha)))}}, nil)
			}
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			spool := filepath.Join(dir, "spool")
			if e := os.Mkdir(spool, 0700); e != nil {
				t.Fatal(e)
			}
			cfg.ArtifactSpool = &artifactspool.Config{Root: spool, MaxArchiveBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxEntries: 8}
			cfg.DockerContext = "orbstack"
			home, _ := os.UserHomeDir()
			cfg.DockerConfig = filepath.Join(home, ".docker")
			cfg.AllowRootlessUnconfined = true
			pubState := filepath.Join(dir, "publisher")
			if e := os.Mkdir(pubState, 0700); e != nil {
				t.Fatal(e)
			}
			pub := publishrunner.Config{ArtifactSpool: cfg.ArtifactSpool, Endpoint: server.URL, TokenEnv: "COMPOSE_PUBLISH_TOKEN", ConfigRevision: "cfg1", Recipe: publication, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: publication.Repository, StateDir: pubState, MaxArchiveBytes: publication.MaxBytes, TimeoutSeconds: 30, FixtureLoopbackHTTP: true}}
			read := func() domain.State {
				t.Helper()
				s, e := store.Read(ctx)
				if e != nil {
					t.Fatal(e)
				}
				return s
			}
			tick := func() {
				t.Helper()
				if e := store.Update(ctx, func(s *domain.State) error {
					for n := range s.Operations {
						s.Operations[n].NextAttemptAt = s.Operations[n].UpdatedAt
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				if _, e := service.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			post(domain.Command{Action: "plan_composition", ID: "initial", ProductID: "p", Data: map[string]any{"name": "A1+B1", "environment_id": "dev", "components": []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: base, TargetBranch: "generated/dev/initial", RevisionIDs: []string{"a1", "b1"}}}}}, nil)
			var initial domain.ExternalOperation
			post(domain.Command{Action: "reconcile_composition", ID: "initial", ProductID: "p"}, &initial)
			sourceSHAs := []string{}
			digests := []string{}
			var acceptedArtifacts, acceptedPRs []byte
			for n, revision := range []string{"a1", "a2", "bad"} {
				if n > 0 {
					s := read()
					env := s.Environments[0]
					post(domain.Command{Action: "update_environment_feature", ProductID: "p", FeatureID: "fa", Data: map[string]any{"environment_id": "dev", "revision_ids": []string{revision}, "idempotency_key": "change-" + revision, "expected_selection": map[string]any{"composition_id": env.DesiredCompositionID, "operation_id": env.DesiredOperationID, "generation": env.SelectionGeneration}}}, nil)
				}
				for range 12 {
					tick()
				}
				s := read()
				parentID := s.Environments[0].DesiredOperationID
				var parent domain.ExternalOperation
				for _, o := range s.Operations {
					if o.ID == parentID {
						parent = o
					}
				}
				if len(parent.Sources) != 1 || parent.Sources[0].Result == nil {
					t.Fatalf("source not composed: %+v", parent)
				}
				src := parent.Sources[0]
				sha := src.Result.SHA
				wantA := []string{"A1", "A2", "A_BAD"}[n]
				if !strings.Contains(string(f.git("show", sha+":a.go")), wantA) || !strings.Contains(string(f.git("show", sha+":b.go")), "B1") {
					t.Fatal("wrong composed content")
				}
				parents := strings.Fields(string(f.git("show", "-s", "--format=%P", sha)))
				selected := []string{a1, a2, bad}[n]
				if src.Plan.BaseSHA != base || len(src.Plan.HeadSHAs) != 2 || src.Plan.HeadSHAs[0] != selected || src.Plan.HeadSHAs[1] != b1 || src.Plan.OperationID != parentID || src.Result.Branch != src.Plan.TargetBranch || len(parents) != 2 || parents[1] != b1 || string(f.git("show", "-s", "--format=%P", parents[0])) != base+" "+selected {
					t.Fatal("wrong ordered merge provenance")
				}
				jobsBefore := len(s.WorkerJobs)
				if jobsBefore != n*2+1 {
					t.Fatalf("wrong generated jobs %d %+v", jobsBefore, s.Operations)
				}
				job := s.WorkerJobs[jobsBefore-1]
				if job.Inputs.SourceProof == nil || job.Inputs.SourceProof.ParentID != parentID || job.Inputs.SourceSHA != sha || job.Inputs.Execution.SourceFetch == nil || job.Inputs.SourceProof.ParentHash == "" {
					t.Fatal("missing native composed proof")
				}
				report, err := runOne(ctx, cfg, client)
				if n == 2 {
					if err == nil || report == nil || report.Result != "CHECK_FAILED" || report.LocalOCI != nil {
						t.Fatalf("semantic failure result %+v %v", report, err)
					}
					for range 10 {
						tick()
					}
					s = read()
					assertOwnedResourcesGone(t, cfg)
					failedParent, failedChild := false, false
					for _, operation := range s.Operations {
						if operation.ID == parentID {
							failedParent = operation.Status == "FAILED"
						}
						if operation.ID == job.Inputs.SourceProof.DeliveryID {
							failedChild = operation.Status == "FAILED" && operation.Native != nil && operation.Native.PublicationJobID == "" && operation.ExistingArtifact == nil && operation.GitOpsPR == nil
						}
					}
					if !failedParent || !failedChild {
						t.Fatalf("failed checks did not stop native parent/child: %+v", s.Operations)
					}
					artifacts, _ := json.Marshal(s.DeliveryArtifacts)
					prs, _ := json.Marshal(provider.proposals)
					if !bytes.Equal(acceptedArtifacts, artifacts) || !bytes.Equal(acceptedPRs, prs) {
						t.Fatal("failed composition changed previously accepted evidence")
					}
					if len(s.WorkerJobs) != 5 || len(s.DeliveryArtifacts) != 2 || len(provider.proposals) != 2 {
						t.Fatal("failed source produced publish/artifact/PR")
					}
					break
				}
				if err != nil || report == nil || report.Result != "PASSED" || report.LocalOCI == nil || report.LocalOCI.SourceSHA != sha {
					t.Fatalf("real composed worker %+v %v", report, err)
				}
				if _, err := artifactspool.Resolve(*cfg.ArtifactSpool, *report.LocalOCI); err != nil {
					t.Fatal(err)
				}
				assertOwnedResourcesGone(t, cfg)
				for range 5 {
					tick()
				}
				if _, e := publishrunner.RunOnce(ctx, pub); e != nil {
					t.Fatal(e)
				}
				for range 12 {
					tick()
				}
				s = read()
				if len(s.DeliveryArtifacts) != n+1 || len(provider.proposals) != n+1 {
					t.Fatalf("missing admitted artifact/PR: %+v", s.Operations)
				}
				a := s.DeliveryArtifacts[n]
				if a.Native == nil || a.SourceCommit != sha || a.Digest != report.LocalOCI.ManifestDigest || provider.proposals[n].Digest != a.Digest {
					t.Fatal("broken source/OCI/publication/PR binding")
				}
				req, _ := http.NewRequest("GET", origin+"/v2/"+publication.Repository+"/manifests/"+a.Digest, nil)
				req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
				res, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				res.Body.Close()
				if res.StatusCode != 200 || res.Header.Get("Docker-Content-Digest") != a.Digest {
					t.Fatal("published digest absent")
				}
				sourceSHAs = append(sourceSHAs, sha)
				digests = append(digests, a.Digest)
				acceptedArtifacts, _ = json.Marshal(s.DeliveryArtifacts)
				acceptedPRs, _ = json.Marshal(provider.proposals)
			}
			s := read()
			before, _ := json.Marshal(s.WorkerAttempts)
			store.Close()
			store = open()
			after, _ := json.Marshal(read().WorkerAttempts)
			if !bytes.Equal(before, after) {
				t.Fatal("receipt lost on reopen")
			}
			if len(sourceSHAs) != 2 || sourceSHAs[0] == sourceSHAs[1] || digests[0] == digests[1] || provider.forbidden != 0 || len(s.DeliveryBuildRuns) != 0 || len(s.RuntimeObservations) != 0 || protected != string(f.git("show-ref", "--heads", "main", "feature/a", "feature/b", "feature/bad")) {
				t.Fatal("source independence or forbidden fallback violated")
			}
			for _, operation := range s.Operations {
				if operation.GitOpsResult != nil || operation.GitOpsPR != nil && operation.GitOpsPR.Merged {
					t.Fatal("fixture manufactured GitOps merge/reconciliation")
				}
			}
			t.Logf("production Git provider→COMPOSE→SourceGitHub→offline RunOne→spool→publisher→Distribution→admission→PR; A1+B1 %s %s; A2+B1 %s %s; semantic failure prevented third artifact/PR; requests=%d merges=%d", sourceSHAs[0], digests[0], sourceSHAs[1], digests[1], f.requests, f.merges)
		})
	}
}
