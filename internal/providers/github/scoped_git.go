package github

import (
	"context"
	"fmt"
	"net/url"
	"releasecontrol/internal/delivery"
	"strings"
)

func (p *Provider) ObserveBranch(ctx context.Context, c delivery.Connection, repository, branch string) (delivery.BranchInfo, error) {
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return delivery.BranchInfo{}, err
	}
	var v struct {
		Name      string
		Protected bool
		Commit    struct{ SHA string }
	}
	err = p.request(ctx, c, "GET", "/repos/"+repo+"/branches/"+url.PathEscape(branch), nil, &v)
	if err != nil {
		return delivery.BranchInfo{}, err
	}
	if v.Name != branch || v.Commit.SHA == "" {
		return delivery.BranchInfo{}, fmt.Errorf("incomplete branch observation")
	}
	return delivery.BranchInfo{Name: v.Name, SHA: v.Commit.SHA, Protected: v.Protected}, nil
}
func (p *Provider) DiscoverBranchPullRequests(ctx context.Context, c delivery.Connection, repository, branch string) ([]delivery.PullRequestObservation, error) {
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Number int
		Head   struct {
			Ref  string
			Repo struct {
				FullName string `json:"full_name"`
			}
		}
	}
	q := url.Values{"head": {strings.Split(repo, "/")[0] + ":" + branch}, "state": {"all"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"10"}}
	if err = p.request(ctx, c, "GET", "/repos/"+repo+"/pulls?"+q.Encode(), nil, &rows); err != nil {
		return nil, err
	}
	out := []delivery.PullRequestObservation{}
	for _, r := range rows {
		if r.Head.Ref != branch || !strings.EqualFold(r.Head.Repo.FullName, repo) {
			continue
		}
		v, e := p.ObservePullRequest(ctx, c, repo, r.Number)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
