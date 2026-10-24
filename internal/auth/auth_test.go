package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
)

type testIssuer struct {
	server           *httptest.Server
	key              *rsa.PrivateKey
	nonce, challenge string
	claims           map[string]any
	mu               sync.Mutex
	pkceVerified     bool
}

func newIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f := &testIssuer{key: key}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			jsonResponse(w, 200, map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			jsonResponse(w, 200, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			_ = r.ParseForm()
			v := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if r.PostForm.Get("code") != "valid" || base64.RawURLEncoding.EncodeToString(v[:]) != f.challenge || r.PostForm.Get("redirect_uri") != "https://rcp.example/auth/callback" {
				jsonResponse(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			f.pkceVerified = true
			claims := map[string]any{"iss": f.server.URL, "sub": "private-subject-123", "aud": "rcp-test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": f.nonce, "realm_access": map[string]any{"roles": []string{"rcp-user"}}}
			for k, v := range f.claims {
				claims[k] = v
			}
			signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: "test"}}, nil)
			if e != nil {
				t.Error(e)
				return
			}
			payload, _ := json.Marshal(claims)
			obj, _ := signer.Sign(payload)
			signed, _ := obj.CompactSerialize()
			jsonResponse(w, 200, map[string]any{"access_token": "provider-secret-not-persisted", "token_type": "Bearer", "id_token": signed})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func configFor(t *testing.T, issuer string) Config {
	t.Helper()
	return Config{Mode: "oidc", PublicURL: "https://rcp.example", IssuerURL: issuer, ClientID: "rcp-test", RolesClaim: "realm_access.roles", AllowedRoles: []string{"rcp-user"}, AdminRoles: []string{"rcp-admin"}, Products: []string{"*"}, EncryptionKey: []byte("0123456789abcdef0123456789abcdef"), FilePath: filepath.Join(t.TempDir(), "auth.db"), SessionTTL: time.Hour}
}
func makeManager(t *testing.T, f *testIssuer, c Config) *Manager {
	t.Helper()
	m, e := New(oidc.ClientContext(context.Background(), f.server.Client()), c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func call(t *testing.T, m *Manager, f *testIssuer, method, path, body string, cookie *http.Cookie, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://rcp.example"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
		r.Header.Set("Origin", "https://rcp.example")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if f != nil {
		r = r.WithContext(oidc.ClientContext(r.Context(), f.server.Client()))
	}
	w := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		jsonResponse(w, 200, map[string]any{"principal": p, "authenticated": ok})
	})).ServeHTTP(w, r)
	return w
}
func parseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var b map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &b); e != nil {
		t.Fatalf("invalid JSON %s: %v", w.Body.String(), e)
	}
	return b
}
func startLogin(t *testing.T, m *Manager, f *testIssuer) (string, *http.Cookie) {
	t.Helper()
	w := call(t, m, f, "GET", "/auth/login?return_to=%2F", "", nil, "")
	if w.Code != 302 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	f.nonce = u.Query().Get("nonce")
	f.challenge = u.Query().Get("code_challenge")
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing S256 PKCE")
	}
	return u.Query().Get("state"), w.Result().Cookies()[0]
}
func loginHuman(t *testing.T, m *Manager, f *testIssuer) *http.Cookie {
	t.Helper()
	state, cookie := startLogin(t, m, f)
	w := call(t, m, f, "GET", "/auth/callback?code=valid&state="+state, "", cookie, "")
	if w.Code != 303 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatal("unsafe cookie")
			}
			return c
		}
	}
	t.Fatal("missing session")
	return nil
}
func TestOIDCChecksAndCookieSecurity(t *testing.T) {
	f := newIssuer(t)
	for _, tc := range []struct {
		name                    string
		claims                  map[string]any
		mutateState, mutatePKCE bool
		want                    int
	}{{"valid", nil, false, false, 303}, {"issuer", map[string]any{"iss": "https://attacker.example"}, false, false, 401}, {"audience", map[string]any{"aud": "other-client"}, false, false, 401}, {"nonce", map[string]any{"nonce": "wrong"}, false, false, 401}, {"expired", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, false, false, 401}, {"no-role", map[string]any{"realm_access": map[string]any{"roles": []string{"unrelated"}}}, false, false, 403}, {"state", nil, true, false, 400}, {"pkce", nil, false, true, 401}} {
		t.Run(tc.name, func(t *testing.T) {
			m := makeManager(t, f, configFor(t, f.server.URL))
			f.claims = tc.claims
			state, cookie := startLogin(t, m, f)
			if tc.mutateState {
				state = "wrong"
			}
			if tc.mutatePKCE {
				f.challenge = "wrong"
			}
			w := call(t, m, f, "GET", "/auth/callback?code=valid&state="+state, "", cookie, "")
			if w.Code != tc.want {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if tc.want == 303 && !f.pkceVerified {
				t.Fatal("PKCE not verified")
			}
			if tc.want == 303 {
				replay := call(t, m, f, "GET", "/auth/callback?code=valid&state="+state, "", cookie, "")
				if replay.Code != 400 {
					t.Fatal("callback replay accepted")
				}
			}
		})
	}
}
func startDevice(t *testing.T, m *Manager, f *testIssuer) map[string]any {
	t.Helper()
	w := call(t, m, f, "POST", "/auth/device/code", `{"name":"Test agent","scope":"read write deploy"}`, nil, "")
	if w.Code != 200 {
		t.Fatalf("device code %d %s", w.Code, w.Body.String())
	}
	return parseBody(t, w)
}
func tokenBody(d map[string]any) string {
	b, _ := json.Marshal(map[string]any{"grant_type": deviceGrant, "device_code": d["device_code"]})
	return string(b)
}
func approve(t *testing.T, m *Manager, f *testIssuer, cookie *http.Cookie, d map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"user_code": d["user_code"]})
	w := call(t, m, f, "POST", "/auth/device/approve", string(b), cookie, "")
	if w.Code != 200 {
		t.Fatalf("approve %d %s", w.Code, w.Body.String())
	}
}
func TestDeviceConcurrentRecoveryRevokeLogoutAndRestart(t *testing.T) {
	f := newIssuer(t)
	c := configFor(t, f.server.URL)
	m := makeManager(t, f, c)
	human := loginHuman(t, m, f)
	d := startDevice(t, m, f)
	approve(t, m, f, human, d)
	m.global = rate.NewLimiter(rate.Inf, 100)
	m.limits = map[string]*rate.Limiter{"127.0.0.1": rate.NewLimiter(rate.Inf, 100)}
	var wg sync.WaitGroup
	tokens := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call(t, m, f, "POST", "/auth/device/token", tokenBody(d), nil, "")
			if w.Code != 200 {
				t.Errorf("redeem %d %s", w.Code, w.Body.String())
				return
			}
			tokens <- parseBody(t, w)["access_token"].(string)
		}()
	}
	wg.Wait()
	close(tokens)
	var token string
	for v := range tokens {
		if token != "" && token != v {
			t.Fatal("recovery minted different token")
		}
		token = v
	}
	if token == "" {
		t.Fatal("no token")
	}
	w := call(t, m, f, "GET", "/auth/agents", "", human, "")
	list := parseBody(t, w)["agents"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d agents", len(list))
	}
	id := list[0].(map[string]any)["id"].(string)
	if w = call(t, m, f, "GET", "/api/state", "", nil, token); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = call(t, m, f, "POST", "/auth/logout", "{}", human, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = call(t, m, f, "GET", "/api/state", "", human, ""); w.Code != 401 {
		t.Fatal("logout session still works")
	}
	if w = call(t, m, f, "GET", "/api/state", "", nil, token); w.Code != 200 {
		t.Fatal("logout revoked independent agent")
	}
	if e := m.Close(); e != nil {
		t.Fatal(e)
	}
	disk, e := os.ReadFile(c.FilePath)
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{token, "private-subject-123", "Test agent", d["device_code"].(string), human.Value} {
		if strings.Contains(string(disk), secret) {
			t.Fatalf("plaintext persisted: %s", secret)
		}
	}
	m2 := makeManager(t, f, c)
	if w = call(t, m2, f, "GET", "/api/state", "", nil, token); w.Code != 200 {
		t.Fatal("agent did not survive restart")
	}
	human2 := loginHuman(t, m2, f)
	w = call(t, m2, f, "POST", "/auth/agents/revoke", `{"id":"`+id+`"}`, human2, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call(t, m2, f, "GET", "/api/state", "", nil, token); w.Code != 401 {
		t.Fatal("revoked token accepted")
	}
	if w = call(t, m2, f, "POST", "/auth/device/token", tokenBody(d), nil, ""); parseBody(t, w)["error"] != "access_denied" {
		t.Fatal("recovered revoked token")
	}
}
func TestDevicePendingSlowdownExpiryDenialAndCSRF(t *testing.T) {
	f := newIssuer(t)
	m := makeManager(t, f, configFor(t, f.server.URL))
	human := loginHuman(t, m, f)
	d := startDevice(t, m, f)
	for _, want := range []string{"authorization_pending", "slow_down"} {
		w := call(t, m, f, "POST", "/auth/device/token", tokenBody(d), nil, "")
		if parseBody(t, w)["error"] != want {
			t.Fatal(w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "https://rcp.example/auth/device/approve", strings.NewReader(`{"user_code":"`+d["user_code"].(string)+`"}`))
	r.AddCookie(human)
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin approval allowed")
	}
	w = call(t, m, f, "POST", "/auth/device/deny", `{"user_code":"`+d["user_code"].(string)+`"}`, human, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call(t, m, f, "POST", "/auth/device/token", tokenBody(d), nil, "")
	if parseBody(t, w)["error"] != "access_denied" {
		t.Fatal(w.Body.String())
	}
	_ = m.store.update(context.Background(), func(st *authState) error {
		v := st.Devices[hash(d["device_code"].(string))]
		v.Expires = time.Now().Add(-time.Second)
		st.Devices[hash(d["device_code"].(string))] = v
		return nil
	})
	w = call(t, m, f, "POST", "/auth/device/token", tokenBody(d), nil, "")
	if parseBody(t, w)["error"] != "expired_token" {
		t.Fatal(w.Body.String())
	}
}
func TestScopeAndServiceCannotApprove(t *testing.T) {
	f := newIssuer(t)
	c := configFor(t, f.server.URL)
	c.Products = []string{"product-a"}
	c.BootstrapToken = strings.Repeat("b", 40)
	m := makeManager(t, f, c)
	for _, body := range []string{`{"name":"bad","scope":"admin"}`, `{"name":"bad","products":["*"]}`} {
		w := call(t, m, f, "POST", "/auth/device/code", body, nil, "")
		if w.Code != 400 {
			t.Fatal("invalid scope accepted")
		}
	}
	w := call(t, m, f, "POST", "/auth/device/approve", `{"user_code":"ANY"}`, nil, c.BootstrapToken)
	if w.Code != 403 {
		t.Fatal("bootstrap can impersonate human")
	}
	w = call(t, m, f, "GET", "/auth/me", "", nil, c.BootstrapToken)
	p := parseBody(t, w)["principal"].(map[string]any)
	if p["kind"] != "service" {
		t.Fatal("wrong bootstrap kind")
	}
}
func TestConfigFailsClosedAndWrongKey(t *testing.T) {
	c := configFor(t, "https://issuer.example")
	for _, change := range []func(*Config){func(c *Config) { c.PublicURL = "http://rcp.example" }, func(c *Config) { c.EncryptionKey = nil }, func(c *Config) { c.AllowedRoles = nil; c.AdminRoles = nil }, func(c *Config) { c.FilePath = "" }, func(c *Config) { c.ClientID = "" }} {
		bad := c
		change(&bad)
		if bad.validate() == nil {
			t.Fatal("incomplete config accepted")
		}
	}
	s, e := openStore(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	_ = s.Close()
	c.EncryptionKey = []byte("different-secret-different-secret")
	if _, e = openStore(context.Background(), c); e == nil {
		t.Fatal("wrong encryption key accepted")
	}
	t.Setenv("RCP_AUTH_MODE", "")
	t.Setenv("RCP_PUBLIC_URL", "https://rcp.example")
	if _, e = ConfigFromEnv(); e == nil {
		t.Fatal("public URL silently enabled local mode")
	}
}
func TestUnauthenticatedBoundaryAndReturnURL(t *testing.T) {
	f := newIssuer(t)
	m := makeManager(t, f, configFor(t, f.server.URL))
	for _, path := range []string{"/api/state", "/api/commands", "/mcp", "/auth/me"} {
		w := call(t, m, f, "GET", path, "", nil, "")
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	w := call(t, m, f, "GET", "/auth/login?return_to=https://attacker.example", "", nil, "")
	u, _ := url.Parse(w.Header().Get("Location"))
	state := u.Query().Get("state")
	_ = m.store.update(context.Background(), func(st *authState) error {
		if st.Flows[hash(state)].ReturnTo != "/" {
			t.Fatal("external return accepted")
		}
		return nil
	})
}
func TestPrivatePostgresRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured; PostgreSQL not verified")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := "auth_test_" + strings.ToLower(hash(randomToken())[:16])
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("TEST_DATABASE_URL must use URL syntax")
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	c := configFor(t, "https://issuer.example")
	c.DatabaseURL = parsed.String()
	s, err := openStore(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	err = s.update(ctx, func(st *authState) error {
		st.Sessions[hash("postgres-session")] = session{Principal: Principal{ID: "test-human", Kind: "human"}, Expires: time.Now().Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.update(ctx, func(st *authState) error {
		if st.Sessions[hash("postgres-session")].Principal.ID != "test-human" {
			t.Fatal("session not persisted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err = s.pool.QueryRow(ctx, "SELECT payload FROM rcp_auth_private WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "test-human") || strings.Contains(string(payload), "postgres-session") {
		t.Fatal("plaintext PostgreSQL auth payload")
	}
}

func TestDevicePageReferrerPolicySupportsNativeApprovalWithoutWeakeningCSRF(t *testing.T) {
	f := newIssuer(t)
	m := makeManager(t, f, configFor(t, f.server.URL))
	human := loginHuman(t, m, f)
	device := startDevice(t, m, f)
	code := device["user_code"].(string)
	page := call(t, m, f, http.MethodGet, "/auth/device?user_code="+url.QueryEscape(code), "", human, "")
	if page.Code != http.StatusOK {
		t.Fatalf("device page: %d", page.Code)
	}
	if got := page.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("native form requires same-origin referrer policy, got %q", got)
	}
	csrf := m.mac("csrf", human.Value)
	if !strings.Contains(page.Body.String(), `name="csrf_token" value="`+csrf+`"`) {
		t.Fatal("device page missing session-bound CSRF form field")
	}
	form := url.Values{"user_code": {code}, "csrf_token": {csrf}}.Encode()
	for _, origin := range []string{"null", "https://attacker.example", "https://rcp.example"} {
		req := httptest.NewRequest(http.MethodPost, "https://rcp.example/auth/device/approve", strings.NewReader(form))
		req.AddCookie(human)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		m.Wrap(http.NotFoundHandler()).ServeHTTP(response, req)
		if origin != "https://rcp.example" {
			if response.Code != http.StatusForbidden {
				t.Fatalf("origin %q accepted despite valid CSRF: %d", origin, response.Code)
			}
		} else if response.Code != http.StatusOK {
			t.Fatalf("native same-origin form failed: %d %s", response.Code, response.Body.String())
		}
	}
}
