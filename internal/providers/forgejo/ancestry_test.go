package forgejo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
)

func TestIsAncestor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	ancestor, descendant := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, body string
		status     int
		want, fail bool
	}{
		{"ancestor", `[]`, 200, true, false},
		{"not ancestor", `[{"sha":"abc"}]`, 200, false, false},
		{"object", `{}`, 200, false, true},
		{"null", `null`, 200, false, true},
		{"scalar", `42`, 200, false, true},
		{"http error", `{}`, 500, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/repos/o/r/commits" {
					t.Error(r.Method, r.URL.Path)
				}
				expected := url.Values{"sha": {ancestor}, "not": {descendant}, "limit": {"1"}, "stat": {"false"}, "verification": {"false"}, "files": {"false"}}
				if r.URL.RawQuery != expected.Encode() {
					t.Error("query", r.URL.RawQuery)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			client := &http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w.Result(), nil
			})}
			cn := buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}
			got, err := NewWithClient(client).IsAncestor(context.Background(), cn, "o/r", ancestor, descendant)
			if got != tc.want || (err != nil) != tc.fail {
				t.Fatal(got, err)
			}
		})
	}
	c := NewWithClient(&http.Client{Transport: statusTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected ancestry HTTP call"); return nil, nil })})
	if got, err := c.IsAncestor(context.Background(), buildconfig.Connection{}, "o/r", ancestor, ancestor); err != nil || !got {
		t.Fatal(got, err)
	}
	for _, pair := range [][2]string{{"bad", descendant}, {ancestor, "bad"}} {
		if _, err := c.IsAncestor(context.Background(), buildconfig.Connection{}, "o/r", pair[0], pair[1]); err == nil {
			t.Fatal("invalid SHA accepted")
		}
	}
}
