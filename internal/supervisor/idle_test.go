package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func TestTmuxWakePinsPaneAndSharesAliasCooldown(t *testing.T) {
	config := testConfig(t, "http://localhost:8000")
	config.SubmitFor = time.Millisecond
	config.TmuxPath = filepath.Join(t.TempDir(), "tmux")
	state := filepath.Join(t.TempDir(), "pane-state")
	t.Setenv("PANE_STATE", state)
	writeFake(t, config.TmuxPath, `case "$1" in
display)
  if [ "$4" = orch ] && [ ! -e "$PANE_STATE" ]; then
    printf '%%11 $7 orch\n'
  else
    printf '%%22 $7 orch\n'
  fi;;
capture-pane)
  test "$4" = %11 || exit 9
  if [ "$(cat "$PANE_STATE" 2>/dev/null)" = submitted ]; then
    printf 'esc to interrupt\n'
  else
    printf 'idle\n'
  fi;;
display-message) test "$4" = %11 && printf '1\n';;
set-buffer) test "${11}" = %11 && printf 'switched' > "$PANE_STATE";;
send-keys) test "$3 $4" = '%11 Enter' && [ "$(cat "$PANE_STATE")" = switched ] && printf 'submitted' > "$PANE_STATE";;
*) exit 9;;
esac`)
	first := idleTestEvent(domain.EventGroupMemberStale, time.Now().UTC())
	first.WakeTarget.TmuxTarget = "orch"
	api := &fakeRCP{events: []domain.ExecutionEvent{first}}
	supervisor, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Client.Transport = handlerTransport{api}
	if err := supervisor.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if status, err := os.ReadFile(state); err != nil || string(status) != "submitted" || len(api.acks) != 1 || api.acks[0].Outcome != string(Delivered) {
		t.Fatalf("did not submit to the original pane: status=%q err=%v acks=%+v", status, err, api.acks)
	}
	second := first
	second.ID, second.WakeTarget.TmuxTarget = "second", "orch:0.1"
	api.events = []domain.ExecutionEvent{second}
	if err := supervisor.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if api.claims != 1 || supervisor.wakes["1|idle|tmux:$7"].IsZero() {
		t.Fatalf("alias received a second budget: claims=%d wakes=%v", api.claims, supervisor.wakes)
	}
}

func TestStalledAndStaleShareActiveGuardButTurnEndDoesNot(t *testing.T) {
	now := time.Now().UTC()
	stalled := idleTestEvent("execution_stalled", now)
	stalled.GroupIdle = nil
	stale := idleTestEvent(domain.EventGroupMemberStale, now)
	active := &fakeAdapter{results: []WakeResult{{Outcome: SkippedActive}}}
	supervisor, api := runCycle(t, []domain.ExecutionEvent{stalled, stale}, active)
	if len(active.requests) != 1 || !active.requests[0].IdleCheck || len(api.acks) != 2 || len(supervisor.wakes) != 0 {
		t.Fatalf("overlapping wakes did not share one guard: requests=%+v acks=%+v", active.requests, api.acks)
	}
	for _, ack := range api.acks {
		if ack.Outcome != string(SkippedActive) {
			t.Fatalf("wrong guarded outcome: %+v", ack)
		}
	}
	turnEnd := stalled
	turnEnd.ID, turnEnd.Type = "turn-end", "execution_continuation_required"
	api.events = []domain.ExecutionEvent{turnEnd}
	if err := supervisor.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(active.requests) != 2 || active.requests[1].IdleCheck || api.acks[2].Outcome != string(Delivered) {
		t.Fatalf("turn-end continuation changed: requests=%+v acks=%+v", active.requests, api.acks)
	}
}

