package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

const sessionCookie = "__Host-rcp_session"
const flowCookie = "__Host-rcp_login"

type Manager struct {
	config   Config
	store    *privateStore
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	limitsMu sync.Mutex
	limits   map[string]*rate.Limiter
	global   *rate.Limiter
}

func New(ctx context.Context, c Config) (*Manager, error) {
	if c.Mode == "" {
		if c.PublicURL != "" || c.IssuerURL != "" || c.ClientID != "" {
			return nil, errors.New("OIDC mode must be explicit")
		}
		c.Mode = "local"
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 8 * time.Hour
	}
	if c.RolesClaim == "" {
		c.RolesClaim = "realm_access.roles"
	}
	if len(c.Products) == 0 {
		c.Products = []string{"*"}
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if e := c.validate(); e != nil {
		return nil, e
	}
	m := &Manager{config: c, limits: map[string]*rate.Limiter{}, global: rate.NewLimiter(20, 100)}
	if !m.Enabled() {
		return m, nil
	}
	provider, e := oidc.NewProvider(ctx, c.IssuerURL)
	if e != nil {
		return nil, e
	}
	m.verifier = provider.Verifier(&oidc.Config{ClientID: c.ClientID})
	m.oauth = oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: c.PublicURL + "/auth/callback", Scopes: []string{oidc.ScopeOpenID}}
	m.store, e = openStore(ctx, c)
	if e != nil {
		return nil, e
	}
	return m, nil
}
func (m *Manager) Enabled() bool { return m.config.Mode == "oidc" }
func (m *Manager) Close() error {
	if m.store != nil {
		return m.store.Close()
	}
	return nil
}
func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (m *Manager) mac(label, v string) string {
	h := hmac.New(sha256.New, m.config.EncryptionKey)
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, code string) {
	jsonResponse(w, status, map[string]string{"error": code, "auth_mode": "oidc"})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if e := dec.Decode(v); e != nil {
		failure(w, 400, "invalid_request")
		return false
	}
	if e := dec.Decode(&struct{}{}); e != io.EOF {
		failure(w, 400, "invalid_request")
		return false
	}
	return true
}
func (m *Manager) limited(w http.ResponseWriter, r *http.Request) bool {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	m.limitsMu.Lock()
	lim := m.limits[host]
	if lim == nil {
		if len(m.limits) >= 4096 {
			m.limits = map[string]*rate.Limiter{}
		}
		lim = rate.NewLimiter(rate.Every(time.Second), 20)
		m.limits[host] = lim
	}
	ok := m.global.Allow() && lim.Allow()
	m.limitsMu.Unlock()
	if !ok {
		w.Header().Set("Retry-After", "5")
		failure(w, 429, "slow_down")
	}
	return !ok
}
func protected(path string) bool {
	return path == "/mcp" || strings.HasPrefix(path, "/mcp/") || path == "/api" || strings.HasPrefix(path, "/api/")
}
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Enabled() {
			if r.URL.Path == "/auth/me" {
				jsonResponse(w, 200, map[string]any{"auth_mode": "local", "principal": nil})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if protected(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-store")
			// Keep device codes off external referrers while allowing browsers to
			// send a same-origin Origin on native approval form submissions.
			w.Header().Set("Referrer-Policy", "same-origin")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		}
		if r.URL.Path == "/auth/login" || r.URL.Path == "/auth/callback" || r.URL.Path == "/auth/device/code" || r.URL.Path == "/auth/device/token" {
			if m.limited(w, r) {
				return
			}
			switch r.URL.Path {
			case "/auth/login":
				m.login(w, r)
			case "/auth/callback":
				m.callback(w, r)
			case "/auth/device/code":
				m.deviceCode(w, r)
			case "/auth/device/token":
				m.deviceToken(w, r)
			}
			return
		}
		p, raw, e := m.authenticate(r)
		if e != nil {
			failure(w, 503, "authentication_unavailable")
			return
		}
		if p.ID == "" {
			if r.URL.Path == "/auth/device" && r.Method == http.MethodGet {
				http.Redirect(w, r, "/auth/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			if protected(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/auth/") {
				failure(w, 401, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(WithPrincipal(r.Context(), p))
		if p.Kind == "human" && r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if !m.csrfOK(r, raw) {
				failure(w, 403, "csrf_failed")
				return
			}
		}
		switch r.URL.Path {
		case "/auth/me":
			if !method(w, r, "GET") {
				return
			}
			body := map[string]any{"auth_mode": "oidc", "principal": p}
			if raw != "" {
				body["csrf_token"] = m.mac("csrf", raw)
			}
			jsonResponse(w, 200, body)
		case "/auth/logout":
			m.logout(w, r, raw)
		case "/auth/device":
			m.devicePage(w, r, p, raw)
		case "/auth/device/approve", "/auth/device/deny":
			m.deviceApprove(w, r, p)
		case "/auth/agents":
			m.listAgents(w, r, p)
		case "/auth/agents/revoke":
			m.revokeAgent(w, r, p)
		default:
			if strings.HasPrefix(r.URL.Path, "/auth/") {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	failure(w, 405, "method_not_allowed")
	return false
}
func (m *Manager) authenticate(r *http.Request) (Principal, string, error) {
	var p Principal
	if header := r.Header.Get("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return p, "", nil
		}
		token := parts[1]
		if m.config.BootstrapToken != "" && equal(hash(token), hash(m.config.BootstrapToken)) {
			return Principal{ID: "service/bootstrap", Kind: "service", Role: "admin", Products: m.config.Products, Permissions: []string{"read", "write", "deploy", "admin"}}, "", nil
		}
		if !strings.HasPrefix(token, "rcpa_") || len(token) > 128 {
			return p, "", nil
		}
		e := m.store.update(r.Context(), func(st *authState) error {
			for _, a := range st.Agents {
				if a.Info.RevokedAt == nil && equal(a.Verifier, hash(token)) {
					p = Principal{ID: a.Info.ID, Kind: "agent", Role: "member", Products: a.Info.Products, Permissions: a.Info.Permissions}
					break
				}
			}
			return nil
		})
		return p, "", e
	}
	cookie, e := r.Cookie(sessionCookie)
	if e != nil || len(cookie.Value) > 128 {
		return p, "", nil
	}
	e = m.store.update(r.Context(), func(st *authState) error {
		if s, ok := st.Sessions[hash(cookie.Value)]; ok && time.Now().Before(s.Expires) {
			p = s.Principal
		}
		return nil
	})
	if p.ID == "" {
		return p, "", e
	}
	return p, cookie.Value, e
}
func (m *Manager) csrfOK(r *http.Request, raw string) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		return equal(origin, m.config.PublicURL)
	}
	provided := r.Header.Get("X-CSRF-Token")
	if provided == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		r.Body = http.MaxBytesReader(nil, r.Body, 8192)
		if r.ParseForm() != nil {
			return false
		}
		provided = r.PostForm.Get("csrf_token")
	}
	return raw != "" && equal(provided, m.mac("csrf", raw))
}
func setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
}
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
func (m *Manager) login(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	returnTo := r.URL.Query().Get("return_to")
	u, e := url.Parse(returnTo)
	if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") || strings.ContainsAny(returnTo, "\\\r\n") {
		returnTo = "/"
	}
	if strings.HasPrefix(returnTo, "/auth/") && !strings.HasPrefix(returnTo, "/auth/device?") {
		returnTo = "/"
	}
	e = m.store.update(r.Context(), func(st *authState) error {
		if len(st.Flows) >= 4096 {
			return errors.New("login capacity")
		}
		st.Flows[hash(state)] = loginFlow{Nonce: nonce, Verifier: verifier, ReturnTo: returnTo, Expires: time.Now().Add(10 * time.Minute)}
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	setCookie(w, flowCookie, state, 10*time.Minute)
	http.Redirect(w, r, m.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}
func (m *Manager) callback(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	state := r.URL.Query().Get("state")
	cookie, e := r.Cookie(flowCookie)
	if e != nil || state == "" || len(state) > 128 || !equal(cookie.Value, state) {
		failure(w, 400, "invalid_state")
		return
	}
	clearCookie(w, flowCookie)
	var flow loginFlow
	e = m.store.update(r.Context(), func(st *authState) error { flow = st.Flows[hash(state)]; delete(st.Flows, hash(state)); return nil })
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	if flow.Nonce == "" || !time.Now().Before(flow.Expires) {
		failure(w, 400, "invalid_state")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, e := m.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if e != nil {
		failure(w, 401, "invalid_code")
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		failure(w, 401, "invalid_identity")
		return
	}
	id, e := m.verifier.Verify(ctx, rawID)
	if e != nil || id.Subject == "" || !equal(id.Nonce, flow.Nonce) {
		failure(w, 401, "invalid_identity")
		return
	}
	var claims map[string]any
	if id.Claims(&claims) != nil {
		failure(w, 401, "invalid_identity")
		return
	}
	roles := claimStrings(claims, m.config.RolesClaim)
	admin := intersects(roles, m.config.AdminRoles)
	if !admin && !intersects(roles, m.config.AllowedRoles) {
		failure(w, 403, "role_required")
		return
	}
	role := "member"
	permissions := []string{"read", "write", "deploy"}
	if admin {
		role = "admin"
		permissions = append(permissions, "admin")
	}
	p := Principal{ID: "human/" + m.mac("subject", id.Issuer+"\x00"+id.Subject), Kind: "human", Role: role, Products: m.config.Products, Permissions: permissions}
	sessionToken := randomToken()
	e = m.store.update(ctx, func(st *authState) error {
		if len(st.Sessions) >= 4096 {
			return errors.New("session capacity")
		}
		st.Sessions[hash(sessionToken)] = session{Principal: p, Expires: time.Now().Add(m.config.SessionTTL)}
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	setCookie(w, sessionCookie, sessionToken, m.config.SessionTTL)
	http.Redirect(w, r, flow.ReturnTo, http.StatusSeeOther)
}
func claimStrings(claims map[string]any, path string) []string {
	var v any = claims
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	values, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
func (m *Manager) logout(w http.ResponseWriter, r *http.Request, raw string) {
	if !method(w, r, "POST") {
		return
	}
	if raw == "" {
		failure(w, 403, "human_session_required")
		return
	}
	if e := m.store.update(r.Context(), func(st *authState) error { delete(st.Sessions, hash(raw)); return nil }); e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	clearCookie(w, sessionCookie)
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
