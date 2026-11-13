package forgejo

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
)

type statusTransport func(*http.Request) (*http.Response, error)

func (f statusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeploymentCombinedStatusHTTP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	os.WriteFile(filepath.Join(dir, "token"), []byte("private-token"), 0600)
	sha := strings.Repeat("a", 40)
	for _, body := range []string{`{"state":"success","statuses":[{"context":"verify","status":"success"}]}`, `{"state":"pending","statuses":[]}`, `{"state":"failure","statuses":[{"context":"verify","status":"failure"}]}`} {
		c := NewWithClient(&http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Path != "/api/v1/repos/o/r/commits/"+sha+"/status" || r.Header.Get("Authorization") != "token private-token" {
				t.Fatal(r)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		status, e := c.CommitStatus(context.Background(), buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}, "o/r", sha)
		if e != nil || status.State == "" {
			t.Fatal(status, e)
		}
	}
}