func TestOversizedEventPagesShrinkAndDispatchBeforeContinuing(t *testing.T) {
	config := testConfig(t, "http://localhost:8000")
	adapter := &fakeAdapter{}
	supervisor, err := New(config, adapter)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeRCP{}
	var limits []int
	supervisor.Client.Transport = handlerTransport{http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			api.ServeHTTP(writer, request)
			return
		}
		query := request.URL.Query()
		limit, _ := strconv.Atoi(query.Get("limit"))
		limits = append(limits, limit)
		if limit > 25 {
			_, _ = writer.Write([]byte(strings.Repeat("x", domain.MaxExecutionEventPageBytes+1)))
			return
		}
		cursor := "second"
		event := testEvent("first", "codex", "session-12345678", "", "group-one", "execution_continuation_required")
		if query.Get("cursor") == "second" {
			if len(api.acks) != 1 || api.acks[0].Outcome != string(Delivered) {
				t.Fatalf("first page not delivered before reading second: %+v", api.acks)
			}
			cursor = ""
			event = testEvent("second", "codex", "session-87654321", "", "group-two", "execution_continuation_required")
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"items": []domain.ExecutionEvent{event}, "next_cursor": cursor})
	})}
	if err := supervisor.Cycle(context.Background(), "product1", "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 2 || fmt.Sprint(limits) != "[100 50 25 25]" {
		t.Fatalf("wake requests=%d; requested page limits=%v", len(adapter.requests), limits)
	}
}

func TestIdleWakeActivePaneMarkers(t *testing.T) {
	config := testConfig(t, "http://localhost:8000")
	for _, marker := range append(append([]string{}, config.ActiveMarkers...), "Building artifacts") {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			pane := filepath.Join(dir, "pane")
			log := filepath.Join(dir, "log")
			t.Setenv("PANE", pane)
			t.Setenv("TMUX_LOG", log)
			if err := os.WriteFile(pane, []byte(strings.Repeat("old output\n", 20)+strings.ToUpper(marker)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			config := config
			config.ActiveMarkers = append(append([]string{}, config.ActiveMarkers...), "Building artifacts")
			config.TmuxPath = filepath.Join(dir, "tmux")
			writeFakeTmux(t, config.TmuxPath, `printf '%s\n' "$1" >> "$TMUX_LOG"
case "$1" in capture-pane) cat "$PANE";; esac`)
			result := (ExecAdapter{Config: config}).Wake(context.Background(), WakeRequest{Kind: "codex", TmuxTarget: "orch:0.1", Message: "test", IdleCheck: true})
			commands, err := os.ReadFile(log)
			if err != nil || result.Outcome != SkippedActive || string(commands) != "display\ncapture-pane\n" {
				t.Fatalf("result=%#v commands=%q error=%v", result, commands, err)
			}
		})
	}
	if paneActive([]byte("background task\n"+strings.Repeat("new output\n", 15)), config.ActiveMarkers) {
		t.Fatal("marker outside the last 15 lines blocked an idle wake")
	}
}

func TestIdleWakePastesAndSubmitsOnce(t *testing.T) {
	config := testConfig(t, "http://localhost:8000")
	config.SubmitFor = 0
	dir := t.TempDir()
	config.TmuxPath = filepath.Join(dir, "tmux")
	t.Setenv("PANE", filepath.Join(dir, "pane"))
	log := filepath.Join(dir, "log")
	t.Setenv("TMUX_LOG", log)
	writeFakeTmux(t, config.TmuxPath, `printf '%s\n' "$1" >> "$TMUX_LOG"
case "$1" in
capture-pane) if test -f "$PANE"; then cat "$PANE"; else printf 'idle\n'; fi;;
display-message) printf '1\n';;
set-buffer) test "$7 $8" = '-dpr -b';;
send-keys) test "$4" = Enter && printf 'esc to interrupt\n' > "$PANE";;
esac`)
	result := (ExecAdapter{Config: config}).Wake(context.Background(), WakeRequest{Kind: "codex", TmuxTarget: "orch:0.1", Message: "header\nmember", IdleCheck: true})
	commands, err := os.ReadFile(log)
	if err != nil || result.Outcome != Delivered || strings.Count(string(commands), "send-keys\n") != 1 || !strings.Contains(string(commands), "set-buffer\n") {
		t.Fatalf("result=%#v commands=%q error=%v", result, commands, err)
	}
}

