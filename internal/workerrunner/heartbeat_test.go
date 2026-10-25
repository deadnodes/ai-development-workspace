package workerrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerclient"
)

func heartbeatFixture(t *testing.T, handler http.HandlerFunc) (*workerclient.Client, *journalState) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, e := workerclient.New(server.URL, strings.Repeat("t", 40))
	if e != nil {
		t.Fatal(e)
	}
	js := &journalState{path: filepath.Join(t.TempDir(), "journal.json"), value: journal{Owner: nonce(), ParentPID: os.Getpid(), Deadline: time.Now().Add(time.Minute), LeaseUntil: time.Now().Add(300 * time.Millisecond)}}
	if e = saveJournal(js.path, js.value); e != nil {
		t.Fatal(e)
	}
	return client, js
}
func TestWorkerHeartbeatStopJoinsKnownLeaseFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	client, js := heartbeatFixture(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(422) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := superviseWorkerHeartbeat(ctx, cancel, client, domain.WorkerAttemptRequest{}, js)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("heartbeat not started")
	}
	joined := make(chan struct{})
	go func() { stop(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("stop ignored in-flight heartbeat")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not join")
	}
	if ctx.Err() == nil {
		t.Fatal("known pre-report lease failure ignored")
	}
	report, e := prepareReport(ctx, Config{}, "", domain.WorkerAttemptRequest{}, &domain.WorkerLocalOCI{}, nil)
	if e == nil || report.Result == "PASSED" || report.LocalOCI != nil {
		t.Fatal("lease failure admitted local PASSED")
	}
	stop() // idempotent join, including deferred cleanup paths.
}
func TestWorkerHeartbeatStoppedBeforeDelayedLostReport(t *testing.T) {
	var heartbeats, reports atomic.Int32
	client, js := heartbeatFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/heartbeat" {
			heartbeats.Add(1)
			json.NewEncoder(w).Encode(domain.WorkerLease{Attempt: domain.WorkerAttempt{LeaseUntil: time.Now().Add(time.Second)}})
			return
		}
		if r.URL.Path != "/api/worker/report" {
			t.Error("unexpected worker route")
			w.WriteHeader(404)
			return
		}
		if reports.Add(1) == 1 {
			time.Sleep(1200 * time.Millisecond)
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(domain.WorkerLease{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := superviseWorkerHeartbeat(ctx, cancel, client, domain.WorkerAttemptRequest{}, js)
	for heartbeats.Load() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("no heartbeat")
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	before := heartbeats.Load()
	if _, e := client.Report(ctx, domain.WorkerReport{Result: "PASSED"}); e == nil {
		t.Fatal("lost response not surfaced")
	}
	if _, e := client.Report(ctx, domain.WorkerReport{Result: "PASSED"}); e != nil {
		t.Fatal("identical report retry lost execution context", e)
	}
	if ctx.Err() != nil || heartbeats.Load() != before || reports.Load() != 2 {
		t.Fatal("terminal report heartbeat lifetime incorrect")
	}
}
func TestWorkerHeartbeatFinalizationPreservesParentCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "lease-expiry"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			client, js := heartbeatFixture(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-release })
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			ctx, cancel := context.WithTimeout(parent, 150*time.Millisecond)
			defer cancel()
			if mode == "lease-expiry" {
				if e := js.update(func(j *journal) { j.LeaseUntil = time.Now().Add(80 * time.Millisecond) }); e != nil {
					t.Fatal(e)
				}
			}
			leaseUntil := js.snapshot().LeaseUntil
			stop := superviseWorkerHeartbeat(ctx, cancel, client, domain.WorkerAttemptRequest{}, js)
			phase := "in-flight"
			select {
			case <-entered:
			case <-ctx.Done():
				// Journal fsync or scheduling may consume the short lease/context
				// before HTTP is admitted. An expired request need not reach a handler.
				select {
				case <-entered:
				default:
					phase = "pre-entry expiry"
				}
			case <-time.After(time.Second):
				t.Fatal("neither heartbeat entry nor context termination observed")
			}
			manualParentCancel := mode == "cancel" && phase == "in-flight"
			if manualParentCancel {
				parentCancel()
			}
			joined := make(chan struct{})
			go func() { stop(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("cancelled heartbeat not bounded")
			}
			if ctx.Err() == nil {
				t.Fatal("finalization erased parent cancellation/deadline")
			}
			if ctx.Err() != context.Canceled && ctx.Err() != context.DeadlineExceeded {
				t.Fatal("unexpected terminal execution context", ctx.Err())
			}
			// Setup can make the default lease expire before the later-started
			// execution deadline. Check the observed cause against its actual bound
			// of assuming the table mode determines which bound wins.
			if ctx.Err() == context.DeadlineExceeded {
				deadline, _ := ctx.Deadline()
				if time.Now().Before(deadline) {
					t.Fatal("execution deadline terminated prematurely")
				}
			}
			if !manualParentCancel && ctx.Err() == context.Canceled && time.Now().Before(leaseUntil) {
				t.Fatal("lease cancellation occurred before lease expiry")
			}
			assertCancelledHeartbeatReport(t, ctx)
			t.Logf("mode=%s phase=%s terminal=%v; stop joined and local PASSED refused", mode, phase, ctx.Err())
		})
	}
}

