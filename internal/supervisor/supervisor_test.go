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

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
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
	return domain.ExecutionEvent{ID: id, ProductID: "product1", IntegrationID: "integration", GroupIDs: []string{group}, Type: typ, Reason: "UNTRUSTED reason must not enter prompt", HarnessKind: kind, SessionRef: session, TmuxTarget: target, WakeTarget: domain.ExecutionWakeTarget{Kind: kind, SessionRef: session, TmuxTarget: target, Source: "executor"}}
}
func testConfig(t *testing.T, base string) Config {
	t.Helper()
	c := Config{RCPURL: base, TokenEnv: "RCP_TEST_TOKEN", SupervisorID: "test-supervisor", ProductIDs: []string{"product1"}, StateFile: filepath.Join(t.TempDir(), "state.json"), IdleWindow: "1ms", WakeTimeout: "5s", VerifyWindow: "200ms", RateLimit: "30s"}
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

func TestCycleAgainstRealTransportAndLocalApplicationStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	httpServer := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer httpServer.Close()
	post := func(c domain.Command, out any) {
		t.Helper()
		raw, _ := json.Marshal(c)
		res, e := http.Post(httpServer.URL+"/api/commands", "application/json", strings.NewReader(string(raw)))
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d", c.Action, res.StatusCode)
		}
		if out != nil {
			if e = json.NewDecoder(res.Body).Decode(out); e != nil {
				t.Fatal(e)
			}
		}
	}
	var product struct {
		ID string `json:"id"`
	}
	post(domain.Command{Action: "create_product", Actor: "test", Data: map[string]any{"name": "Supervisor contract"}}, &product)
	var feature struct {
		ID string `json:"id"`
	}
	post(domain.Command{Action: "create_feature", Actor: "test", ProductID: product.ID, Data: map[string]any{"title": "Execution", "problem": "test", "goal": "test"}}, &feature)
	var integration struct {
		ID string `json:"id"`
	}
	post(domain.Command{Action: "create_integration", Actor: "test", FeatureID: feature.ID, Data: map[string]any{"title": "Harness", "objective": "test"}}, &integration)
	var group domain.ExecutionGroup
	post(domain.Command{Action: "create_execution_group", Actor: "test", ProductID: product.ID, Data: map[string]any{"name": "group", "orchestrator_id": "orch", "orchestrator_harness": map[string]any{"kind": "codex", "tmux_target": "orch-e2e"}, "integration_ids": []string{integration.ID}}}, &group)
	if group.ID == "" {
		t.Fatal("group not created")
	}
	eventURL := httpServer.URL + "/api/products/" + product.ID + "/execution-events?due_only=true&unresolved_only=true"
	response, e := http.Get(eventURL)
	if e != nil {
		t.Fatal(e)
	}
	var due struct {
		Items []domain.ExecutionEvent `json:"items"`
	}
	e = json.NewDecoder(response.Body).Decode(&due)
	response.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	if len(due.Items) != 1 {
		t.Fatalf("due events: %+v", due.Items)
	}
	a := &fakeAdapter{}
	c := testConfig(t, httpServer.URL)
	c.ProductIDs = []string{product.ID}
	c.HeadlessKinds = nil
	if err = c.Normalize(); err != nil {
		t.Fatal(err)
	}
	sup, err := New(c, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.Cycle(ctx, product.ID, "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(a.requests) != 1 || a.requests[0].GroupID != group.ID || a.requests[0].TmuxTarget != "orch-e2e" || len(a.requests[0].Events) != 1 || a.requests[0].Events[0].WakeTarget.Source != "group_orchestrator" {
		t.Fatalf("wakes: %+v", a.requests)
	}
	response, e = http.Get(strings.Replace(eventURL, "?due_only=true&", "?", 1))
	if e != nil {
		t.Fatal(e)
	}
	var remaining struct {
		Items []domain.ExecutionEvent `json:"items"`
	}
	e = json.NewDecoder(response.Body).Decode(&remaining)
	response.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	if len(remaining.Items) != 1 || len(remaining.Items[0].Acknowledgements) != 1 || remaining.Items[0].Acknowledgements[0].Outcome != "delivered" {
		t.Fatalf("server ack: %+v", remaining.Items)
	}
	response, e = http.Get(httpServer.URL + "/api/execution-groups/" + group.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	var got struct {
		Group domain.ExecutionGroup `json:"group"`
	}
	if e = json.NewDecoder(response.Body).Decode(&got); e != nil {
		t.Fatal(e)
	}
	if got.Group.ID != group.ID || got.Group.OrchestratorHarness.TmuxTarget != "orch-e2e" {
		t.Fatalf("group readback: %+v", got.Group)
	}
	var integration2 struct {
		ID string `json:"id"`
	}
	post(domain.Command{Action: "create_integration", Actor: "test", FeatureID: feature.ID, Data: map[string]any{"title": "No orchestrator", "objective": "test"}}, &integration2)
	var group2 domain.ExecutionGroup
	post(domain.Command{Action: "create_execution_group", Actor: "test", ProductID: product.ID, Data: map[string]any{"name": "no orchestrator", "orchestrator_id": "orch", "integration_ids": []string{integration2.ID}}}, &group2)
	if err = sup.Cycle(ctx, product.ID, "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(a.requests) != 1 {
		t.Fatalf("empty target unexpectedly woke: %+v", a.requests)
	}
	response, e = http.Get(httpServer.URL + "/api/products/" + product.ID + "/execution-events?unresolved_only=true")
	if e != nil {
		t.Fatal(e)
	}
	var all struct {
		Items []domain.ExecutionEvent `json:"items"`
	}
	e = json.NewDecoder(response.Body).Decode(&all)
	response.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	foundEscalated := false
	for _, ev := range all.Items {
		if ev.IntegrationID == integration2.ID {
			if ev.WakeTarget != (domain.ExecutionWakeTarget{}) {
				t.Fatalf("unexpected wake target: %+v", ev.WakeTarget)
			}
			if len(ev.Acknowledgements) == 0 || ev.Acknowledgements[len(ev.Acknowledgements)-1].Outcome != "unsupported" || !ev.EscalationRequired {
				t.Fatalf("unsupported event not escalated: %+v", ev)
			}
			foundEscalated = true
		}
	}
	if !foundEscalated {
		t.Fatal("missing unassigned event without orchestrator harness")
	}
}

func TestRCPResponseAllowsAdditiveFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":"","new_server_field":true}`))
	}))
	defer server.Close()
	c := testConfig(t, server.URL)
	s, err := New(c, &fakeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Items      []domain.ExecutionEvent `json:"items"`
		NextCursor string                  `json:"next_cursor"`
	}
	if err = s.request(context.Background(), http.MethodGet, "/events", "test-token", nil, &response); err != nil {
		t.Fatalf("additive server field rejected: %v", err)
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
	c := Config{TmuxPath: filepath.Join(dir, "tmux"), PgrepPath: filepath.Join(dir, "pgrep"), CodexPath: filepath.Join(dir, "codex"), WakeFor: 5 * time.Second, VerifyFor: 200 * time.Millisecond, IdleFor: time.Millisecond, HeadlessKinds: []string{"codex"}}
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

func TestHeadlessFallbackDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, filepath.Join(dir, "tmux"), "case \"$1\" in has-session) exit 1;; esac\n")
	writeFake(t, filepath.Join(dir, "codex"), "exit 0\n")
	r := (ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), CodexPath: filepath.Join(dir, "codex"), WakeFor: time.Second}}).Wake(context.Background(), WakeRequest{Kind: "codex", SessionRef: "session-12345678", TmuxTarget: "orch", Message: "hi"})
	if r.Outcome != Unsupported || r.Detail != "headless fallback disabled" {
		t.Fatalf("default was not tmux-only: %+v", r)
	}
}

