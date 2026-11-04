package intelligence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{BaseURL: "https://provider.example/v1", Model: "compatible-model", APIKeyEnv: "MODEL_KEY", EncryptionKeyEnv: "ENCRYPTION_KEY", ProductIDs: []string{"p"}, MaxInputBytes: 32768, MaxOutputBytes: 32768, MaxCompletionTokens: 1024, MaxRequestsPerDay: 10, TimeoutSeconds: 2}
}
func testLookup(k string) string {
	if k == "MODEL_KEY" {
		return strings.Repeat("x", 32)
	}
	if k == "ENCRYPTION_KEY" {
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	}
	return ""
}
func testAuthority(t *testing.T) *Authority {
	t.Helper()
	a, e := New(testConfig(), testLookup)
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func TestCompatibleRequestAndReportedUsage(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("wrong fixed endpoint or authentication")
		}
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["max_completion_tokens"] != float64(1024) || in["stream"] != false || in["tools"] != nil {
			t.Error("missing bounded request contract")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": `{"decision":"draft","summary":"Done","changes":[],"warnings":[],"patch":null}`}}}, "usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}})
	}))
	defer server.Close()
	a := testAuthority(t)
	a.c.BaseURL = server.URL + "/v1"
	a.http = server.Client()
	a.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	raw, usage, e := a.Complete(context.Background(), "annotation", map[string]string{"goal": "Ship useful work"})
	if e != nil || !json.Valid(raw) || !usage.Reported || usage.TotalTokens != 20 || requests != 1 {
		t.Fatalf("request failed: usage=%+v error=%v count=%d", usage, e, requests)
	}
}

func TestRejectedResponsesDoNotLeakOrRetry(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{}","refusal":"no"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{}","tool_calls":[{}]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{}","Content":"bad"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"email: a@example.com"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}],"usage":{"completion_tokens":9999}}`,
		`{"choices":[]} {"choices":[]}`,
	} {
		t.Run(body[:min(len(body), 60)], func(t *testing.T) {
			calls := 0
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = io.WriteString(w, body) }))
			defer s.Close()
			a := testAuthority(t)
			a.c.BaseURL = s.URL
			a.http = s.Client()
			_, _, e := a.Complete(context.Background(), "summary", map[string]string{"safe": "context"})
			if e == nil || calls != 1 || strings.Contains(e.Error(), "example.com") {
				t.Fatalf("expected redacted single-call failure: %v calls%d", e, calls)
			}
		})
	}
}

func TestUnknownUsageEncryptionAndPrivacy(t *testing.T) {
	a := testAuthority(t)
	sealed, e := a.Seal("p/task/kind/hash/input", map[string]string{"text": "private context"})
	if e != nil || strings.Contains(sealed, "private context") {
		t.Fatal("not encrypted")
	}
	var v map[string]string
	if a.Open("p/task/kind/hash/input", sealed, &v) != nil || v["text"] != "private context" {
		t.Fatal("cannot read own envelope")
	}
	if a.Open("q/task/kind/hash/input", sealed, &v) == nil {
		t.Fatal("ciphertext scope swap admitted")
	}
	if a.ValidateInput(map[string]string{"email": "person@example.com"}) == nil || a.ValidateInput(strings.Repeat("a", 40000)) == nil {
		t.Fatal("privacy/input bound bypass")
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":1,"A":2}`, `{} {}`, `{"a":[{"b":1,"b":2}]}`} {
		if Decode([]byte(raw), &map[string]any{}) == nil {
			t.Fatal("ambiguous JSON admitted")
		}
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}],"usage":{}}`)
	}))
	defer s.Close()
	a.c.BaseURL = s.URL
	a.http = s.Client()
	_, usage, e := a.Complete(context.Background(), "summary", map[string]string{"safe": "context"})
	if e != nil || usage.Reported {
		t.Fatal("missing usage asserted known")
	}
}

func TestConfigurationAndEndpointGuards(t *testing.T) {
	for _, url := range []string{"http://provider.example/v1", "https://name:password@provider.example/v1", "https://provider.example/v1?key=x", "https://provider.example/../v1"} {
		c := testConfig()
		c.BaseURL = url
		if _, e := New(c, testLookup); e == nil {
			t.Fatalf("unsafe endpoint admitted: %s", url)
		}
	}
	c := testConfig()
	c.ProductIDs = []string{"*"}
	if _, e := New(c, testLookup); e == nil {
		t.Fatal("wildcard grant admitted")
	}
	a := testAuthority(t)
	if a.Allows("other") || !a.Allows("p") {
		t.Fatal("Product grants ignored")
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.2", "100.100.100.100", "169.254.169.254", "::1", "fc00::1", "198.18.0.1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatalf("private address admitted: %s", ip)
		}
	}
	if _, e := a.dial(context.Background(), "tcp", "127.0.0.1:443"); e == nil {
		t.Fatal("private dial admitted without opt-in")
	}
	if e := a.http.CheckRedirect(httptest.NewRequest("GET", "https://other.example", nil), nil); e == nil {
		t.Fatal("credential redirect admitted")
	}
}
