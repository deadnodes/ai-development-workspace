package agentconnect

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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/agentguide"
)

func oneProductPage() map[string]any {
	return map[string]any{"projection": "compact_navigation", "scope": map[string]any{"collection": "products"}, "items": []any{map[string]any{"id": "p0"}}, "pagination": map[string]any{"returned": 1, "has_more": false}}
}

func metadataServer(t *testing.T, page any, jsonResponse bool) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sdk := mcp.NewServer(&mcp.Implementation{Name: "discovery-test", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "get_state"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Limit int `json:"limit"`
	}) (*mcp.CallToolResult, any, error) {
		if in.Limit != 2 {
			t.Errorf("unbounded discovery: limit=%d", in.Limit)
		}
		return &mcp.CallToolResult{StructuredContent: page}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, &mcp.StreamableHTTPOptions{JSONResponse: jsonResponse})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent-kit":
			json.NewEncoder(w).Encode(agentguide.Get())
		case "/mcp":
			if r.Method == http.MethodGet {
				t.Error("standalone SSE opened")
			}
			handler.ServeHTTP(w, r)
		case "/api/products/p0/context":
			json.NewEncoder(w).Encode(map[string]any{"product": map[string]string{"id": "p0"}})
		default:
			t.Errorf("unexpected/fallback request: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func requireEmptyWorkspace(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed validation wrote workspace: %v %v", entries, err)
	}
}

func TestBoundedMetadataSelectionAndMalformedPages(t *testing.T) {
	for _, jsonResponse := range []bool{false, true} {
		server := metadataServer(t, oneProductPage(), jsonResponse)
		if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: t.TempDir()}); err != nil {
			t.Fatal("valid bounded page refused", jsonResponse, err)
		}
	}
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong projection", func(p map[string]any) { p["projection"] = "full_state" }},
		{"missing projection", func(p map[string]any) { delete(p, "projection") }},
		{"wrong collection", func(p map[string]any) { p["scope"].(map[string]any)["collection"] = "features" }},
		{"foreign scope product", func(p map[string]any) { p["scope"].(map[string]any)["product"] = map[string]any{"id": "foreign"} }},
		{"foreign scope feature", func(p map[string]any) { p["scope"].(map[string]any)["feature"] = map[string]any{"id": "foreign"} }},
		{"foreign item product", func(p map[string]any) { p["items"].([]any)[0].(map[string]any)["product_id"] = "foreign" }},
		{"foreign item feature", func(p map[string]any) { p["items"].([]any)[0].(map[string]any)["feature_id"] = "foreign" }},
		{"missing has_more", func(p map[string]any) { delete(p["pagination"].(map[string]any), "has_more") }},
		{"null has_more", func(p map[string]any) { p["pagination"].(map[string]any)["has_more"] = nil }},
		{"string has_more", func(p map[string]any) { p["pagination"].(map[string]any)["has_more"] = "false" }},
		{"more results", func(p map[string]any) {
			p["pagination"].(map[string]any)["has_more"] = true
			p["pagination"].(map[string]any)["next_cursor"] = "cursor"
		}},
		{"cursor without more", func(p map[string]any) { p["pagination"].(map[string]any)["next_cursor"] = "cursor" }},
		{"null cursor", func(p map[string]any) { p["pagination"].(map[string]any)["next_cursor"] = nil }},
		{"missing count", func(p map[string]any) { delete(p["pagination"].(map[string]any), "returned") }},
		{"wrong count", func(p map[string]any) { p["pagination"].(map[string]any)["returned"] = 0 }},
		{"fraction count", func(p map[string]any) { p["pagination"].(map[string]any)["returned"] = 1.5 }},
		{"no products", func(p map[string]any) { p["items"] = []any{}; p["pagination"].(map[string]any)["returned"] = 0 }},
		{"two products", func(p map[string]any) {
			p["items"] = []any{map[string]any{"id": "p0"}, map[string]any{"id": "p1"}}
			p["pagination"].(map[string]any)["returned"] = 2
		}},
		{"duplicate products", func(p map[string]any) {
			p["items"] = []any{map[string]any{"id": "p0"}, map[string]any{"id": "p0"}}
			p["pagination"].(map[string]any)["returned"] = 2
		}},
		{"too many products", func(p map[string]any) {
			p["items"] = []any{map[string]any{"id": "p0"}, map[string]any{"id": "p1"}, map[string]any{"id": "p2"}}
			p["pagination"].(map[string]any)["returned"] = 3
		}},
		{"blank id", func(p map[string]any) { p["items"].([]any)[0].(map[string]any)["id"] = " " }},
		{"large id", func(p map[string]any) { p["items"].([]any)[0].(map[string]any)["id"] = strings.Repeat("x", 129) }},
		{"control id", func(p map[string]any) { p["items"].([]any)[0].(map[string]any)["id"] = "p\x00" }},
		{"oversized JSON success", func(p map[string]any) { p["guidance"] = strings.Repeat("x", discoveryBodyLimit+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := oneProductPage()
			tc.change(page)
			server := metadataServer(t, page, true)
			root := t.TempDir()
			_, err := Install(context.Background(), Options{URL: server.URL, Workspace: root})
			if err == nil || !strings.Contains(err.Error(), "--product-id") {
				t.Fatal("malformed/ambiguous selection accepted", err)
			}
			requireEmptyWorkspace(t, root)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countingBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *countingBody) Close() error               { b.closed = true; return nil }

func TestDiscoveryWholeBodyCapAllStatusesAndTypes(t *testing.T) {
	for _, status := range []int{200, 400, 401, 500} {
		for _, contentType := range []string{"application/json", "text/event-stream", "text/plain"} {
			for _, declared := range []int64{-1, 0, discoveryBodyLimit + 1} {
				body := &countingBody{Reader: strings.NewReader(`{"ok":true}` + strings.Repeat(" ", discoveryBodyLimit))}
				transport := discoveryTransport{origin: "http://127.0.0.1:1234", token: "fixture-token", next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Authorization") != "Bearer fixture-token" {
						t.Error("credential missing at correct origin")
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, ContentLength: declared, Body: body}, nil
				})}
				req, _ := http.NewRequest("POST", "http://127.0.0.1:1234/mcp", nil)
				if _, err := transport.RoundTrip(req); err == nil {
					t.Fatal("oversized body accepted", status, contentType, declared)
				}
				if !body.closed || declared > discoveryBodyLimit && body.reads != 0 {
					t.Fatal("response not closed or declared oversize read")
				}
			}
		}
	}
	body := &countingBody{Reader: strings.NewReader(strings.Repeat("x", discoveryBodyLimit))}
	transport := discoveryTransport{origin: "http://127.0.0.1:1234", next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, ContentLength: -1}, nil
	})}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:1234/mcp", nil)
	res, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal("exact body budget rejected", err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if len(data) != discoveryBodyLimit || !body.closed {
		t.Fatal("body budget boundary changed")
	}
}

