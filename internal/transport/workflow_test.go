package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// Each run owns a schema; the configured database's application records are untouched.
func isolatedDatabase(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL HTTP/MCP workflow")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("workflow_%d", time.Now().UnixNano())
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestPostgresHTTPMCPWorkflow(t *testing.T) {
	db := isolatedDatabase(t)
	ctx := context.Background()
	store, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
	defer server.Close()
	command := func(action, id string, data map[string]any, refs ...string) domain.Command {
		c := domain.Command{Action: action, Actor: "agent/test", ID: id, Data: data}
		for i := 0; i < len(refs); i += 2 {
			switch refs[i] {
			case "product":
				c.ProductID = refs[i+1]
			case "feature":
				c.FeatureID = refs[i+1]
			case "integration":
				c.IntegrationID = refs[i+1]
			case "gate":
				c.GateID = refs[i+1]
			case "check":
				c.CheckID = refs[i+1]
			case "result":
				c.ResultID = refs[i+1]
			}
		}
		return c
	}
	post := func(c domain.Command, want int) map[string]any {
		t.Helper()
		b, _ := json.Marshal(c)
		res, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s: status %d want %d: %s", c.Action, res.StatusCode, want, body)
		}
		var out map[string]any
		if err = json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	post(command("create_product", "product", map[string]any{"name": "Payments"}), 200)
	post(command("create_feature", "feature", map[string]any{"title": "Payments v2", "problem": "Unreliable callbacks", "goal": "Reliable payment flow"}, "product", "product"), 200)
	post(command("create_integration", "integration", map[string]any{"title": "Provider callback", "objective": "Handle cancellation", "acceptance_criteria": []string{"Missing transaction is safe"}, "owner": "agent/backend", "working_areas": []string{"services/payments/**"}}, "feature", "feature"), 200)
	post(command("start_integration", "", map[string]any{}, "integration", "integration"), 200)
	// JSON case folding must not bypass the explicit lifecycle command.
	post(command("update_integration", "", map[string]any{"Status": "released"}, "integration", "integration"), 422)
	post(command("record_progress", "progress", map[string]any{"title": "Client complete", "completed": []string{"Provider client"}, "remaining": []string{"Cancelled callback"}}, "feature", "feature", "integration", "integration"), 200)
	post(command("record_decision", "decision", map[string]any{"title": "Keep v1", "body": "Keep the v1 callback", "reason": "Existing clients cannot migrate atomically"}, "feature", "feature"), 200)
	post(command("record_discovery", "discovery", map[string]any{"title": "Missing ID", "body": "Cancellation may omit transaction_id"}, "feature", "feature", "integration", "integration"), 200)
	post(command("create_gate", "gate", map[string]any{"title": "User flow", "reason": "First complete flow", "integration_ids": []string{"integration"}, "blocking": true}, "feature", "feature"), 200)
	post(command("add_check", "check", map[string]any{"title": "Cancellation", "mechanism": "browser", "instructions": "Cancel a payment"}, "gate", "gate"), 200)
	post(command("record_check_result", "failed", map[string]any{"result": "failed", "commit": "abc123", "steps": []string{"Cancel payment"}, "observations": "Duplicate payment", "artifacts": []map[string]string{{"kind": "trace", "url": "https://example.com/trace", "label": "Trace"}}}, "check", "check"), 200)
	post(command("record_check_result", "failed", map[string]any{"result": "passed", "commit": "abc123"}, "check", "check"), 422)
	post(command("complete_integration", "", map[string]any{}, "integration", "integration"), 422)
	post(command("record_finding", "finding", map[string]any{"title": "Duplicate payment", "severity": "major", "integration_ids": []string{"integration"}, "gate_ids": []string{"gate"}}, "result", "failed"), 200)
	post(command("resolve_finding", "finding", map[string]any{"body": "Made callback idempotent", "commit": "def456"}), 200)
	post(command("complete_integration", "", map[string]any{}, "integration", "integration"), 422)
	post(command("record_check_result", "passed", map[string]any{"result": "passed", "commit": "def456", "observations": "Exactly one payment"}, "check", "check"), 200)
	post(command("complete_integration", "", map[string]any{}, "integration", "integration"), 200)

	client := mcp.NewClient(&mcp.Implementation{Name: "workflow-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"get_state", "resume", "execute"} {
		if !names[name] {
			t.Errorf("MCP missing %s", name)
		}
	}
	handoff := command("handoff", "handoff", map[string]any{"title": "Resume here", "completed": []string{"Provider client", "Cancellation"}, "current": "Ready for release planning", "next": []string{"Plan release"}, "warnings": []string{"Do not remove v1 callback"}}, "feature", "feature", "integration", "integration")
	called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: handoff})
	if err != nil {
		t.Fatal(err)
	}
	if called.IsError {
		t.Fatalf("MCP execute error: %+v", called.Content)
	}
	resumed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "resume", Arguments: map[string]any{"feature_id": "feature"}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.IsError {
		t.Fatalf("MCP resume error: %+v", resumed.Content)
	}
	response, err := http.Get(server.URL + "/api/features/feature/context")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var httpContext any
	if err = json.NewDecoder(response.Body).Decode(&httpContext); err != nil {
		t.Fatal(err)
	}
	mcpBytes, err := json.Marshal(resumed.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var mcpContext any
	if err = json.Unmarshal(mcpBytes, &mcpContext); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(httpContext, mcpContext) {
		t.Fatalf("HTTP and MCP context differ: HTTP=%v MCP=%v", httpContext, mcpContext)
	}
	if !strings.Contains(string(mcpBytes), "Do not remove v1 callback") {
		t.Fatal("resume omitted structured handoff warning")
	}

	// Concurrent independent actors use distinct store connections, exercising the DB lock.
	second, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	services := []*application.Service{application.New(store), application.New(second)}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := services[i%2].Execute(ctx, command("record_discovery", fmt.Sprintf("parallel-%d", i), map[string]any{"title": fmt.Sprintf("Observation %d", i), "body": "Concurrent durable note"}, "feature", "feature"))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := services[0].State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := application.New(reopened).State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("state changed after reopening database")
	}
	if len(after.Memories) != 16 {
		t.Fatalf("concurrent notes lost: got %d memories", len(after.Memories))
	}
	if len(after.Results) != 2 || after.Results[0].Result != "failed" && after.Results[1].Result != "failed" {
		t.Fatal("check rerun lost failed evidence")
	}
	if len(after.Integrations) != 1 || after.Integrations[0].Status != "ready" {
		t.Fatal("integration not ready")
	}
	featureCreated := false
	for _, event := range after.Events {
		if event.Action == "create_feature" && event.FeatureID == "feature" {
			featureCreated = true
		}
	}
	if !featureCreated {
		t.Fatal("feature creation missing feature-scoped audit provenance")
	}
	if len(after.Events) != 27 {
		t.Fatalf("expected exactly one event per successful command (failed commands add none), got %d", len(after.Events))
	}
}

