package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"releasecontrol/internal/delivery"
)

var _ delivery.CheckRunProvider = (*Provider)(nil)
var checkSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
var checkExternal = regexp.MustCompile(`^rcp:[A-Za-z0-9_-]{1,128}$`)

func checkText(s string, max int, multiline bool) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}
func (p *Provider) checkIdentity(c delivery.Connection, v delivery.CheckRunIdentity) error {
	if c.AppID <= 0 || c.InstallationID <= 0 || v.RepositoryID <= 0 || v.AppID != c.AppID || !checkSHA.MatchString(v.HeadSHA) || !strings.HasPrefix(v.Name, "RCP /") || !checkText(v.Name, 128, false) || !checkExternal.MatchString(v.ExternalID) {
		return errors.New("invalid check identity")
	}
	// Numeric repository locators are resolved by the application from its
	// immutable registered binding, never through a broad installation token.
	if !repoName.MatchString(v.Repository) {
		return errors.New("check requires exact owner/repository")
	}
	_, err := p.repository(context.Background(), c, v.Repository)
	return err
}
func checkTimes(status, conclusion string, started, completed time.Time) bool {
	valid := func(v time.Time) bool {
		return !v.IsZero() && v.Year() >= 2000 && v.Year() <= 9999 && v.Nanosecond() == 0
	}
	switch status {
	case "queued":
		return conclusion == "" && started.IsZero() && completed.IsZero()
	case "in_progress":
		return conclusion == "" && valid(started) && completed.IsZero()
	case "completed":
		return (conclusion == "success" || conclusion == "failure" || conclusion == "cancelled" || conclusion == "timed_out") && valid(started) && valid(completed) && !completed.Before(started)
	}
	return false
}
func checkOutput(v delivery.CheckRunOutput) bool {
	return checkText(v.Title, 256, false) && checkText(v.Summary, 4096, true)
}