func TestDiscoveryOriginRedirectAndHTTPFailuresWriteNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		t.Error("redirect leaked request/credential")
	}))
	defer foreign.Close()
	for _, mode := range []string{"redirect", "unauthorized", "oversized-error", "chunked-json", "chunked-sse"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/agent-kit":
					json.NewEncoder(w).Encode(agentguide.Get())
				case "/mcp":
					switch mode {
					case "redirect":
						http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
					case "unauthorized":
						w.WriteHeader(401)
					case "oversized-error":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(500)
						io.WriteString(w, strings.Repeat("x", discoveryBodyLimit+1))
					default:
						w.Header().Set("Content-Type", "application/json")
						if mode == "chunked-sse" {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						w.(http.Flusher).Flush()
						io.WriteString(w, strings.Repeat("x", discoveryBodyLimit+1))
					}
				default:
					t.Error("discovery fallback or scoped read after failed selection")
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			root := t.TempDir()
			if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root}); err == nil {
				t.Fatal("failed discovery accepted")
			}
			requireEmptyWorkspace(t, root)
		})
	}
	if foreignCalls.Load() != 0 {
		t.Fatal("followed redirect")
	}
	called := false
	transport := discoveryTransport{origin: "https://correct.invalid", token: "secret", next: roundTripFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })}
	req, _ := http.NewRequest("POST", "https://foreign.invalid/mcp", nil)
	if _, err := transport.RoundTrip(req); err == nil || called {
		t.Fatal("foreign origin dispatched")
	}
}

