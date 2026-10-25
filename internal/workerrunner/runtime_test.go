package workerrunner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/testdocker"
	"strings"
	"sync"
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
	fixtureDocker := testdocker.Load(t)
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
	e := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: fixtureDocker.BuildKitImage, Platform: fixtureDocker.Platform, CheckArgv: []string{"go", "test", "/source/main.go", "/source/main_test.go"}, BuildArgv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20}
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
	cfg := Config{ArtifactSpool: &artifactspool.Config{Root: spoolRoot, MaxArchiveBytes: 16777216, MaxTotalBytes: 67108864, MaxEntries: 4}, Endpoint: server.URL, TokenEnv: "RCP_FIXTURE_WORKER_TOKEN", ProductID: "p", RepositoryID: "r", ConfigRevision: "cfg1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: stateDir, DockerContext: fixtureDocker.Context, DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true}
	configFile := filepath.Join(root, "worker.json")
	b, _ := json.Marshal(cfg)
	if err = os.WriteFile(configFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	var manifests []string
	var binaryDigests []string
	stopDiagnostics := startFixtureDockerDiagnostics(t, cfg)
	defer stopDiagnostics()
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
		archivePath, err := artifactspool.Resolve(*cfg.ArtifactSpool, *report.LocalOCI)
		if err != nil {
			t.Fatalf("report preceded durable spool: %v", err)
		}
		marker := "rcp-worker-fixture"
		if n == 2 {
			marker = "rcp-worker-changed"
		}
		binaryDigests = append(binaryDigests, fixtureOCIELFDigest(t, archivePath, *report.LocalOCI, e.ImagePath, marker))
		manifests = append(manifests, report.LocalOCI.ManifestDigest)
		if report.LocalOCI.SourceSHA != sha {
			t.Fatal("compiled source provenance mismatch")
		}
		if report.LocalOCI.Platform != fixtureDocker.Platform {
			t.Fatal("compiled native platform mismatch", report.LocalOCI.Platform)
		}
		assertOwnedResourcesGone(t, cfg)
	}
	stopDiagnostics()
	if manifests[0] != manifests[1] {
		t.Fatal("clean fixture builds not deterministic", manifests)
	}
	if binaryDigests[0] != binaryDigests[1] || binaryDigests[2] == binaryDigests[0] {
		t.Fatal("compiled binary must repeat for clean builds and change with source", binaryDigests)
	}
	const expectedManifest = "sha256:b53328e30a220c8a754ee29df1dfefd23280c11f0c310418d81796c8729058e9"
	if fixtureDocker.Platform == "linux/arm64" && manifests[0] != expectedManifest {
		t.Fatal("pinned fixture manifest differs from verified golden identity", manifests[0])
	}
	if fixtureDocker.Platform == "linux/amd64" {
		// New native coverage: do not fabricate a golden from the first result.
		// The generic verifier checks OCI platform/contents; this Go fixture
		// separately checks ELF machine, source marker and binary digests.
		t.Logf("observed AMD64 fixture manifest=%s; independent architecture-specific golden not established", manifests[0])
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
	t.Logf("verified %s ELF64: clean binary=%s changed-source binary=%s; source literal markers present", fixtureDocker.Platform, binaryDigests[0], binaryDigests[2])
}

// A separate empty fixture captures only pinned BuildKit startup output. It
// receives no application snapshot, credentials, host mounts or worker request.
func TestRealBuildKitStartupDiagnostics(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real isolated Docker execution requires explicit opt-in")
	}
	profile := testdocker.Load(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dockerConfig := filepath.Join(home, ".docker")
	root, err := os.MkdirTemp("", "rcp-buildkit-startup-")
	if err != nil {
		t.Fatal(err)
	}
	owner := nonce()
	dir := filepath.Join(root, owner)
	file := filepath.Join(dir, "journal.json")
	journalReady := false
	t.Cleanup(func() {
		if journalReady {
			if err := cleanup(file); err != nil {
				t.Errorf("startup fixture cleanup uncertain; journal retained at %s: %v", file, err)
				return
			}
			assertOwnedResourcesGone(t, Config{StateDir: root, DockerContext: profile.Context, DockerConfig: dockerConfig})
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "rcp-" + owner + "-buildkit"
	now := time.Now()
	j := journal{Owner: owner, ParentPID: os.Getpid(), DockerContext: profile.Context, DockerConfig: dockerConfig, Deadline: now.Add(time.Minute), LeaseUntil: now.Add(time.Minute), Resources: []ownedResource{{Name: name, CreateState: "CREATING"}}}
	if err := saveJournal(file, j); err != nil {
		t.Fatal(err)
	}
	journalReady = true
	d := docker{context: profile.Context, config: dockerConfig}
	e := &domain.WorkerExecution{BuildKitImage: profile.BuildKitImage, Platform: profile.Platform, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128}
	createCtx, finishCreate := context.WithTimeout(context.Background(), 20*time.Second)
	id, err := d.createBuildKit(createCtx, name, owner, e)
	finishCreate()
	if err != nil {
		t.Fatal("empty startup fixture create failed", err)
	}
	j.Resources[0].ID, j.Resources[0].CreateState = id, "CREATED"
	if err := saveJournal(file, j); err != nil {
		t.Fatal(err)
	}
	probe, finishProbe := context.WithTimeout(context.Background(), 20*time.Second)
	defer finishProbe()
	identity, err := d.output(probe, nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, id)
	if err != nil || stringTrim(identity) != id+" "+owner {
		t.Fatal("empty startup fixture ownership not proven")
	}
	stdout, stderr := &boundedBuffer{remaining: 8192}, &boundedBuffer{remaining: 8192}
	cmd := d.command(probe, "start", "--attach", id)
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = stdout, stderr, 2*time.Second
	runErr := cmd.Run()
	// Quoting prevents startup text from inserting terminal control sequences.
	t.Logf("empty BuildKit startup stdout=%q stderr=%q", stdout.String(), stderr.String())
	stateCtx, finishState := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishState()
	data, err := d.output(stateCtx, nil, 2048, "inspect", "--format", `{"id":{{json .Id}},"owner":{{json (index .Config.Labels "rcp.worker.owner")}},"status":{{json .State.Status}},"exit_code":{{.State.ExitCode}},"oom_killed":{{.State.OOMKilled}}}`, id)
	if err != nil {
		t.Fatal("empty startup fixture state not observable")
	}
	state, err := decodeFixtureDockerState(data, id, owner)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" || runErr != nil && probe.Err() == nil {
		t.Fatalf("empty BuildKit startup failed: status=%s exit=%d oom=%t attach=%v", state.Status, state.ExitCode, state.OOMKilled, runErr)
	}
	if err := d.waitBuildKit(stateCtx, id); err != nil {
		t.Fatal("empty startup fixture process sandbox not ready", err)
	}
	// CI may install an executable attachment for this pinned RootlessKit.
	// A working daemon alone must not hide a globally relaxed host policy or
	// a profile applied to the whole container instead of the executable.
	if expected := os.Getenv("RCP_TEST_APPARMOR_PROFILE"); expected != "" {
		label, err := d.output(stateCtx, nil, 1024, "exec", id, "cat", "/proc/1/attr/current")
		if err != nil || stringTrim(label) != expected+" (unconfined)" {
			t.Fatalf("RootlessKit executable attachment not proven: profile=%q err=%v", label, err)
		}
		t.Logf("RootlessKit executable profile=%q; process sandbox ready", stringTrim(label))
	}
}

type fixtureDockerState struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Status    string `json:"status"`
	ExitCode  int    `json:"exit_code"`
	OOMKilled bool   `json:"oom_killed"`
}

func decodeFixtureDockerState(data []byte, id, owner string) (fixtureDockerState, error) {
	var state fixtureDockerState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if !containerIDPattern.MatchString(id) || !ownerPattern.MatchString(owner) || state.ID != id || state.Owner != owner {
		return fixtureDockerState{}, fmt.Errorf("fixture diagnostics ownership mismatch")
	}
	return state, nil
}

// Observe only this temporary worker's durable exact IDs and selected container
// state fields. Never emit full inspect output, environment, arguments, source,
// journal contents or daemon logs. This does not change execution or cleanup.
func startFixtureDockerDiagnostics(t *testing.T, cfg Config) func() {
	t.Helper()
	setting := os.Getenv("RCP_TEST_DOCKER_DIAGNOSTICS")
	if setting == "" {
		return func() {}
	}
	if setting != "1" {
		t.Fatal("fixture Docker diagnostics accepts only explicit 1 or unset")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d := docker{context: cfg.DockerContext, config: cfg.DockerConfig}
	go func() {
		defer close(done)
		seen := map[fixtureDockerState]bool{}
		capture := func() {
			entries, err := os.ReadDir(cfg.StateDir)
			if err != nil {
				return
			}
			for _, entry := range entries {
				if !entry.IsDir() || !ownerPattern.MatchString(entry.Name()) {
					continue
				}
				j, err := readJournal(filepath.Join(cfg.StateDir, entry.Name(), "journal.json"))
				if err != nil || j.Owner != entry.Name() {
					continue
				}
				for _, resource := range j.Resources {
					role := ""
					switch resource.Name {
					case "rcp-" + j.Owner + "-check":
						role = "check"
					case "rcp-" + j.Owner + "-buildkit":
						role = "buildkit"
					}
					if role == "" || !containerIDPattern.MatchString(resource.ID) || len(seen) >= 32 {
						continue
					}
					probe, finish := context.WithTimeout(ctx, 2*time.Second)
					data, err := d.output(probe, nil, 2048, "inspect", "--format", `{"id":{{json .Id}},"owner":{{json (index .Config.Labels "rcp.worker.owner")}},"status":{{json .State.Status}},"exit_code":{{.State.ExitCode}},"oom_killed":{{.State.OOMKilled}}}`, resource.ID)
					finish()
					if err != nil {
						continue
					}
					state, err := decodeFixtureDockerState(data, resource.ID, j.Owner)
					if err != nil || seen[state] {
						continue
					}
					seen[state] = true
					t.Logf("fixture Docker diagnostic: role=%s id=%s status=%s exit_code=%d oom_killed=%t", role, state.ID, state.Status, state.ExitCode, state.OOMKilled)
				}
			}
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				capture()
			}
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func TestFixtureDockerDiagnosticsOwnership(t *testing.T) {
	t.Setenv("RCP_TEST_DOCKER_DIAGNOSTICS", "1")
	stop := startFixtureDockerDiagnostics(t, Config{StateDir: t.TempDir()})
	stop()
	stop() // Explicit and cleanup stops must both join safely, without Docker.
	id, owner := strings.Repeat("a", 64), strings.Repeat("b", 32)
	valid := fixtureDockerState{ID: id, Owner: owner, Status: "exited", ExitCode: 1}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeFixtureDockerState(data, id, owner)
	if err != nil || state != valid {
		t.Fatal("exact-owned selected state rejected", err)
	}
	for _, tc := range []struct {
		data      []byte
		id, owner string
	}{
		{data, strings.Repeat("c", 64), owner}, {data, id, strings.Repeat("d", 32)},
		{data, "partial-id", owner}, {data, id, "unknown-owner"}, {[]byte("{"), id, owner},
	} {
		if _, err := decodeFixtureDockerState(tc.data, tc.id, tc.owner); err == nil {
			t.Fatal("foreign or invalid diagnostic identity accepted")
		}
	}
}

// Inspect only bounded in-memory snapshots; never extract archive paths or run
// the binary. The generic production OCI verifier permits non-ELF image content.
func fixtureOCIELFDigest(t *testing.T, archivePath string, receipt domain.WorkerLocalOCI, imagePath, marker string) string {
	t.Helper()
	const max = int64(16 << 20)
	read := func(r io.Reader) []byte {
		t.Helper()
		b, err := io.ReadAll(io.LimitReader(r, max+1))
		if err != nil || int64(len(b)) > max {
			t.Fatalf("fixture archive/layer outside read bound: %v", err)
		}
		return b
	}
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	archive := read(f)
	verified, err := VerifyOCIBytes(archive, max, receipt.Platform)
	if err != nil || verified.ArchiveHash != receipt.ArchiveHash || verified.ManifestDigest != receipt.ManifestDigest {
		t.Fatalf("fixture OCI snapshot differs from verified receipt: %v", err)
	}
	files := fixtureTarEntries(t, archive, max)
	var manifest imageManifest
	if err := json.Unmarshal(files["blobs/sha256/"+strings.TrimPrefix(receipt.ManifestDigest, "sha256:")], &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("fixture requires exact single-layer manifest: %v", err)
	}
	layer := files["blobs/sha256/"+strings.TrimPrefix(manifest.Layers[0].Digest, "sha256:")]
	switch manifest.Layers[0].MediaType {
	case "application/vnd.oci.image.layer.v1.tar":
	case "application/vnd.oci.image.layer.v1.tar+gzip":
		gz, err := gzip.NewReader(bytes.NewReader(layer))
		if err != nil {
			t.Fatal(err)
		}
		layer = read(gz)
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unsupported fixture layer media type")
	}
	compiled := fixtureTarEntries(t, layer, max)[imagePath]
	if err := validateFixtureELF(compiled, receipt.Platform, marker); err != nil {
		t.Fatal(err)
	}
	return digestBytes(compiled)
}

func fixtureTarEntries(t *testing.T, data []byte, max int64) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil || n >= 128 {
			t.Fatalf("invalid/beyond-bound fixture tar: %v", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) || h.Size < 0 || h.Size > max-total {
			t.Fatal("fixture requires bounded regular entries")
		}
		if _, duplicate := files[h.Name]; duplicate {
			t.Fatal("duplicate fixture tar entry")
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(b)) != h.Size {
			t.Fatalf("truncated fixture tar entry: %v", err)
		}
		files[h.Name] = b
		total += h.Size
	}
}

func validateFixtureELF(compiled []byte, platform, marker string) error {
	f, err := elf.NewFile(bytes.NewReader(compiled))
	if err != nil {
		return fmt.Errorf("Go fixture binary must be ELF: %w", err)
	}
	defer f.Close()
	expected, ok := map[string]elf.Machine{"linux/arm64": elf.EM_AARCH64, "linux/amd64": elf.EM_X86_64}[platform]
	if !ok || f.Class != elf.ELFCLASS64 || f.Machine != expected {
		return fmt.Errorf("Go fixture ELF class/machine mismatch: %s/%s for %s", f.Class, f.Machine, platform)
	}
	if !bytes.Contains(compiled, []byte(marker)) {
		return fmt.Errorf("Go fixture binary lacks expected source literal %q", marker)
	}
	return nil
}

func TestGoFixtureELFContract(t *testing.T) {
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		t.Run(platform, func(t *testing.T) {
			machine := elf.EM_AARCH64
			if platform == "linux/amd64" {
				machine = elf.EM_X86_64
			}
			// Minimal parseable ELF64 header; acceptance still uses real Go output.
			header := make([]byte, 64)
			copy(header, []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
			binary.LittleEndian.PutUint16(header[16:], uint16(elf.ET_EXEC))
			binary.LittleEndian.PutUint16(header[18:], uint16(machine))
			binary.LittleEndian.PutUint32(header[20:], 1)
			binary.LittleEndian.PutUint16(header[52:], 64)
			compiled := append(header, []byte("rcp-worker-fixture")...)
			if err := validateFixtureELF(compiled, platform, "rcp-worker-fixture"); err != nil {
				t.Fatal(err)
			}
			wrongMachine := bytes.Clone(compiled)
			binary.LittleEndian.PutUint16(wrongMachine[18:], uint16(elf.EM_386))
			wrongClass := bytes.Clone(compiled)
			wrongClass[4] = byte(elf.ELFCLASS32)
			for _, tc := range []struct {
				bytes  []byte
				marker string
			}{
				{[]byte("compiled-fixture"), "rcp-worker-fixture"},
				{wrongMachine, "rcp-worker-fixture"}, {wrongClass, "rcp-worker-fixture"},
				{compiled, "rcp-worker-changed"},
			} {
				if err := validateFixtureELF(tc.bytes, platform, tc.marker); err == nil {
					t.Fatal("invalid Go fixture binary accepted")
				}
			}
		})
	}
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
