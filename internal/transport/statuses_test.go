package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
)

func TestStatusMetadataHTTPMCPParity(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 1), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	response, err := http.Get(server.URL + "/api/statuses")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var actual map[string]any
	if err = json.NewDecoder(response.Body).Decode(&actual); err != nil {
		t.Fatal(err)
	}
	expectedBytes, _ := json.Marshal(transport.StatusSchema())
	var expected map[string]any
	json.Unmarshal(expectedBytes, &expected)
	if response.StatusCode != 200 || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("metadata mismatch: %#v", actual)
	}
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "status-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_status_schema", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("metadata tool: %+v %v", result, err)
	}
	b, _ := json.Marshal(result.StructuredContent)
	var viaMCP map[string]any
	json.Unmarshal(b, &viaMCP)
	if !reflect.DeepEqual(actual, viaMCP) {
		t.Fatalf("MCP differs: %#v", viaMCP)
	}
	for _, command := range []map[string]any{
		{"action": "update_feature", "actor": "agent/test", "feature_id": "f", "data": map[string]any{"status": "banana"}},
		{"action": "transition_integration", "actor": "agent/test", "integration_id": "i", "data": map[string]any{"status": "released"}},
		{"action": "update_integration", "actor": "agent/test", "integration_id": "i", "data": map[string]any{"status": "ready"}},
	} {
		result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: command})
		if err == nil && !result.IsError {
			t.Fatalf("invalid status reached tool: %#v", command)
		}
	}
	select {
	case c := <-service.commands:
		t.Fatalf("invalid status reached application: %+v", c)
	default:
	}
}
