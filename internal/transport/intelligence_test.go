package transport_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligence"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

type transportModelFixture struct {
	*intelligence.Authority
	calls int
}

func (f *transportModelFixture) Complete(_ context.Context, _ string, _ any) (json.RawMessage, intelligence.Usage, error) {
	f.calls++
	return json.RawMessage(`{"decision":"draft","summary":"Useful customer wording","changes":[],"warnings":[],"patch":null}`), intelligence.Usage{}, nil
}

func TestIntelligenceActualHTTPMCPAndLocalRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "intelligence.db")
	store, e := persistence.OpenLocal(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	a, e := intelligence.New(intelligence.Config{BaseURL: "https://fixture.example/v1", Model: "fixture-model", APIKeyEnv: "MODEL_KEY", EncryptionKeyEnv: "ENCRYPTION_KEY", ProductIDs: []string{"p"}, MaxInputBytes: 32768, MaxOutputBytes: 32768, MaxCompletionTokens: 512, MaxRequestsPerDay: 10, TimeoutSeconds: 2}, func(k string) string {
		if k == "MODEL_KEY" {
			return strings.Repeat("x", 32)
		}
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	})
	if e != nil {
		t.Fatal(e)
	}
	fake := &transportModelFixture{Authority: a}
	service := application.New(store)
	service.ConfigureIntelligence(fake)
	for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "Product"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "Feature", "goal": "Ship useful behavior"}}} {
		c.Actor = "operator"
		if _, e = service.Execute(ctx, c); e != nil {
			t.Fatal(e)
		}
	}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	post := func(c domain.Command) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(c)
		r, e := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		return r.StatusCode, raw
	}
	c := domain.Command{Action: "queue_intelligence_task", Actor: "agent", ProductID: "p", FeatureID: "f", Data: map[string]any{"task_kind": "summary", "request_key": "http-model-request-01", "excerpts": []string{"Confidential selected log excerpt"}}}
	status, raw := post(c)
	if status != 200 || bytes.Contains(raw, []byte("Confidential selected")) {
		t.Fatalf("HTTP queue failed or leaked input: %d %s", status, raw)
	}
	var op domain.ExternalOperation
	if json.Unmarshal(raw, &op) != nil || op.Intelligence == nil {
		t.Fatal("missing typed durable operation")
	}
	session, e := mcp.NewClient(&mcp.Implementation{Name: "intelligence-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	replayed, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: c})
	if e != nil || replayed.IsError {
		body, _ := json.Marshal(replayed)
		t.Fatalf("MCP parity/idempotency failed: %v %s", e, body)
	}
	_ = session.Close()
	server.Close()
	store.Close()
	// Actual persisted queue survives closing and reopening the local database.
	store, e = persistence.OpenLocal(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	service = application.New(store)
	service.ConfigureIntelligence(fake)
	if ran, e := service.Tick(ctx); e != nil || !ran || fake.calls != 1 {
		t.Fatalf("persisted direct model task failed: ran%v error%v calls%d", ran, e, fake.calls)
	}
	server = httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	r, e := http.Get(server.URL + "/api/intelligence-tasks/" + op.ID)
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(body, []byte("Useful customer wording")) || bytes.Contains(body, []byte("Confidential selected")) {
		t.Fatalf("scoped draft read failed: %d %s", r.StatusCode, body)
	}
	session, e = mcp.NewClient(&mcp.Implementation{Name: "intelligence-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_intelligence_task", Arguments: map[string]any{"task_id": op.ID}})
	if e != nil || result.IsError {
		t.Fatalf("MCP draft query failed: %v %+v", e, result)
	}
	r, e = http.Get(server.URL + "/api/state")
	if e != nil {
		t.Fatal(e)
	}
	body, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if bytes.Contains(body, []byte("Useful customer wording")) || bytes.Contains(body, []byte("Confidential selected")) {
		t.Fatal("raw payload leaked in unscoped state export")
	}
	// Actual MCP schema accepts the same typed request and rejects unexpected input.
	c.Data["request_key"] = "mcp-model-request-02"
	reply, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: c})
	if e != nil || reply.IsError {
		t.Fatal("MCP queue schema rejected supported task", e)
	}
	c.Data["unknown_payload"] = "unapproved"
	reply, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: c})
	if e == nil && !reply.IsError {
		t.Fatal("MCP accepted unknown command field")
	}
}
