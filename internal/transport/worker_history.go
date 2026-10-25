package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/url"
	"releasecontrol/internal/domain"
	"strconv"
	"unicode/utf8"
)

type workerHistoryService interface {
	WorkerHistory(context.Context, domain.WorkerHistoryRequest) (domain.WorkerHistoryPage, error)
}

// Exact keys and duplicates matter because they select authorization scopes.
func decodeWorkerHistory(raw []byte) (domain.WorkerHistoryRequest, error) {
	var out domain.WorkerHistoryRequest
	bad := errors.New("invalid worker history arguments")
	if len(raw) == 0 || len(raw) > 8192 || !utf8.Valid(raw) {
		return out, bad
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return out, bad
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return out, bad
		}
		k, ok := t.(string)
		if !ok || seen[k] {
			return out, bad
		}
		seen[k] = true
		switch k {
		case "product_id":
			e = d.Decode(&out.ProductID)
			if out.ProductID == "" {
				return out, bad
			}
		case "feature_id":
			e = d.Decode(&out.FeatureID)
			if out.FeatureID == "" {
				return out, bad
			}
		case "job_id":
			e = d.Decode(&out.JobID)
			if out.JobID == "" {
				return out, bad
			}
		case "collection":
			e = d.Decode(&out.Collection)
			if out.Collection == "" {
				return out, bad
			}
		case "cursor":
			e = d.Decode(&out.Cursor)
			if out.Cursor == "" {
				return out, bad
			}
		case "limit":
			e = d.Decode(&out.Limit)
			if out.Limit < 1 || out.Limit > 100 {
				return out, bad
			}
		default:
			return out, bad
		}
		if e != nil {
			return out, bad
		}
	}
	t, e = d.Token()
	if e != nil || t != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return out, bad
	}
	return out, nil
}
func registerWorkerHistoryRoutes(mux *http.ServeMux, service Service) {
	mux.HandleFunc("GET /api/worker-history", func(w http.ResponseWriter, r *http.Request) {
		values, e := strconvQueryHistory(r)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid worker history arguments"})
			return
		}
		raw, _ := json.Marshal(values)
		in, e := decodeWorkerHistory(raw)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid worker history arguments"})
			return
		}
		reader, ok := service.(workerHistoryService)
		if !ok {
			write(w, 501, map[string]string{"error": "bounded worker history reader unavailable"})
			return
		}
		out, e := reader.WorkerHistory(r.Context(), in)
		respond(w, out, e)
	})
}
func strconvQueryHistory(r *http.Request) (map[string]any, error) {
	values := map[string]any{}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return nil, e
	}
	for k, v := range q {
		if len(v) != 1 {
			return nil, errors.New("duplicate worker history argument")
		}
		if k == "limit" {
			n, e := strconv.Atoi(v[0])
			if e != nil {
				return nil, e
			}
			values[k] = n
		} else {
			values[k] = v[0]
		}
	}
	return values, nil
}
func registerWorkerHistoryTool(server *mcp.Server, service Service) {
	props := map[string]any{}
	for _, key := range []string{"product_id", "feature_id", "job_id"} {
		props[key] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	}
	props["collection"] = map[string]any{"type": "string", "enum": []string{"jobs", "attempts", "links"}}
	props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	props["cursor"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 2048}
	server.AddTool(&mcp.Tool{Name: "get_worker_history", Description: "Bounded native worker recorded history. Requires Product; optional Feature filters frozen revision membership. Page jobs, then attempts and complete links with job_id. Default25/max100. Recorded status and unverified snapshot provenance are not runtime/success proof. Does not include GitHub Actions history or raw reports/logs.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"product_id"}, "properties": props}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fail := func() (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Worker history read failed: verify exact scope, collection and cursor; bounded reader required."}}}, nil
		}
		in, e := decodeWorkerHistory(req.Params.Arguments)
		if e != nil {
			return fail()
		}
		reader, ok := service.(workerHistoryService)
		if !ok {
			return fail()
		}
		out, e := reader.WorkerHistory(ctx, in)
		if e != nil {
			return fail()
		}
		raw, e := json.Marshal(out)
		if e != nil || len(raw) > 32<<10 {
			return fail()
		}
		result := &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
		wire, e := json.Marshal(result)
		if e != nil || len(wire) > 128<<10 {
			return fail()
		}
		return result, nil
	})
}
