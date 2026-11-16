package transport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"releasecontrol/internal/domain"
	"strings"
)

type signatureService interface {
	SignArtifact(context.Context, domain.SignArtifactRequest) (domain.ArtifactSignature, error)
	ListSignatures(context.Context, domain.ListSignaturesRequest) (domain.SignaturePage, error)
}

func registerSignatureRoutes(mux *http.ServeMux, service Service) {
	mux.HandleFunc("POST /api/signatures", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(signatureService)
		if !ok {
			write(w, 501, map[string]string{"error": "signing unavailable"})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "application/json required"})
			return
		}
		var in domain.SignArtifactRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid signing request"})
			return
		}
		v, e := s.SignArtifact(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/signatures", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(signatureService)
		if !ok {
			write(w, 501, map[string]string{"error": "signing unavailable"})
			return
		}
		values, e := strconvQueryHistory(r)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid signing query"})
			return
		}
		b, _ := json.Marshal(values)
		var in domain.ListSignaturesRequest
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil {
			write(w, 400, map[string]string{"error": "invalid signing query"})
			return
		}
		v, e := s.ListSignatures(r.Context(), in)
		respond(w, v, e)
	})
}
func registerSignatureTools(server *mcp.Server, service Service) {
	mcp.AddTool(server, &mcp.Tool{Name: "sign_artifact", Description: "Explicitly request digest-only signing/backfill in a configured Product/profile/image scope. Requires Product write permission, allowed repository/key and an existing digest. Durable request_key replay; inspect list_signatures for asynchronous results. Does not authorize deployment or production promotion."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.SignArtifactRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(signatureService)
		if !ok {
			return nil, nil, errors.New("signing unavailable")
		}
		v, e := s.SignArtifact(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_signatures", Description: "List Product-scoped explicit signing handles and per-key signature evidence. Optional exact digest filter; default 25/max100 with a scope-bound cursor. Build signing evidence is also available through get_build."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ListSignaturesRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(signatureService)
		if !ok {
			return nil, nil, errors.New("signing unavailable")
		}
		v, e := s.ListSignatures(ctx, in)
		return nil, v, e
	})
}
