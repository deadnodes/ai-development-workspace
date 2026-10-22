package workerrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type proofDaemon struct {
	daemon               string
	present, namePresent bool
	fail                 bool
	observed             int
	mutate               func()
}

func (s *proofDaemon) identity() (string, error) {
	if s.fail {
		return "", io.ErrUnexpectedEOF
	}
	return s.daemon, nil
}
func (s *proofDaemon) container(id string) (daemonContainer, bool, error) {
	s.observed++
	if s.mutate != nil {
		s.mutate()
		s.mutate = nil
	}
	if s.fail {
		return daemonContainer{}, false, io.ErrUnexpectedEOF
	}
	return daemonContainer{}, s.present || (!containerIDPattern.MatchString(id) && s.namePresent), nil
}
func (s *proofDaemon) close() {}
func recoveryFixture(t *testing.T) (string, string, journal) {
	t.Helper()
	state, _ := filepath.EvalSymlinks(t.TempDir())
	if e := os.Chmod(state, 0700); e != nil {
		t.Fatal(e)
	}
	owner := nonce()
	dir := filepath.Join(state, owner)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	parent := exec.Command("/bin/sleep", "30")
	if e := parent.Start(); e != nil {
		t.Fatal(e)
	}
	_ = parent.Process.Kill()
	_ = parent.Wait()
	r := ownedResource{Name: "rcp-" + owner + "-check", ID: strings.Repeat("a", 64), CreateState: "CREATE_UNCERTAIN"}
	r.Presence = &presenceProof{DaemonID: "fixture-daemon", ID: r.ID, Name: r.Name, Owner: owner, ObservedAt: time.Now().Add(-time.Minute)}
	j := journal{Owner: owner, ParentPID: parent.Process.Pid, Deadline: time.Now().Add(-time.Second), LeaseUntil: time.Now().Add(-time.Second), Resources: []ownedResource{r}}
	file := filepath.Join(dir, "journal.json")
	if e := saveJournal(file, j); e != nil {
		t.Fatal(e)
	}
	return state, file, j
}
func hashFixture(t *testing.T, state, owner string) string {
	t.Helper()
	v, e := InspectLocalCleanup(state, owner)
	if e != nil {
		t.Fatal(e)
	}
	return v.JournalHash
}
func TestLocalCleanupClosureDurableIdempotentAndPreservesUncertainty(t *testing.T) {
	state, file, j := recoveryFixture(t)
	hash := hashFixture(t, state, j.Owner)
	session := &proofDaemon{daemon: "fixture-daemon"}
	open := func(docker) (daemonSession, error) { return session, nil }
	result, e := reconcileLocalCleanup(state, j.Owner, hash, "operator/fixture", open)
	if e != nil || !result.Done || result.ServerReconciled {
		t.Fatal(result, e)
	}
	after, e := readJournal(file)
	if e != nil || after.Closure == nil || after.Resources[0].CreateState != "CREATE_UNCERTAIN" || after.Report != nil || !after.LeaseUntil.Equal(j.LeaseUntil) {
		t.Fatal(after, e)
	}
	bytesBefore, _ := os.ReadFile(file)
	if _, e = reconcileLocalCleanup(state, j.Owner, hash, "operator/fixture", func(docker) (daemonSession, error) { t.Fatal("replay queried provider"); return nil, nil }); e != nil {
		t.Fatal(e)
	}
	bytesAfter, _ := os.ReadFile(file)
	if string(bytesBefore) != string(bytesAfter) {
		t.Fatal("replay rewrote proof")
	}
	if _, e = reconcileLocalCleanup(state, j.Owner, hash, "other", open); e == nil {
		t.Fatal("conflicting proof accepted")
	}
	if e = Recover(state); e != nil {
		t.Fatal("closed journal blocks unrelated execution", e)
	}
	after.Resources[0].CreateState = "REMOVED"
	rawForged, _ := json.Marshal(after)
	if e = os.WriteFile(file, rawForged, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = readJournal(file); e == nil {
		t.Fatal("forged closure accepted")
	}
}
func TestLocalCleanupRefusesUnknownAndChangedEvidence(t *testing.T) {
	cases := []string{"zero-id", "missing-proof", "wrong-daemon", "resource-present", "replacement", "disconnect", "live", "lease", "deadline", "hash", "owner", "symlink-journal", "symlink-owner", "unsafe-state", "unknown-field", "corrupt", "cas", "path-replaced", "busy"}
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			state, file, j := recoveryFixture(t)
			hash := hashFixture(t, state, j.Owner)
			s := &proofDaemon{daemon: "fixture-daemon"}
			owner := j.Owner
			switch scenario {
			case "zero-id":
				j.Resources[0].ID = ""
				j.Resources[0].Presence = nil
			case "missing-proof":
				j.Resources[0].Presence = nil
			case "wrong-daemon":
				s.daemon = "different-daemon"
			case "resource-present":
				s.present = true
			case "replacement":
				s.namePresent = true
			case "disconnect":
				s.fail = true
			case "live":
				j.ParentPID = os.Getpid()
			case "lease":
				j.LeaseUntil = time.Now().Add(time.Minute)
			case "deadline":
				j.Deadline = time.Now().Add(time.Minute)
			case "hash":
				hash = strings.Repeat("0", 64)
			case "owner":
				owner = nonce()
			case "cas":
				s.mutate = func() { j.LeaseUntil = j.LeaseUntil.Add(-time.Second); _ = saveJournal(file, j) }
			case "path-replaced":
				s.mutate = func() {
					dir := filepath.Dir(file)
					_ = os.Rename(dir, dir+"-old")
					_ = os.Mkdir(dir, 0700)
					_ = saveJournal(file, j)
				}
			}
			if scenario == "zero-id" || scenario == "missing-proof" || scenario == "live" || scenario == "lease" || scenario == "deadline" {
				if e := saveJournal(file, j); e != nil {
					t.Fatal(e)
				}
				hash = hashFixture(t, state, j.Owner)
			}
			switch scenario {
			case "symlink-journal":
				_ = os.Rename(file, file+"-old")
				_ = os.Symlink(file+"-old", file)
			case "symlink-owner":
				dir := filepath.Dir(file)
				_ = os.Rename(dir, dir+"-old")
				_ = os.Symlink(dir+"-old", dir)
			case "unsafe-state":
				_ = os.Chmod(state, 0755)
			case "unknown-field":
				b, _ := os.ReadFile(file)
				b = append([]byte(`{"unknown":true,`), b[1:]...)
				_ = os.WriteFile(file, b, 0600)
			case "corrupt":
				_ = os.WriteFile(file, []byte("{"), 0600)
			case "busy":
				f, e := os.OpenFile(filepath.Join(state, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
				if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
					t.Fatal("fixture lock")
				}
			}
			before, _ := os.ReadFile(file)
			_, e := reconcileLocalCleanup(state, owner, hash, "operator/fixture", func(docker) (daemonSession, error) { return s, nil })
			if e == nil {
				t.Fatal("unsafe closure accepted")
			}
			after, _ := os.ReadFile(file)
			if scenario != "cas" && scenario != "path-replaced" && string(before) != string(after) {
				t.Fatal("refusal modified journal")
			}
		})
	}
}
func TestParentDeadRequiresPositiveEvidence(t *testing.T) {
	for _, c := range []struct {
		name  string
		kill  error
		state string
		psErr error
		want  bool
	}{{"esrch", syscall.ESRCH, "", nil, true}, {"eperm", syscall.EPERM, "Z", nil, false}, {"unknown", syscall.EIO, "Z", nil, false}, {"zombie", nil, "Z+\n", nil, true}, {"ps-error", nil, "Z", errors.New("ps"), false}, {"ambiguous", nil, "Z\nS", nil, false}, {"empty", nil, "", nil, false}, {"live", nil, "S", nil, false}} {
		t.Run(c.name, func(t *testing.T) {
			if got := proveParentDead(1, func(int) error { return c.kill }, func(int) (string, error) { return c.state, c.psErr }); got != c.want {
				t.Fatal(got)
			}
		})
	}
}

