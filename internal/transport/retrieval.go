package transport

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"releasecontrol/internal/domain"
	"strconv"
)

type contextRetrievalService interface {
	SearchContext(context.Context, domain.ContextSearchRequest) (domain.ContextSearchResult, error)
	ContextExcerpt(context.Context, domain.ContextExcerptRequest) (domain.ContextExcerpt, error)
	GraphNeighbors(context.Context, domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error)
}

func retrievalInteger(r *http.Request, key string) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, nil
	}
	v, e := strconv.Atoi(raw)
	if e != nil {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return v, nil
}
func registerRetrieval(mux *http.ServeMux, server *mcp.Server, service Service) {
	reader, ok := service.(contextRetrievalService)
	if !ok {
		return
	}
	mcp.AddTool(server, &mcp.Tool{Name: "search_context", Description: "Find authored Feature/Integration/Memory/Repository text within one Product using indexed lexical/full-text search. Returns at most20 small source cards, never State/audit. Start here, follow graph_neighbors, then request a bounded get_context_excerpt by source ID. Matching text is evidence, not release acceptance."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ContextSearchRequest) (*mcp.CallToolResult, any, error) {
		v, e := reader.SearchContext(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_context_excerpt", Description: "Read one curated authored source text window by exact Product and ID. Unicode-character offset; max_chars1..8000(default2000). next_offset continues the same source. No raw provider documents, events, logs or credentials. Use search_context to find sources."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ContextExcerptRequest) (*mcp.CallToolResult, any, error) {
		v, e := reader.ContextExcerpt(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "graph_neighbors", Description: "Traverse recorded Product/Feature/Integration/Memory/Repository relations, including integration dependencies and repository references. Product-scoped, depth1..2, at most30 nodes. Edges are typed stored links, never inferred semantic claims. Returns a bounded discovery tree; truncated signals additional neighbors."}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.GraphNeighborsRequest) (*mcp.CallToolResult, any, error) {
		v, e := reader.GraphNeighbors(ctx, in)
		return nil, v, e
	})
	mux.HandleFunc("GET /api/context/search", func(w http.ResponseWriter, r *http.Request) {
		limit, e := retrievalInteger(r, "limit")
		if e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		v, e := reader.SearchContext(r.Context(), domain.ContextSearchRequest{ProductID: r.URL.Query().Get("product_id"), FeatureID: r.URL.Query().Get("feature_id"), Query: r.URL.Query().Get("query"), Limit: limit})
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/context/excerpt", func(w http.ResponseWriter, r *http.Request) {
		offset, e := retrievalInteger(r, "offset")
		if e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		max, e := retrievalInteger(r, "max_chars")
		if e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		v, e := reader.ContextExcerpt(r.Context(), domain.ContextExcerptRequest{ProductID: r.URL.Query().Get("product_id"), ID: r.URL.Query().Get("id"), Offset: offset, MaxChars: max})
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/context/graph", func(w http.ResponseWriter, r *http.Request) {
		depth, e := retrievalInteger(r, "depth")
		if e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		limit, e := retrievalInteger(r, "limit")
		if e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		v, e := reader.GraphNeighbors(r.Context(), domain.GraphNeighborsRequest{ProductID: r.URL.Query().Get("product_id"), ID: r.URL.Query().Get("id"), Depth: depth, Limit: limit})
		respond(w, v, e)
	})
}
