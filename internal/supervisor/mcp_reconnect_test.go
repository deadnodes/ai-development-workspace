package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"releasecontrol/internal/domain"
)

func reconnectHarness(kind, target string) domain.ExecutionHarness {
	return domain.ExecutionHarness{Kind: kind, SessionRef: "session-12345678", TmuxTarget: target}
}

type reconnectBindAdapter struct {
	*fakeAdapter
	available bool
}

func (a *reconnectBindAdapter) bind(context.Context, string) tmuxBinding {
	if !a.available {
		return tmuxBinding{}
	}
	return tmuxBinding{pane: "%1", session: "$1"}
}

func TestMCPReconnectFingerprintBaselineChangeAndKindFiltering(t *testing.T) {
	fingerprint := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	changed := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	first := reconnectHarness("claude_code", "claude:0.1")
	second := reconnectHarness("claude_code", "claude:0.1")
	second.SessionRef = "another-session"
	targets := []domain.ExecutionHarness{first, second, reconnectHarness("codex", "codex:0.1")}
	adapter := &fakeAdapter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"mcp_tools_fingerprint": fingerprint})
		case "/api/products/product1/supervisor-orchestrators":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": targets})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := testConfig(t, server.URL)
	s, err := New(config, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 0 {
		t.Fatalf("first fingerprint caused sends: %+v", adapter.requests)
	}
	state, err := os.ReadFile(config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved persistedState
	if err := json.Unmarshal(state, &saved); err != nil || saved.MCPToolsFingerprint != fingerprint {
		t.Fatalf("baseline not persisted: state=%+v err=%v", saved, err)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 0 {
		t.Fatalf("unchanged fingerprint caused sends: %+v", adapter.requests)
	}
	fingerprint = changed
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 1 || adapter.requests[0].Message != "/mcp reconnect release-control" || !adapter.requests[0].IdleCheck || adapter.requests[0].Kind != "claude_code" {
		t.Fatalf("unexpected reconnect requests: %+v", adapter.requests)
	}
	if len(s.mcpReconnectPending) != 0 {
		t.Fatalf("successful target remained pending: %+v", s.mcpReconnectPending)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 1 {
		t.Fatalf("confirmed reconnect repeated on next cycle: %+v", adapter.requests)
	}
}

func TestMCPReconnectPendingSurvivesTmuxBindAvailabilityChange(t *testing.T) {
	fingerprint := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	current := []domain.ExecutionHarness{reconnectHarness("claude_code", "claude:0.1")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"mcp_tools_fingerprint": fingerprint})
		case "/api/products/product1/supervisor-orchestrators":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": current})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := &reconnectBindAdapter{
		fakeAdapter: &fakeAdapter{results: []WakeResult{{Outcome: SkippedActive, Detail: "active pane"}, {Outcome: Delivered, Detail: "ok"}}},
	}
	s, err := New(testConfig(t, server.URL), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if binding := adapter.bind(context.Background(), "claude:0.1"); binding.pane != "" {
		t.Fatalf("expected tmux bind failure, got %+v", binding)
	}
	targetsBefore, err := s.currentMCPReconnectTargets(context.Background(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, current := targetsBefore["target:claude:0.1"]; !current {
		t.Fatalf("target key missing while bind failed: %+v", targetsBefore)
	}
	fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(s.mcpReconnectPending) != 1 {
		t.Fatalf("failed reconnect delivery did not retain pending work: %+v", s.mcpReconnectPending)
	}
	pendingKey := ""
	for key := range s.mcpReconnectPending {
		pendingKey = key
	}
	if pendingKey != "target:claude:0.1" {
		t.Fatalf("unexpected pending key: %q", pendingKey)
	}
	// A later successful bind must not change the identity used to reconcile pending work.
	adapter.available = true
	if binding := adapter.bind(context.Background(), "claude:0.1"); binding.pane == "" {
		t.Fatal("expected later tmux bind success")
	}
	targets, err := s.currentMCPReconnectTargets(context.Background(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, current := targets[pendingKey]; !current {
		t.Fatalf("pending key %q disappeared after bind became available: targets=%+v", pendingKey, targets)
	}
	if len(targetsBefore) != 1 || len(targets) != 1 {
		t.Fatalf("target set changed with bind availability: before=%+v after=%+v", targetsBefore, targets)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 2 || len(s.mcpReconnectPending) != 0 {
		t.Fatalf("pending reconnect was lost after bind availability changed: requests=%+v pending=%+v", adapter.requests, s.mcpReconnectPending)
	}
}

func TestMCPReconnectRetriesBusyTargetUntilConfirmed(t *testing.T) {
	fingerprint := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"mcp_tools_fingerprint": fingerprint})
			return
		}
		if r.URL.Path == "/api/products/product1/supervisor-orchestrators" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []domain.ExecutionHarness{reconnectHarness("claude_code", "claude:0.1")}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	adapter := &fakeAdapter{results: []WakeResult{{Outcome: SkippedActive, Detail: "active pane"}, {Outcome: Delivered, Detail: "ok"}}}
	s, err := New(testConfig(t, server.URL), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 1 || len(s.mcpReconnectPending) != 1 {
		t.Fatalf("busy target not retained: requests=%+v pending=%+v", adapter.requests, s.mcpReconnectPending)
	}
	if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 2 || len(s.mcpReconnectPending) != 0 {
		t.Fatalf("target did not retry and clear: requests=%+v pending=%+v", adapter.requests, s.mcpReconnectPending)
	}
}

func TestMCPReconnectDropsTargetsThatAreNoLongerCurrent(t *testing.T) {
	for _, origin := range []string{"unregistered user orchestrator", "closed group orchestrator"} {
		t.Run(origin, func(t *testing.T) {
			fingerprint := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			current := []domain.ExecutionHarness{reconnectHarness("claude_code", "claude:0.1")}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					_ = json.NewEncoder(w).Encode(map[string]string{"mcp_tools_fingerprint": fingerprint})
				case "/api/products/product1/supervisor-orchestrators":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": current})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			adapter := &fakeAdapter{results: []WakeResult{{Outcome: SkippedActive, Detail: "active pane"}}}
			s, err := New(testConfig(t, server.URL), adapter)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
				t.Fatal(err)
			}
			fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
				t.Fatal(err)
			}
			if len(adapter.requests) != 1 || len(s.mcpReconnectPending) != 1 {
				t.Fatalf("expected pending reconnect before target removal: requests=%+v pending=%+v", adapter.requests, s.mcpReconnectPending)
			}
			current = nil
			if err := s.reconnectMCPTools(context.Background(), "test-token"); err != nil {
				t.Fatal(err)
			}
			if len(adapter.requests) != 1 || len(s.mcpReconnectPending) != 0 {
				t.Fatalf("stale target was sent or retained: requests=%+v pending=%+v", adapter.requests, s.mcpReconnectPending)
			}
		})
	}
}
