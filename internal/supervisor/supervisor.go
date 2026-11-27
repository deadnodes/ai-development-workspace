package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"releasecontrol/internal/agentconnect"
	"releasecontrol/internal/domain"
)

type Outcome string

const (
	Delivered     Outcome = "delivered"
	Failed        Outcome = "failed"
	Unsupported   Outcome = "unsupported"
	SkippedActive Outcome = "skipped_active"
)

type WakeResult struct {
	Outcome Outcome
	Detail  string
}
type WakeRequest struct {
	Kind, SessionRef, TmuxTarget, GroupID, Message string
	Events                                         []domain.ExecutionEvent
	IdleCheck                                      bool
	bound                                          bool
	tmux                                           tmuxBinding
}
type Adapter interface {
	Wake(context.Context, WakeRequest) WakeResult
}
type tmuxBinding struct{ pane, session string }

type sessionBatch struct {
	key     string
	request WakeRequest
	events  []domain.ExecutionEvent
}

type Supervisor struct {
	Config  Config
	Client  *http.Client
	Adapter Adapter
	Now     func() time.Time
	mu      sync.Mutex
	wakes   map[string]time.Time
}

func New(c Config, adapter Adapter) (*Supervisor, error) {
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	if c.StateFile == "" {
		return nil, errors.New("state_file is required")
	}
	if adapter == nil {
		adapter = ExecAdapter{Config: c}
	}
	return &Supervisor{Config: c, Client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects forbidden") }}, Adapter: adapter, Now: time.Now, wakes: map[string]time.Time{}}, nil
}

func credential(c Config) (string, error) {
	return agentconnect.ResolveCredential(c.RCPURL, c.Profile, c.TokenEnv)
}

func (s *Supervisor) Run(ctx context.Context) error {
	token, err := credential(s.Config)
	if err != nil {
		return err
	}
	if err = s.loadState(); err != nil {
		return err
	}
	ticker := time.NewTicker(s.Config.PollEvery)
	defer ticker.Stop()
	for {
		for _, product := range s.Config.ProductIDs {
			if err := s.Cycle(ctx, product, token); err != nil {
				slog.Error("supervisor poll failed", "product_id", product, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) Cycle(ctx context.Context, productID, token string) error {
	cursor := ""
	limit := 100
	seenCursors := map[string]bool{}
	activePanes := map[string]bool{}
	for {
		var result struct {
			Items      []domain.ExecutionEvent `json:"items"`
			NextCursor string                  `json:"next_cursor"`
		}
		query := fmt.Sprintf("?due_only=true&unresolved_only=true&limit=%d", limit)
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		path := "/api/products/" + url.PathEscape(productID) + "/execution-events" + query
		if err := s.request(ctx, http.MethodGet, path, token, nil, &result); err != nil {
			if errors.Is(err, errResponseTooLarge) && limit > 1 {
				limit = max(1, limit/2)
				continue
			}
			return err
		}
		if result.NextCursor != "" && seenCursors[result.NextCursor] {
			return errors.New("RCP cursor did not advance")
		}
		if err := s.dispatchPage(ctx, productID, token, result.Items, activePanes); err != nil {
			return err
		}
		if result.NextCursor == "" {
			return nil
		}
		cursor = result.NextCursor
		seenCursors[cursor] = true
	}
}

func (s *Supervisor) dispatchPage(ctx context.Context, productID, token string, events []domain.ExecutionEvent, activePanes map[string]bool) error {
	batches := map[string]*sessionBatch{}
	bindings := map[string]tmuxBinding{}
	resolver, hasResolver := s.Adapter.(interface {
		bind(context.Context, string) tmuxBinding
	})
	for _, ev := range events {
		wake := ev.WakeTarget
		request := WakeRequest{Kind: wake.Kind, SessionRef: wake.SessionRef, TmuxTarget: wake.TmuxTarget, IdleCheck: domain.IsGuardedExecutionWake(ev.Type)}
		sessionKey, paneKey := wake.Kind+"|"+wake.SessionRef, wake.TmuxTarget
		if hasResolver && wake.TmuxTarget != "" && s.allowed(wake.Kind) && validOrchestratorWake(ev) {
			binding, resolved := bindings[wake.TmuxTarget]
			if !resolved {
				binding = resolver.bind(ctx, wake.TmuxTarget)
				bindings[wake.TmuxTarget] = binding
			}
			request.bound, request.tmux = true, binding
			if binding.pane != "" {
				sessionKey, paneKey = "tmux:"+binding.session, binding.pane
			}
		}
		category := "0|"
		if ev.Type == domain.EventGroupOwnerBlocked {
			category = "2|owner-blocked|"
		} else if request.IdleCheck {
			category = "1|idle|"
		}
		key := category + sessionKey
		batchKey := key + "|" + paneKey
		if request.IdleCheck {
			batchKey += "|" + wake.GroupID
		}
		if !validOrchestratorWake(ev) {
			batchKey += "|unsupported"
		}
		b := batches[batchKey]
		if b == nil {
			b = &sessionBatch{key: key, request: request}
			batches[batchKey] = b
		}
		b.events = append(b.events, ev)
	}
	keys := make([]string, 0, len(batches))
	for k := range batches {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b := batches[key]
		if err := s.deliverBatch(ctx, productID, token, b, activePanes); err != nil {
			return err
		}
	}
	return nil
}

// deliverBatch claims every event before waking once, then records the same durable outcome for each claim.
func (s *Supervisor) deliverBatch(ctx context.Context, productID, token string, batch *sessionBatch, activePanes map[string]bool) error {
	request, key, events := batch.request, batch.key, batch.events
	if request.IdleCheck {
		limit := s.Config.IdleRateFor
		if events[0].Type == domain.EventGroupOwnerBlocked {
			limit = s.Config.OwnerRateFor
		}
		s.mu.Lock()
		last := s.wakes[key]
		s.mu.Unlock()
		if !last.IsZero() && s.Now().Sub(last) < limit {
			return nil
		}
	}
	claimed := make([]struct {
		ev domain.ExecutionEvent
		id string
	}, 0, len(events))
	for _, ev := range events {
		var out string
		cmd := command("claim_execution_event_delivery", productID, s.Config.SupervisorID, map[string]any{"event_id": ev.ID, "supervisor_id": s.Config.SupervisorID})
		if err := s.request(ctx, http.MethodPost, "/api/commands", token, cmd, &out); err != nil {
			continue
		}
		if out != "" {
			if !validOrchestratorWake(ev) {
				data := map[string]any{"event_id": ev.ID, "delivery_id": out, "supervisor_id": s.Config.SupervisorID, "outcome": string(Unsupported), "detail": "missing valid orchestrator target"}
				if err := s.request(ctx, http.MethodPost, "/api/commands", token, command("ack_execution_event", productID, s.Config.SupervisorID, data), nil); err != nil {
					return err
				}
				continue
			}
			claimed = append(claimed, struct {
				ev domain.ExecutionEvent
				id string
			}{ev, out})
		}
	}
	if len(claimed) == 0 {
		return nil
	}
	result := WakeResult{Outcome: Unsupported, Detail: "unsupported"}
	var stateErr error
	if !s.allowed(request.Kind) {
		result = WakeResult{Outcome: Unsupported, Detail: "harness kind not allowed"}
	} else if request.TmuxTarget == "" && request.SessionRef == "" {
		result = WakeResult{Outcome: Unsupported, Detail: "missing session_ref and tmux_target"}
	} else if request.IdleCheck {
		var idleEvent domain.ExecutionEvent
		for _, delivery := range claimed {
			if domain.IsGroupIdleEvent(delivery.ev.Type) && (idleEvent.ID == "" || delivery.ev.Type == domain.EventGroupMemberStale) {
				idleEvent = delivery.ev
			}
		}
		now := s.Now()
		if idleEvent.ID != "" {
			request.GroupID = idleEvent.WakeTarget.GroupID
			request.Message = groupIdlePrompt(idleEvent, now)
		} else {
			event := claimed[0].ev
			request.GroupID = wakeGroup(event)
			request.Message = "[RCP supervisor] Execution stalled. Call next_work and follow the fenced next action."
			if request.GroupID != "" {
				request.Message = "[RCP supervisor] Execution stalled. Call get_execution_group with group_id=" + request.GroupID + " and follow each member's next_action."
			}
			if reminder := orchestratorSkillReminder(event); reminder != "" {
				request.Message += " " + reminder
			}
		}
		if request.Message != "" && ValidTmuxTarget(request.TmuxTarget) {
			for _, delivery := range claimed {
				request.Events = append(request.Events, delivery.ev)
			}
			paneKey := request.tmux.pane
			if paneKey == "" {
				paneKey = request.TmuxTarget
			}
			if activePanes[paneKey] {
				result = WakeResult{Outcome: SkippedActive, Detail: "active pane"}
			} else {
				result = s.Adapter.Wake(ctx, request)
				if result.Outcome == SkippedActive {
					activePanes[paneKey] = true
				}
			}
			if result.Outcome == Delivered || result.Outcome == Failed {
				s.mu.Lock()
				s.wakes[key] = now
				s.mu.Unlock()
				stateErr = s.saveState()
			}
		} else {
			result.Detail = "idle wake requires valid group payload and tmux target"
		}
	} else {
		groupID := ""
		workAvailable, resolvedBlocker := false, false
		for _, delivery := range claimed {
			workAvailable = workAvailable || workAvailabilityWake(delivery.ev)
			resolvedBlocker = resolvedBlocker || blockerResolutionWake(delivery.ev)
			if groupID == "" {
				groupID = wakeGroup(delivery.ev)
			}
		}
		if !ValidSessionRef(request.SessionRef) && request.TmuxTarget == "" {
			result = WakeResult{Outcome: Unsupported, Detail: "invalid session_ref"}
		} else {
			types := make([]string, 0, len(claimed))
			for _, c := range claimed {
				types = append(types, safeEventType(c.ev.Type))
			}
			message := fmt.Sprintf("[RCP supervisor] Unfinished work in execution group %s (%d events: %s). Call get_execution_group with group_id=%s and continue every member. Work counts as done only when each Integration reaches its desired_state. For every member: follow its next_action from get_execution_group (resume with your fence if lost, claim if unassigned), transition with execution_fence, release_execution(outcome=completed) at the end.", groupID, len(claimed), strings.Join(types, ", "), groupID)
			if groupID == "" {
				message = "[RCP supervisor] Unfinished work. Call next_work and follow the fenced next action for the affected integration."
			}
			if resolvedBlocker && groupID == "" {
				for _, delivery := range claimed {
					if blockerResolutionWake(delivery.ev) {
						message += " A blocker was resolved. Call next_work as the current assignee and follow the fenced resume item for integration_id=" + delivery.ev.IntegrationID + "."
					}
				}
			}
			if workAvailable {
				message += " Call next_work and list_questions as your current principal. Follow the fenced next action on resume items, claim eligible unstarted integrations, and plan ready features with no integrations."
			}
			var answered []string
			for _, delivery := range claimed {
				for _, questionID := range delivery.ev.QuestionIDs {
					if idPattern.MatchString(questionID) && !slices.Contains(answered, questionID) && len(answered) < 10 {
						answered = append(answered, questionID)
					}
				}
			}
			if len(answered) > 0 {
				message += " Owner answered questions: " + strings.Join(answered, ", ") + ". Call get_question for each to read the answer, then continue the affected work."
			} else {
				for _, delivery := range claimed {
					if delivery.ev.Cause == domain.ExecutionCauseQuestionContinuation && idPattern.MatchString(delivery.ev.ID) {
						message += " Read question_ids on event " + delivery.ev.ID + " via list_execution_events in this product. Call get_question for each ID to read the triggering answer or withdrawal."
						break
					}
				}
			}
			if reminder := orchestratorSkillReminder(claimed[0].ev); reminder != "" {
				message += " " + reminder
			}
			s.mu.Lock()
			last := s.wakes[key]
			now := s.Now()
			limited := !last.IsZero() && now.Sub(last) < s.Config.RateFor
			s.mu.Unlock()
			if limited {
				result = WakeResult{Outcome: Failed, Detail: "rate limited"}
			} else {
				for _, c := range claimed {
					request.Events = append(request.Events, c.ev)
				}
				request.GroupID, request.Message = groupID, message
				result = s.Adapter.Wake(ctx, request)
				if result.Detail != "busy" {
					s.mu.Lock()
					s.wakes[key] = now
					s.mu.Unlock()
					if err := s.saveState(); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, c := range claimed {
		data := map[string]any{"event_id": c.ev.ID, "delivery_id": c.id, "supervisor_id": s.Config.SupervisorID, "outcome": string(result.Outcome), "detail": result.Detail}
		if err := s.request(ctx, http.MethodPost, "/api/commands", token, command("ack_execution_event", productID, s.Config.SupervisorID, data), nil); err != nil {
			return err
		}
	}
	return stateErr
}

var idleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func groupIdlePrompt(event domain.ExecutionEvent, now time.Time) string {
	payload := event.GroupIdle
	if payload == nil || event.WakeTarget.Source != "group_orchestrator" || payload.GroupID != event.WakeTarget.GroupID || !idleIDPattern.MatchString(payload.GroupID) || len(payload.Members) == 0 {
		return ""
	}
	minutes := max(0, now.Sub(payload.GroupActivityAt)/time.Minute)
	instruction := "Check each member's state and next_action, continue eligible work or report blockers."
	if event.Type == domain.EventGroupOwnerBlocked {
		instruction = "Remind the owner about the outstanding questions or blockers. Do not resume work."
	}
	var prompt strings.Builder
	if event.Type == domain.EventGroupMemberStale {
		fmt.Fprintf(&prompt, "[RCP supervisor] Group %s: %d unfinished members without recent recorded progress. Call get_execution_group with group_id=%s. %s", payload.GroupID, payload.MemberTotal, payload.GroupID, instruction)
	} else {
		fmt.Fprintf(&prompt, "[RCP supervisor] Group %s: %d unfinished members, no recorded activity for %d min. Call get_execution_group with group_id=%s. %s", payload.GroupID, payload.MemberTotal, minutes, payload.GroupID, instruction)
	}
	reminder := orchestratorSkillReminder(event)
	for _, member := range payload.Members[:min(len(payload.Members), domain.MaxExecutionIdlePreview)] {
		if !idleIDPattern.MatchString(member.IntegrationID) {
			continue
		}
		state, action := member.State, member.NextAction
		switch state {
		case "unassigned", "claimed", "running", "waiting_external", "blocked", "stalled", "lost", "exited_incomplete", "paused":
		default:
			state = "unknown"
		}
		switch action {
		case "none", "claim", "wait", "resume", "takeover", "advance", "continue":
		default:
			action = "unknown"
		}
		age := "unknown"
		if member.SecondsSinceLastProgress != nil && *member.SecondsSinceLastProgress >= 0 {
			age = fmt.Sprintf("%ds", *member.SecondsSinceLastProgress)
		}
		fmt.Fprintf(&prompt, "\n%s %s progress %s next %s", member.IntegrationID, state, age, action)
	}
	if reminder != "" {
		prompt.WriteByte('\n')
		prompt.WriteString(reminder)
	}
	return prompt.String()
}

func orchestratorSkillReminder(event domain.ExecutionEvent) string {
	if !domain.ValidAgentSkillName(event.OrchestratorSkill) || !domain.ValidAgentRoleProductID(event.ProductID) {
		return ""
	}
	reminder := fmt.Sprintf("Load get_agent_skill(name=%q, product_id=%q) if not loaded, check your executors and wake stuck ones.", event.OrchestratorSkill, event.ProductID)
	if event.Type == domain.EventGroupOwnerBlocked {
		reminder += " Do not wake owner-blocked work."
	}
	return reminder
}

func (s *Supervisor) allowed(k string) bool {
	for _, v := range s.Config.AllowedKinds {
		if v == k {
			return true
		}
	}
	return false
}
func blockerResolutionWake(event domain.ExecutionEvent) bool {
	return event.Type == "execution_continuation_required" && event.Cause == domain.ExecutionCauseBlockerResolved && idPattern.MatchString(event.IntegrationID)
}

func workAvailabilityWake(event domain.ExecutionEvent) bool {
	return event.Type == "work_available" && (event.Cause == domain.ExecutionCauseQueueAvailable || event.Cause == domain.ExecutionCauseQuestionContinuation)
}

func wakeGroup(event domain.ExecutionEvent) string {
	if event.WakeTarget.Source == "group_orchestrator" && idleIDPattern.MatchString(event.WakeTarget.GroupID) {
		return event.WakeTarget.GroupID
	}
	return ""
}

func validOrchestratorWake(event domain.ExecutionEvent) bool {
	return wakeGroup(event) != "" || event.WakeTarget.Source == "user_orchestrator" && !domain.IsGroupIdleEvent(event.Type)
}

func safeEventType(t string) string {
	switch t {
	case "execution_lost", "execution_stalled", "execution_unassigned", "execution_continuation_required", "work_available":
		return t
	}
	return "unknown"
}

func command(action, product, supervisor string, data map[string]any) domain.Command {
	return domain.Command{Action: action, Actor: "supervisor/" + supervisor, ProductID: product, Data: data}
}

var errResponseTooLarge = errors.New("RCP response exceeds byte limit")

func (s *Supervisor) request(ctx context.Context, method, path, token string, in, out any) error {
	u, err := url.Parse(s.Config.RCPURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()))) {
		return errors.New("invalid RCP URL")
	}
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return errors.New("cannot encode request")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.Config.RCPURL, "/")+path, body)
	if err != nil {
		return errors.New("cannot create RCP request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.Client.Do(req)
	if err != nil {
		return errors.New("RCP request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("RCP request rejected (%d)", res.StatusCode)
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, domain.MaxExecutionEventPageBytes+1))
	if err != nil {
		return errors.New("invalid RCP response")
	}
	if len(raw) > domain.MaxExecutionEventPageBytes {
		return errResponseTooLarge
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err = d.Decode(out); err != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid RCP response")
	}
	return nil
}

type persistedState struct {
	Wakes map[string]time.Time `json:"wakes"`
}

func (s *Supervisor) loadState() error {
	if s.Config.StateFile == "" {
		return errors.New("state_file required")
	}
	if err := os.MkdirAll(filepath.Dir(s.Config.StateFile), 0700); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.Config.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err = json.Unmarshal(raw, &state); err != nil {
		return errors.New("invalid supervisor state file")
	}
	if state.Wakes == nil {
		state.Wakes = map[string]time.Time{}
	}
	s.mu.Lock()
	s.wakes = state.Wakes
	s.mu.Unlock()
	return nil
}
func (s *Supervisor) saveState() error {
	s.mu.Lock()
	state := persistedState{Wakes: map[string]time.Time{}}
	for k, v := range s.wakes {
		state.Wakes[k] = v
	}
	s.mu.Unlock()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.Config.StateFile), 0700); err != nil {
		return err
	}
	tmp := s.Config.StateFile + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Config.StateFile)
}

var tmuxTargetPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func ValidTmuxTarget(v string) bool { return tmuxTargetPattern.MatchString(v) }
func paneHash(b []byte) string      { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
