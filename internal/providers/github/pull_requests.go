package github

import (
	"context"
	"fmt"
	"releasecontrol/internal/delivery"
	"strings"
	"time"
)

func (p *Provider) ObservePullRequest(ctx context.Context, c delivery.Connection, repository string, number int) (delivery.PullRequestObservation, error) {
	var out delivery.PullRequestObservation
	if number <= 0 {
		return out, fmt.Errorf("positive pull request number required")
	}
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return out, err
	}
	var pr struct {
		Number    int       `json:"number"`
		URL       string    `json:"html_url"`
		Title     string    `json:"title"`
		State     string    `json:"state"`
		Draft     bool      `json:"draft"`
		Merged    bool      `json:"merged"`
		MergeSHA  string    `json:"merge_commit_sha"`
		CreatedAt time.Time `json:"created_at"`
		MergedAt  time.Time `json:"merged_at"`
		Head      struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err = p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		return out, err
	}
	if pr.Number != number || pr.Head.Ref == "" || pr.Base.Ref == "" || pr.Head.SHA == "" {
		return out, fmt.Errorf("incomplete or mismatched pull request identity")
	}
	state := strings.ToUpper(pr.State)
	if pr.Merged {
		state = "MERGED"
	} else {
		pr.MergeSHA = ""
	} // Open PR merge SHA is a test merge, not a completed merge.
	if state == "OPEN" && pr.Draft {
		state = "DRAFT"
	}
	if state != "DRAFT" && state != "OPEN" && state != "CLOSED" && state != "MERGED" {
		return out, fmt.Errorf("unknown pull request state")
	}
	return delivery.PullRequestObservation{Number: pr.Number, URL: pr.URL, Title: pr.Title, State: state, Head: pr.Head.Ref, Base: pr.Base.Ref, HeadSHA: pr.Head.SHA, MergeSHA: pr.MergeSHA, CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt}, nil
}
