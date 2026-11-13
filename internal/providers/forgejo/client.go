// Package forgejo implements bounded source observation and Actions builds.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strconv"
	"strings"
	"time"
)

type Run struct {
	EventPayload string    `json:"event_payload"`
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	SHA          string    `json:"commit_sha"`
	URL          string    `json:"html_url"`
	Event        string    `json:"event"`
	TriggerEvent string    `json:"trigger_event"`
	Branch       string    `json:"prettyref"`
	Workflow     string    `json:"workflow_id"`
	Created      time.Time `json:"created"`
	Started      time.Time `json:"started"`
	Stopped      time.Time `json:"stopped"`
}
type Filter struct {
	SHA, Status, Event string
	Page, Limit        int
}
type Page struct {
	Runs  []Run `json:"workflow_runs"`
	Total int   `json:"total_count"`
}
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("Forgejo returned HTTP %d", e.Status) }

// Uncertain means a dispatch may have reached Actions; never automatically retry it.
type Uncertain struct{}

func (*Uncertain) Error() string { return "Forgejo dispatch outcome unknown" }

type Client struct{ http *http.Client }

func New() *Client { return NewWithClient(http.DefaultClient) }
func NewWithClient(h *http.Client) *Client {
	v := *h
	v.Timeout = 30 * time.Second
	v.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{&v}
}
func token(ref string) (string, error) {
	bad := errors.New("Forgejo credential unavailable")
	if filepath.Base(ref) != ref || ref == "." || ref == ".." {
		return "", bad
	}
	root := os.Getenv("RCP_SECRET_DIR")
	if root == "" {
		root = "/run/secrets"
	}
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", bad
	}
	p, e := filepath.EvalSymlinks(filepath.Join(root, ref))
	if e != nil {
		return "", bad
	}
	rel, e := filepath.Rel(root, p)
	if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", bad
	}
	f, e := os.Open(p)
	if e != nil {
		return "", bad
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	s := strings.TrimSpace(string(b))
	if e != nil || len(b) > 65536 || s == "" || strings.ContainsAny(s, "\r\n") {
		return "", bad
	}
	return s, nil
}
func (c *Client) request(ctx context.Context, cn buildconfig.Connection, method, path string, body any, out any) error {
	t, e := token(cn.TokenRef)
	if e != nil {
		return e
	}
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(cn.APIURL, "/")+"/api/v1"+path, bytes.NewReader(b))
	if e != nil {
		return errors.New("invalid Forgejo request")
	}
	req.Header.Set("Authorization", "token "+t)
	req.Header.Set("Content-Type", "application/json")
	res, e := c.http.Do(req)
	if e != nil {
		if method == "POST" {
			return &Uncertain{}
		}
		return errors.New("Forgejo request failed")
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if method == "POST" && (e != nil || len(raw) > 2<<20 || (res.StatusCode >= 500 || res.StatusCode == 408 || res.StatusCode >= 300 && res.StatusCode < 400)) {
		return &Uncertain{}
	}
	if e != nil || len(raw) > 2<<20 {
		return errors.New("Forgejo response exceeds bounds")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &APIError{res.StatusCode}
	}
	if bytes.Contains(raw, []byte(t)) {
		if method == "POST" {
			return &Uncertain{}
		}
		return errors.New("invalid Forgejo response")
	}
	if json.Unmarshal(raw, out) != nil {
		if method == "POST" {
			return &Uncertain{}
		}
		return errors.New("invalid Forgejo response")
	}
	return nil
}
func repoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/repos/" + strings.Join(parts, "/")
}
func (c *Client) BranchHead(ctx context.Context, cn buildconfig.Connection, repo, branch string) (string, error) {
	var v struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	e := c.request(ctx, cn, "GET", repoPath(repo)+"/branches/"+url.PathEscape(branch), nil, &v)
	if e == nil && !buildconfig.SHA.MatchString(v.Commit.ID) {
		e = errors.New("invalid Forgejo branch SHA")
	}
	return v.Commit.ID, e
}
func (c *Client) CommitExists(ctx context.Context, cn buildconfig.Connection, repo, sha string) error {
	var v struct {
		SHA string `json:"sha"`
	}
	if e := c.request(ctx, cn, "GET", repoPath(repo)+"/git/commits/"+url.PathEscape(sha), nil, &v); e != nil {
		return e
	}
	if v.SHA != sha {
		return errors.New("invalid Forgejo commit")
	}
	return nil
}
func (c *Client) Dispatch(ctx context.Context, cn buildconfig.Connection, repo, workflow, ref, sha, requestID string) (Run, error) {
	var info struct {
		ID int64 `json:"id"`
	}
	e := c.request(ctx, cn, "POST", repoPath(repo)+"/actions/workflows/"+url.PathEscape(workflow)+"/dispatches", map[string]any{"ref": ref, "inputs": map[string]string{"sha": sha, "request_id": requestID}, "return_run_info": true}, &info)
	if e == nil && info.ID <= 0 {
		e = &Uncertain{}
	}
	return Run{ID: info.ID}, e
}
func (c *Client) GetRun(ctx context.Context, cn buildconfig.Connection, repo string, id int64) (Run, error) {
	var r Run
	e := c.request(ctx, cn, "GET", repoPath(repo)+"/actions/runs/"+strconv.FormatInt(id, 10), nil, &r)
	if e == nil && (r.ID != id || !validRun(r)) {
		e = errors.New("invalid Forgejo run")
	}
	return r, e
}
func (c *Client) ListRuns(ctx context.Context, cn buildconfig.Connection, repo, workflow string, f Filter) (Page, error) {
	if f.Limit < 1 || f.Limit > 50 {
		f.Limit = 50
	}
	if f.Page < 1 {
		f.Page = 1
	}
	q := url.Values{"workflow_id": {workflow}, "limit": {strconv.Itoa(f.Limit)}, "page": {strconv.Itoa(f.Page)}}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Event != "" {
		q.Set("event", f.Event)
	}
	if f.SHA != "" {
		q.Set("head_sha", f.SHA)
	}
	var p Page
	e := c.request(ctx, cn, "GET", repoPath(repo)+"/actions/runs?"+q.Encode(), nil, &p)
	if e == nil {
		if p.Total < 0 || len(p.Runs) > f.Limit {
			return Page{}, errors.New("invalid Forgejo run page")
		}
		for _, r := range p.Runs {
			if r.ID <= 0 || !validRun(r) {
				return Page{}, errors.New("invalid Forgejo run")
			}
		}
	}
	return p, e
}

func validRun(r Run) bool {
	return len(r.URL) <= 2048 && len(r.Branch) <= 270 && len(r.Status) <= 64 && len(r.Event) <= 64 && len(r.TriggerEvent) <= 64 && len(r.Workflow) <= 255 && (r.SHA == "" || buildconfig.SHA.MatchString(r.SHA))
}

// DispatchInputs parses only bounded event metadata. Payloads must never be logged.
func (r Run) DispatchInputs() (sha, requestID string) {
	if len(r.EventPayload) > 256<<10 {
		return "", ""
	}
	var event struct {
		Inputs struct {
			SHA       string `json:"sha"`
			RequestID string `json:"request_id"`
		} `json:"inputs"`
	}
	if json.Unmarshal([]byte(r.EventPayload), &event) != nil {
		return "", ""
	}
	return event.Inputs.SHA, event.Inputs.RequestID
}

// EffectiveEvent preserves the explicit event, falling back for Forgejo 15 runs.
func (r Run) EffectiveEvent() string {
	if r.Event != "" {
		return r.Event
	}
	return r.TriggerEvent
}
