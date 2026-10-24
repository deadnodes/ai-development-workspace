package registryobserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/registryobserver/fixture"
	"releasecontrol/internal/workerconfig"
)

type protocolFixture struct {
	mu                        sync.Mutex
	lease                     domain.WorkerLease
	claimed                   bool
	claims, starts, reports   int
	lostStart, lostReport     bool
	denyReport, denyHeartbeat bool
}

func (p *protocolFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("o", 32) {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/api/worker/claim":
		p.claims++
		if p.claimed {
			json.NewEncoder(w).Encode(nil)
			return
		}
		p.claimed = true
		json.NewEncoder(w).Encode(p.lease)
	case "/api/worker/start":
		p.starts++
		p.lease.Job.Status = "RUNNING"
		p.lease.Attempt.Status = "RUNNING"
		if p.lostStart {
			p.lostStart = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(p.lease)
	case "/api/worker/heartbeat":
		if p.denyHeartbeat {
			w.WriteHeader(409)
			return
		}
		json.NewEncoder(w).Encode(p.lease)
	case "/api/worker/report":
		p.reports++
		var report domain.WorkerReport
		if json.NewDecoder(r.Body).Decode(&report) != nil || p.denyReport || domain.ValidateRegistryWorkerReport(p.lease.Job, p.lease.Attempt, report, time.Now().UTC()) != nil {
			w.WriteHeader(409)
			return
		}
		if p.lease.Attempt.Report != nil && !reflect.DeepEqual(*p.lease.Attempt.Report, report) {
			w.WriteHeader(409)
			return
		}
		p.lease.Job.Status = "SUCCEEDED"
		p.lease.Attempt.Status = "SUCCEEDED"
		p.lease.Attempt.Report = &report
		if p.lostReport {
			p.lostReport = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(p.lease)
	default:
		w.WriteHeader(404)
	}
}
func runnerFixture(t *testing.T, handler http.Handler) (Config, *protocolFixture, *atomic.Int64) {
	t.Helper()
	f := fixture.New()
	var reads atomic.Int64
	if handler == nil {
		handler = f.Handler()
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1); handler.ServeHTTP(w, r) }))
	t.Cleanup(registry.Close)
	r := recipeFor(registry.URL)
	in := inputsFor(r, f.Roots)
	inputs := domain.WorkerInputs{RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, ConfigRevision: "cfg1", RepositoryID: r.RepositoryID, Trust: "registry_observation", Parameters: map[string]any{}, Registry: &in}
	h := workerconfig.Fingerprint(inputs)
	now := time.Now().UTC()
	p := &protocolFixture{lease: domain.WorkerLease{Job: domain.WorkerJob{Meta: domain.Meta{ID: "job", ProductID: r.ProductID, CreatedAt: now, UpdatedAt: now}, Capability: "registry_observe", Inputs: inputs, InputHash: h, Status: "CLAIMED", Fence: 1, Deadline: now.Add(time.Minute)}, Attempt: domain.WorkerAttempt{Meta: domain.Meta{ID: "attempt", ProductID: r.ProductID, CreatedAt: now, UpdatedAt: now}, JobID: "job", Fence: 1, InputHash: h, LeaseUntil: now.Add(time.Minute), Status: "CLAIMED"}}}
	api := httptest.NewServer(p)
	t.Cleanup(api.Close)
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	os.Chmod(dir, 0700)
	t.Setenv("OBSERVER_TEST_TOKEN", strings.Repeat("o", 32))
	return Config{Endpoint: api.URL, TokenEnv: "OBSERVER_TEST_TOKEN", ConfigRevision: "cfg1", StateDir: dir, Recipe: r}, p, &reads
}
func TestObserverRunnerLostStartAndReportAreDurable(t *testing.T) {
	for _, lost := range []string{"start", "report"} {
		t.Run(lost, func(t *testing.T) {
			c, p, reads := runnerFixture(t, nil)
			p.lostStart = lost == "start"
			p.lostReport = lost == "report"
			if _, e := RunOnce(context.Background(), c); e == nil {
				t.Fatal("lost reply not surfaced")
			}
			n := reads.Load()
			if lost == "start" && n != 0 {
				t.Fatal("read before START acknowledged")
			}
			raw, e := os.ReadFile(filepath.Join(c.StateDir, "observer.json"))
			if e != nil {
				t.Fatal(e)
			}
			var before journal
			if decode(raw, &before) != nil {
				t.Fatal("missing durable lease/report")
			}
			if lost == "report" && before.Report == nil {
				t.Fatal("report not durable before send")
			}
			out, e := RunOnce(context.Background(), c)
			if e != nil || out == nil || out.Job.Status != "SUCCEEDED" {
				t.Fatal("recovery failed", e)
			}
			if lost == "report" {
				if reads.Load() != n || !out.Attempt.Report.Registry.StartedAt.Equal(before.Report.Registry.StartedAt) || !out.Attempt.Report.Registry.FinishedAt.Equal(before.Report.Registry.FinishedAt) {
					t.Fatal("report replay rescanned or renewed freshness")
				}
			}
			if p.claims != 1 {
				t.Fatal("duplicate job claim")
			}
			if next, e := RunOnce(context.Background(), c); e != nil || next != nil {
				t.Fatal("completed journal didn't safely allow next claim")
			}
			if p.reports != 2 && lost == "report" {
				t.Fatal("blind report duplicate count")
			}
		})
	}
}
func TestObserverRunnerConfigDriftAndCancelRejectPassed(t *testing.T) {
	t.Run("config", func(t *testing.T) {
		c, p, reads := runnerFixture(t, nil)
		p.lostReport = true
		RunOnce(context.Background(), c)
		n := reads.Load()
		if n == 0 {
			t.Fatal("fixture did not execute initial observation")
		}
		c.ConfigRevision = "cfg2"
		if _, e := RunOnce(context.Background(), c); e == nil || reads.Load() != n {
			t.Fatal("changed config resumed or rescanned")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		started := make(chan struct{}, 1)
		blocked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		})
		c, p, reads := runnerFixture(t, blocked)
		p.denyHeartbeat = true
		if _, e := RunOnce(context.Background(), c); e == nil {
			t.Fatal("cancelled/failed heartbeat passed")
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if reads.Load() == 0 {
			t.Fatal("fixture did not begin GET")
		}
		if p.reports != 0 {
			t.Fatal("passed report after lost heartbeat")
		}
	})
}
func TestObserverRunnerRejectsUnsafeJournalAndIdentity(t *testing.T) {
	for _, mode := range []string{"symlink", "public_state", "mismatched_scope"} {
		t.Run(mode, func(t *testing.T) {
			c, p, reads := runnerFixture(t, nil)
			switch mode {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				os.WriteFile(outside, []byte("private"), 0600)
				os.Symlink(outside, filepath.Join(c.StateDir, "observer.json"))
			case "public_state":
				os.Chmod(c.StateDir, 0755)
			case "mismatched_scope":
				p.lease.Job.Inputs.Registry.Scope.Repository = "other/image"
			}
			if _, e := RunOnce(context.Background(), c); e == nil || reads.Load() != 0 {
				t.Fatal("unsafe journal/scope executed")
			}
		})
	}
}

