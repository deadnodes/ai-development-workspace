package forgejo

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"releasecontrol/internal/buildconfig"
)

type Commit struct {
	SHA         string
	Title       string
	Author      string
	CommittedAt time.Time
}

// CommitRange lists commits reachable from head and not base, newest first.
// An empty base deliberately returns only the head commit.
func (c *Client) CommitRange(ctx context.Context, cn buildconfig.Connection, repo, base, head string, limit int) ([]Commit, bool, error) {
	if !buildconfig.SHA.MatchString(head) || base != "" && !buildconfig.SHA.MatchString(base) {
		return nil, false, errors.New("invalid commit range SHA")
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	if base == "" {
		limit = 1
	}
	query := url.Values{"sha": {head}, "limit": {strconv.Itoa(limit)}, "stat": {"false"}, "verification": {"false"}, "files": {"false"}}
	if base != "" {
		query.Set("not", base)
	}
	var raw []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := c.request(ctx, cn, "GET", repoPath(repo)+"/commits?"+query.Encode(), nil, &raw); err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, false, errors.New("invalid commits array")
	}
	out := make([]Commit, 0, len(raw))
	for _, v := range raw {
		if !buildconfig.SHA.MatchString(v.SHA) {
			return nil, false, errors.New("invalid commit SHA")
		}
		title := strings.SplitN(v.Commit.Message, "\n", 2)[0]
		author := v.Commit.Author.Name
		if v.Author != nil && v.Author.Login != "" {
			author = v.Author.Login
		}
		out = append(out, Commit{v.SHA, title, author, v.Commit.Author.Date})
	}
	return out, base != "" && len(raw) == limit, nil
}
