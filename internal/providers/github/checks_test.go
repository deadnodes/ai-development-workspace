package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func checkIdentityFixture() delivery.CheckRunIdentity {
	return delivery.CheckRunIdentity{Repository: "acme/app", RepositoryID: 101, HeadSHA: strings.Repeat("a", 40), Name: "RCP / checks and build", ExternalID: "rcp:" + strings.Repeat("b", 32), AppID: 11}
}
func checkWriteFixture() delivery.CheckRunWrite {
	return delivery.CheckRunWrite{Identity: checkIdentityFixture(), Status: "queued", Output: delivery.CheckRunOutput{Title: "RCP checks and build", Summary: "Exact source checks/build only; not a deployment."}}
}
func checkWire(w delivery.CheckRunWrite) map[string]any {
	id := w.ID
	if id == 0 {
		id = 41
	}
	var started, completed any
	if !w.StartedAt.IsZero() {
		started = w.StartedAt
	}
	if !w.CompletedAt.IsZero() {
		completed = w.CompletedAt
	}
	return map[string]any{"id": id, "app": map[string]any{"id": w.Identity.AppID}, "name": w.Identity.Name, "head_sha": w.Identity.HeadSHA, "external_id": w.Identity.ExternalID, "status": w.Status, "conclusion": w.Conclusion, "started_at": started, "completed_at": completed, "output": w.Output}
}

type checkHTTPFixture struct {
	Provider                  *Provider
	Connection                delivery.Connection
	Requests, Tokens, Generic atomic.Int32
	TokenResponse             func(map[string]any)
	Handler                   http.HandlerFunc
	TokenStatus               int
}

func newCheckHTTPFixture(t *testing.T) *checkHTTPFixture {
	t.Helper()
	f := &checkHTTPFixture{}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("RCP_CHECK_FIXTURE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Requests.Add(1)
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Error("changed GitHub API version")
		}
		if r.URL.Path == "/app/installations/22/access_tokens" {
			var payload struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("invalid token request")
			}
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				t.Error("missing App JWT")
				w.WriteHeader(401)
				return
			}
			signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature) != nil {
				t.Error("invalid App JWT")
			}
			if len(payload.Repositories) == 0 && len(payload.Permissions) == 0 {
				f.Generic.Add(1)
				jsonOut(w, map[string]any{"token": "generic-fixture", "expires_at": time.Now().Add(time.Hour)})
				return
			}
			f.Tokens.Add(1)
			if len(payload.Repositories) != 1 || payload.Repositories[0] != "app" || len(payload.Permissions) != 1 || payload.Permissions["checks"] != "write" {
				t.Error("token request widened repository/permission")
			}
			if f.TokenStatus != 0 {
				w.WriteHeader(f.TokenStatus)
				fmt.Fprint(w, "token-response-secret-sentinel")
				return
			}
			response := map[string]any{"token": "checks-scoped-fixture", "expires_at": time.Now().Add(time.Hour), "permissions": map[string]string{"checks": "write", "metadata": "read"}, "repositories": []map[string]any{{"id": 101, "full_name": "acme/app"}}}
			if f.TokenResponse != nil {
				f.TokenResponse(response)
			}
			w.WriteHeader(201)
			jsonOut(w, response)
			return
		}
		if r.Header.Get("Authorization") != "Bearer checks-scoped-fixture" {
			t.Error("checks API used non-scoped token")
		}
		if f.Handler == nil {
			t.Error("unexpected check HTTP")
			w.WriteHeader(500)
			return
		}
		f.Handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("RCP_GITHUB_API_URL", server.URL)
	f.Provider = New()
	f.Connection = delivery.Connection{ID: "conn", AppID: 11, InstallationID: 22, PrivateKeyRef: "env:RCP_CHECK_FIXTURE_KEY", APIURL: server.URL, Owner: "acme"}
	return f
}

