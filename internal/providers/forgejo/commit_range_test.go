package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
	"time"
)

func TestCommitRangeQueryAndParsing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	for _, base := range []string{"", strings.Repeat("b", 40)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wantLimit := "50"
			if base == "" {
				wantLimit = "1"
			}
			if r.URL.Query().Get("sha") != sha || r.URL.Query().Get("limit") != wantLimit {
				t.Errorf("query %v", r.URL.Query())
			}
			if base == "" && r.URL.Query().Get("not") != "" || base != "" && r.URL.Query().Get("not") != base {
				t.Errorf("not query %v", r.URL.Query())
			}
			fmt.Fprintf(w, `[{"sha":%q,"commit":{"message":"Title\nbody","author":{"name":"Author","date":%q}},"author":{"login":"login"}}]`, sha, time.Now().UTC().Format(time.RFC3339))
		}))
		c := New()
		v, tr, e := c.CommitRange(context.Background(), buildconfig.Connection{APIURL: srv.URL, TokenRef: "token"}, "o/r", base, sha, 50)
		srv.Close()
		if e != nil || tr || len(v) != 1 || v[0].Title != "Title" || v[0].Author != "login" {
			t.Fatalf("%+v %v %v", v, tr, e)
		}
	}
}
