package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"releasecontrol/internal/buildconfig"
)

// httptest handlers through an in-memory transport need no listening socket.
func treeTestClient(t *testing.T, h http.HandlerFunc) (*Client, buildconfig.Connection) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewWithClient(&http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "token private-token" {
			t.Fatal("missing authentication")
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Result(), nil
	})})
	return c, buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}
}
func TestCommitPullRequest(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, code := range []int{200, 404, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, cn := treeTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/repos/o/r/commits/"+sha+"/pull" {
					t.Fatal(r.URL)
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 12, "merged": true, "base": map[string]string{"ref": "main"}, "head": map[string]string{"sha": sha}, "merge_commit_sha": sha})
			})
			pr, found, err := c.CommitPullRequest(context.Background(), cn, "o/r", sha)
			if code == 500 {
				if err == nil {
					t.Fatal("missing error")
				}
				return
			}
			if err != nil || found != (code == 200) {
				t.Fatal(pr, found, err)
			}
			if found && (pr.Number != 12 || !pr.Merged || pr.BaseRef != "main" || pr.HeadSHA != sha || pr.MergeCommitSHA != sha) {
				t.Fatal(pr)
			}
		})
	}
}
func TestTreesEqual(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, test := range []string{"equal sorted", "different blob", "different subtree", "mode", "path", "type", "truncated", "too many", "incomplete", "duplicate", "missing metadata", "http error"} {
		t.Run(test, func(t *testing.T) {
			c, cn := treeTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("per_page") != "1000" || r.URL.Query().Get("recursive") != "" {
					t.Fatal(r.URL)
				}
				second := strings.Contains(r.URL.Path, b)
				entries := []treeEntry{{"README", "100644", "blob", a}, {"src", "040000", "tree", b}}
				truncated := false
				count := 2
				if second {
					switch test {
					case "different blob":
						entries[0].SHA = b
					case "different subtree":
						entries[1].SHA = a
					case "mode":
						entries[0].Mode = "100755"
					case "path":
						entries[0].Path = "other"
					case "type":
						entries[0].Type = "commit"
					case "truncated":
						truncated = true
					case "too many":
						count = 1001
					case "incomplete":
						count = 3
					case "duplicate":
						entries[1] = entries[0]
					case "missing metadata":
						_, _ = w.Write([]byte(`{"tree":[]}`))
						return
					case "http error":
						w.WriteHeader(503)
						return
					}
					entries[0], entries[1] = entries[1], entries[0]
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sha": a, "tree": entries, "truncated": truncated, "total_count": count})
			})
			equal, err := c.TreesEqual(context.Background(), cn, "o/r", a, b)
			if test == "http error" {
				if err == nil {
					t.Fatal("missing error")
				}
				return
			}
			if err != nil || equal != (test == "equal sorted") {
				t.Fatal(equal, err)
			}
		})
	}
}