// This helper runs the actual runner in a killable process, with only fixture
// configuration and a fake observer credential inherited from the parent.
func TestObserverRuntimeSubprocess(t *testing.T) {
	path := os.Getenv("RCP_OBSERVER_PROCESS_CONFIG")
	if path == "" {
		return
	}
	c, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	out, e := RunOnce(context.Background(), c)
	if e != nil || out == nil || out.Job.Status != "SUCCEEDED" {
		t.Fatal("observer process did not complete", e)
	}
}
func TestObserverRunnerProcessCrashRetainsEarliestObservationWindow(t *testing.T) {
	f := fixture.New()
	first := make(chan struct{})
	var blocked atomic.Bool
	base := f.Handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocked.CompareAndSwap(false, true) {
			close(first)
			<-r.Context().Done()
			return
		}
		base.ServeHTTP(w, r)
	})
	c, p, _ := runnerFixture(t, handler)
	file := filepath.Join(c.StateDir, "operator.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(file, b, 0600) != nil {
		t.Fatal("config write")
	}
	spawn := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestObserverRuntimeSubprocess$", "-test.count=1")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.StateDir, "OBSERVER_TEST_TOKEN=" + strings.Repeat("o", 32), "RCP_OBSERVER_PROCESS_CONFIG=" + file}
		return cmd
	}
	firstProcess := spawn()
	if firstProcess.Start() != nil {
		t.Fatal("process start")
	}
	defer func() {
		if firstProcess.ProcessState == nil {
			firstProcess.Process.Kill()
			firstProcess.Wait()
		}
	}()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("child never started registry read")
	}
	raw, e := os.ReadFile(filepath.Join(c.StateDir, "observer.json"))
	if e != nil {
		t.Fatal(e)
	}
	var before journal
	if decode(raw, &before) != nil || before.StartedAt.IsZero() || before.Report != nil {
		t.Fatal("observation start not durable before GET")
	}
	time.Sleep(30 * time.Millisecond)
	firstProcess.Process.Kill()
	firstProcess.Wait()
	second := spawn()
	if out, e := second.CombinedOutput(); e != nil {
		t.Fatalf("restart process failed: %v %s", e, out)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.claims != 1 || p.lease.Attempt.Report == nil || !p.lease.Attempt.Report.Registry.StartedAt.Equal(before.StartedAt) || p.lease.Attempt.Report.Registry.FinishedAt.Sub(before.StartedAt) < 30*time.Millisecond {
		t.Fatal("restart duplicated intent or renewed original observation start")
	}
}

func TestObserverRunnerExpiredObservationWindowDoesNotRescan(t *testing.T) {
	c, p, reads := runnerFixture(t, nil)
	old := time.Now().UTC().Add(-31 * time.Second)
	p.lease.Attempt.CreatedAt = old.Add(-time.Second)
	p.lease.Job.CreatedAt = p.lease.Attempt.CreatedAt
	p.lease.Job.Status = "RUNNING"
	p.lease.Attempt.Status = "RUNNING"
	p.claimed = true
	j := journal{ConfigHash: workerconfig.Fingerprint(c), Lease: p.lease, StartedAt: old}
	if durable(filepath.Join(c.StateDir, "observer.json"), j) != nil {
		t.Fatal("journal")
	}
	if _, e := RunOnce(context.Background(), c); e == nil || reads.Load() != 0 {
		t.Fatal("expired original observation window rescanned/extended")
	}
	raw, _ := os.ReadFile(filepath.Join(c.StateDir, "observer.json"))
	var after journal
	decode(raw, &after)
	if !after.StartedAt.Equal(old) {
		t.Fatal("restart freshened timestamp")
	}
}
