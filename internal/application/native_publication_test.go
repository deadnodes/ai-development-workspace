package application

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/ociverify"
	"releasecontrol/internal/workerconfig"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const nativeToken = "native-publication-token-fixture-0000000000001"
const nativeBuildToken = "native-build-token-fixture-00000000000000001"

type nativeRegistry struct {
	mu     sync.Mutex
	data   map[string][]byte
	hook   func()
	writes atomic.Int32
	status atomic.Int32
}

func (m *nativeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && m.hook != nil {
		m.hook()
	}
	if status := m.status.Load(); status != 0 {
		w.WriteHeader(int(status))
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.URL.Path
	switch r.Method {
	case "GET":
		b, ok := m.data[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	case "POST":
		m.writes.Add(1)
		w.Header().Set("Location", strings.TrimSuffix(key, "/")+"/session")
		w.WriteHeader(202)
	case "PUT":
		m.writes.Add(1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(key, "/uploads/") {
			key = strings.SplitN(key, "/uploads/", 2)[0] + "/" + r.URL.Query().Get("digest")
		}
		m.data[key] = b
		w.WriteHeader(201)
	default:
		w.WriteHeader(405)
	}
}
func nativeArchive(t *testing.T) ([]byte, domain.WorkerLocalOCI) {
	t.Helper()
	files := map[string][]byte{}
	add := func(media string, b []byte) map[string]any {
		d := byteDigest(b)
		files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = b
		return map[string]any{"mediaType": media, "digest": d, "size": len(b)}
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "app", Size: 3, Mode: 0555})
	tw.Write([]byte("app"))
	tw.Close()
	config, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{byteDigest(layer.Bytes())}}})
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": add("application/vnd.oci.image.config.v1+json", config), "layers": []any{add("application/vnd.oci.image.layer.v1.tar", layer.Bytes())}})
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{add("application/vnd.oci.image.manifest.v1+json", manifest)}})
	files["index.json"] = index
	files["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	var archive bytes.Buffer
	tw = tar.NewWriter(&archive)
	for n, b := range files {
		tw.WriteHeader(&tar.Header{Name: n, Size: int64(len(b)), Mode: 0600})
		tw.Write(b)
	}
	tw.Close()
	receipt, e := ociverify.VerifyOCIBytes(archive.Bytes(), 1<<20, "linux/arm64")
	if e != nil {
		t.Fatal(e)
	}
	return archive.Bytes(), receipt
}
func byteDigest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

type nativeFixture struct {
	s         *Service
	store     workerTestStore
	reopen    func() workerTestStore
	recipe    workerconfig.PublicationRecipe
	build     domain.WorkerJob
	lease     *domain.WorkerLease
	p         workerconfig.Principal
	registry  *nativeRegistry
	server    *httptest.Server
	publisher ocipublisher.Config
}

