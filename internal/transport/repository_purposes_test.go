package transport_test

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
	"strings"
	"testing"
)

func TestRepositoryPurposeAndPublicationMCPSchema(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 8), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "package-schema", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct {
		action string
		data   map[string]any
	}{
		{"create_repository", map[string]any{"name": "SDK", "url": "https://example.test/sdk", "role": "LIBRARY"}},
		{"classify_repository", map[string]any{"role": "MIXED"}},
		{"create_application", map[string]any{"name": "SDK", "repository_id": "repo", "kind": "LIBRARY"}},
		{"configure_publication", map[string]any{"application_id": "lib", "format": "npm", "registry_url": "https://registry.example", "package_name": "@example/sdk"}},
		{"record_package_artifact", map[string]any{"application_id": "lib", "publication_target_id": "target", "source_commit": strings.Repeat("a", 40), "version": "1.2.3", "checksum": "sha256:" + strings.Repeat("b", 64), "uri": "https://registry.example/sdk.tgz"}},
	} {
		command := map[string]any{"action": tc.action, "actor": "agent/test", "product_id": "product", "id": "record", "data": tc.data}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: command})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", tc.action, err, result)
		}
		actual := <-service.commands
		if actual.Action != tc.action {
			t.Fatalf("wrong command: %+v", actual)
		}
	}
	for _, tc := range []struct {
		action string
		data   map[string]any
	}{
		{"create_repository", map[string]any{"name": "repo", "url": "https://example.test", "role": "random"}},
		{"create_application", map[string]any{"name": "lib", "repository_id": "repo", "kind": "random"}},
		{"configure_publication", map[string]any{"application_id": "lib", "format": "random", "registry_url": "https://example.test", "package_name": "lib"}},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"action": tc.action, "actor": "agent/test", "product_id": "product", "data": tc.data}})
		if err == nil && !result.IsError {
			t.Fatalf("invalid enum accepted: %+v", tc)
		}
	}
	select {
	case c := <-service.commands:
		t.Fatalf("invalid enum reached application: %+v", c)
	default:
	}
}
