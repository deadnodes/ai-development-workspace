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
	"sort"
	"strings"
	"sync"
	"time"

	"releasecontrol/internal/agentconnect"
	"releasecontrol/internal/domain"
)

type Outcome string

const (
	Delivered   Outcome = "delivered"
	Failed      Outcome = "failed"
	Unsupported Outcome = "unsupported"
)

type WakeResult struct {
	Outcome Outcome
	Detail  string
}
type WakeRequest struct {
	Kind, SessionRef, TmuxTarget, GroupID, Message string
	Events                                         []domain.ExecutionEvent
}
type Adapter interface {
	Wake(context.Context, WakeRequest) WakeResult
}
type sessionBatch struct {
	key, kind, session, target string
	events                     []domain.ExecutionEvent
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
	var events []domain.ExecutionEvent
	cursor := ""
	for {
		var result struct {
			Items      []domain.ExecutionEvent `json:"items"`
			NextCursor string                  `json:"next_cursor"`
		}
		query := "?due_only=true&unresolved_only=true&limit=100"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		path := "/api/products/" + url.PathEscape(productID) + "/execution-events" + query
		if err := s.request(ctx, http.MethodGet, path, token, nil, &result); err != nil {
			return err
		}
		events = append(events, result.Items...)
		if result.NextCursor == "" {
			break
		}
		if result.NextCursor == cursor {
			return errors.New("RCP cursor did not advance")
		}
		cursor = result.NextCursor
	}
	batches := map[string]*sessionBatch{}
	for _, ev := range events {
		sessionKey := ev.SessionRef
		if ev.TmuxTarget != "" {
			sessionKey = "tmux:" + ev.TmuxTarget
		}
		key := ev.HarnessKind + "|" + sessionKey
		b := batches[key]
		if b == nil {
			b = &sessionBatch{key: key, kind: ev.HarnessKind, session: ev.SessionRef, target: ev.TmuxTarget}
			batches[key] = b
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
		if err := s.deliverBatch(ctx, productID, token, b.kind, b.session, b.target, b.key, b.events); err != nil {
			return err
		}
	}
	return nil
}

// deliverBatch claims every event before waking once, then records the same durable outcome for each claim.
func (s *Supervisor) deliverBatch(ctx context.Context, productID, token string, kind, session, target, key string, events []domain.ExecutionEvent) error {
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
	if !s.allowed(kind) {
		result = WakeResult{Outcome: Unsupported, Detail: "harness kind not allowed"}
	} else if target == "" && session == "" {
		result = WakeResult{Outcome: Unsupported, Detail: "missing session_ref and tmux_target"}
	} else {
		groupID := ""
		for _, c := range claimed {
			for _, gid := range c.ev.GroupIDs {
				if idPattern.MatchString(gid) {
					groupID = gid
					break
				}
			}
			if groupID != "" {
				break
			}
		}
		if groupID == "" {
			result = WakeResult{Outcome: Unsupported, Detail: "missing valid execution group"}
		} else if !ValidSessionRef(session) && target == "" {
			result = WakeResult{Outcome: Unsupported, Detail: "invalid session_ref"}
		} else {
			types := make([]string, 0, len(claimed))
			for _, c := range claimed {
				types = append(types, safeEventType(c.ev.Type))
			}
			message := fmt.Sprintf("[RCP supervisor] Unfinished work in execution group %s (%d events: %s). Call get_execution_group with group_id=%s, continue every member that is not completed, keep heartbeats, report state; do not restart completed members.", groupID, len(claimed), strings.Join(types, ", "), groupID)
			s.mu.Lock()
			last := s.wakes[key]
			now := s.Now()
			limited := !last.IsZero() && now.Sub(last) < s.Config.RateFor
			s.mu.Unlock()
			if limited {
				result = WakeResult{Outcome: Failed, Detail: "rate limited"}
			} else {
				claimedEvents := make([]domain.ExecutionEvent, 0, len(claimed))
				for _, c := range claimed {
					claimedEvents = append(claimedEvents, c.ev)
				}
				result = s.Adapter.Wake(ctx, WakeRequest{Kind: kind, SessionRef: session, TmuxTarget: target, GroupID: groupID, Message: message, Events: claimedEvents})
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
	return nil
}

func (s *Supervisor) allowed(k string) bool {
	for _, v := range s.Config.AllowedKinds {
		if v == k {
			return true
		}
	}
	return false
}
func safeEventType(t string) string {
	switch t {
	case "execution_lost", "execution_stalled", "execution_unassigned", "execution_continuation_required":
		return t
	}
	return "unknown"
}

func command(action, product, supervisor string, data map[string]any) domain.Command {
	return domain.Command{Action: action, Actor: "supervisor/" + supervisor, ProductID: product, Data: data}
}

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
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("invalid RCP response")
	}
	if out != nil {
		d := json.NewDecoder(bytes.NewReader(raw))
		if err = d.Decode(out); err != nil {
			return errors.New("invalid RCP response")
		}
		if d.Decode(new(any)) != io.EOF {
			return errors.New("invalid RCP response")
		}
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