func TestExplicitProductSkipsDiscoveryAndMatchesContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mismatch := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/agent-kit":
				json.NewEncoder(w).Encode(agentguide.Get())
			case "/api/products/p0/context":
				id := "p0"
				if mismatch {
					id = "other"
				}
				json.NewEncoder(w).Encode(map[string]any{"product": map[string]string{"id": id}})
			default:
				t.Error("explicit Product used listing/full state")
				w.WriteHeader(500)
			}
		}))
		root := t.TempDir()
		_, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, ProductID: "p0"})
		server.Close()
		if (err != nil) != mismatch {
			t.Fatal("context identity result incorrect", mismatch, err)
		}
		if mismatch {
			requireEmptyWorkspace(t, root)
		}
	}
}

func TestDiscoveryJSONRejectsDuplicateFields(t *testing.T) {
	raw := `{"projection":"compact_navigation","scope":{"collection":"products"},"items":[{"id":"p0"}],"pagination":{"returned":1,"has_more":true,"has_more":false}}`
	if _, err := navigationProduct([]byte(raw)); err == nil {
		t.Fatal("duplicate required flag accepted")
	}
	if _, err := navigationProduct(bytes.ReplaceAll([]byte(raw), []byte(`"has_more":true,"has_more":false`), []byte(`"has_more":null`))); err == nil {
		t.Fatal("null required flag accepted")
	}
}

// The proxy mutates only synthetic loopback responses, keeping the real SDK and
// installer in the path so ambiguity cannot be hidden by a direct parser test.
func rewrittenDiscoveryServer(t *testing.T, upstream string, rewrite func(string, string, []byte) []byte) *httptest.Server {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded := r.Clone(r.Context())
		forwarded.RequestURI = ""
		forwarded.URL.Scheme, forwarded.URL.Host = target.Scheme, target.Host
		res, err := http.DefaultTransport.RoundTrip(forwarded)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		data = rewrite(r.URL.Path, res.Header.Get("Content-Type"), data)
		for key, values := range res.Header {
			if key != "Content-Length" {
				w.Header()[key] = values
			}
		}
		w.WriteHeader(res.StatusCode)
		w.Write(data)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestDiscoverySSEDuplicatePaginationRefusesInstall(t *testing.T) {
	upstream := metadataServer(t, oneProductPage(), false)
	var mutations atomic.Int32
	proxy := rewrittenDiscoveryServer(t, upstream.URL, func(_ string, kind string, data []byte) []byte {
		if strings.HasPrefix(kind, "text/event-stream") && bytes.Contains(data, []byte(`"has_more":false`)) {
			mutations.Add(1)
			return bytes.Replace(data, []byte(`"has_more":false`), []byte(`"has_more":true,"has_more":false`), 1)
		}
		return data
	})
	root := t.TempDir()
	_, err := Install(context.Background(), Options{URL: proxy.URL, Workspace: root})
	if mutations.Load() != 1 {
		t.Fatal("actual SSE ambiguity not exercised", mutations.Load(), err)
	}
	if err == nil {
		t.Fatal("duplicate SSE pagination accepted")
	}
	requireEmptyWorkspace(t, root)
}

func TestScopedProductContextStrictJSONBeforeWrites(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			body []byte
		}{
			{"conflicting product id", []byte(`{"product":{"id":"foreign","id":"p0"}}`)},
			{"duplicate product object", []byte(`{"product":{"id":"foreign"},"product":{"id":"p0"}}`)},
			{"escaped duplicate id", []byte(`{"product":{"id":"foreign","\u0069d":"p0"}}`)},
			{"invalid encoding", append([]byte(`{"product":{"id":"p0"},"other":"`), 0xff, '"', '}')},
			{"trailing data", []byte(`{"product":{"id":"p0"}} {}`)},
			{"oversized valid prefix", []byte(`{"product":{"id":"p0"}}` + strings.Repeat(" ", 2<<20))},
		} {
			t.Run(fmt.Sprintf("%s/explicit=%t", tc.name, explicit), func(t *testing.T) {
				upstream := kitServer(t, 1, "")
				var mutations atomic.Int32
				proxy := rewrittenDiscoveryServer(t, upstream.URL, func(path, _ string, data []byte) []byte {
					if path == "/api/products/p0/context" {
						mutations.Add(1)
						return tc.body
					}
					return data
				})
				root := t.TempDir()
				options := Options{URL: proxy.URL, Workspace: root}
				if explicit {
					options.ProductID = "p0"
				}
				if _, err := Install(context.Background(), options); err == nil {
					t.Fatal("ambiguous/invalid scoped context accepted")
				}
				if mutations.Load() != 1 {
					t.Fatal("scoped context not exercised")
				}
				requireEmptyWorkspace(t, root)
			})
		}
	}
}

