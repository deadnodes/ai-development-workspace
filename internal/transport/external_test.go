package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
		{"/api/features/f/graph", "get_feature_graph", "f"},
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
		{"get_feature_graph", "feature_id", "f", "get_feature_graph"},
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

func TestExistingArtifactHTTPAndMCPDelegateSameCommand(t *testing.T) {
	svc := &boundaryService{commands: make(chan domain.Command, 3), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "artifact-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct{ name, key, digest string }{
		{name: "omitted"},
		{name: "request key only", key: "reuse_ARTIFACT-42"},
		{name: "digest only", digest: "sha256:" + strings.Repeat("a", 64)},
		{name: "both exact bytes", key: " Reuse_Key-42 ", digest: " sha256:" + strings.Repeat("b", 64) + " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{"environment_id": "dev", "application_id": "app", "revision_id": "revision", "artifact_id": "artifact"}
			args := map[string]any{"actor": "agent/test", "integration_id": "integration", "environment_id": "dev", "application_id": "app", "revision_id": "revision", "artifact_id": "artifact"}
			if tc.key != "" {
				data["request_key"] = tc.key
				args["request_key"] = tc.key
			}
			if tc.digest != "" {
				data["expected_digest"] = tc.digest
				args["expected_digest"] = tc.digest
			}
			// Whitespace in the final case must remain exact at this transport boundary;
			// application validation retains authority to reject invalid semantic inputs.
			command := domain.Command{Action: "deploy_existing_artifact", Actor: "agent/test", IntegrationID: "integration", Data: data}
			raw, _ := json.Marshal(command)
			response, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
			if got := <-svc.commands; !reflect.DeepEqual(command, got) {
				t.Fatalf("HTTP differs: %+v", got)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: command})
			if err != nil || result.IsError {
				t.Fatalf("execute: %v %+v", err, result)
			}
			if got := <-svc.commands; !reflect.DeepEqual(command, got) {
				t.Fatalf("generic MCP differs: %+v", got)
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_existing_artifact", Arguments: args})
			if err != nil || result.IsError {
				t.Fatalf("named tool: %v %+v", err, result)
			}
			if got := <-svc.commands; !reflect.DeepEqual(command, got) {
				t.Fatalf("dedicated MCP differs: %+v", got)
			}
		})
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "deploy_existing_artifact" {
			found = true
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			required := map[string]bool{}
			for _, name := range schema.Required {
				required[name] = true
			}
			for _, name := range []string{"request_key", "expected_digest"} {
				if schema.Properties[name].Type != "string" || required[name] {
					t.Fatal("new field missing or required", name)
				}
			}
			for _, name := range []string{"integration_id", "environment_id", "application_id", "revision_id", "artifact_id"} {
				if !required[name] {
					t.Fatal("existing required field changed", name)
				}
			}
			if len(required) != 5 {
				t.Fatal("required inputs changed", schema.Required)
			}
		}
	}
	if !found {
		t.Fatal("dedicated tool missing from tools/list")
	}
	invalid, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy_existing_artifact", Arguments: map[string]any{"integration_id": "integration", "environment_id": "dev", "application_id": "app", "revision_id": "revision", "artifact_id": "artifact", "force": true}})
	if err == nil && (invalid == nil || !invalid.IsError) {
		t.Fatal("unknown input accepted")
	}
	if len(svc.commands) != 0 {
		t.Fatal("invalid tool input reached application")
	}
}

func TestAutomaticActorAttribution(t *testing.T) {
	svc := &boundaryService{commands: make(chan domain.Command, 10)}
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	for _, actor := range []string{"", "human/local", "agent/reviewer"} {
		body, _ := json.Marshal(map[string]any{"action": "create_product", "actor": actor, "data": map[string]any{"name": "Example"}})
		res, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		want := actor
		if want == "" {
			want = "agent"
		}
		if got := (<-svc.commands).Actor; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "actor-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"action": "create_product", "data": map[string]any{"name": "Example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatal(result)
	}
	if got := (<-svc.commands).Actor; got != "agent" {
		t.Fatal(got)
	}
}
