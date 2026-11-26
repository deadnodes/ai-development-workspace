package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const codeInstructions = `Download the SDK once: curl -fsS -H "Authorization: Bearer $RCP_TOKEN" "$RCP_URL/api/code-sdk/rcp.ts" -o rcp.ts. Write script.ts importing rcp from "./rcp.ts", run node script.ts (Node 24), and print only the final compact result. Batch reads with Promise.all. Scripts run locally with the agent's own credentials and the same authorization as MCP calls. Write actions remain the same attributed commands and policies, with no new authority. The server never runs submitted code.`

// codeRPC reuses the MCP dispatch and the caller's authenticated context. The SDK
// creates a default-initialized ephemeral session for each stateless request.
func codeRPC(ctx context.Context, handler http.Handler, method string, params any) (json.RawMessage, int, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, 500, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, 500, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, fmt.Errorf("MCP HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		return nil, 500, err
	}
	if envelope.Error != nil {
		status := 400
		if strings.HasPrefix(envelope.Error.Message, "unknown tool ") {
			status = 404
		}
		return nil, status, fmt.Errorf("%s", envelope.Error.Message)
	}
	return envelope.Result, 200, nil
}

type codeTool struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  any    `json:"input_schema"`
	OutputSchema any    `json:"output_schema"`
}