func TestTmuxAdapterRequiresIdleAndVerifiesPaneChange(t *testing.T) {
	dir := t.TempDir()
	state, log := filepath.Join(dir, "pane"), filepath.Join(dir, "argv")
	if err := os.WriteFile(state, []byte("idle prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	writeFake(t, filepath.Join(dir, "tmux"), "printf '%s\\n' \"$*\" >> \"$TMUX_LOG\"\ncase \"$1\" in has-session) exit 0;; capture-pane) cat \"$PANE\";; send-keys) case \"$*\" in *' -l '*) printf 'working\\n' > \"$PANE\";; *Enter*) printf 'Codex: esc to interrupt\\n' > \"$PANE\";; esac;; esac\n")
	t.Setenv("TMUX_LOG", log)
	t.Setenv("PANE", state)
	a := ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), IdleFor: time.Millisecond, SubmitFor: time.Millisecond, WakeFor: 2 * time.Second}}
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

func TestTmuxWakeVerificationBusyMarkerAndUnconsumedFalsePositive(t *testing.T) {
	for _, tc := range []struct {
		name, final string
		want        Outcome
	}{
		{"busy marker", "Claude Esc to interrupt", Delivered},
		{"still typed", "idle prompt\\nliteral safe", Failed},
		{"wrapped typed message", "idle prompt\\n[RCP supervisor] a long message\\nthat wraps across lines", Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pane := filepath.Join(dir, "pane")
			_ = os.WriteFile(pane, []byte("idle prompt"), 0600)
			// The send operation changes the pane to include the typed line; only the
			// post-Enter scripted output determines whether that line was consumed.
			writeFake(t, filepath.Join(dir, "tmux"), "case \"$1\" in has-session) exit 0;; capture-pane) cat \"$PANE\";; send-keys) case \"$*\" in *' -l '*) printf 'idle prompt\\nliteral safe\\n' > \"$PANE\";; *Enter*) printf '"+strings.ReplaceAll(tc.final, "\\n", "\\n")+"\\n' > \"$PANE\";; esac;; esac\n")
			t.Setenv("PANE", pane)
			a := ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), IdleFor: time.Millisecond, SubmitFor: time.Millisecond, WakeFor: 5 * time.Second, VerifyFor: 200 * time.Millisecond}}
			got := a.Wake(context.Background(), WakeRequest{Kind: "codex", TmuxTarget: "orch", Message: "literal safe"})
			if got.Outcome != tc.want {
				t.Fatalf("got %+v", got)
			}
			if tc.want == Failed && got.Detail != "wake not observed" {
				t.Fatalf("wrong failure: %+v", got)
			}
		})
	}
}