// A protocol fixture checks that EOF never causes a second transport or retry.
func TestDaemonPipeNoReconnectOnDisconnect(t *testing.T) {
	r, w := io.Pipe()
	in, out := io.Pipe()
	p := &daemonPipe{input: out, reader: bufio.NewReader(r)}
	go func() {
		defer w.Close()
		defer in.Close()
		reader := bufio.NewReader(in)
		req, e := http.ReadRequest(reader)
		if e != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = io.WriteString(w, "HTTP/1.1 200 OK\r\nContent-Length: 23\r\n\r\n{\"ID\":\"fixture-daemon\"}")
	}()
	if _, e := p.identity(); e != nil {
		t.Fatal(e)
	}
	if _, _, e := p.container(strings.Repeat("a", 64)); e == nil {
		t.Fatal("disconnected identity was reused")
	}
}
func TestPresenceRejectsForeignAndAbsent(t *testing.T) {
	r := ownedResource{ID: strings.Repeat("a", 64), Name: "rcp-" + nonce() + "-check"}
	for _, s := range []*proofDaemon{{daemon: "fixture", fail: true}, {daemon: "fixture"}, {daemon: "fixture", present: true}} {
		if _, e := presenceOn(s, r, nonce()); e == nil {
			t.Fatal("unbound ownership accepted")
		}
	}
}
func TestRealLocalCleanupRecovery(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real Docker requires explicit opt-in")
	}
	state, file, j := recoveryFixture(t)
	j.Resources[0].ID = ""
	j.Resources[0].Presence = nil
	home, _ := os.UserHomeDir()
	d := docker{context: "orbstack", config: filepath.Join(home, ".docker")}
	j.DockerContext = d.context
	j.DockerConfig = d.config
	if e := saveJournal(file, j); e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(filepath.Dir(file), "source")
	if e := os.Mkdir(source, 0755); e != nil {
		t.Fatal(e)
	}
	// Existing cached fixture image; exact ID plus owner-label cleanup only.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, e := d.output(ctx, nil, 1024, "create", "--pull", "never", "--name", j.Resources[0].Name, "--label", "rcp.worker.owner="+j.Owner, "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", "/bin/true")
	if e != nil {
		t.Fatal(e)
	}
	exact := stringTrim(id)
	defer func() {
		label, _ := d.output(context.Background(), nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, exact)
		if stringTrim(label) == exact+" "+j.Owner {
			_, _ = d.output(context.Background(), nil, 1024, "rm", "--force", exact)
		}
	}()
	if e = cleanup(file); !errors.Is(e, errUncertainCreate) {
		t.Fatal(e)
	}
	stored, e := readJournal(file)
	if e != nil || stored.Resources[0].Presence == nil || stored.Resources[0].ID != exact || stored.Done {
		t.Fatal("durable actual presence missing", stored, e)
	}
	v, e := InspectLocalCleanup(state, j.Owner)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReconcileLocalCleanup(state, j.Owner, v.JournalHash, "operator/fixture"); e != nil {
		t.Fatal(e)
	}
	binary := filepath.Join(t.TempDir(), "worker")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/worker").CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	// Credentialless native watchdog restart exits without executing original work.
	if b, e := exec.Command(binary, "watchdog", file, j.Owner).CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	if e = Recover(state); e != nil {
		t.Fatal(e)
	}
	after, _ := readJournal(file)
	if !after.Done || after.Report != nil {
		t.Fatal("restart fabricated report")
	}
	// A native run-once reaches a new Claim after Recover. The server returns no
	// eligible work; any start/report or old-work execution is a fixture failure.
	var claims, unexpected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/claim" {
			unexpected.Add(1)
			w.WriteHeader(400)
			return
		}
		var c domain.WorkerClaim
		if json.NewDecoder(r.Body).Decode(&c) != nil || c.RecipeID != "unrelated" {
			unexpected.Add(1)
			w.WriteHeader(400)
			return
		}
		claims.Add(1)
		_, _ = io.WriteString(w, "null")
	}))
	defer server.Close()
	execution := &domain.WorkerExecution{Kind: "source_check", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", Platform: "linux/arm64", CheckArgv: []string{"/bin/true"}, Network: "none", TimeoutSeconds: 30, MemoryMB: 128, StorageMB: 16, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 4096}
	recipe := workerconfig.Recipe{ID: "unrelated", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/fixture/unrelated", Capability: "source_check", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: execution}
	recipe.Hash = workerconfig.RecipeHash(recipe)
	cfg := Config{Endpoint: server.URL, TokenEnv: "RCP_FIXTURE_CLEANUP_TOKEN", ProductID: "p", RepositoryID: "r", ConfigRevision: "fixture", Recipe: recipe, StateDir: state, DockerContext: d.context, DockerConfig: d.config}
	configFile := filepath.Join(t.TempDir(), "operator.json")
	raw, _ := json.Marshal(cfg)
	if e = os.WriteFile(configFile, raw, 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(binary, "run-once", configFile)
	cmd.Env = append(cleanEnv(), "RCP_FIXTURE_CLEANUP_TOKEN="+strings.Repeat("x", 32))
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	if claims.Load() != 1 || unexpected.Load() != 0 {
		t.Fatal("restart replayed original or failed new claim", claims.Load(), unexpected.Load())
	}
	b, _ := json.Marshal(after.Closure)
	t.Logf("real owned late CREATE %s: durable closure %s; native watchdog restart passed", exact, b)
}

type ownedProofDaemon struct {
	proofDaemon
	resource ownedResource
	owner    string
}

func (s *ownedProofDaemon) container(id string) (daemonContainer, bool, error) {
	if s.fail {
		return daemonContainer{}, false, io.ErrUnexpectedEOF
	}
	v := daemonContainer{ID: s.resource.ID, Name: "/" + s.resource.Name}
	v.Config.Labels = map[string]string{"rcp.worker.owner": s.owner}
	return v, s.present, nil
}
func TestPresenceBindsIdentityAndOwnershipOnOneSession(t *testing.T) {
	owner := nonce()
	r := ownedResource{ID: strings.Repeat("b", 64), Name: "rcp-" + owner + "-check"}
	s := &ownedProofDaemon{proofDaemon: proofDaemon{daemon: "original-daemon", present: true}, resource: r, owner: owner}
	p, e := presenceOn(s, r, owner)
	if e != nil || !validPresence(p, r, owner) {
		t.Fatal(p, e)
	}
	r.Presence = p
	j := journal{Owner: owner, Resources: []ownedResource{r}}
	if e = verifyAbsent(&proofDaemon{daemon: "retargeted-daemon"}, j); e == nil {
		t.Fatal("separate daemon invocation manufactured proof")
	}
	s.owner = nonce()
	if _, e = presenceOn(s, r, owner); e == nil {
		t.Fatal("foreign ownership accepted")
	}
	s.owner = owner
	s.resource.Name = "rcp-" + owner + "-buildkit"
	if _, e = presenceOn(s, r, owner); e == nil {
		t.Fatal("contradictory name accepted")
	}
}
func TestLocalClosureCorruptionRejected(t *testing.T) {
	for _, kind := range []string{"not-done", "owner", "hash", "operator", "time", "result", "resource", "daemon", "state", "missing", "proof", "report", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			state, file, j := recoveryFixture(t)
			hash := hashFixture(t, state, j.Owner)
			if _, e := reconcileLocalCleanup(state, j.Owner, hash, "operator/fixture", func(docker) (daemonSession, error) { return &proofDaemon{daemon: "fixture-daemon"}, nil }); e != nil {
				t.Fatal(e)
			}
			j, e := readJournal(file)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "not-done":
				j.Done = false
			case "owner":
				j.Closure.Owner = nonce()
			case "hash":
				j.Closure.PreconditionHash = strings.Repeat("0", 64)
			case "operator":
				j.Closure.OperatorRef = "person@example.invalid"
			case "time":
				j.Closure.VerifiedAt = j.LeaseUntil.Add(-time.Hour)
			case "result":
				j.Closure.Result = "PASSED"
			case "resource":
				j.Closure.Resources[0].ID = strings.Repeat("f", 64)
			case "daemon":
				j.Closure.Resources[0].DaemonID = "different"
			case "state":
				j.Closure.Resources[0].State = "REMOVED"
			case "missing":
				j.Closure.Resources = nil
			case "proof":
				j.Resources[0].Presence = nil
			case "report":
				j.Report = &domain.WorkerReport{Result: "PASSED"}
			}
			rawForged, _ := json.Marshal(j)
			if e = os.WriteFile(file, rawForged, 0600); e != nil {
				t.Fatal(e)
			}
			if kind == "duplicate" {
				raw, _ := os.ReadFile(file)
				raw = append([]byte(`{"done":true,`), raw[1:]...)
				_ = os.WriteFile(file, raw, 0600)
			}
			if _, e = readJournal(file); e == nil {
				t.Fatal("forged closure accepted")
			}
		})
	}
}
func TestConcurrentCleanupCannotOverwriteOperatorClosure(t *testing.T) {
	state, file, j := recoveryFixture(t)
	hash := hashFixture(t, state, j.Owner)
	session := &proofDaemon{daemon: "fixture-daemon", mutate: func() {
		if _, e := updateJournal(file, func(v *journal) error { v.LeaseUntil = time.Now(); return nil }); e == nil {
			t.Fatal("watchdog bypassed operator journal lock")
		}
	}}
	if _, e := reconcileLocalCleanup(state, j.Owner, hash, "operator/fixture", func(docker) (daemonSession, error) { return session, nil }); e != nil {
		t.Fatal(e)
	}
	// A watchdog snapshot/cleanup after closure must not query the provider.
	if e := cleanupWith(file, &raceDocker{}); e != nil {
		t.Fatal(e)
	}
}

