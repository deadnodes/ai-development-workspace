package transport

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"testing"
)

func TestWorkerHistoryStrictInput(t *testing.T) {
	for _, raw := range []string{`{"product_id":"p","product_id":"x"}`, `{"Product_ID":"p"}`, `{"product_id":null}`, `{"product_id":"p","limit":null}`, `{"product_id":"p","limit":1.5}`, `{"product_id":"p","job_id":""}`, `{"product_id":"p","logs":true}`, `{} []`} {
		if _, e := decodeWorkerHistory([]byte(raw)); e == nil {
			t.Fatal("accepted", raw)
		}
	}
}
func TestWorkerHistoryHTTPAndMCPProjection(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = store.Update(ctx, func(s *domain.State) error {
		s.Products = append(s.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
		s.WorkerJobs = append(s.WorkerJobs, domain.WorkerJob{Meta: domain.Meta{ID: "j", ProductID: "p"}, Status: "FAILED"})
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	service := application.New(store)
	server := httptest.NewServer(New(service, Options{}))
	defer server.Close()
	response, e := http.Get(server.URL + "/api/worker-history?product_id=p")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode, string(raw))
	}
	var hp domain.WorkerHistoryPage
	if e = json.Unmarshal(raw, &hp); e != nil {
		t.Fatal(e)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "history-test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_worker_history", Arguments: map[string]any{"product_id": "p"}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	mr, _ := json.Marshal(result.StructuredContent)
	var mp domain.WorkerHistoryPage
	if e = json.Unmarshal(mr, &mp); e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(hp)
	b, _ := json.Marshal(mp)
	if string(a) != string(b) {
		t.Fatalf("HTTP/MCP differ %s %s", a, b)
	}
	for _, suffix := range []string{"?product_id=p&product_id=x", "?product_id=p&unknown=x", "?product_id=p&limit=101"} {
		r, e := http.Get(server.URL + "/api/worker-history" + suffix)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode < 400 {
			t.Fatal("bad query accepted", suffix)
		}
	}
}