func TestTmuxWakeRetriesSwallowedEnter(t *testing.T) {
	dir := t.TempDir()
	pane, calls := filepath.Join(dir, "pane"), filepath.Join(dir, "enters")
	_ = os.WriteFile(pane, []byte("idle prompt"), 0600)
	writeFake(t, filepath.Join(dir, "tmux"), `case "$1" in
has-session) exit 0;;
capture-pane) cat "$PANE";;
send-keys) case "$*" in *" -l "*) printf 'typed wrapped\nmessage\n' > "$PANE";; *Enter*) n=0; test -f "$ENTERS" && n=$(cat "$ENTERS"); n=$((n+1)); printf '%s' "$n" > "$ENTERS"; if [ "$n" -ge 2 ]; then printf 'Codex: Esc to Interrupt\n' > "$PANE"; fi;; esac;;
esac
`)
	t.Setenv("PANE", pane)
	t.Setenv("ENTERS", calls)
	a := ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), IdleFor: time.Millisecond, SubmitFor: time.Millisecond, WakeFor: 5 * time.Second, VerifyFor: 4 * time.Second}}
	got := a.Wake(context.Background(), WakeRequest{Kind: "codex", TmuxTarget: "orch", Message: "[RCP supervisor] a long message that wraps"})
	if got.Outcome != Delivered || got.Detail != "tmux wake observed" {
		t.Fatalf("retry failed to deliver: %+v", got)
	}
	if raw, _ := os.ReadFile(calls); string(raw) != "2" {
		t.Fatalf("expected exactly one Enter retry, got %q", raw)
	}
}

func TestTmuxWakeSubmitsLeftoverWithoutTypingDuplicate(t *testing.T) {
	dir := t.TempDir()
	pane, log := filepath.Join(dir, "pane"), filepath.Join(dir, "argv")
	_ = os.WriteFile(pane, []byte("idle\n[RCP supervisor] previous pending message"), 0600)
	writeFake(t, filepath.Join(dir, "tmux"), `printf '%s\n' "$*" >> "$TMUX_LOG"
case "$1" in has-session) exit 0;; capture-pane) cat "$PANE";; send-keys) case "$*" in *Enter*) printf 'Codex: esc to interrupt\n' > "$PANE";; esac;; esac
`)
	t.Setenv("PANE", pane)
	t.Setenv("TMUX_LOG", log)
	a := ExecAdapter{Config: Config{TmuxPath: filepath.Join(dir, "tmux"), IdleFor: time.Millisecond, SubmitFor: time.Millisecond, WakeFor: 2 * time.Second, VerifyFor: time.Second}}
	got := a.Wake(context.Background(), WakeRequest{Kind: "codex", TmuxTarget: "orch", Message: "[RCP supervisor] duplicate must not be typed"})
	if got.Outcome != Delivered {
		t.Fatalf("leftover wake not submitted: %+v", got)
	}
	raw, _ := os.ReadFile(log)
	if strings.Contains(string(raw), "-l") || strings.Count(string(raw), "Enter") != 1 {
		t.Fatalf("expected only one Enter, got %s", raw)
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
	if c.PollEvery != 15*time.Second || c.IdleFor != 5*time.Second || c.WakeFor != 120*time.Second || c.SubmitFor != 800*time.Millisecond {
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