func TestHTTPBoundary(t *testing.T) {
	handler := transport.New(nil, transport.Options{Token: "test-secret"})
	cases := []struct {
		name, path, body, origin, host, token, contentType string
		want                                               int
	}{
		{name: "untrusted host", path: "/healthz", host: "attacker.example", want: 403},
		{name: "cross origin", path: "/healthz", host: "localhost", origin: "https://attacker.example", want: 403},
		{name: "missing token", path: "/api/state", host: "localhost", want: 401},
		{name: "wrong token", path: "/api/state", host: "localhost", token: "Bearer wrong", want: 401},
		{name: "malformed", path: "/api/commands", host: "localhost", token: "Bearer test-secret", body: "{", contentType: "application/json", want: 400},
		{name: "unknown envelope", path: "/api/commands", host: "localhost", token: "Bearer test-secret", body: `{"unknown":true}`, contentType: "application/json", want: 400},
		{name: "multiple objects", path: "/api/commands", host: "localhost", token: "Bearer test-secret", body: `{} {}`, contentType: "application/json", want: 400},
		{name: "content type", path: "/api/commands", host: "localhost", token: "Bearer test-secret", body: `{}`, contentType: "text/plain", want: 415},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := "GET"
			if tc.body != "" {
				method = "POST"
			}
			req := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
			req.Host = tc.host
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Authorization", tc.token)
			req.Header.Set("Content-Type", tc.contentType)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("got %d want %d: %s", res.Code, tc.want, res.Body.String())
			}
		})
	}
}
