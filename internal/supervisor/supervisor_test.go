package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type fakeAdapter struct {
	mu       sync.Mutex
	results  []WakeResult
	requests []WakeRequest
}

func (f *fakeAdapter) Wake(_ context.Context, r WakeRequest) WakeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if r.TmuxTarget == "" && r.Kind != "codex" && r.Kind != "claude_code" {
		return WakeResult{Outcome: Unsupported, Detail: "no headless adapter"}
	}
	if len(f.results) > 0 {
		x := f.results[0]
		f.results = f.results[1:]
		return x
	}
	return WakeResult{Outcome: Delivered, Detail: "ok"}
}

type fakeRCP struct {
	mu     sync.Mutex
	events []domain.ExecutionEvent
	claims int
	acks   []domain.ExecutionEventAck
}

func (f *fakeRCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauthorized", 401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "execution-events"):
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.events, "next_cursor": ""})
	case r.Method == http.MethodPost:
		var c domain.Command
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		switch c.Action {
		case "claim_execution_event_delivery":
			f.claims++
			_ = json.NewEncoder(w).Encode("delivery-" + c.Data["event_id"].(string))
		case "ack_execution_event":
			f.acks = append(f.acks, domain.ExecutionEventAck{Outcome: c.Data["outcome"].(string), Detail: c.Data["detail"].(string)})
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, "unknown", 422)
		}
	default:
		http.NotFound(w, r)
	}
}

func testEvent(id, kind, session, target, group, typ string) domain.ExecutionEvent {
	return domain.ExecutionEvent{ID: id, ProductID: "product1", IntegrationID: "integration", GroupIDs: []string{group}, Type: typ, Reason: "UNTRUSTED reason must not enter prompt", HarnessKind: kind, SessionRef: session, TmuxTarget: target}
}
func testConfig(t *testing.T, base string) Config {
	t.Helper()
	c := Config{RCPURL: base, TokenEnv: "RCP_TEST_TOKEN", SupervisorID: "test-supervisor", ProductIDs: []string{"product1"}, StateFile: filepath.Join(t.TempDir(), "state.json"), IdleWindow: "1ms", WakeTimeout: "50ms", RateLimit: "30s"}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	return c
}
func runCycle(t *testing.T, events []domain.ExecutionEvent, a *fakeAdapter) (*Supervisor, *fakeRCP) {
	t.Helper()
	api := &fakeRCP{events: events}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	c := testConfig(t, server.URL)
	s, err := New(c, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	return s, api
}

func TestCycleIdleTmuxDeliveryAndOneWakeForSession(t *testing.T) {
	a := &fakeAdapter{}
	events := []domain.ExecutionEvent{testEvent("e1", "codex", "session-12345678", "orch:0.1", "aabbccdd", "execution_stalled"), testEvent("e2", "codex", "session-12345678", "orch:0.1", "aabbccdd", "execution_lost")}
	s, api := runCycle(t, events, a)
	if len(a.requests) != 1 || len(api.acks) != 2 || api.acks[0].Outcome != "delivered" || api.acks[1].Outcome != "delivered" {
		t.Fatalf("wake=%d ack=%+v", len(a.requests), api.acks)
	}
	if !strings.Contains(a.requests[0].Message, "2 events: execution_stalled, execution_lost") || strings.Contains(a.requests[0].Message, "UNTRUSTED") {
		t.Fatalf("unsafe or incomplete message: %s", a.requests[0].Message)
	}
	if _, err := os.Stat(s.Config.StateFile); err != nil {
		t.Fatal("wake state not persisted", err)
	}
}

func TestBusyTmuxFailedThenDelivered(t *testing.T) {
	a := &fakeAdapter{results: []WakeResult{{Outcome: Failed, Detail: "busy"}, {Outcome: Delivered, Detail: "ok"}}}
	ev := []domain.ExecutionEvent{testEvent("e1", "codex", "session-12345678", "orch", "aabbccdd", "execution_stalled")}
	s, api := runCycle(t, ev, a)
	if api.acks[0].Outcome != "failed" || api.acks[0].Detail != "busy" {
		t.Fatalf("busy ack: %+v", api.acks[0])
	}
	if err := s.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(a.requests) != 2 || api.acks[1].Outcome != "delivered" {
		t.Fatalf("second cycle: requests=%d ack=%+v", len(a.requests), api.acks)
	}
}

func TestUnsupportedAndInvalidSessionAreAcknowledged(t *testing.T) {
	for _, tc := range []struct {
		name, kind, session string
		want                string
	}{{"unsupported", "claude_desktop", "session-12345678", "unsupported"}, {"invalid", "codex", "short", "unsupported"}} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdapter{}
			_, api := runCycle(t, []domain.ExecutionEvent{testEvent("e1", tc.kind, tc.session, "", "aabbccdd", "execution_unassigned")}, a)
			wantCalls := 0
			if tc.name == "unsupported" {
				wantCalls = 1
			}
			if len(a.requests) != wantCalls || api.acks[0].Outcome != tc.want {
				t.Fatalf("wake=%d ack=%+v", len(a.requests), api.acks)
			}
		})
	}
}