func TestCheckRunLifecycleAndIsolatedNarrowToken(t *testing.T) {
	f := newCheckHTTPFixture(t)
	ctx := context.Background()
	desired := checkWriteFixture()
	f.Handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" || r.Method == "PATCH" {
			var payload map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Fatal("invalid check write")
			}
			for _, forbidden := range []string{"actions", "annotations", "details_url", "rerequest", "text"} {
				if payload[forbidden] != nil {
					t.Error("unsupported check write field")
				}
			}
			if r.Method == "POST" {
				w.WriteHeader(201)
			}
			jsonOut(w, checkWire(desired))
			return
		}
		jsonOut(w, checkWire(desired))
	}
	if _, e := f.Provider.token(ctx, f.Connection); e != nil {
		t.Fatal(e)
	}
	observed, e := f.Provider.CreateCheckRun(ctx, f.Connection, desired)
	if e != nil || !observed.Found || observed.ID != 41 {
		t.Fatalf("queued:%+v %v", observed, e)
	}
	desired.ID = 41
	desired.Status = "in_progress"
	desired.StartedAt = time.Now().UTC().Truncate(time.Second)
	if _, e = f.Provider.UpdateCheckRun(ctx, f.Connection, desired); e != nil {
		t.Fatal(e)
	}
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out"} {
		desired.Status = "completed"
		desired.Conclusion = conclusion
		desired.CompletedAt = desired.StartedAt.Add(time.Second)
		if _, e = f.Provider.UpdateCheckRun(ctx, f.Connection, desired); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = f.Provider.ReadCheckRun(ctx, f.Connection, desired.Identity, 41); e != nil {
		t.Fatal(e)
	}
	if f.Generic.Load() != 1 || f.Tokens.Load() != 1 {
		t.Fatal("generic/scoped cache collision or repeated token request")
	}
}

func TestCheckRunInvalidInputMakesNoHTTP(t *testing.T) {
	f := newCheckHTTPFixture(t)
	cases := []struct {
		name   string
		change func(*delivery.CheckRunWrite)
	}{
		{"app", func(w *delivery.CheckRunWrite) { w.Identity.AppID++ }}, {"physical-repo", func(w *delivery.CheckRunWrite) { w.Identity.RepositoryID = 0 }},
		{"numeric", func(w *delivery.CheckRunWrite) { w.Identity.Repository = "101" }}, {"owner", func(w *delivery.CheckRunWrite) { w.Identity.Repository = "other/app" }},
		{"sha", func(w *delivery.CheckRunWrite) { w.Identity.HeadSHA = "main" }}, {"name", func(w *delivery.CheckRunWrite) { w.Identity.Name = "CI" }}, {"control", func(w *delivery.CheckRunWrite) { w.Identity.Name = "RCP / x\n" }},
		{"external", func(w *delivery.CheckRunWrite) { w.Identity.ExternalID = "caller arbitrary" }}, {"title", func(w *delivery.CheckRunWrite) { w.Output.Title = strings.Repeat("a", 257) }},
		{"summary", func(w *delivery.CheckRunWrite) { w.Output.Summary = strings.Repeat("a", 4097) }}, {"status", func(w *delivery.CheckRunWrite) { w.Status = "completed" }},
		{"create-id", func(w *delivery.CheckRunWrite) { w.ID = 2 }}, {"create-time", func(w *delivery.CheckRunWrite) { w.StartedAt = time.Now() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := checkWriteFixture()
			tc.change(&w)
			if _, e := f.Provider.CreateCheckRun(context.Background(), f.Connection, w); e == nil {
				t.Fatal("accepted invalid check")
			}
		})
	}
	w := checkWriteFixture()
	w.ID = 41
	w.Status = "in_progress"
	w.StartedAt = time.Now().UTC().Truncate(time.Second).Add(time.Nanosecond)
	if _, e := f.Provider.UpdateCheckRun(context.Background(), f.Connection, w); e == nil {
		t.Fatal("subsecond mutable timestamp accepted")
	}
	if f.Requests.Load() != 0 {
		t.Fatal("invalid input performed HTTP")
	}
}

func TestCheckRunTokenCachePinsPhysicalRepositoryAndErrorsDoNotWiden(t *testing.T) {
	f := newCheckHTTPFixture(t)
	f.Handler = func(w http.ResponseWriter, r *http.Request) { jsonOut(w, checkWire(checkWriteFixture())) }
	identity := checkIdentityFixture()
	if _, err := f.Provider.ReadCheckRun(context.Background(), f.Connection, identity, 41); err != nil {
		t.Fatal(err)
	}
	identity.RepositoryID = 102
	if _, err := f.Provider.ReadCheckRun(context.Background(), f.Connection, identity, 41); err == nil {
		t.Fatal("name reuse consumed cached token for different numeric repository")
	}
	if f.Tokens.Load() != 2 || f.Requests.Load() != 3 {
		t.Fatal("physical token scope was not independently validated")
	}
	for _, status := range []int{401, 403, 422, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			f.TokenStatus = status
			_, err := f.Provider.CreateCheckRun(context.Background(), f.Connection, checkWriteFixture())
			if err == nil || strings.Contains(err.Error(), "token-response-secret-sentinel") || f.Requests.Load() != 1 || f.Generic.Load() != 0 {
				t.Fatalf("scoped token error caused disclosure/permission fallback:%v", err)
			}
		})
	}
}

