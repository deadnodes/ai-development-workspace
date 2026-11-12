package forgejo

import (
	"context"
	"encoding/json"
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

func fixture(t *testing.T, h http.HandlerFunc) (*Client, buildconfig.Connection) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	if e := os.WriteFile(filepath.Join(dir, "token"), []byte("secret-forgejo-token"), 0600); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(), buildconfig.Connection{APIURL: srv.URL, TokenRef: "token"}
}
func TestActionsContract(t *testing.T) {
	sha := strings.Repeat("a", 40)
	c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret-forgejo-token" {
			t.Error("authorization")
		}
		switch {
		case strings.Contains(r.URL.Path, "dispatches"):
			var b map[string]any
			if json.NewDecoder(r.Body).Decode(&b) != nil || b["ref"] != "topic" || b["return_run_info"] != true {
				t.Error("dispatch payload")
			}
			if inputs, ok := b["inputs"].(map[string]any); !ok || inputs["sha"] != sha || inputs["request_id"] != "build-id" {
				t.Error("inputs")
			}
			if r.URL.Path != "/api/v1/repos/o/r/actions/workflows/build.yml/dispatches" {
				t.Error(r.URL.Path)
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":42,"run_number":2}`)
		case strings.Contains(r.URL.Path, "branches"):
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, sha)
		case strings.HasSuffix(r.URL.Path, "/42"):
			fmt.Fprintf(w, `{"id":42,"status":"success","commit_sha":%q,"html_url":"https://git/run/42","created":"2026-11-12T10:00:00Z"}`, sha)
		default:
			if r.URL.Query().Get("head_sha") != sha || r.URL.Query().Get("workflow_id") != "build.yml" || r.URL.Query().Get("event") != "workflow_dispatch" {
				t.Error("filters")
			}
			fmt.Fprint(w, `{"workflow_runs":[{"id":42}],"total_count":1}`)
		}
	})
	ctx := context.Background()
	if head, e := c.BranchHead(ctx, cn, "o/r", "topic"); e != nil || head != sha {
		t.Fatal(head, e)
	}
	if r, e := c.Dispatch(ctx, cn, "o/r", "build.yml", "topic", sha, "build-id"); e != nil || r.ID != 42 {
		t.Fatal(r, e)
	}
	if r, e := c.GetRun(ctx, cn, "o/r", 42); e != nil || r.Status != "success" || r.Created.IsZero() {
		t.Fatal(r, e)
	}
	if p, e := c.ListRuns(ctx, cn, "o/r", "build.yml", Filter{SHA: sha, Event: "workflow_dispatch"}); e != nil || len(p.Runs) != 1 {
		t.Fatal(p, e)
	}
}
func TestErrorsBoundedAndRedacted(t *testing.T) {
	for _, mode := range []string{"unauthorized", "oversize", "invalid", "server", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "unauthorized":
					w.WriteHeader(401)
					fmt.Fprint(w, "secret-forgejo-token")
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", (2<<20)+1))
				case "invalid":
					fmt.Fprint(w, "secret-forgejo-token")
				case "server":
					w.WriteHeader(500)
				case "redirect":
					w.Header().Set("Location", "https://other.invalid/secret-forgejo-token")
					w.WriteHeader(302)
				}
			})
			_, e := c.GetRun(context.Background(), cn, "o/r", 1)
			if e == nil || strings.Contains(e.Error(), "secret-forgejo-token") {
				t.Fatal(e)
			}
			_, e = c.Dispatch(context.Background(), cn, "o/r", "build.yml", "main", strings.Repeat("a", 40), "build-id")
			if e == nil || strings.Contains(e.Error(), "secret-forgejo-token") {
				t.Fatal(e)
			}
			if mode == "invalid" || mode == "oversize" || mode == "server" {
				if _, ok := e.(*Uncertain); !ok {
					t.Fatalf("expected uncertain: %T", e)
				}
			}
		})
	}
}
func TestDispatchTimeoutAndSecretContainment(t *testing.T) {
	c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(25 * time.Millisecond) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, e := c.Dispatch(ctx, cn, "o/r", "build.yml", "main", strings.Repeat("a", 40), "build-id"); e == nil {
		t.Fatal("timeout accepted")
	} else if _, ok := e.(*Uncertain); !ok {
		t.Fatal(e)
	}
	cn.TokenRef = "../token"
	if _, e := c.BranchHead(context.Background(), cn, "o/r", "main"); e == nil {
		t.Fatal("traversal accepted")
	}
}

func TestTokenResolvedAtUseAndNeverReflected(t *testing.T) {
	c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token rotated-token-value" {
			t.Error("token not resolved at use")
		}
		fmt.Fprint(w, `{"id":1,"html_url":"https://git/rotated-token-value"}`)
	})
	if e := os.WriteFile(filepath.Join(os.Getenv("RCP_SECRET_DIR"), "token"), []byte("rotated-token-value"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := c.GetRun(context.Background(), cn, "o/r", 1)
	if e == nil || strings.Contains(e.Error(), "rotated-token-value") {
		t.Fatal("reflected credential", e)
	}
}

func TestCommitExistenceBoundedContract(t *testing.T) {
	sha := strings.Repeat("b", 40)
	for _, mode := range []string{"exists", "unknown", "wrong", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/repos/o/r/git/commits/"+sha {
					t.Error(r.URL.Path)
				}
				switch mode {
				case "exists":
					fmt.Fprintf(w, `{"sha":%q}`, sha)
				case "unknown":
					w.WriteHeader(404)
				case "wrong":
					fmt.Fprintf(w, `{"sha":%q}`, strings.Repeat("a", 40))
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", (2<<20)+1))
				}
			})
			e := c.CommitExists(context.Background(), cn, "o/r", sha)
			if (mode == "exists") != (e == nil) {
				t.Fatal(e)
			}
		})
	}
}
