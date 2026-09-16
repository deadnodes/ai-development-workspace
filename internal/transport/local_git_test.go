package transport_test

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"testing"
)

func TestLocalGitToolsExplicitlyPlanWithoutFilesystem(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	list, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"get_local_git_state", "plan_local_git_sync", "record_local_git_sync"} {
		if !names[name] {
			t.Fatal("missing", name)
		}
	}
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "plan_local_git_sync", Arguments: map[string]any{"product_id": "p", "checkout_id": "c", "actor": "agent", "mode": "FETCH", "expected_head": "123"}})
	if e == nil && (result == nil || !result.IsError) {
		t.Fatal("unconfigured filesystem accepted")
	}
}
