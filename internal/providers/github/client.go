// Package github implements the deliberately narrow GitHub App delivery adapter.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"releasecontrol/internal/delivery"
)

var ErrUnauthorized = errors.New("provider authorization failed")
var ErrConflict = errors.New("provider expected state changed")

// ErrNotFound is kept as a provider-level alias for compatibility with
// callers that already use the GitHub adapter directly.
var ErrNotFound = delivery.ErrNotFound
var ErrUnsupported = errors.New("unsupported provider operation")

// APIError intentionally excludes response bodies, credentials, and signed URLs.
type APIError struct {
	Status    int
	Operation string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s returned HTTP %d", e.Operation, e.Status)
}
func (e *APIError) Is(target error) bool {
	return target == ErrUnauthorized && (e.Status == 401 || e.Status == 403) || target == ErrConflict && (e.Status == 409 || e.Status == 422) || target == ErrNotFound && e.Status == 404
}

type cachedToken struct {
	value   string
	expires time.Time
}
type Provider struct {
	client      *http.Client
	mu          sync.Mutex
	tokens      map[string]cachedToken
	now         func() time.Time
	registryURL string
}

var _ delivery.Provider = (*Provider)(nil)

func New() *Provider { return NewWithClient(nil) }
func NewWithClient(client *http.Client) *Provider {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Timeout = 30 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{client: &copy, tokens: map[string]cachedToken{}, now: time.Now, registryURL: "https://ghcr.io"}
}