func TestRateLimitSurvivesRestart(t *testing.T) {
	a := &fakeAdapter{}
	api := &fakeRCP{events: []domain.ExecutionEvent{testEvent("e1", "codex", "session-12345678", "", "aabbccdd", "execution_stalled")}}
	server := httptest.NewServer(api)
	defer server.Close()
	c := testConfig(t, server.URL)
	s, err := New(c, a)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Now = func() time.Time { return now }
	if err = s.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(c, a)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return now.Add(time.Second) }
	if err = restarted.loadState(); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(a.requests) != 1 || api.acks[len(api.acks)-1].Detail != "rate limited" {
		t.Fatalf("restart double-wake: %d, %+v", len(a.requests), api.acks)
	}
}

func writeFake(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}
func TestHeadlessFallbackAndLiveProcessGuard(t *testing.T) {
	dir := t.TempDir()
	tmuxLog := filepath.Join(dir, "tmux.log")
	codexLog := filepath.Join(dir, "codex.log")
	pgrepLog := filepath.Join(dir, "pgrep.log")
	writeFake(t, filepath.Join(dir, "tmux"), "printf '%s\\n' \"$*\" >> \"$TMUX_LOG\"\nexit 1\n")
	writeFake(t, filepath.Join(dir, "pgrep"), "printf '%s\\n' \"$*\" >> \"$PGREP_LOG\"\nexit \"${PGREP_EXIT:-1}\"\n")
	writeFake(t, filepath.Join(dir, "codex"), "printf '%s\\n' \"$*\" >> \"$CODEX_LOG\"\nexit 0\n")
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("PGREP_LOG", pgrepLog)
	t.Setenv("CODEX_LOG", codexLog)
	c := Config{TmuxPath: filepath.Join(dir, "tmux"), PgrepPath: filepath.Join(dir, "pgrep"), CodexPath: filepath.Join(dir, "codex"), WakeFor: time.Second, IdleFor: time.Millisecond}
	req := WakeRequest{Kind: "codex", SessionRef: "session-12345678", TmuxTarget: "orch", Message: "safe"}
	a := ExecAdapter{Config: c}
	if got := a.Wake(context.Background(), req); got.Outcome != Delivered {
		t.Fatalf("fallback: %+v", got)
	}
	if raw, _ := os.ReadFile(codexLog); !strings.Contains(string(raw), "exec resume session-12345678 safe") {
		t.Fatalf("argv not recorded: %s", raw)
	}
	t.Setenv("PGREP_EXIT", "0")
	if got := a.Wake(context.Background(), req); got.Outcome != Failed || got.Detail != "busy" {
		t.Fatalf("live process not guarded: %+v", got)
	}
	if raw, _ := os.ReadFile(codexLog); strings.Count(string(raw), "exec resume") != 1 {
		t.Fatalf("woke live session: %s", raw)
	}
}

func TestTmuxAdapterRequiresIdleAndVerifiesPaneChange(t *testing.T) {
	dir := t.TempDir()
	state, log := filepath.Join(dir, "pane"), filepath.Join(dir, "argv")
	if err := os.WriteFile(state, []byte("idle prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	writeFake(t, filepath.Join(dir, "tmux"), "printf '%s\\n' \"$*\" >> \"$TMUX_LOG\"\ncase \"$1\" in has-session) exit 0;; capture-pane) cat \"$PANE\";; send-keys) case \"$*\" in *' -l '*) printf 'working\\n' > \"$PANE\";; esac;; esac\n")
	t.Setenv("TMUX_LOG", log)
	t.Setenv("PANE", state)
	a := ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), IdleFor: time.Millisecond, WakeFor: 2 * time.Second}}
	r := a.Wake(context.Background(), WakeRequest{Kind: "codex", SessionRef: "session-12345678", TmuxTarget: "orch:0.1", Message: "literal safe"})
	if r.Outcome != Delivered {
		t.Fatalf("idle tmux wake: %+v", r)
	}
	if err := os.WriteFile(state, []byte("Claude: Esc to interrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	r = a.Wake(context.Background(), WakeRequest{Kind: "codex", SessionRef: "session-12345678", TmuxTarget: "orch:0.1", Message: "safe"})
	if r.Outcome != Failed || r.Detail != "busy" {
		t.Fatalf("busy tmux pane: %+v", r)
	}
}

func TestMessageUsesOnlyValidatedFields(t *testing.T) {
	a := &fakeAdapter{}
	ev := testEvent("secret-event-id", "codex", "session-12345678", "", "not-valid!", "bad type with secrets")
	ev.GroupIDs = []string{"aabbccdd"}
	_, _ = runCycle(t, []domain.ExecutionEvent{ev}, a)
	msg := a.requests[0].Message
	for _, bad := range []string{"secret-event-id", "not-valid!", "bad type", "UNTRUSTED", "reason"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("message included untrusted value %q: %s", bad, msg)
		}
	}
}

func TestConfigDefaultsAndTmuxValidation(t *testing.T) {
	c := Config{RCPURL: "https://example.invalid", SupervisorID: "sup", ProductIDs: []string{"product"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.PollEvery != 15*time.Second || c.IdleFor != 5*time.Second || c.WakeFor != 120*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
	for _, v := range []string{"orch", "orch:0.1", "session_a"} {
		if !ValidTmuxTarget(v) {
			t.Fatalf("rejected %q", v)
		}
	}
	for _, v := range []string{"", "bad target", "x/0", "a" + strings.Repeat("x", 64)} {
		if ValidTmuxTarget(v) {
			t.Fatalf("accepted %q", v)
		}
	}
}
