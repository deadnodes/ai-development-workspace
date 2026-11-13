package forgejo

import (
	"context"
	"errors"
	"net/url"
	"releasecontrol/internal/buildconfig"
)

type CommitStatus struct {
	State      string          `json:"state"`
	Statuses   []StatusContext `json:"statuses"`
	TotalCount int             `json:"total_count"`
	SHA        string          `json:"sha"`
}
type StatusContext struct {
	Context string `json:"context"`
	Status  string `json:"status"`
}

func (c *Client) CommitStatus(ctx context.Context, cn buildconfig.Connection, repo, sha string) (CommitStatus, error) {
	var out CommitStatus
	if !buildconfig.SHA.MatchString(sha) {
		return out, errors.New("invalid status SHA")
	}
	err := c.request(ctx, cn, "GET", repoPath(repo)+"/commits/"+url.PathEscape(sha)+"/status", nil, &out)
	if err == nil && (out.SHA != "" && out.SHA != sha || len(out.Statuses) > 100 || out.TotalCount > len(out.Statuses)) {
		return CommitStatus{}, errors.New("incomplete status evidence")
	}
	for _, s := range out.Statuses {
		if len(s.Context) > 256 || len(s.Status) > 32 {
			return CommitStatus{}, errors.New("invalid status evidence")
		}
	}
	return out, err
}