func TestCheckRunTokenResponseRejectsWideningAndWrongPhysicalRepository(t *testing.T) {
	for _, name := range []string{"missing-permission", "broad-permission", "wrong-id", "wrong-name", "multiple", "missing-repository", "expired", "unbounded-lifetime"} {
		t.Run(name, func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			f.TokenResponse = func(v map[string]any) {
				switch name {
				case "missing-permission":
					v["permissions"] = map[string]string{}
				case "broad-permission":
					v["permissions"] = map[string]string{"checks": "write", "contents": "write"}
				case "wrong-id":
					v["repositories"] = []map[string]any{{"id": 102, "full_name": "acme/app"}}
				case "wrong-name":
					v["repositories"] = []map[string]any{{"id": 101, "full_name": "acme/reused"}}
				case "multiple":
					v["repositories"] = []map[string]any{{"id": 101, "full_name": "acme/app"}, {"id": 102, "full_name": "acme/other"}}
				case "missing-repository":
					delete(v, "repositories")
				case "expired":
					v["expires_at"] = time.Now()
				case "unbounded-lifetime":
					v["expires_at"] = time.Now().Add(24 * time.Hour)
				}
			}
			if _, e := f.Provider.CreateCheckRun(context.Background(), f.Connection, checkWriteFixture()); e == nil {
				t.Fatal("invalid scoped token accepted")
			}
			if f.Requests.Load() != 1 {
				t.Fatal("invalid token reached check API")
			}
		})
	}
}

func TestCheckRunFindPaginationAndStrictIdentity(t *testing.T) {
	for _, mode := range []string{"found", "absent", "duplicate", "wrong-app", "wrong-sha", "wrong-name", "budget", "oversized-page", "short-hidden-duplicate", "changed-total", "truncated-second-page"} {
		t.Run(mode, func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			f.Handler = func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				v := checkIdentityFixture()
				if r.Method != "GET" || r.URL.Path != "/repos/acme/app/commits/"+v.HeadSHA+"/check-runs" || q.Get("filter") != "all" || q.Get("check_name") != v.Name || q.Get("app_id") != "11" || q.Get("per_page") != "100" {
					t.Error("wrong exact checks listing")
				}
				runs := []any{}
				if q.Get("page") == "1" || mode == "budget" {
					for range 100 {
						runs = append(runs, map[string]any{"external_id": "unrelated", "app": map[string]int{"id": 999}})
					}
				} else if mode != "absent" {
					raw := checkWire(checkWriteFixture())
					switch mode {
					case "wrong-app":
						raw["app"] = map[string]int{"id": 99}
					case "wrong-sha":
						raw["head_sha"] = strings.Repeat("c", 40)
					case "wrong-name":
						raw["name"] = "RCP / another"
					}
					runs = append(runs, raw)
					if mode == "duplicate" {
						runs = append(runs, raw)
					}
				}
				if mode == "oversized-page" {
					runs = append(runs, map[string]any{"external_id": "other"})
				}
				total := 101
				if mode == "absent" {
					total = 100
				}
				if mode == "duplicate" {
					total = 102
				}
				if mode == "budget" {
					total = 1001
				}
				if mode == "short-hidden-duplicate" {
					runs = []any{checkWire(checkWriteFixture())}
					total = 2
				}
				if mode == "changed-total" && q.Get("page") == "2" {
					total = 102
				}
				if mode == "truncated-second-page" {
					total = 103
				}
				jsonOut(w, map[string]any{"total_count": total, "check_runs": runs})
			}
			found, e := f.Provider.FindCheckRun(context.Background(), f.Connection, checkIdentityFixture())
			switch mode {
			case "found":
				if e != nil || !found.Found || found.ID != 41 {
					t.Fatalf("find:%+v %v", found, e)
				}
			case "absent":
				if e != nil || found.Found {
					t.Fatal("missing check recreated/adopted")
				}
			default:
				if e == nil {
					t.Fatal("ambiguous/invalid listing accepted")
				}
			}
		})
	}
}