func newNativeFixture(t *testing.T, kind string) *nativeFixture {
	t.Helper()
	ctx := context.Background()
	_, seed, _ := externalFixture(t)
	store, reopen := openWorkerTestStore(t, kind)
	if e := store.Update(ctx, func(st *domain.State) error { *st = seed.state; return nil }); e != nil {
		t.Fatal(e)
	}
	m := &nativeRegistry{data: map[string][]byte{}}
	server := httptest.NewServer(m)
	t.Cleanup(server.Close)
	e := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "tool@sha256:" + strings.Repeat("a", 64), BuildKitImage: "build@sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64", CheckArgv: []string{"check"}, BuildArgv: []string{"build"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 60, MemoryMB: 256, StorageMB: 32, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 4096}
	buildRecipe := workerconfig.Recipe{ID: "build", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/owner/source", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: e}
	buildRecipe.Hash = workerconfig.RecipeHash(buildRecipe)
	r := workerconfig.PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "test", BuildRecipeHash: buildRecipe.Hash, Origin: server.URL, Repository: "fixture/image", MaxBytes: 1 << 20, TimeoutSeconds: 10, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
	r.Hash = workerconfig.PublicationHash(r)
	t.Setenv("NATIVE_BUILD_TOKEN", nativeBuildToken)
	t.Setenv("NATIVE_PUBLISH_TOKEN", nativeToken)
	cfg := workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{buildRecipe}, Publications: []workerconfig.PublicationRecipe{r}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
	a, err := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	s := New(store)
	s.ConfigureWorkers(a)
	exec(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "profile": r.Profile, "image_repository": nativeImage(r)}})
	v, err := s.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "native-build-fixture-key", "recipe_id": buildRecipe.ID, "recipe_revision": buildRecipe.Revision, "recipe_hash": buildRecipe.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": shaHead, "parameters": map[string]any{}, "deadline": time.Now().Add(time.Minute), "max_attempts": 1}})
	if err != nil {
		t.Fatal(err)
	}
	build := v.(domain.WorkerJob)
	bp, _ := a.Authenticate(nativeBuildToken, time.Now())
	lease, err := s.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	req := workerRequest(lease)
	if _, err = s.StartWorker(ctx, bp, req); err != nil {
		t.Fatal(err)
	}
	archive, receipt := nativeArchive(t)
	receipt.JobID = build.ID
	receipt.AttemptID = lease.Attempt.ID
	receipt.InputHash = build.InputHash
	receipt.SourceSHA = shaHead
	receipt.RecipeHash = buildRecipe.Hash
	receipt.ConfigRevision = "cfg1"
	receipt.ToolchainImage = e.ToolchainImage
	if _, err = s.ReportWorker(ctx, bp, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", LocalOCI: &receipt}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	os.Chmod(dir, 0700)
	file := filepath.Join(dir, "image.tar")
	os.WriteFile(file, archive, 0600)
	publisher := ocipublisher.Config{AllowedOrigin: r.Origin, AllowedRepository: r.Repository, Archive: file, StateDir: dir, MaxArchiveBytes: r.MaxBytes, TimeoutSeconds: r.TimeoutSeconds, FixtureLoopbackHTTP: true}
	f := &nativeFixture{s: s, store: store, reopen: reopen, recipe: r, build: build, registry: m, server: server, publisher: publisher}
	f.p, _ = a.Authenticate(nativeToken, time.Now())
	t.Cleanup(func() { f.store.Close() })
	return f
}
func (f *nativeFixture) queue(t *testing.T) domain.WorkerJob {
	t.Helper()
	v, e := f.s.Execute(context.Background(), domain.Command{Action: "queue_worker_publication", Actor: "operator", ProductID: "p", Data: map[string]any{"build_job_id": f.build.ID, "component_id": "component", "recipe_id": f.recipe.ID, "recipe_hash": f.recipe.Hash, "recipe_revision": f.recipe.Revision, "config_revision": "cfg1", "request_key": "native-publication-fixture-key", "deadline": time.Now().Add(time.Minute)}})
	if e != nil {
		t.Fatal(e)
	}
	return v.(domain.WorkerJob)
}
func (f *nativeFixture) start(t *testing.T) domain.WorkerReport {
	t.Helper()
	f.queue(t)
	l, e := f.s.ClaimWorker(context.Background(), f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish", RecipeID: f.recipe.ID, RecipeHash: f.recipe.Hash})
	if e != nil || l == nil {
		t.Fatal(e)
	}
	if _, e = f.s.StartWorker(context.Background(), f.p, workerRequest(l)); e != nil {
		t.Fatal(e)
	}
	f.lease = l
	p := *l.Job.Inputs.Publication
	f.publisher.Plan = ocipublisher.Plan{ID: l.Job.ID, Approved: true, ProductID: "p", ComponentID: p.ComponentID, RepositoryID: "source", Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
	if _, e = ocipublisher.Publish(context.Background(), f.publisher); e != nil {
		t.Fatal(e)
	}
	return domain.WorkerReport{WorkerAttemptRequest: workerRequest(l), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: p.Receipt.ManifestDigest}}
}
func TestNativeAdmissionReopenReplayRestoreAndPR(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := newNativeFixture(t, kind)
			ctx := context.Background()
			report := f.start(t)
			if _, e := f.s.ReportWorker(ctx, f.p, report); e != nil {
				t.Fatal(e)
			}
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			for range 2 {
				if _, e := f.s.ReportWorker(ctx, f.p, report); e != nil {
					t.Fatal(e)
				}
			}
			st, _ := f.store.Read(ctx)
			if len(st.DeliveryArtifacts) != 1 || st.DeliveryArtifacts[0].BuildRunID != 0 || st.DeliveryArtifacts[0].Native == nil {
				t.Fatal("native admission invalid", st.DeliveryArtifacts)
			}
			a := st.DeliveryArtifacts[0]
			bad := report
			bad.Publication = &domain.WorkerPublicationReport{ManifestDigest: "sha256:" + strings.Repeat("0", 64)}
			if _, e := f.s.ReportWorker(ctx, f.p, bad); e == nil {
				t.Fatal("conflict accepted")
			}
			pr := &fakeGitOpsPR{fakeDelivery: &fakeDelivery{}}
			f.s.provider = pr
			exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			c := deployCommand()
			c.Action = "deploy_existing_artifact"
			c.Data["artifact_id"] = a.ID
			exec(t, f.s, c)
			for range 6 {
				f.store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						st.Operations[i].NextAttemptAt = time.Time{}
					}
					return nil
				})
				if _, e := f.s.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			st, _ = f.store.Read(ctx)
			op := operationByID(&st, "deploy")
			if op.Phase != "WAIT_GITOPS_PR" || pr.dispatches != 0 || pr.applies != 0 || pr.proposals != 1 || len(st.DeliveryArtifacts) != 1 {
				t.Fatalf("native delivery wrong %+v dispatch=%d pr=%d", op, pr.dispatches, pr.proposals)
			}
			backup, e := f.s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			restored, _ := openWorkerTestStore(t, kind)
			defer restored.Close()
			copy := NewWithProvider(restored, pr)
			copy.ConfigureWorkers(f.s.externalWorkers)
			if _, e = copy.RestoreBackup(ctx, "operator", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			writes := f.registry.writes.Load()
			c.ID = "restored-deploy"
			c.Actor = "operator"
			if _, e = copy.Execute(ctx, c); e == nil {
				t.Fatal("restored native artifact deploy accepted")
			}
			q := domain.Command{Action: "queue_worker_publication", Actor: "operator", ProductID: "p", Data: map[string]any{"build_job_id": f.build.ID, "component_id": "component", "recipe_id": f.recipe.ID, "recipe_hash": f.recipe.Hash, "recipe_revision": f.recipe.Revision, "config_revision": "cfg1", "request_key": "restored-native-publication", "deadline": time.Now().Add(time.Minute)}}
			if _, e = copy.Execute(ctx, q); e == nil {
				t.Fatal("restored native queue accepted")
			}
			// Restored build receipts are historical only: no native build replay or new queue/claim.
			bp, _ := copy.externalWorkers.Authenticate(nativeBuildToken, time.Now())
			buildAttempt := st.WorkerAttempts[0]
			if _, e = copy.ReportWorker(ctx, bp, *buildAttempt.Report); e == nil {
				t.Fatal("restored build report accepted")
			}
			if l, e := copy.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"}); e != nil || l != nil {
				t.Fatal("restored build claim escaped", e)
			}
			buildRecipe, _ := copy.externalWorkers.Recipe("build")
			if _, e = copy.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "restored-build-fixture-key", "recipe_id": buildRecipe.ID, "recipe_revision": buildRecipe.Revision, "recipe_hash": buildRecipe.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": shaHead, "parameters": map[string]any{}, "deadline": time.Now().Add(time.Minute), "max_attempts": 1}}); e == nil {
				t.Fatal("restored build queue accepted")
			}
			if _, e = copy.Query(ctx, "get_worker_job", f.build.ID); e != nil {
				t.Fatal("restored history unavailable", e)
			}
			if f.registry.writes.Load() != writes || pr.proposals != 1 {
				t.Fatal("restored external writes")
			}
		})
	}
}
func TestNativePostIORecheckAndNoNetworkUnderLock(t *testing.T) {
	for _, mode := range []string{"cancel", "rotate", "fence", "config", "expire", "deadline", "corrupt", "missing", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeFixture(t, "local")
			report := f.start(t)
			var once sync.Once
			if mode == "corrupt" || mode == "missing" {
				f.registry.mu.Lock()
				for k := range f.registry.data {
					if strings.Contains(k, "/blobs/") {
						if mode == "corrupt" {
							f.registry.data[k] = []byte("corrupt")
						} else {
							delete(f.registry.data, k)
						}
						break
					}
				}
				f.registry.mu.Unlock()
			} else if mode == "unauthorized" {
				f.registry.status.Store(401)
			} else {
				f.registry.hook = func() {
					once.Do(func() {
						if mode == "rotate" {
							os.Setenv("NATIVE_PUBLISH_TOKEN", nativeToken+"rotated")
							return
						}
						done := make(chan error, 1)
						go func() {
							done <- f.store.Update(context.Background(), func(st *domain.State) error {
								j := workerJob(st, report.JobID)
								a := workerAttempt(st, report.AttemptID)
								switch mode {
								case "cancel":
									j.CancellationRequested = true
								case "fence":
									j.Fence++
								case "config":
									j.Inputs.ConfigRevision = "changed"
								case "expire":
									a.LeaseUntil = time.Now().Add(-time.Second)
								case "deadline":
									j.Deadline = time.Now().Add(-time.Second)
								}
								return nil
							})
						}()
						select {
						case e := <-done:
							if e != nil {
								t.Error(e)
							}
						case <-time.After(time.Second):
							t.Error("network occurred under store lock")
						}
					})
				}
			}
			if _, e := f.s.ReportWorker(context.Background(), f.p, report); e == nil {
				t.Fatal("invalid post-I/O admission")
			}
			st, _ := f.store.Read(context.Background())
			if len(st.DeliveryArtifacts) != 0 {
				t.Fatal("invalid artifact admitted")
			}
		})
	}
}
func TestNativeOrdinaryExpiryReconciliationPreservesAttempt(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := newNativeFixture(t, kind)
			report := f.start(t)
			expireWorker(t, f.store, report.AttemptID)
			if _, e := f.s.ReportWorker(context.Background(), f.p, report); e == nil {
				t.Fatal("expired report accepted")
			}
			v, e := f.s.Execute(context.Background(), domain.Command{Action: "reconcile_worker_publication", Actor: "operator", ProductID: "p", ID: report.JobID, Data: map[string]any{}})
			if e != nil {
				t.Fatal(e)
			}
			for range 2 {
				if _, e = f.s.Execute(context.Background(), domain.Command{Action: "reconcile_worker_publication", Actor: "operator", ProductID: "p", ID: report.JobID, Data: map[string]any{}}); e != nil {
					t.Fatal("successful reconciliation replay rejected", e)
				}
			}
			if v.(domain.WorkerJob).Status != "SUCCEEDED" {
				t.Fatal(v)
			}
			st, _ := f.store.Read(context.Background())
			a := workerAttempt(&st, report.AttemptID)
			if a.Status != "EXPIRED" || a.Report != nil || len(st.DeliveryArtifacts) != 1 || st.DeliveryArtifacts[0].Native.Reconciliation == nil {
				t.Fatal("reconciliation rewrote attempt")
			}
			if _, e := f.s.ReportWorker(context.Background(), f.p, report); e == nil {
				t.Fatal("expired report accepted after reconciliation")
			}
		})
	}
}

