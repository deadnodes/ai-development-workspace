package transport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
)

func TestExecutionGroupCreateThroughHTTPWithLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(New(application.New(store), Options{}))
	defer server.Close()
	client := server.Client()
	command := func(body string) {
		t.Helper()
		resp, err := client.Post(server.URL+"/api/commands", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command returned HTTP %d: %s", resp.StatusCode, body)
		}
	}
	command(`{"action":"create_product","actor":"agent","id":"p","data":{"name":"P"}}`)
	command(`{"action":"create_feature","actor":"agent","id":"f","product_id":"p","data":{"title":"F","goal":"G"}}`)
	for i := 1; i <= 4; i++ {
		command(fmt.Sprintf(`{"action":"create_integration","actor":"agent","id":"i%d","feature_id":"f","data":{"title":"I%d","objective":"O"}}`, i, i))
	}
	command(`{"action":"transition_integration","actor":"agent","integration_id":"i1","data":{"status":"working"}}`)
	command(`{"action":"transition_integration","actor":"agent","integration_id":"i1","data":{"status":"ready"}}`)
	command(`{"action":"create_execution_group","actor":"agent","product_id":"p","data":{"name":"G","orchestrator_id":"orch","integration_ids":["i1","i2","i3","i4"]}}`)
}

func TestExecuteFenceOverMCPTransport(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(New(application.New(store), Options{}))
	defer server.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fenced-execute-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(action string, args map[string]any) (*mcp.CallToolResult, error) {
		args["action"] = action
		if args["actor"] == nil {
			args["actor"] = "agent/test"
		}
		if args["data"] == nil {
			args["data"] = map[string]any{}
		}
		return session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: args})
	}
	for _, x := range []struct {
		action string
		args   map[string]any
	}{
		{"create_product", map[string]any{"id": "p", "data": map[string]any{"name": "P"}}},
		{"create_feature", map[string]any{"id": "f", "product_id": "p", "data": map[string]any{"title": "F", "problem": "P", "goal": "G"}}},
		{"create_integration", map[string]any{"id": "i", "feature_id": "f", "data": map[string]any{"title": "I", "objective": "O"}}},
	} {
		if result, err := call(x.action, x.args); err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", x.action, err, result)
		}
	}
	claimed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "claim_execution", Arguments: map[string]any{"actor": "agent/test", "product_id": "p", "integration_id": "i", "data": map[string]any{"orchestrator_id": "orch", "agent_id": "agent"}}})
	if err != nil || claimed.IsError {
		t.Fatalf("claim execution: %v %+v", err, claimed)
	}
	result, err := call("transition_integration", map[string]any{"integration_id": "i", "data": map[string]any{"status": "working", "execution_fence": 1}})
	if err != nil || result.IsError {
		t.Fatalf("fenced transition rejected over MCP: %v %s", err, result.Content[0].(*mcp.TextContent).Text)
	}
	result, err = call("record_progress", map[string]any{"feature_id": "f", "integration_id": "i", "data": map[string]any{"title": "stale", "execution_fence": 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "STALE_FENCE") {
		t.Fatalf("stale fence did not return STALE_FENCE: %s", result.Content[0].(*mcp.TextContent).Text)
	}
}