func TestCheckRunReadAndWriteRejectMalformedOrChangedObservation(t *testing.T) {
	for _, mode := range []string{"id", "app", "sha", "name", "external", "conclusion", "time", "output", "annotations", "text", "malformed", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			f.Handler = func(w http.ResponseWriter, r *http.Request) {
				if mode == "malformed" {
					fmt.Fprint(w, "{broken")
					return
				}
				if mode == "oversize" {
					fmt.Fprint(w, strings.Repeat("x", (8<<20)+1))
					return
				}
				raw := checkWire(checkWriteFixture())
				switch mode {
				case "id":
					raw["id"] = 42
				case "app":
					raw["app"] = map[string]int{"id": 12}
				case "sha":
					raw["head_sha"] = strings.Repeat("d", 40)
				case "name":
					raw["name"] = "RCP / other"
				case "external":
					raw["external_id"] = "rcp:other"
				case "conclusion":
					raw["conclusion"] = "success"
				case "time":
					raw["started_at"] = "invalid"
				case "output":
					raw["output"] = map[string]string{"title": "x", "summary": strings.Repeat("a", 4097)}
				case "details":
					raw["details_url"] = "https://arbitrary.invalid"
				case "annotations":
					raw["output"] = map[string]any{"title": "x", "summary": "safe", "annotations_count": 1}
				case "text":
					raw["output"] = map[string]string{"title": "x", "summary": "safe", "text": "raw logs"}
				}
				jsonOut(w, raw)
			}
			if _, e := f.Provider.ReadCheckRun(context.Background(), f.Connection, checkIdentityFixture(), 41); e == nil {
				t.Fatal("changed adopted check accepted")
			}
		})
	}
	// Same valid identity but different output must not acknowledge a frozen write.
	f := newCheckHTTPFixture(t)
	f.Handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		v := checkWriteFixture()
		v.Output.Summary = "different safe content"
		jsonOut(w, checkWire(v))
	}
	if _, e := f.Provider.CreateCheckRun(context.Background(), f.Connection, checkWriteFixture()); e == nil {
		t.Fatal("write response acknowledged changed output")
	}
}

func TestCheckRunIgnoresDefaultAppHomepageWithoutFollowingOrPersisting(t *testing.T) {
	for _, link := range []string{"https://app-homepage.example.invalid", "http://127.0.0.1:1/private?token=untrusted-link-sentinel"} {
		t.Run(link, func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			f.Handler = func(w http.ResponseWriter, r *http.Request) {
				raw := checkWire(checkWriteFixture())
				raw["details_url"] = link
				jsonOut(w, raw)
			}
			observed, err := f.Provider.ReadCheckRun(context.Background(), f.Connection, checkIdentityFixture(), 41)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(observed)
			if strings.Contains(string(b), link) || f.Requests.Load() != 2 {
				t.Fatal("provider details URL became persisted authority or was followed")
			}
		})
	}
}

func TestCheckRunHTTPFailuresAndRedirectNeverLeakCredentials(t *testing.T) {
	for _, status := range []int{301, 401, 403, 404, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var leaked atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
			defer target.Close()
			f := newCheckHTTPFixture(t)
			f.Handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, "secret-body-sentinel")
			}
			_, e := f.Provider.ReadCheckRun(context.Background(), f.Connection, checkIdentityFixture(), 41)
			if e == nil || strings.Contains(e.Error(), "secret-body-sentinel") || strings.Contains(e.Error(), "checks-scoped-fixture") || leaked.Load() != 0 {
				t.Fatalf("error disclosure/redirect:%v", e)
			}
		})
	}
}

func TestCheckRunTimeoutAndLostWriteReplyAreUncertainWithoutRetry(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			f := newCheckHTTPFixture(t)
			var writes atomic.Int32
			f.Handler = func(w http.ResponseWriter, r *http.Request) {
				writes.Add(1)
				if lost {
					c, _, _ := w.(http.Hijacker).Hijack()
					c.Close()
					return
				}
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, e := f.Provider.CreateCheckRun(ctx, f.Connection, checkWriteFixture()); e == nil {
				t.Fatal("uncertain write acknowledged")
			}
			if writes.Load() != 1 {
				t.Fatal("write replayed or not attempted")
			}
		})
	}
}