func secret(ref string) ([]byte, error) {
	var b []byte
	switch {
	case strings.HasPrefix(ref, "env:"):
		key := strings.TrimPrefix(ref, "env:")
		if !regexp.MustCompile(`^RCP_[A-Za-z0-9_]+$`).MatchString(key) {
			return nil, errors.New("invalid environment secret reference")
		}
		b = []byte(os.Getenv(key))
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if !filepath.IsAbs(path) {
			return nil, errors.New("secret file reference must be absolute")
		}
		root := os.Getenv("RCP_SECRET_DIR")
		if root == "" {
			root = "/run/secrets"
		}
		resolved, e := filepath.EvalSymlinks(path)
		if e != nil {
			return nil, errors.New("secret file unavailable")
		}
		root, e = filepath.EvalSymlinks(root)
		if e != nil {
			return nil, errors.New("secret directory unavailable")
		}
		rel, e := filepath.Rel(root, resolved)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("secret file outside configured directory")
		}
		path = resolved
		f, e := os.Open(path)
		if e != nil {
			return nil, errors.New("cannot read secret file")
		}
		defer f.Close()
		b, e = io.ReadAll(io.LimitReader(f, 65537))
		if e != nil {
			return nil, errors.New("cannot read secret file")
		}
	default:
		return nil, errors.New("secret reference must use env: or file:")
	}
	if len(b) == 0 || len(b) > 65536 {
		return nil, errors.New("secret unavailable or oversized")
	}
	return b, nil
}
func apiBase(c delivery.Connection) (string, error) {
	s := c.APIURL
	if s == "" {
		s = "https://api.github.com"
	}
	u, e := url.Parse(s)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid GitHub API base URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return "", errors.New("GitHub API requires HTTPS")
	}
	allowed := u.Scheme == "https" && u.Host == "api.github.com" && (u.Path == "" || u.Path == "/")
	configured := strings.TrimRight(os.Getenv("RCP_GITHUB_API_URL"), "/")
	if configured != "" && strings.TrimRight(s, "/") == configured {
		allowed = true
	}
	for _, host := range strings.Split(os.Getenv("RCP_GITHUB_API_ALLOWED_HOSTS"), ",") {
		if strings.TrimSpace(host) != "" && strings.EqualFold(strings.TrimSpace(host), u.Host) && u.Scheme == "https" {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("GitHub API host is not operator-allowlisted")
	}
	return strings.TrimRight(s, "/"), nil
}
func jwt(c delivery.Connection, now time.Time) (string, error) {
	if c.AppID <= 0 || c.InstallationID <= 0 {
		return "", errors.New("positive App and installation IDs required")
	}
	raw, e := secret(c.PrivateKeyRef)
	if e != nil {
		return "", e
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("invalid App private key PEM")
	}
	var key *rsa.PrivateKey
	if v, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
		key = v
	} else {
		v, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return "", errors.New("invalid App RSA private key")
		}
		key, _ = v.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return "", errors.New("App key must be RSA 2048 bits or stronger")
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	b, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(c.AppID, 10)})
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(input))
	sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if e != nil {
		return "", errors.New("cannot sign App JWT")
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (p *Provider) do(ctx context.Context, method, target, auth string, input any) ([]byte, http.Header, int, error) {
	var body io.Reader
	if input != nil {
		b, e := json.Marshal(input)
		if e != nil {
			return nil, nil, 0, errors.New("invalid provider request")
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, target, body)
	if e != nil {
		return nil, nil, 0, errors.New("invalid provider URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "release-control-plane")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := p.client.Do(req)
	if e != nil {
		return nil, nil, 0, errors.New("provider request failed or timed out")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
	if e != nil || len(b) > 8<<20 {
		return nil, nil, res.StatusCode, errors.New("provider response unreadable or oversized")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return b, res.Header, res.StatusCode, &APIError{Status: res.StatusCode, Operation: method}
	}
	return b, res.Header, res.StatusCode, nil
}
func (p *Provider) token(ctx context.Context, c delivery.Connection) (string, error) {
	base, e := apiBase(c)
	if e != nil {
		return "", e
	}
	key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.tokens[key]; t.value != "" && t.expires.After(p.now().Add(time.Minute)) {
		return t.value, nil
	}
	signed, e := jwt(c, p.now())
	if e != nil {
		return "", e
	}
	b, _, _, e := p.do(ctx, http.MethodPost, base+"/app/installations/"+strconv.FormatInt(c.InstallationID, 10)+"/access_tokens", signed, map[string]any{})
	if e != nil {
		return "", e
	}
	var result struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if json.Unmarshal(b, &result) != nil || result.Token == "" || !result.Expires.After(p.now().Add(time.Minute)) {
		return "", errors.New("invalid installation token response")
	}
	p.tokens[key] = cachedToken{result.Token, result.Expires}
	return result.Token, nil
}
func (p *Provider) request(ctx context.Context, c delivery.Connection, method, path string, input, out any) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("invalid API request path")
	}
	base, e := apiBase(c)
	if e != nil {
		return e
	}
	token, e := p.token(ctx, c)
	if e != nil {
		return e
	}
	b, _, _, e := p.do(ctx, method, base+path, token, input)
	if e != nil {
		return e
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("invalid provider JSON")
	}
	return nil
}
func (p *Provider) TestConnection(ctx context.Context, c delivery.Connection) error {
	_, e := p.DiscoverRepositories(ctx, c)
	return e
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (p *Provider) repository(ctx context.Context, c delivery.Connection, repo string) (string, error) {
	if n, e := strconv.ParseInt(repo, 10, 64); e == nil && n > 0 {
		var out delivery.RepositoryInfo
		if e = p.request(ctx, c, "GET", "/repositories/"+repo, nil, &out); e != nil {
			return "", e
		}
		if out.ID != n {
			return "", errors.New("repository identity mismatch")
		}
		repo = out.FullName
	}
	if !repoName.MatchString(repo) {
		return "", errors.New("repository must be numeric ID or owner/name")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return "", errors.New("invalid repository name")
		}
	}
	if c.Owner != "" && !strings.EqualFold(strings.Split(repo, "/")[0], c.Owner) {
		return "", errors.New("repository outside connection owner")
	}
	return repo, nil
}
func (p *Provider) DiscoverRepositories(ctx context.Context, c delivery.Connection) ([]delivery.RepositoryInfo, error) {
	out := []delivery.RepositoryInfo{}
	for page := 1; page <= 1000; page++ {
		var response struct {
			Repositories []delivery.RepositoryInfo `json:"repositories"`
		}
		if e := p.request(ctx, c, "GET", fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &response); e != nil {
			return nil, e
		}
		for _, r := range response.Repositories {
			if r.ID <= 0 || !repoName.MatchString(r.FullName) {
				return nil, errors.New("invalid repository identity")
			}
			if c.Owner == "" || strings.EqualFold(strings.Split(r.FullName, "/")[0], c.Owner) {
				out = append(out, r)
			}
		}
		if len(response.Repositories) < 100 {
			return out, nil
		}
	}
	return nil, errors.New("repository pagination limit exceeded")
}
func (p *Provider) Branches(ctx context.Context, c delivery.Connection, repo string) ([]delivery.BranchInfo, error) {
	name, e := p.repository(ctx, c, repo)
	if e != nil {
		return nil, e
	}
	out := []delivery.BranchInfo{}
	for page := 1; page <= 1000; page++ {
		var list []struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if e = p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/branches?per_page=100&page=%d", name, page), nil, &list); e != nil {
			return nil, e
		}
		for _, v := range list {
			out = append(out, delivery.BranchInfo{Name: v.Name, SHA: v.Commit.SHA, Protected: v.Protected})
		}
		if len(list) < 100 {
			return out, nil
		}
	}
	return nil, errors.New("branch pagination limit exceeded")
}

var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (p *Provider) Compare(ctx context.Context, c delivery.Connection, repo, base, head string) (delivery.CompareResult, error) {
	out := delivery.CompareResult{Commits: []delivery.CommitInfo{}}
	if !fullSHA.MatchString(base) || !fullSHA.MatchString(head) {
		return out, errors.New("comparison requires pinned full SHAs")
	}
	name, e := p.repository(ctx, c, repo)
	if e != nil {
		return out, e
	}
	for page := 1; page <= 100; page++ {
		var result struct {
			Ahead  int    `json:"ahead_by"`
			Behind int    `json:"behind_by"`
			Status string `json:"status"`
			Base   struct {
				SHA string `json:"sha"`
			} `json:"base_commit"`
			Commits []struct {
				SHA    string `json:"sha"`
				URL    string `json:"html_url"`
				Commit struct {
					Message string `json:"message"`
					Author  struct {
						Name string    `json:"name"`
						Date time.Time `json:"date"`
					} `json:"author"`
				} `json:"commit"`
			} `json:"commits"`
		}
		if e = p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=100&page=%d", name, base, head, page), nil, &result); e != nil {
			return out, e
		}
		if result.Base.SHA != base {
			return out, errors.New("comparison base identity mismatch")
		}
		out.BaseSHA = base
		out.HeadSHA = head
		out.Ahead = result.Ahead
		out.Behind = result.Behind
		out.Status = result.Status
		for _, v := range result.Commits {
			out.Commits = append(out.Commits, delivery.CommitInfo{SHA: v.SHA, Message: v.Commit.Message, Author: v.Commit.Author.Name, AuthoredAt: v.Commit.Author.Date, URL: v.URL})
		}
		if len(result.Commits) < 100 {
			return out, nil
		}
	}
	return out, errors.New("comparison pagination limit exceeded")
}
