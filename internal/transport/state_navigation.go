package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"unicode/utf8"
)

type statePageService interface {
	StatePage(context.Context, domain.StatePageRequest) (domain.StatePage, error)
}

func registerStateNavigationTool(server *mcp.Server, service Service) {
	server.AddTool(&mcp.Tool{Name: "get_state", Description: "Compact bounded navigation only: {} lists Products; product_id lists Features; feature_id lists Integrations with parent identities. Optional limit (1..100, default25) and scope-bound cursor. No raw state, history, credentials or readiness proof. Use scoped context/query tools for detail; operator backup for exports.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"product_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "feature_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 2048}}}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fail := func(message string) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}, nil
		}
		in, e := decodeStatePageArguments(req.Params.Arguments)
		if e != nil {
			return fail("Invalid get_state arguments: use exact product_id/feature_id, limit 1..100 and a cursor from the same scope")
		}
		reader, ok := service.(statePageService)
		if !ok {
			return fail("get_state requires a bounded metadata reader; update the service implementation")
		}
		out, e := reader.StatePage(ctx, in)
		if e != nil {
			if errors.Is(e, application.ErrValidation) {
				return fail(e.Error())
			}
			return fail("get_state metadata read failed; ask the owning operator to inspect the store")
		}
		raw, e := json.Marshal(out)
		if e != nil || len(raw) > 32<<10 {
			return fail("get_state projection exceeded the navigation budget")
		}
		result := &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
		wire, e := json.Marshal(result)
		if e != nil || len(wire) > 128<<10 {
			return fail("get_state result exceeded the wire budget")
		}
		return result, nil
	})
}

// Parse raw JSON so duplicate keys and encoding/json's case-insensitive struct
// matching cannot silently select a different scope. Never echo input values.
func decodeStatePageArguments(raw json.RawMessage) (domain.StatePageRequest, error) {
	var out domain.StatePageRequest
	bad := errors.New("invalid navigation arguments")
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 8192 || !utf8.Valid(raw) {
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
		case "cursor":
			e = d.Decode(&out.Cursor)
			if out.Cursor == "" {
				return out, bad
			}
		case "limit":
			var v json.RawMessage
			e = d.Decode(&v)
			if e == nil {
				e = json.Unmarshal(v, &out.Limit)
			}
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
