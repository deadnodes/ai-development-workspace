package auth

import (
	"crypto/rand"
	"encoding/base32"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

func (m *Manager) deviceCode(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var req struct {
		Name     string   `json:"name"`
		Scope    string   `json:"scope"`
		Products []string `json:"products"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		failure(w, 400, "invalid_request")
		return
	}
	scopes := strings.Fields(req.Scope)
	if len(scopes) == 0 {
		scopes = []string{"read", "write"}
	}
	scopes = unique(scopes)
	for _, v := range scopes {
		if v != "read" && v != "write" && v != "deploy" {
			failure(w, 400, "invalid_scope")
			return
		}
	}
	if len(req.Products) == 0 {
		req.Products = m.config.Products
	}
	req.Products = unique(req.Products)
	if !subset(req.Products, m.config.Products) {
		failure(w, 400, "invalid_scope")
		return
	}
	raw := randomToken()
	userBytes := make([]byte, 7)
	if _, e := rand.Read(userBytes); e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	user := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(userBytes)
	user = user[:5] + "-" + user[5:]
	d := device{Name: req.Name, UserHash: m.mac("user-code", normalizeCode(user)), Status: "pending", Products: req.Products, Permissions: scopes, Expires: time.Now().Add(10 * time.Minute), Interval: 5}
	full := false
	e := m.store.update(r.Context(), func(st *authState) error {
		if len(st.Devices) >= 4096 {
			full = true
			return nil
		}
		st.Devices[hash(raw)] = d
		return nil
	})
	if e != nil || full {
		failure(w, 503, "authentication_unavailable")
		return
	}
	uri := m.config.PublicURL + "/auth/device"
	jsonResponse(w, 200, map[string]any{"device_code": raw, "user_code": user, "verification_uri": uri, "verification_uri_complete": uri + "?user_code=" + url.QueryEscape(user), "expires_in": 600, "interval": 5})
}
func unique(v []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range v {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
func subset(want, allowed []string) bool {
	if len(want) == 0 {
		return false
	}
	if intersects(allowed, []string{"*"}) {
		return true
	}
	for _, v := range want {
		if !intersects(allowed, []string{v}) {
			return false
		}
	}
	return true
}
func normalizeCode(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}
func (m *Manager) deviceToken(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var req struct {
		GrantType  string `json:"grant_type"`
		DeviceCode string `json:"device_code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.GrantType != deviceGrant {
		failure(w, 400, "unsupported_grant_type")
		return
	}
	if len(req.DeviceCode) != 43 {
		failure(w, 400, "invalid_grant")
		return
	}
	token := "rcpa_" + m.mac("agent-token", req.DeviceCode)
	code := ""
	var scopes []string
	e := m.store.update(r.Context(), func(st *authState) error {
		key := hash(req.DeviceCode)
		d, ok := st.Devices[key]
		now := time.Now()
		if !ok {
			code = "invalid_grant"
			return nil
		}
		if !now.Before(d.Expires) {
			code = "expired_token"
			return nil
		}
		if d.Status == "denied" {
			code = "access_denied"
			return nil
		}
		if d.Status == "issued" {
			a, ok := st.Agents[d.AgentID]
			if !ok || a.Info.RevokedAt != nil {
				code = "access_denied"
				return nil
			}
			scopes = d.Permissions
			return nil
		}
		if !d.LastPoll.IsZero() && now.Sub(d.LastPoll) < time.Duration(d.Interval)*time.Second {
			d.Interval += 5
			if d.Interval > 60 {
				d.Interval = 60
			}
			d.LastPoll = now
			st.Devices[key] = d
			code = "slow_down"
			return nil
		}
		d.LastPoll = now
		if d.Status != "approved" {
			st.Devices[key] = d
			code = "authorization_pending"
			return nil
		}
		id := "agent/" + randomToken()
		a := agent{ID: id, Name: d.Name, Products: d.Products, Permissions: d.Permissions, CreatedAt: now}
		st.Agents[id] = agentRecord{Info: a, Owner: d.Owner, Verifier: hash(token)}
		d.AgentID = id
		d.Status = "issued"
		st.Devices[key] = d
		scopes = d.Permissions
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	if code != "" {
		failure(w, 400, code)
		return
	}
	jsonResponse(w, 200, map[string]any{"access_token": token, "token_type": "Bearer", "scope": strings.Join(scopes, " ")})
}

var approvalPage = template.Must(template.New("device").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize agent · Release Control Plane</title><style>body{font:17px system-ui;max-width:680px;margin:12vh auto;padding:24px;line-height:1.6}button,input{font:inherit;padding:10px}button{cursor:pointer}code{overflow-wrap:anywhere}</style><h1>Authorize an agent</h1>{{if .Device}}<p>Only approve a code displayed by an agent you started.</p><dl><dt>Code</dt><dd><code>{{.Code}}</code></dd><dt>Agent name (supplied by agent)</dt><dd>{{.Device.Name}}</dd><dt>Permissions</dt><dd>{{range .Device.Permissions}}<code>{{.}}</code> {{end}}</dd><dt>Products</dt><dd>{{range .Device.Products}}<code>{{.}}</code> {{end}}</dd></dl><p>The agent receives its own permanent credential. Signing out does not revoke it. Revoke it from your agent list when it is no longer needed.</p><form method="post" action="/auth/device/approve"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><input type="hidden" name="user_code" value="{{.Code}}"><button type="submit">Approve this agent</button><button type="submit" formaction="/auth/device/deny">Deny</button></form>{{else}}<form method="get" action="/auth/device"><label>Agent verification code <input name="user_code" required maxlength="24" autocomplete="off"></label><button>Continue</button></form>{{end}}</html>`))

func human(w http.ResponseWriter, p Principal) bool {
	if p.Kind != "human" {
		failure(w, 403, "human_session_required")
		return false
	}
	return true
}
func (m *Manager) devicePage(w http.ResponseWriter, r *http.Request, p Principal, raw string) {
	if !method(w, r, "GET") || !human(w, p) || m.limited(w, r) {
		return
	}
	code := r.URL.Query().Get("user_code")
	var d *device
	if code != "" {
		if len(code) > 24 {
			failure(w, 400, "invalid_user_code")
			return
		}
		e := m.store.update(r.Context(), func(st *authState) error {
			for _, v := range st.Devices {
				if equal(v.UserHash, m.mac("user-code", normalizeCode(code))) && time.Now().Before(v.Expires) && v.Status == "pending" {
					copy := v
					d = &copy
					break
				}
			}
			return nil
		})
		if e != nil {
			failure(w, 503, "authentication_unavailable")
			return
		}
		if d == nil {
			failure(w, 400, "invalid_user_code")
			return
		}
		if !subset(d.Products, p.Products) || !subset(d.Permissions, p.Permissions) {
			failure(w, 403, "invalid_scope")
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = approvalPage.Execute(w, struct {
		Device     *device
		Code, CSRF string
	}{d, code, m.mac("csrf", raw)})
}
func (m *Manager) deviceApprove(w http.ResponseWriter, r *http.Request, p Principal) {
	if !method(w, r, "POST") || !human(w, p) || m.limited(w, r) {
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if r.ParseForm() != nil {
			failure(w, 400, "invalid_request")
			return
		}
		req.UserCode = r.PostForm.Get("user_code")
	} else if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.UserCode) > 24 || req.UserCode == "" {
		failure(w, 400, "invalid_user_code")
		return
	}
	code := "invalid_user_code"
	e := m.store.update(r.Context(), func(st *authState) error {
		for k, d := range st.Devices {
			if !equal(d.UserHash, m.mac("user-code", normalizeCode(req.UserCode))) {
				continue
			}
			if !time.Now().Before(d.Expires) {
				code = "expired_token"
				return nil
			}
			d.Attempts++
			if d.Status != "pending" || d.Attempts > 10 {
				st.Devices[k] = d
				code = "invalid_grant"
				return nil
			}
			if !subset(d.Products, p.Products) || !subset(d.Permissions, p.Permissions) {
				st.Devices[k] = d
				code = "invalid_scope"
				return nil
			}
			d.Status = "approved"
			if r.URL.Path == "/auth/device/deny" {
				d.Status = "denied"
			}
			d.Owner = p.ID
			st.Devices[k] = d
			code = ""
			break
		}
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	if code != "" {
		failure(w, 400, code)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>Agent authorization</title><p>Decision saved. Return to your agent.</p>"))
		return
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
func (m *Manager) listAgents(w http.ResponseWriter, r *http.Request, p Principal) {
	if !method(w, r, "GET") || !human(w, p) {
		return
	}
	out := []agent{}
	e := m.store.update(r.Context(), func(st *authState) error {
		for _, a := range st.Agents {
			if a.Owner == p.ID || p.Role == "admin" {
				out = append(out, a.Info)
			}
		}
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	jsonResponse(w, 200, map[string]any{"agents": out})
}
func (m *Manager) revokeAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	if !method(w, r, "POST") || !human(w, p) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	found := false
	e := m.store.update(r.Context(), func(st *authState) error {
		a, ok := st.Agents[req.ID]
		if !ok || (a.Owner != p.ID && p.Role != "admin") {
			return nil
		}
		found = true
		if a.Info.RevokedAt == nil {
			now := time.Now()
			a.Info.RevokedAt = &now
			st.Agents[req.ID] = a
		}
		return nil
	})
	if e != nil {
		failure(w, 503, "authentication_unavailable")
		return
	}
	if !found {
		failure(w, 404, "agent_not_found")
		return
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