func formatSSEFixture(t *testing.T, raw []byte, eol string, bom, multiline, eof bool) []byte {
	t.Helper()
	// Preserve ignored named data, comments and empty-data/id-only priming events.
	var lines []string
	lines = append(lines, ": fixture comment", "ignored-field", "event: notice", "data: not JSON", "", "id: priming", "data:", "")
	for _, line := range strings.Split(string(raw), "\n") {
		if multiline && strings.HasPrefix(line, "data:") && strings.TrimSpace(strings.TrimPrefix(line, "data:")) != "" {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), "", "  "); err != nil {
				t.Error(err)
				return raw
			}
			for _, part := range strings.Split(pretty.String(), "\n") {
				lines = append(lines, "data: "+part)
			}
		} else {
			lines = append(lines, line)
		}
	}
	result := strings.Join(lines, eol)
	if eof {
		result = strings.TrimSuffix(result, eol+eol)
	}
	if bom {
		result = "\xef\xbb\xbf" + result
	}
	return []byte(result)
}

func TestDiscoveryValidSSEFramingThroughSDKAndInstall(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n", "\r"} {
		for _, bom := range []bool{false, true} {
			for _, multiline := range []bool{false, true} {
				for _, eof := range []bool{false, true} {
					t.Run(fmt.Sprintf("eol=%q/bom=%t/multiline=%t/eof=%t", eol, bom, multiline, eof), func(t *testing.T) {
						upstream := metadataServer(t, oneProductPage(), false)
						var rewrites atomic.Int32
						proxy := rewrittenDiscoveryServer(t, upstream.URL, func(_ string, kind string, data []byte) []byte {
							if strings.HasPrefix(kind, "text/event-stream") {
								rewrites.Add(1)
								return formatSSEFixture(t, data, eol, bom, multiline, eof)
							}
							return data
						})
						binding, err := Install(context.Background(), Options{URL: proxy.URL, Workspace: t.TempDir()})
						if err != nil || binding.ProductID != "p0" || rewrites.Load() < 2 {
							t.Fatal("valid SSE framing rejected or SDK path not exercised", binding, err, rewrites.Load())
						}
					})
				}
			}
		}
	}
}

func TestStrictDiscoverySSERejectsAmbiguousMessageAndPreservesFraming(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("data: {\"x\":1,\"x\":2}\n\n"),
		[]byte("event: message\rdata: {\"x\":1,\"\\u0078\":2}"),
		[]byte("data: {\"x\":1,\n: comment\ndata: \"x\":2}\n\n"),
		[]byte("event: message \t\ndata: {\"x\":1,\"x\":2}\n\n"),
		[]byte("data: {\"x\":1} {}\n\n"),
		[]byte("data: [\xff]\n\n"),
		[]byte(strings.Repeat("x\n", discoveryBodyLimit/2)), // normalization must not widen body budget
	} {
		if _, err := strictDiscoverySSE(data); err == nil {
			t.Fatal("ambiguous/invalid/oversized normalized SSE accepted")
		}
	}
	for _, data := range []string{"", ":comment", "id: priming\ndata:\n\n", "data\n\n", "event: notice\ndata: not JSON\n\n", "\xef\xbb\xbfdata: {\"ok\":true}\r\n\r\n", "data: {\n: comment\ndata: \"ok\":true}\n", "data: {\"ok\":true}"} {
		normalized, err := strictDiscoverySSE([]byte(data))
		if err != nil {
			t.Fatal("valid SSE fixture rejected", err)
		}
		if len(normalized) > discoveryBodyLimit || bytes.ContainsRune(normalized, '\r') || bytes.HasPrefix(normalized, []byte{0xef, 0xbb, 0xbf}) {
			t.Fatal("SSE normalization outside contract")
		}
	}
}