func idleTestEvent(kind string, now time.Time) domain.ExecutionEvent {
	event := testEvent(kind, "codex", "session-12345678", "orch:0.1", "group-id", kind)
	event.WakeTarget.Source = "group_orchestrator"
	event.WakeTarget.GroupID = "group-id"
	event.CreatedAt = now
	event.GroupIdle = &domain.ExecutionGroupIdlePayload{GroupID: "group-id", GroupActivityAt: now.Add(-31 * time.Minute), MemberTotal: 25}
	progress := int64(125)
	for index := range domain.MaxExecutionIdlePreview {
		event.GroupIdle.Members = append(event.GroupIdle.Members, domain.ExecutionGroupIdleMember{IntegrationID: fmt.Sprintf("member-%d", index), FeatureID: "private-feature", State: "unassigned", NextAction: "claim", SecondsSinceLastProgress: &progress})
	}
	return event
}

func TestIdleWakeRateLimitsAndPrompt(t *testing.T) {
	now := time.Now().UTC()
	event := idleTestEvent(domain.EventGroupMemberStale, now)
	event.OrchestratorSkill = "team-orchestrator"
	event.GroupIdle.Members[0].State, event.GroupIdle.Members[0].NextAction = "untrusted\nstate", "untrusted\naction"
	active := &fakeAdapter{results: []WakeResult{{Outcome: SkippedActive}}}
	supervisor, api := runCycle(t, []domain.ExecutionEvent{event, idleTestEvent(domain.EventGroupWaitTimeout, now)}, active)
	if len(active.requests) != 1 || len(api.acks) != 2 || api.acks[0].Outcome != string(SkippedActive) || len(supervisor.wakes) != 0 {
		t.Fatalf("active wake requests=%#v acks=%#v", active.requests, api.acks)
	}
	prompt := active.requests[0].Message
	if !active.requests[0].IdleCheck || len(strings.Split(prompt, "\n")) != 12 || !strings.Contains(prompt, "25 unfinished members") || !strings.Contains(prompt, "member-1 unassigned progress 125s next claim") || !strings.Contains(prompt, "get_execution_group with group_id=group-id") || !strings.Contains(prompt, `Load get_agent_skill(name="team-orchestrator", product_id="product1")`) || strings.Contains(prompt, "UNTRUSTED") || strings.Contains(prompt, "untrusted") || strings.Contains(prompt, "private-feature") {
		t.Fatalf("invalid idle prompt: %q", prompt)
	}
	cycle := func(s *Supervisor) {
		t.Helper()
		if err := s.Cycle(context.Background(), "product1", "test-token"); err != nil {
			t.Fatal(err)
		}
	}
	supervisor.Now = func() time.Time { return now }
	cycle(supervisor)
	owners := idleTestEvent(domain.EventGroupOwnerBlocked, now)
	if reminder := groupIdlePrompt(owners, now); !strings.Contains(reminder, "Remind the owner") || !strings.Contains(reminder, "Do not resume work") {
		t.Fatalf("invalid owner reminder: %q", reminder)
	}
	api.mu.Lock()
	api.events = []domain.ExecutionEvent{event, owners}
	api.mu.Unlock()
	cycle(supervisor)
	if len(active.requests) != 3 || api.claims != 5 {
		t.Fatalf("owner reminder must be rate-limited separately: wakes=%d claims=%d", len(active.requests), api.claims)
	}
	restartedAdapter := &fakeAdapter{}
	restarted, err := New(supervisor.Config, restartedAdapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.loadState(); err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return now }
	restarted.Client.Transport = supervisor.Client.Transport
	now = now.Add(14 * time.Minute)
	cycle(restarted)
	if len(restartedAdapter.requests) != 0 || api.claims != 5 {
		t.Fatal("local rate limit consumed a delivery or woke the session after restart")
	}
	now = now.Add(time.Minute)
	cycle(restarted)
	if len(restartedAdapter.requests) != 1 || api.claims != 6 {
		t.Fatal("idle rate limit did not expire after 15 minutes")
	}
	now = now.Add(3*time.Hour + 45*time.Minute)
	cycle(restarted)
	if len(restartedAdapter.requests) != 3 || !strings.Contains(restartedAdapter.requests[2].Message, "Remind the owner") {
		t.Fatal("owner reminder rate limit did not expire after four hours")
	}
}