// The token request explicitly narrows both the repository and permission. Its
// cache namespace cannot collide with the adapter's ordinary installation token.
func (p *Provider) checkToken(ctx context.Context, c delivery.Connection, identity delivery.CheckRunIdentity) (string, error) {
	repo := identity.Repository
	base, err := apiBase(c)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal([]any{base, c.ID, c.AppID, c.InstallationID, c.PrivateKeyRef, strings.ToLower(repo), identity.RepositoryID, "checks:write"})
	key := "checks:" + string(raw)
	p.mu.Lock()
	defer p.mu.Unlock()
	if token := p.tokens[key]; token.value != "" && token.expires.After(p.now().Add(time.Minute)) {
		return token.value, nil
	}
	signed, err := jwt(c, p.now())
	if err != nil {
		return "", err
	}
	payload := map[string]any{"repositories": []string{strings.Split(repo, "/")[1]}, "permissions": map[string]string{"checks": "write"}}
	b, _, status, err := p.do(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", base, c.InstallationID), signed, payload)
	if err != nil {
		return "", err
	}
	var response struct {
		Token        string            `json:"token"`
		Expires      time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if status != 201 || json.Unmarshal(b, &response) != nil || response.Token == "" || len(response.Token) > 65536 || !response.Expires.After(p.now().Add(time.Minute)) || response.Expires.After(p.now().Add(65*time.Minute)) || response.Permissions["checks"] != "write" {
		return "", errors.New("invalid scoped checks token response")
	}
	for permission, value := range response.Permissions {
		if permission != "checks" && !(permission == "metadata" && value == "read") {
			return "", errors.New("unexpected scoped checks token permission")
		}
	}
	if len(response.Repositories) != 1 || response.Repositories[0].ID != identity.RepositoryID || !strings.EqualFold(response.Repositories[0].FullName, repo) {
		return "", errors.New("scoped checks token repository mismatch")
	}
	p.tokens[key] = cachedToken{value: response.Token, expires: response.Expires}
	return response.Token, nil
}
func (p *Provider) checkRequest(ctx context.Context, c delivery.Connection, v delivery.CheckRunIdentity, method, path string, input any, expected int) ([]byte, error) {
	if err := p.checkIdentity(c, v); err != nil {
		return nil, err
	}
	base, err := apiBase(c)
	if err != nil {
		return nil, err
	}
	token, err := p.checkToken(ctx, c, v)
	if err != nil {
		return nil, err
	}
	b, _, status, err := p.do(ctx, method, base+"/repos/"+v.Repository+path, token, input)
	if err != nil {
		return nil, err
	}
	if status != expected {
		return nil, errors.New("unexpected checks HTTP status")
	}
	return b, nil
}

type checkResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Output      struct {
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Text        string `json:"text"`
		Annotations int    `json:"annotations_count"`
	} `json:"output"`
	DetailsURL string            `json:"details_url"`
	Actions    []json.RawMessage `json:"actions"`
}

func parseCheck(b []byte, identity delivery.CheckRunIdentity) (delivery.CheckRunObservation, error) {
	var v checkResponse
	if json.Unmarshal(b, &v) != nil {
		return delivery.CheckRunObservation{}, errors.New("invalid check JSON")
	}
	actual := delivery.CheckRunIdentity{Repository: identity.Repository, RepositoryID: identity.RepositoryID, HeadSHA: v.HeadSHA, Name: v.Name, ExternalID: v.ExternalID, AppID: v.App.ID}
	output := delivery.CheckRunOutput{Title: v.Output.Title, Summary: v.Output.Summary}
	// GitHub fills an omitted details_url from the App homepage. It is neither
	// followed nor retained in our narrow observation/authority model.
	if v.ID <= 0 || actual != identity || !checkTimes(v.Status, v.Conclusion, v.StartedAt, v.CompletedAt) || !checkOutput(output) || v.Output.Text != "" || v.Output.Annotations != 0 || len(v.Actions) != 0 {
		return delivery.CheckRunObservation{}, errors.New("check identity or content mismatch")
	}
	return delivery.CheckRunObservation{Found: true, ID: v.ID, Identity: actual, Status: v.Status, Conclusion: v.Conclusion, StartedAt: v.StartedAt, CompletedAt: v.CompletedAt, Output: output}, nil
}
func (p *Provider) ReadCheckRun(ctx context.Context, c delivery.Connection, v delivery.CheckRunIdentity, id int64) (delivery.CheckRunObservation, error) {
	if id <= 0 {
		return delivery.CheckRunObservation{}, errors.New("positive check ID required")
	}
	b, err := p.checkRequest(ctx, c, v, "GET", fmt.Sprintf("/check-runs/%d", id), nil, 200)
	if err != nil {
		return delivery.CheckRunObservation{}, err
	}
	result, err := parseCheck(b, v)
	if err == nil && result.ID != id {
		return delivery.CheckRunObservation{}, errors.New("adopted check ID mismatch")
	}
	return result, err
}
func (p *Provider) FindCheckRun(ctx context.Context, c delivery.Connection, v delivery.CheckRunIdentity) (delivery.CheckRunObservation, error) {
	var result delivery.CheckRunObservation
	total, seen := -1, 0
	for page := 1; page <= 10; page++ {
		query := url.Values{"check_name": {v.Name}, "app_id": {fmt.Sprint(v.AppID)}, "filter": {"all"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		b, err := p.checkRequest(ctx, c, v, "GET", "/commits/"+v.HeadSHA+"/check-runs?"+query.Encode(), nil, 200)
		if err != nil {
			return delivery.CheckRunObservation{}, err
		}
		var response struct {
			Total int               `json:"total_count"`
			Runs  []json.RawMessage `json:"check_runs"`
		}
		if json.Unmarshal(b, &response) != nil || response.Total < len(response.Runs) || response.Total > 1000 || len(response.Runs) > 100 || response.Runs == nil || (total >= 0 && response.Total != total) || seen+len(response.Runs) > response.Total {
			return delivery.CheckRunObservation{}, errors.New("invalid checks page")
		}
		total = response.Total
		seen += len(response.Runs)
		for _, raw := range response.Runs {
			var header struct {
				ExternalID string `json:"external_id"`
			}
			if json.Unmarshal(raw, &header) != nil {
				return delivery.CheckRunObservation{}, errors.New("invalid check identity JSON")
			}
			if header.ExternalID != v.ExternalID {
				continue
			}
			observed, err := parseCheck(raw, v)
			if err != nil {
				return delivery.CheckRunObservation{}, err
			}
			if result.Found {
				return delivery.CheckRunObservation{}, errors.New("duplicate exact check identity")
			}
			result = observed
		}
		if seen == total {
			return result, nil
		}
		if len(response.Runs) < 100 {
			return delivery.CheckRunObservation{}, errors.New("truncated checks pagination")
		}
	}
	return delivery.CheckRunObservation{}, errors.New("checks pagination bound exceeded")
}
func (p *Provider) writeCheck(ctx context.Context, c delivery.Connection, w delivery.CheckRunWrite, create bool) (delivery.CheckRunObservation, error) {
	if err := p.checkIdentity(c, w.Identity); err != nil {
		return delivery.CheckRunObservation{}, err
	}
	if !checkOutput(w.Output) || !checkTimes(w.Status, w.Conclusion, w.StartedAt, w.CompletedAt) || (create && (w.ID != 0 || w.Status != "queued")) || (!create && (w.ID <= 0 || w.Status == "queued")) {
		return delivery.CheckRunObservation{}, errors.New("invalid check write")
	}
	payload := map[string]any{"name": w.Identity.Name, "external_id": w.Identity.ExternalID, "status": w.Status, "output": w.Output}
	method, path, expected := "POST", "/check-runs", 201
	if create {
		payload["head_sha"] = w.Identity.HeadSHA
	} else {
		method = "PATCH"
		path = fmt.Sprintf("/check-runs/%d", w.ID)
		expected = 200
	}
	if !w.StartedAt.IsZero() {
		payload["started_at"] = w.StartedAt.UTC().Format(time.RFC3339)
	}
	if !w.CompletedAt.IsZero() {
		payload["completed_at"] = w.CompletedAt.UTC().Format(time.RFC3339)
		payload["conclusion"] = w.Conclusion
	}
	b, err := p.checkRequest(ctx, c, w.Identity, method, path, payload, expected)
	if err != nil {
		return delivery.CheckRunObservation{}, err
	}
	result, err := parseCheck(b, w.Identity)
	if err != nil {
		return result, err
	}
	if (!create && result.ID != w.ID) || result.Status != w.Status || result.Conclusion != w.Conclusion || !result.StartedAt.Equal(w.StartedAt) || !result.CompletedAt.Equal(w.CompletedAt) || result.Output != w.Output {
		return delivery.CheckRunObservation{}, errors.New("check write response differs from frozen payload")
	}
	return result, nil
}
func (p *Provider) CreateCheckRun(ctx context.Context, c delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	return p.writeCheck(ctx, c, w, true)
}
func (p *Provider) UpdateCheckRun(ctx context.Context, c delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	return p.writeCheck(ctx, c, w, false)
}