func TestNativeRestoreActiveSourceContinuesAndQuarantinePersists(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := newNativeFixture(t, kind)
			ctx := context.Background()
			report := f.start(t)
			backup, e := f.s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			restored, reopen := openWorkerTestStore(t, kind)
			defer func() { restored.Close() }()
			copy := New(restored)
			copy.ConfigureWorkers(f.s.externalWorkers)
			if _, e = copy.RestoreBackup(ctx, "operator", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			restored.Close()
			restored = reopen()
			copy.store = restored
			if _, e = f.s.ReportWorker(ctx, f.p, report); e != nil {
				t.Fatal("source stopped after independent restore", e)
			}
			for _, command := range []domain.Command{{Action: "reconcile_worker_publication", ID: report.JobID, ProductID: "p", Actor: "operator", Data: map[string]any{}}, {Action: "configure_component", ProductID: "p", Actor: "operator", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "profile": f.recipe.Profile, "image_repository": nativeImage(f.recipe)}}} {
				if _, e = copy.Execute(ctx, command); e == nil {
					t.Fatal("restored native mutation accepted", command.Action)
				}
			}
			if _, e = copy.ReportWorker(ctx, f.p, report); e == nil {
				t.Fatal("restored publication report accepted")
			}
			st, _ := restored.Read(ctx)
			j := workerJob(&st, report.JobID)
			if !j.CancellationRequested || j.Status != "RECONCILIATION_REQUIRED" || len(st.NativeQuarantines) != 1 || len(st.DeliveryArtifacts) != 0 {
				t.Fatal("restore quarantine lost")
			}
			second, e := copy.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			third, _ := openWorkerTestStore(t, kind)
			defer third.Close()
			again := New(third)
			if _, e = again.RestoreBackup(ctx, "operator", second.ArchiveBase64, second.SHA256); e != nil {
				t.Fatal(e)
			}
			st, _ = third.Read(ctx)
			if len(st.NativeQuarantines) != 1 || !nativeQuarantined(&st, "p") {
				t.Fatal("quarantine not preserved across backup")
			}
		})
	}
}
func TestNativeClaimsRolesFiltersAndUnforgeableQueue(t *testing.T) {
	f := newNativeFixture(t, "local")
	ctx := context.Background()
	job := f.queue(t)
	bp, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
	if _, e := f.s.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"}); e == nil {
		t.Fatal("builder claimed publication")
	}
	for _, claim := range []domain.WorkerClaim{{ProductID: "other", RepositoryID: "source", Capability: "registry_publish"}, {ProductID: "p", RepositoryID: "other", Capability: "registry_publish"}, {ProductID: "p", RepositoryID: "source", Capability: "registry_publish", RecipeID: f.recipe.ID, RecipeHash: "sha256:" + strings.Repeat("0", 64)}} {
		if _, e := f.s.ClaimWorker(ctx, f.p, claim); e == nil {
			t.Fatal("invalid claim accepted")
		}
	}
	l, e := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish", RecipeID: f.recipe.ID, RecipeHash: f.recipe.Hash})
	if e != nil || l == nil || l.Job.ID != job.ID {
		t.Fatal("valid claim stranded", e)
	}
	if _, e = f.s.StartWorker(ctx, bp, workerRequest(l)); e == nil {
		t.Fatal("builder started publisher lease")
	}
	v, e := f.s.Query(ctx, "get_worker_job", job.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(v.(map[string]any)["attempts"].([]domain.WorkerAttempt)) != 1 {
		t.Fatal("bounded query wrong")
	}
	c := domain.Command{Action: "queue_worker_publication", Actor: "operator", ProductID: "p", Data: map[string]any{"build_job_id": f.build.ID, "component_id": "component", "recipe_id": f.recipe.ID, "recipe_hash": f.recipe.Hash, "recipe_revision": f.recipe.Revision, "config_revision": "cfg1", "request_key": "native-unforgeable-target", "deadline": time.Now().Add(time.Minute), "source_sha": strings.Repeat("c", 40)}}
	if _, e = f.s.Execute(ctx, c); e == nil {
		t.Fatal("caller source override accepted")
	}
}
func TestNativeDisabledAuthorityReopenBlocksWithoutPanic(t *testing.T) {
	f := newNativeFixture(t, "local")
	report := f.start(t)
	if _, e := f.s.ReportWorker(context.Background(), f.p, report); e != nil {
		t.Fatal(e)
	}
	f.s.ConfigureWorkers(nil)
	st, _ := f.store.Read(context.Background())
	if e := f.s.publicationEligible(&st, workerJob(&st, report.JobID)); e == nil {
		t.Fatal("disabled authority eligible")
	}
	if _, e := f.s.inspectNativeArtifact(context.Background(), domain.ExternalOperation{Meta: domain.Meta{ProductID: "p"}, ApplicationID: "component", ExistingArtifact: &st.DeliveryArtifacts[0], Snapshot: &domain.DeliverySnapshot{Revision: domain.IntegrationRevision{RepositoryID: "source", HeadCommit: shaHead}, Build: domain.ComponentBuild{ImageRepository: nativeImage(f.recipe)}}}); e == nil {
		t.Fatal("disabled native registry inspection accepted")
	}
}