func TestDaemonPipeDisconnectAfterIdentityRefusesBothProofPhases(t *testing.T) {
	for _, phase := range []string{"presence", "absence"} {
		t.Run(phase, func(t *testing.T) {
			r, w := io.Pipe()
			in, out := io.Pipe()
			p := &daemonPipe{input: out, reader: bufio.NewReader(r)}
			go func() {
				defer w.Close()
				defer in.Close()
				reader := bufio.NewReader(in)
				req, e := http.ReadRequest(reader)
				if e != nil {
					return
				}
				_ = req.Body.Close()
				_, _ = io.WriteString(w, "HTTP/1.1 200 OK\r\nContent-Length: 23\r\n\r\n{\"ID\":\"fixture-daemon\"}")
			}()
			owner := nonce()
			resource := ownedResource{ID: strings.Repeat("d", 64), Name: "rcp-" + owner + "-check"}
			resource.Presence = &presenceProof{DaemonID: "fixture-daemon", ID: resource.ID, Name: resource.Name, Owner: owner, ObservedAt: time.Now()}
			if phase == "presence" {
				if _, e := presenceOn(p, resource, owner); e == nil {
					t.Fatal("disconnect manufactured presence")
				}
			} else {
				if e := verifyAbsent(p, journal{Owner: owner, Resources: []ownedResource{resource}}); e == nil {
					t.Fatal("disconnect manufactured absence")
				}
			}
		})
	}
}