func codeTools(ctx context.Context, handler http.Handler) ([]codeTool, error) {
	tools := []codeTool{}
	cursor := ""
	for {
		raw, _, err := codeRPC(ctx, handler, "tools/list", map[string]any{"cursor": cursor})
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []struct {
				Name         string `json:"name"`
				Description  string `json:"description"`
				InputSchema  any    `json:"inputSchema"`
				OutputSchema any    `json:"outputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for _, tool := range page.Tools {
			tools = append(tools, codeTool{tool.Name, tool.Description, tool.InputSchema, tool.OutputSchema})
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("MCP tools/list cursor did not advance")
		}
		cursor = page.NextCursor
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func registerCodeMode(mux routeRegistrar, server *mcp.Server, handler http.Handler) {
	mcp.AddTool(server, &mcp.Tool{Name: "get_code_api", Description: "Discover the typed agent-side TypeScript SDK and optionally filter tool declarations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Query string `json:"query,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		tools, err := codeTools(ctx, handler)
		if err != nil {
			return nil, nil, err
		}
		selected := []codeTool{}
		names := []string{}
		for _, tool := range tools {
			if strings.Contains(strings.ToLower(tool.Name+" "+tool.Description), strings.ToLower(in.Query)) {
				selected = append(selected, tool)
				names = append(names, tool.Name)
			}
		}
		return nil, map[string]any{"instructions": codeInstructions, "sdk_path": "/api/code-sdk/rcp.ts", "call_path": "/api/tools/{name}", "declarations": codeDeclarations(selected), "tools": names}, nil
	})
	mux.HandleFunc("POST /api/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 12<<20))
		if err != nil {
			write(w, 413, map[string]string{"error": err.Error()})
			return
		}
		args := map[string]any{}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &args); err != nil || args == nil {
				write(w, 400, map[string]string{"error": "Expected a JSON arguments object"})
				return
			}
		}
		raw, status, err := codeRPC(r.Context(), handler, "tools/call", map[string]any{"name": r.PathValue("name"), "arguments": args})
		if err != nil {
			write(w, status, map[string]string{"error": err.Error()})
			return
		}
		var result struct {
			IsError    bool            `json:"isError"`
			Structured json.RawMessage `json:"structuredContent"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		parts := []string{}
		for _, content := range result.Content {
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		}
		text := strings.Join(parts, "\n")
		if result.IsError {
			write(w, 422, map[string]string{"error": text})
			return
		}
		if len(result.Structured) > 0 {
			write(w, 200, result.Structured)
			return
		}
		if json.Valid([]byte(text)) {
			write(w, 200, json.RawMessage(text))
			return
		}
		write(w, 200, map[string]string{"text": text})
	})
	mux.HandleFunc("GET /api/tools", func(w http.ResponseWriter, r *http.Request) {
		tools, err := codeTools(r.Context(), handler)
		if err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		write(w, 200, map[string]any{"tools": tools})
	})
	mux.HandleFunc("GET /api/code-sdk/rcp.ts", func(w http.ResponseWriter, r *http.Request) {
		tools, err := codeTools(r.Context(), handler)
		if err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		scheme := r.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "http"
			if r.TLS != nil {
				scheme = "https"
			}
		}
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, codeSDK(tools, scheme+"://"+host))
	})
}

var tsIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func tsKey(s string) string {
	if tsIdentifier.MatchString(s) {
		return s
	}
	return jsString(s)
}

// jsString quotes s as a JavaScript string literal (JSON escapes are valid JS).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
func tsDoc(s string) string {
	return "/** " + strings.ReplaceAll(strings.ReplaceAll(s, "*/", "* /"), "\r", "") + " */\n"
}
func tsLiteral(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "any"
	}
	switch v.(type) {
	case string, float64, bool, nil:
		return string(b)
	}
	return "any"
}

func schemaTS(schema any, depth int) string {
	if depth >= 8 {
		return "any"
	}
	if schema == nil {
		return "unknown"
	}
	s, ok := schema.(map[string]any)
	if !ok {
		return "any"
	}
	if v, ok := s["const"]; ok {
		return tsLiteral(v)
	}
	if values, ok := s["enum"].([]any); ok && len(values) > 0 {
		parts := []string{}
		for _, v := range values {
			parts = append(parts, tsLiteral(v))
		}
		return strings.Join(parts, " | ")
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if values, ok := s[key].([]any); ok && len(values) > 0 {
			parts := []string{}
			for _, v := range values {
				parts = append(parts, "("+schemaTS(v, depth+1)+")")
			}
			union := strings.Join(parts, " | ")
			// Variant branches (e.g. execute's per-action oneOf) usually only add a
			// const discriminator and required keys; keep the shared base fields.
			_, hasProps := s["properties"]
			_, hasRequired := s["required"]
			if s["type"] == "object" || hasProps || hasRequired {
				base := map[string]any{}
				for k, v := range s {
					if k != "anyOf" && k != "oneOf" {
						base[k] = v
					}
				}
				return "(" + schemaTS(base, depth) + ") & (" + union + ")"
			}
			return union
		}
	}
	if types, ok := s["type"].([]any); ok {
		parts := []string{}
		for _, typ := range types {
			clone := map[string]any{}
			for k, v := range s {
				clone[k] = v
			}
			clone["type"] = typ
			parts = append(parts, schemaTS(clone, depth+1))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
		return "any"
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "string", "boolean", "null":
		return typ
	case "number", "integer":
		return "number"
	case "array":
		return "Array<" + schemaTS(s["items"], depth+1) + ">"
	}
	props, hasProps := s["properties"].(map[string]any)
	requiredList, hasRequired := s["required"].([]any)
	if typ == "object" || hasProps || hasRequired {
		required := map[string]bool{}
		for _, v := range requiredList {
			if key, ok := v.(string); ok {
				required[key] = true
			}
		}
		keys := []string{}
		for key := range props {
			keys = append(keys, key)
		}
		for key := range required {
			if _, ok := props[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		fields := []string{}
		for _, key := range keys {
			doc := ""
			if p, ok := props[key].(map[string]any); ok {
				if d, ok := p["description"].(string); ok {
					doc = tsDoc(d)
				}
			}
			optional := "?"
			if required[key] {
				optional = ""
			}
			fields = append(fields, doc+tsKey(key)+optional+": "+schemaTS(props[key], depth+1)+";")
		}
		if extra, ok := s["additionalProperties"]; ok && extra != false {
			value := "any"
			if _, ok := extra.(map[string]any); ok {
				value = schemaTS(extra, depth+1)
			}
			if len(fields) == 0 {
				return "Record<string, " + value + ">"
			}
			members := []string{value}
			for _, key := range keys {
				members = append(members, schemaTS(props[key], depth+1))
				if !required[key] {
					members = append(members, "undefined")
				}
			}
			fields = append(fields, "[key: string]: "+strings.Join(members, " | ")+";")
		}
		return "{\n" + strings.Join(fields, "\n") + "\n}"
	}
	// No type information: unknown keeps intersections with typed base fields precise.
	return "unknown"
}

func codeInputName(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			if upper && r >= 'a' && r <= 'z' {
				r -= 32
			}
			b.WriteRune(r)
			upper = false
		} else {
			upper = true
		}
	}
	result := b.String() + "Input"
	if result[0] >= '0' && result[0] <= '9' {
		result = "Tool" + result
	}
	return result
}
func codeDeclarations(tools []codeTool) string {
	var b strings.Builder
	used := map[string]bool{}
	names := make([]string, len(tools))
	for i, tool := range tools {
		name := codeInputName(tool.Name)
		base := name
		for n := 2; used[name]; n++ {
			name = base + strconv.Itoa(n)
		}
		used[name] = true
		names[i] = name
		typ := schemaTS(tool.InputSchema, 0)
		b.WriteString(tsDoc(tool.Description))
		if strings.HasPrefix(typ, "{\n") {
			fmt.Fprintf(&b, "export interface %s %s\n", name, typ)
		} else {
			fmt.Fprintf(&b, "export type %s = %s;\n", name, typ)
		}
	}
	b.WriteString("export const rcp = {\n")
	for i, tool := range tools {
		b.WriteString(tsDoc(tool.Description))
		fmt.Fprintf(&b, "%s: (args: %s): Promise<any> => call(%s, args),\n", tsKey(tool.Name), names[i], jsString(tool.Name))
	}
	b.WriteString("};\nexport default rcp;\n")
	return b.String()
}
func codeSDK(tools []codeTool, origin string) string {
	return "// Generated from the live MCP registry. Node 24: import rcp from './rcp.ts'; run node script.ts.\n" +
		`import { readFileSync } from "node:fs";
const baseURL = (process.env.RCP_URL || ` + jsString(origin) + `).replace(/\/+$/, "");
const token = (process.env.RCP_TOKEN || (process.env.RCP_TOKEN_FILE ? readFileSync(process.env.RCP_TOKEN_FILE, "utf8") : "")).trim();
async function call(name: string, args: any): Promise<any> {
 const response = await fetch(baseURL + "/api/tools/" + encodeURIComponent(name), {
  method: "POST",
  headers: { "Content-Type": "application/json", ...(token ? { Authorization: "Bearer " + token } : {}) },
  body: JSON.stringify(args),
 });
 const text = await response.text();
 let result: any;
 try { result = text ? JSON.parse(text) : null; } catch { result = { text }; }
 if (!response.ok) throw new Error("RCP HTTP " + response.status + " " + name + ": " + (result?.error || result?.text || response.statusText));
 return result;
}
` + codeDeclarations(tools)
}