func TestNativeQuarantineRejectsProductDeletionAcrossReopenAndBackup(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := newNativeFixture(t, kind)
			ctx := context.Background()
			backup, e := f.s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			restored, reopen := openWorkerTestStore(t, kind)
			defer func() { restored.Close() }()
			copy := New(restored)
			copy.ConfigureWorkers(f.s.externalWorkers)
			if _, e = copy.RestoreBackup(ctx, "operator", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			check := func(s *Service, store workerTestStore) {
				t.Helper()
				before, e := store.Read(ctx)
				if e != nil {
					t.Fatal(e)
				}
				for _, op := range before.Operations {
					if !terminalOperation(op.Status) {
						t.Fatal("fixture must reach deletion gate with all operations terminal")
					}
				}
				if len(before.Products) != 1 || len(before.NativeQuarantines) != 1 {
					t.Fatal("restored fixture missing Product/quarantine")
				}
				_, e = s.Execute(ctx, domain.Command{Action: "delete_product", Actor: "operator", ProductID: "p", Data: map[string]any{"name": before.Products[0].Name}})
				if e == nil || !strings.Contains(e.Error(), "native-quarantined product cannot be deleted") {
					t.Fatal("quarantined deletion not rejected at quarantine gate", e)
				}
				after, e := store.Read(ctx)
				if e != nil {
					t.Fatal(e)
				}
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if !bytes.Equal(a, b) {
					t.Fatal("rejected deletion changed Product, quarantine or history")
				}
				if _, e = s.Execute(ctx, domain.Command{Action: "create_product", Actor: "operator", ID: "p", Data: map[string]any{"name": "recreated"}}); e == nil {
					t.Fatal("quarantined Product ID became reusable")
				}
				recipe, _ := s.externalWorkers.Recipe("build")
				if _, e = s.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "quarantine-deletion-build", "recipe_id": recipe.ID, "recipe_revision": recipe.Revision, "recipe_hash": recipe.Hash, "config_revision": "cfg1", "revision_id": "rev", "source_sha": shaHead, "parameters": map[string]any{}, "deadline": time.Now().Add(time.Minute), "max_attempts": 1}}); e == nil {
					t.Fatal("native queue escaped quarantine after deletion attempt")
				}
				p, e := s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				if l, e := s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"}); e != nil || l != nil {
					t.Fatal("native claim escaped quarantine", e)
				}
				if _, e = s.Query(ctx, "get_worker_job", f.build.ID); e != nil {
					t.Fatal("quarantined history unavailable", e)
				}
			}
			check(copy, restored)
			restored.Close()
			restored = reopen()
			copy.store = restored
			check(copy, restored)
			next, e := copy.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			second, _ := openWorkerTestStore(t, kind)
			defer second.Close()
			again := New(second)
			again.ConfigureWorkers(f.s.externalWorkers)
			if _, e = again.RestoreBackup(ctx, "operator", next.ArchiveBase64, next.SHA256); e != nil {
				t.Fatal(e)
			}
			check(again, second)
			original, e := f.store.Read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.s.Execute(ctx, domain.Command{Action: "delete_product", Actor: "operator", ProductID: "p", Data: map[string]any{"name": original.Products[0].Name}}); e != nil {
				t.Fatal("normal unquarantined deletion changed", e)
			}
			original, e = f.store.Read(ctx)
			if e != nil || len(original.Products) != 0 {
				t.Fatal("normal Product was not deleted", e)
			}
		})
	}
}
