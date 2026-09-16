package transport_test

import (
	"bytes"
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

// The deterministic boundary service proves transports delegate, without invoking providers.
type boundaryService struct {
	commands chan domain.Command
	queries  chan [2]string
}

func (s *boundaryService) State(context.Context) (domain.State, error) {
	return domain.EmptyState(), nil
}
func (s *boundaryService) Resume(context.Context, string) (any, error) {
	return map[string]string{"context": "feature"}, nil
}
func (s *boundaryService) Query(_ context.Context, name, id string) (any, error) {
	s.queries <- [2]string{name, id}
	return map[string]string{"query": name, "id": id, "status": "unknown"}, nil
}
func (s *boundaryService) Execute(_ context.Context, c domain.Command) (any, error) {
	s.commands <- c
	return map[string]string{"id": "operation", "status": "QUEUED"}, nil
}
func TestProviderSemanticTransportsDelegate(t *testing.T) {
	svc := &boundaryService{commands: make(chan domain.Command, 10), queries: make(chan [2]string, 20)}
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	for _, tc := range []struct{ path, name, id string }{
		{"/api/products/p/configuration", "get_product_configuration", "p"},
		{"/api/integrations/i/context", "get_integration_context", "i"}, {"/api/integrations/i/git", "get_integration_git", "i"}, {"/api/environments/dev/state", "get_environment_state", "dev"}, {"/api/operations/op", "get_operation", "op"}, {"/api/attention?product_id=p", "list_attention", "p"}, {"/api/connections/c/repositories", "discover_repositories", "c"}, {"/api/repositories/r/branches", "list_branches", "r"},
	} {
		response, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d", tc.path, response.StatusCode)
		}
		if got := <-svc.queries; got != [2]string{tc.name, tc.id} {
			t.Fatalf("query %v", got)
		}
	}
	payload := []byte(`{"actor":"human/test","environment_id":"dev","application_id":"app","revision_id":"rev","expected_digest":"sha256:abc"}`)
	response, err := http.Post(server.URL+"/api/integrations/i/deploy", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	viaHTTP := <-svc.commands
	if viaHTTP.Action != "deploy_integration" || viaHTTP.IntegrationID != "i" || viaHTTP.Actor != "human/test" || viaHTTP.Data["revision_id"] != "rev" {
		t.Fatalf("wrong command %+v", viaHTTP)
	}
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "semantic-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct{ tool, field, id, query string }{
		{"get_product_configuration", "product_id", "p", "get_product_configuration"},
		{"get_integration_context", "integration_id", "i", "get_integration_context"}, {"get_git_state", "integration_id", "i", "get_integration_git"}, {"get_environment_state", "environment_id", "dev", "get_environment_state"}, {"get_operation", "operation_id", "op", "get_operation"}, {"get_attention_required", "product_id", "p", "list_attention"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: map[string]any{tc.field: tc.id}})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %+v", tc.tool, result)
		}
		if got := <-svc.queries; got != [2]string{tc.query, tc.id} {
			t.Fatalf("query %v", got)
		}
	}
	var args map[string]any
	if err = json.Unmarshal(payload, &args); err != nil {
		t.Fatal(err)
	}
	args["integration_id"] = "i"
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_integration", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("deploy tool: %+v", result)
	}
	if viaMCP := <-svc.commands; !reflect.DeepEqual(viaHTTP, viaMCP) {
		t.Fatalf("different domain commands: HTTP=%+v MCP=%+v", viaHTTP, viaMCP)
	}

	minimal, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_integration", Arguments: map[string]any{"actor": "agent", "integration_id": "i", "environment_id": "dev"}})
	if err != nil || minimal.IsError {
		t.Fatalf("minimal deployment failed: %v %+v", err, minimal)
	}
	minimalCommand := <-svc.commands
	if !reflect.DeepEqual(minimalCommand.Data, map[string]any{"environment_id": "dev"}) {
		t.Fatalf("optional inputs not omitted: %+v", minimalCommand)
	}
}
func TestDeployEndpointRejectsUnknownInput(t *testing.T) {
	svc := &boundaryService{commands: make(chan domain.Command, 1), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	response, err := http.Post(server.URL+"/api/integrations/i/deploy", "application/json", bytes.NewBufferString(`{"actor":"x","force":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal(response.StatusCode)
	}
	if len(svc.commands) != 0 {
		t.Fatal("invalid deployment reached application")
	}
}