type recoveryResponseWriter struct{ bytes.Buffer }

func (*recoveryResponseWriter) Close() error { return nil }
func responseFixture(bodies ...string) *daemonPipe {
	var raw strings.Builder
	for _, body := range bodies {
		fmt.Fprintf(&raw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
	return &daemonPipe{input: &recoveryResponseWriter{}, reader: bufio.NewReader(strings.NewReader(raw.String()))}
}
func TestDaemonSemanticFieldAliasesRefused(t *testing.T) {
	owner := nonce()
	id := strings.Repeat("a", 64)
	r := ownedResource{ID: id, Name: "rcp-" + owner + "-check"}
	good := fmt.Sprintf(`{"Id":%q,"Name":%q,"Config":{"Labels":{"rcp.worker.owner":%q,"X":"a","x":"b"}}}`, id, "/"+r.Name, owner)
	for _, body := range []string{`{"ID":"foreign","id":"expected"}`, `{"id":"expected"}`} {
		if _, e := presenceOn(responseFixture(body, good), r, owner); e == nil {
			t.Fatal("noncanonical daemon identity accepted")
		}
	}
	for _, body := range []string{
		fmt.Sprintf(`{"Id":%q,"ID":%q,"Name":%q,"Config":{"Labels":{"rcp.worker.owner":%q}}}`, strings.Repeat("f", 64), id, "/"+r.Name, owner),
		fmt.Sprintf(`{"Id":%q,"Name":%q,"Config":{"Labels":{"rcp.worker.owner":"foreign"}},"config":{"Labels":{"rcp.worker.owner":%q}}}`, id, "/"+r.Name, owner),
	} {
		if _, e := presenceOn(responseFixture(`{"ID":"expected"}`, body), r, owner); e == nil {
			t.Fatal("semantic inspect alias accepted")
		}
	}
	if _, e := presenceOn(responseFixture(`{"ID":"expected"}`, good), r, owner); e != nil {
		t.Fatal("case-distinct arbitrary labels refused", e)
	}
}
func TestJournalSemanticAliasesAndBoundedWrites(t *testing.T) {
	_, file, j := recoveryFixture(t)
	raw, _ := json.Marshal(j)
	aliased := append([]byte(`{"OWNER":"foreign",`), raw[1:]...)
	if _, e := decodeJournal(aliased); e == nil {
		t.Fatal("journal struct alias accepted")
	}
	j.Resources[0].Presence = nil
	raw, _ = json.Marshal(j)
	j.SourceSHA = strings.Repeat("a", 8100-len(raw))
	if e := saveJournal(file, j); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(file)
	proof := &presenceProof{DaemonID: "fixture-daemon", ID: j.Resources[0].ID, Name: j.Resources[0].Name, Owner: j.Owner, ObservedAt: time.Now()}
	if _, e := updateJournal(file, func(v *journal) error { v.Resources[0].Presence = proof; return nil }); e == nil {
		t.Fatal("oversized presence persisted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("oversize update replaced readable journal")
	}
	if _, e := readJournal(file); e != nil {
		t.Fatal(e)
	}
	j.SourceSHA = strings.Repeat("a", 8193)
	if e := saveJournal(file, j); e == nil {
		t.Fatal("oversized save accepted")
	}
	after, _ = os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("oversize save replaced journal")
	}
}

func TestRealCleanupContinuesWhenPresenceExceedsJournalBound(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("real Docker requires explicit opt-in")
	}
	state, file, j := recoveryFixture(t)
	home, _ := os.UserHomeDir()
	d := docker{context: "orbstack", config: filepath.Join(home, ".docker")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, e := d.output(ctx, nil, 1024, "create", "--pull", "never", "--name", j.Resources[0].Name, "--label", "rcp.worker.owner="+j.Owner, "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", "/bin/true")
	if e != nil {
		t.Fatal(e)
	}
	exact := stringTrim(id)
	defer func() {
		identity, _ := d.output(context.Background(), nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, exact)
		if stringTrim(identity) == exact+" "+j.Owner {
			_, _ = d.output(context.Background(), nil, 1024, "rm", "--force", exact)
		}
	}()
	j.DockerContext = d.context
	j.DockerConfig = d.config
	j.Resources[0].ID = exact
	j.Resources[0].Presence = nil
	raw, _ := json.Marshal(j)
	j.SourceSHA = strings.Repeat("a", 8100-len(raw))
	if e = saveJournal(file, j); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(file)
	if e = cleanup(file); !errors.Is(e, errUncertainCreate) {
		t.Fatal("bounded optional proof blocked conservative cleanup", e)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("unpersistable proof changed original journal")
	}
	stored, e := readJournal(file)
	if e != nil || stored.Done || stored.Resources[0].Presence != nil {
		t.Fatal("cleanup manufactured closure or unreadable state", e)
	}
	session, e := openDaemon(d)
	if e != nil {
		t.Fatal(e)
	}
	defer session.close()
	if _, present, e := session.container(exact); e != nil || present {
		t.Fatal("optional proof failure prevented exact removal", e)
	}
	v, e := InspectLocalCleanup(state, j.Owner)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reconcileLocalCleanup(state, j.Owner, v.JournalHash, "operator/fixture", func(docker) (daemonSession, error) { t.Fatal("missing proof reached provider"); return nil, io.EOF }); e == nil {
		t.Fatal("capacity failure granted closure")
	}
	t.Logf("actual exact owned ID %s removed; 8100-byte original journal preserved readable without presence/closure", exact)
}
