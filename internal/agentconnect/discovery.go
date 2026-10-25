package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const discoveryBodyLimit = 128 << 10

// This discovery-only transport caps the complete response before the SDK can
// decode JSON, SSE or an error body. A valid JSON prefix is never accepted after
// silent truncation. Declared oversize is rejected without reading the body.
type discoveryTransport struct {
	origin, token string
	next          http.RoundTripper
}

func (t discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	origin, err := url.Parse(t.origin)
	if err != nil || r.URL.Scheme != origin.Scheme || r.URL.Host != origin.Host || r.URL.User != nil {
		return nil, errors.New("discovery refused a different origin")
	}
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Del("Authorization")
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.next.RoundTrip(clone)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	if response.Body == nil {
		return nil, errors.New("discovery response has no body")
	}
	defer response.Body.Close()
	if response.ContentLength > discoveryBodyLimit {
		return nil, errors.New("discovery response exceeds 128 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, discoveryBodyLimit+1))
	if err != nil || len(data) > discoveryBodyLimit {
		return nil, errors.New("discovery response unreadable or exceeds 128 KiB")
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "application/json" && len(data) != 0 {
		var raw json.RawMessage
		if decodeUniqueJSON(data, &raw) != nil {
			return nil, errors.New("discovery returned ambiguous or invalid JSON")
		}
	}
	if mediaType == "text/event-stream" {
		data, err = strictDiscoverySSE(data)
		if err != nil {
			return nil, errors.New("discovery returned ambiguous or invalid SSE")
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	return response, nil
}

// Validate message JSON before the SDK collapses duplicate object fields. This
// is a discovery-only validation/framing adapter, not a replacement MCP client.
// SDK v1.8 reads LF lines and trims field values: normalize valid SSE line endings
// and an initial BOM while validating the exact data bytes it will consume.
func strictDiscoverySSE(data []byte) ([]byte, error) {
	bad := errors.New("invalid discovery SSE")
	if len(data) > discoveryBodyLimit || !utf8.Valid(data) {
		return nil, bad
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	var normalized, payload bytes.Buffer
	name, hasData := "", false
	validate := func() error {
		if (name == "" || name == "message") && payload.Len() != 0 {
			var raw json.RawMessage
			if decodeUniqueJSON(payload.Bytes(), &raw) != nil {
				return bad
			}
		}
		name, hasData = "", false
		payload.Reset()
		return nil
	}
	for start := 0; start < len(data); {
		end := bytes.IndexByte(data[start:], '\n')
		terminated := end >= 0
		if terminated {
			end += start
		} else {
			end = len(data)
		}
		line := data[start:end]
		if len(line) == 0 {
			if err := validate(); err != nil {
				return nil, err
			}
		} else if line[0] != ':' {
			field, value, colon := bytes.Cut(line, []byte(":"))
			if !colon {
				// SSE permits a field without ':' as an empty value. Supply the
				// colon for the SDK's stricter line reader, without inventing data.
				line = append(bytes.Clone(line), ':')
			}
			switch string(field) {
			case "event":
				name = strings.TrimSpace(string(value))
			case "data":
				if hasData {
					payload.WriteByte('\n')
				}
				payload.Write(bytes.TrimSpace(value))
				hasData = true
			}
		}
		size := normalized.Len() + len(line)
		if terminated {
			size++
		}
		if size > discoveryBodyLimit {
			return nil, bad
		}
		normalized.Write(line)
		if terminated {
			normalized.WriteByte('\n')
		}
		start = end + 1
	}
	// The pinned SDK dispatches a pending data event at EOF, even without a
	// final blank line; validate it too rather than leaving an unchecked tail.
	if err := validate(); err != nil {
		return nil, err
	}
	return normalized.Bytes(), nil
}

func discoverProduct(ctx context.Context, endpoint, token string) (string, error) {
	fail := func() (string, error) {
		return "", errors.New("provide --product-id: bounded Product discovery unavailable, malformed or ambiguous")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 16384, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: discoveryTransport{origin: endpoint, token: token, next: transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "release-control-connect", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp", HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: discoveryBodyLimit}, nil)
	if err != nil {
		return fail()
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_state", Arguments: map[string]any{"limit": 2}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		return fail()
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil || len(raw) > 32<<10 {
		return fail()
	}
	id, err := navigationProduct(raw)
	if err != nil {
		return fail()
	}
	return id, nil
}

func validProductID(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && strings.TrimSpace(id) == id && !strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func navigationProduct(raw []byte) (string, error) {
	bad := errors.New("invalid bounded Product page")
	var page map[string]any
	if decodeUniqueJSON(raw, &page) != nil {
		return "", bad
	}
	if page["projection"] != "compact_navigation" {
		return "", bad
	}
	scope, ok := page["scope"].(map[string]any)
	if !ok || scope["collection"] != "products" || scope["product"] != nil || scope["feature"] != nil {
		return "", bad
	}
	for key := range scope {
		if key != "collection" && key != "product" && key != "feature" {
			return "", bad
		}
	}
	items, ok := page["items"].([]any)
	if !ok || len(items) > 2 {
		return "", bad
	}
	pagination, ok := page["pagination"].(map[string]any)
	if !ok {
		return "", bad
	}
	count, ok := pagination["returned"].(json.Number)
	if !ok {
		return "", bad
	}
	returned, err := strconv.Atoi(string(count))
	if err != nil || returned != len(items) {
		return "", bad
	}
	more, ok := pagination["has_more"].(bool)
	if !ok {
		return "", bad
	}
	cursor := ""
	if v, present := pagination["next_cursor"]; present {
		var ok bool
		cursor, ok = v.(string)
		if !ok {
			return "", bad
		}
	}
	if more || cursor != "" || len(items) != 1 {
		return "", bad
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		return "", bad
	}
	id, ok := item["id"].(string)
	if !ok || !validProductID(id) {
		return "", bad
	}
	for _, key := range []string{"product_id", "feature_id"} {
		if v, present := item[key]; present && v != "" {
			return "", bad
		}
	}
	return id, nil
}

// Reject duplicate keys and invalid UTF-8 before typed decoding can silently
// overwrite a required field. A depth bound also limits malformed kit/page work.
func decodeUniqueJSON(data []byte, out any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid JSON encoding")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds bound")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON field")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(out)
}
