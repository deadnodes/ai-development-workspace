package forgejo

import (
	"context"
	"errors"
	"net/url"
	"releasecontrol/internal/buildconfig"
	"strconv"
)

// ErrStatusPageLimit means complete CI evidence could not be observed within bounds.
var ErrStatusPageLimit = errors.New("Forgejo status unavailable: pagination exceeds 20 pages")

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
	const limit, maxPages = 50, 20
	for page := 1; ; page++ {
		var part CommitStatus
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
		if err := c.request(ctx, cn, "GET", repoPath(repo)+"/commits/"+url.PathEscape(sha)+"/status?"+q.Encode(), nil, &part); err != nil {
			return CommitStatus{}, err
		}
		if part.SHA != "" && part.SHA != sha || len(part.Statuses) > limit {
			return CommitStatus{}, errors.New("incomplete status evidence")
		}
		if page == 1 {
			out = part
		} else {
			out.Statuses = append(out.Statuses, part.Statuses...)
		}
		// The combined endpoint contains the latest status per context, not history.
		if len(out.Statuses) >= out.TotalCount {
			break
		}
		if len(part.Statuses) == 0 {
			return CommitStatus{}, errors.New("incomplete status evidence")
		}
		if page == maxPages {
			return CommitStatus{}, ErrStatusPageLimit
		}
	}
	for _, s := range out.Statuses {
		if len(s.Context) > 256 || len(s.Status) > 32 {
			return CommitStatus{}, errors.New("invalid status evidence")
		}
	}
	return out, nil
}
