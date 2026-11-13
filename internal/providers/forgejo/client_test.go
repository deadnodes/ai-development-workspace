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
	payload := `{"ref":"main","inputs":{"sha":"` + sha + `","request_id":"build-id"},"repository":{"full_name":"o/r"},"sender":{"login":"bot"}}`
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
			fmt.Fprint(w, `{"id":42,"run_number":2,"jobs":["build"]}`)
		case strings.Contains(r.URL.Path, "branches"):
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, sha)
		case strings.HasSuffix(r.URL.Path, "/42"):
			fmt.Fprintf(w, `{"id":42,"status":"success","commit_sha":%q,"html_url":"https://git/run/42","created":"2026-11-12T10:00:00Z","event_payload":%q}`, sha, payload)
		default:
			if r.URL.Query().Get("head_sha") != sha || r.URL.Query().Get("workflow_id") != "build.yml" || r.URL.Query().Get("event") != "workflow_dispatch" {
				t.Error("filters")
			}
			fmt.Fprintf(w, `{"workflow_runs":[{"id":42,"event_payload":%q}],"total_count":1}`, payload)
		}
	})
	ctx := context.Background()
	if head, e := c.BranchHead(ctx, cn, "o/r", "topic"); e != nil || head != sha {
		t.Fatal(head, e)
	}
	if r, e := c.Dispatch(ctx, cn, "o/r", "build.yml", "topic", sha, "build-id"); e != nil || r.ID != 42 {
		t.Fatal(r, e)
	}
	if r, e := c.GetRun(ctx, cn, "o/r", 42); e != nil || r.Status != "success" || r.Created.IsZero() || r.EventPayload != payload {
		t.Fatal(r, e)
	}
	if p, e := c.ListRuns(ctx, cn, "o/r", "build.yml", Filter{SHA: sha, Event: "workflow_dispatch"}); e != nil || len(p.Runs) != 1 || p.Runs[0].EventPayload != payload {
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

func TestDispatchInputsBounded(t *testing.T) {
	sha := strings.Repeat("a", 40)
	payload := `{"ref":"refs/heads/main","inputs":{"sha":"` + sha + `","request_id":"request-1"},"repository":{"full_name":"o/r"},"sender":{"login":"bot"}}`
	for _, raw := range []string{"", `{`, `{}`, `{"inputs":null}`, `{"inputs":{"sha":42}}`, payload + strings.Repeat(" ", 256<<10)} {
		a, b := (Run{EventPayload: raw}).DispatchInputs()
		if a != "" || b != "" {
			t.Fatal("invalid payload matched")
		}
	}
	a, b := (Run{EventPayload: payload}).DispatchInputs()
	if a != sha || b != "request-1" {
		t.Fatal("dispatch inputs lost")
	}
}

func TestForgejoRunListingFiltersAndPageLimit(t *testing.T) {
	for _, status := range []string{"waiting", "running", ""} {
		t.Run(status, func(t *testing.T) {
			sha := ""
			if status == "" {
				sha = strings.Repeat("a", 40)
			}
			c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/api/v1/repos/o/r/actions/runs" || q.Get("workflow_id") != "build.yml" || q.Get("event") != "workflow_dispatch" || q.Get("status") != status || q.Get("head_sha") != sha || (sha == "" && q.Has("head_sha")) || q.Get("page") != "2" || q.Get("limit") != "50" {
					t.Error(r.URL.String())
				}
				fmt.Fprint(w, `{"workflow_runs":[],"total_count":0}`)
			})
			if _, err := c.ListRuns(context.Background(), cn, "o/r", "build.yml", Filter{SHA: sha, Status: status, Event: "workflow_dispatch", Page: 2, Limit: 100}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListRunsOmitsEmptyFilters(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name   string
		filter Filter
	}{
		{name: "unfiltered"},
		{name: "status", filter: Filter{Status: "waiting"}},
		{name: "event", filter: Filter{Event: "workflow_dispatch"}},
		{name: "sha", filter: Filter{SHA: sha}},
		{name: "all", filter: Filter{Status: "running", Event: "workflow_dispatch", SHA: sha}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cn := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Has("status") && q.Get("status") == "" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"message":"unknown status: "}`)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/o/r/actions/runs" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				for key, want := range map[string]string{"status": tc.filter.Status, "event": tc.filter.Event, "head_sha": tc.filter.SHA} {
					if q.Has(key) != (want != "") || q.Get(key) != want {
						t.Errorf("filter %s: got %v, want %q (present=%t)", key, q[key], want, want != "")
					}
				}
				if q.Get("workflow_id") != "build.yml" || q.Get("limit") != "50" || q.Get("page") != "1" {
					t.Errorf("unexpected listing parameters: %s", r.URL.RawQuery)
				}
				fmt.Fprint(w, `{"workflow_runs":[{"id":42,"status":"success"}],"total_count":1}`)
			})
			page, err := c.ListRuns(context.Background(), cn, "o/r", "build.yml", tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 || len(page.Runs) != 1 || page.Runs[0].ID != 42 {
				t.Fatalf("unexpected run page: %+v", page)
			}
		})
	}
}

func TestForgejo15EffectiveEvent(t *testing.T) {
	for _, tc := range []struct{ event, trigger, want string }{
		{"", "workflow_dispatch", "workflow_dispatch"},
		{"push", "workflow_dispatch", "push"},
		{"workflow_dispatch", "push", "workflow_dispatch"},
		{"", "", ""},
		{"", strings.Repeat("x", 65), ""},
	} {
		t.Run(tc.event+tc.trigger, func(t *testing.T) {
			payload := `{"inputs":{"sha":"` + strings.Repeat("a", 40) + `","request_id":"build-id"}}`
			c, cn := inMemoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				run := fmt.Sprintf(`{"id":42,"workflow_id":"build.yml","status":"running","event":%q,"trigger_event":%q,"event_payload":%q}`, tc.event, tc.trigger, payload)
				if strings.HasSuffix(r.URL.Path, "/42") {
					fmt.Fprint(w, run)
				} else {
					fmt.Fprintf(w, `{"workflow_runs":[%s],"total_count":1}`, run)
				}
			})
			run, err := c.GetRun(context.Background(), cn, "o/r", 42)
			page, listErr := c.ListRuns(context.Background(), cn, "o/r", "build.yml", Filter{Event: "workflow_dispatch"})
			if len(tc.trigger) > 64 {
				if err == nil || listErr == nil {
					t.Fatal("unbounded trigger accepted")
				}
				return
			}
			if err != nil || listErr != nil || run.EffectiveEvent() != tc.want || len(page.Runs) != 1 || page.Runs[0].EffectiveEvent() != tc.want {
				t.Fatal(run, page, err, listErr)
			}
			if sha, request := run.DispatchInputs(); sha != strings.Repeat("a", 40) || request != "build-id" {
				t.Fatal("inputs lost")
			}
		})
	}
}

// Exercise real request/decoding paths without requiring a listening socket.
type handlerTransport struct{ handler http.HandlerFunc }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, r)
	return recorder.Result(), nil
}
func inMemoryFixture(t *testing.T, handler http.HandlerFunc) (*Client, buildconfig.Connection) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("secret-forgejo-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return NewWithClient(&http.Client{Transport: handlerTransport{handler}}), buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}
}
