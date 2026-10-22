package transport_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDevActivationMCPRejectsAmbiguousScopeAndAuthority(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 8), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "activation-boundary", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, raw := range []string{
		`{"product_id":"p","product_id":"q","reservation_operation_id":"r","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1}}`,
		`{"product_id":"p","reservation_operation_id":"r","reservation_operation_id":"other","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1}}`,
		`{"product_id":"p","reservation_operation_id":"r","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1,"generation":2}}`,
		`{"product_id":"p","Reservation_operation_id":"r","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1}}`,
		`{"product_id":"p","reservation_operation_id":"r","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1},"artifact_id":"injected"}`,
		`{"product_id":"p","reservation_operation_id":"r","request_key":"activation-request-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1},"approve":true}`,
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "deploy_candidate_dev", Arguments: json.RawMessage(raw)})
		if err == nil && (result == nil || !result.IsError) {
			t.Fatal("ambiguous scope/authority reached command layer")
		}
	}
	if len(service.commands) != 0 {
		t.Fatal("rejected MCP input reached application")
	}
}
