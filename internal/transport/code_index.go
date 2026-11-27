package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

type codeIndexService interface {
	UpsertCodeIndex(context.Context, domain.CodeIndexWrite) (domain.CodeIndexWriteResult, error)
	GetCodeMap(context.Context, domain.CodeMapRequest) (any, error)
	SearchCode(context.Context, domain.CodeSearchRequest) (domain.CodeSearchResult, error)
	GetCodeFileOutline(context.Context, domain.CodeOutlineRequest) (any, error)
	GetCodeIndexStatus(context.Context, domain.CodeStatusRequest) (domain.CodeStatusResult, error)
}

func registerCodeIndex(mux routeRegistrar, server *mcp.Server, service Service) {
	index, ok := service.(codeIndexService)
	if !ok {
		return
	}
	addCodeTool(server, &mcp.Tool{Name: "upsert_code_index", Description: "Store one locally generated, secret-filtered snapshot for an attached repository and branch. An unchanged digest is a no-op. Requires Product write access."}, index.UpsertCodeIndex)
	addCodeTool(server, &mcp.Tool{Name: "get_code_map", Description: "Read a bounded repository tree, manifest summaries, entry points and symbol counts. Omit branch for the last indexed snapshot. Index evidence is not live Git state."}, index.GetCodeMap)
	addCodeTool(server, &mcp.Tool{Name: "search_code", Description: "Search indexed paths, symbols, route paths, dependencies and documentation within a Product. Returns bounded commit-attributed cards, not raw source files."}, index.SearchCode)
	addCodeTool(server, &mcp.Tool{Name: "get_code_file_outline", Description: "Read indexed symbols, routes and a bounded documentation excerpt for an exact relative path in the latest snapshot."}, index.GetCodeFileOutline)
	addCodeTool(server, &mcp.Tool{Name: "get_code_index_status", Description: "Read scoped code index snapshots with branch, commit, digest, age and truncation flags. An unlisted repository has no index in this result; check truncation."}, index.GetCodeIndexStatus)
	mux.HandleFunc("POST /api/code/index", func(w http.ResponseWriter, r *http.Request) {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" {
			write(w, 415, map[string]string{"error": "Use Content-Type: application/json"})
			return
		}
		var input domain.CodeIndexWrite
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, domain.CodeIndexBytes+(4<<10)))
		dec.DisallowUnknownFields()
		err := dec.Decode(&input)
		if err == nil {
			err = dec.Decode(new(any))
		} else if errors.Is(err, io.EOF) {
			err = errors.New("missing index body")
		}
		if !errors.Is(err, io.EOF) {
			var oversized *http.MaxBytesError
			if errors.As(err, &oversized) {
				write(w, 413, map[string]string{"error": "code index request exceeds byte limit"})
			} else {
				write(w, 400, map[string]string{"error": "invalid code index JSON"})
			}
			return
		}
		result, err := index.UpsertCodeIndex(r.Context(), input)
		respond(w, result, err)
	})
	codeGet(mux, "GET /api/code/map", func(ctx context.Context, query url.Values, depth, limit int) (any, error) {
		return index.GetCodeMap(ctx, domain.CodeMapRequest{ProductID: query.Get("product_id"), RepositoryID: query.Get("repository_id"), Branch: query.Get("branch"), PathPrefix: query.Get("path_prefix"), Depth: depth, Limit: limit})
	})
	codeGet(mux, "GET /api/code/search", func(ctx context.Context, query url.Values, _, limit int) (any, error) {
		return index.SearchCode(ctx, domain.CodeSearchRequest{ProductID: query.Get("product_id"), RepositoryID: query.Get("repository_id"), Query: query.Get("query"), Kind: query.Get("kind"), Limit: limit})
	})
	codeGet(mux, "GET /api/code/outline", func(ctx context.Context, query url.Values, _, _ int) (any, error) {
		return index.GetCodeFileOutline(ctx, domain.CodeOutlineRequest{ProductID: query.Get("product_id"), RepositoryID: query.Get("repository_id"), Path: query.Get("path")})
	})
	codeGet(mux, "GET /api/code/status", func(ctx context.Context, query url.Values, _, _ int) (any, error) {
		return index.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: query.Get("product_id"), RepositoryID: query.Get("repository_id"), Branch: query.Get("branch")})
	})
}

func addCodeTool[Input, Output any](server *mcp.Server, tool *mcp.Tool, fn func(context.Context, Input) (Output, error)) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, any, error) {
		result, err := fn(ctx, input)
		return nil, result, err
	})
}

func codeGet(mux routeRegistrar, route string, query func(context.Context, url.Values, int, int) (any, error)) {
	mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
		depth, depthErr := retrievalInteger(r, "depth")
		limit, limitErr := retrievalInteger(r, "limit")
		if err := errors.Join(depthErr, limitErr); err != nil {
			write(w, 400, map[string]string{"error": err.Error()})
			return
		}
		result, err := query(r.Context(), r.URL.Query(), depth, limit)
		respond(w, result, err)
	})
}