func assertCancelledHeartbeatReport(t *testing.T, ctx context.Context) {
	t.Helper()
	report, err := prepareReport(ctx, Config{}, "", domain.WorkerAttemptRequest{}, &domain.WorkerLocalOCI{}, nil)
	if err == nil || report == nil || report.Result == "PASSED" || report.LocalOCI != nil {
		t.Fatal("cancelled finalization admitted a local successful result", err)
	}
}

func TestWorkerHeartbeatAlreadyExpiredBeforeEntry(t *testing.T) {
	for _, mode := range []string{"parent-cancel", "parent-deadline", "lease-expiry"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			// The server is outside the fake-clock bubble. Fake time tests only
			// already-expired contexts; any admitted HTTP request is a failure.
			client, js := heartbeatFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusUnprocessableEntity)
			})
			synctest.Test(t, func(t *testing.T) {
				parent, parentCancel := context.WithCancel(context.Background())
				defer parentCancel()
				if mode == "parent-deadline" {
					var cancelDeadline context.CancelFunc
					parent, cancelDeadline = context.WithDeadline(parent, time.Now().Add(-time.Nanosecond))
					defer cancelDeadline()
				}
				ctx, cancel := context.WithTimeout(parent, 150*time.Millisecond)
				defer cancel()
				leaseUntil := time.Now().Add(80 * time.Millisecond)
				if mode == "lease-expiry" {
					leaseUntil = time.Now().Add(-80 * time.Millisecond)
				}
				if err := js.update(func(j *journal) { j.LeaseUntil = leaseUntil }); err != nil {
					t.Fatal(err)
				}
				if mode == "parent-cancel" {
					parentCancel()
				}
				stop := superviseWorkerHeartbeat(ctx, cancel, client, domain.WorkerAttemptRequest{}, js)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
					t.Fatal("expired context or lease did not terminate execution")
				}
				joined := make(chan struct{})
				go func() { stop(); close(joined) }()
				select {
				case <-joined:
				case <-time.After(time.Second):
					t.Fatal("expired heartbeat did not join")
				}
				want := context.Canceled
				if mode == "parent-deadline" {
					want = context.DeadlineExceeded
				}
				if ctx.Err() != want {
					t.Fatalf("expired %s: got %v, want %v", mode, ctx.Err(), want)
				}
				assertCancelledHeartbeatReport(t, ctx)
				stop()
			})
			if requests.Load() != 0 {
				t.Fatal("already-expired heartbeat reached HTTP handler", requests.Load())
			}
		})
	}
}
