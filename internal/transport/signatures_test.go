package transport

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

type signatureStub struct {
	Service
	calls int
}

func (s *signatureStub) SignArtifact(_ context.Context, in domain.SignArtifactRequest) (domain.ArtifactSignature, error) {
	s.calls++
	return domain.ArtifactSignature{Request: in, Status: "PENDING"}, nil
}
func (s *signatureStub) ListSignatures(_ context.Context, in domain.ListSignaturesRequest) (domain.SignaturePage, error) {
	s.calls++
	return domain.SignaturePage{Items: []domain.ArtifactSignature{}}, nil
}
func TestSignatureHTTPStrictRoutes(t *testing.T) {
	s := &signatureStub{}
	mux := http.NewServeMux()
	registerSignatureRoutes(mux, s)
	for _, test := range []struct {
		method, path, body string
		want               int
	}{{"POST", "/api/signatures", `{"product_id":"p","profile_id":"app","image_id":"web","image_ref":"r/o/r@sha256:x","key_ids":["prod"],"reason":"explicit request","request_key":"req"}`, 200}, {"POST", "/api/signatures", `{"unknown":true}`, 400}, {"POST", "/api/signatures", `{} {}`, 400}, {"GET", "/api/signatures?product_id=p&limit=25", "", 200}, {"GET", "/api/signatures?product_id=p&unexpected=1", "", 400}, {"GET", "/api/signatures?product_id=p&product_id=x", "", 400}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
	}
	if s.calls != 2 {
		t.Fatal(s.calls)
	}
}
func TestSignatureMCPRegistration(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s := &signatureStub{}
	registerSignatureTools(server, s)
	ctx := context.Background()
	a, b := mcp.NewInMemoryTransports()
	session, e := server.Connect(ctx, a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, e := client.Connect(ctx, b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	tools, e := cs.ListTools(ctx, nil)
	if e != nil || len(tools.Tools) != 2 {
		t.Fatal(tools, e)
	}
	_, e = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_signatures", Arguments: json.RawMessage(`{"product_id":"p"}`)})
	if e != nil || s.calls != 1 {
		t.Fatal(e, s.calls)
	}
}
