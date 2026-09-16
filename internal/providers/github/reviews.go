package github

import (
	"context"
	"fmt"
	"releasecontrol/internal/delivery"
	"time"
)

func (p *Provider) PullRequestReview(ctx context.Context, c delivery.Connection, repository string, number int) (delivery.PullRequestReview, error) {
	if number <= 0 {
		return delivery.PullRequestReview{}, fmt.Errorf("positive pull request number required")
	}
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return delivery.PullRequestReview{}, err
	}
	root := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
		State  string `json:"state"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err = p.request(ctx, c, "GET", root, nil, &pr); err != nil {
		return delivery.PullRequestReview{}, err
	}
	if pr.Number != number {
		return delivery.PullRequestReview{}, fmt.Errorf("pull request identity mismatch")
	}
	out := delivery.PullRequestReview{Number: number, URL: pr.URL, Head: pr.Head.Ref, Base: pr.Base.Ref, HeadSHA: pr.Head.SHA, State: pr.State, Comments: []delivery.ReviewComment{}}
	for _, stream := range []struct{ kind, path string }{{"inline", root + "/comments"}, {"issue", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)}, {"review", root + "/reviews"}} {
		for page := 1; page <= 10; page++ {
			var rows []struct {
				ID          int64     `json:"id"`
				Body        string    `json:"body"`
				URL         string    `json:"html_url"`
				Path        string    `json:"path"`
				Line        int       `json:"line"`
				Commit      string    `json:"commit_id"`
				Reply       int64     `json:"in_reply_to_id"`
				UpdatedAt   time.Time `json:"updated_at"`
				SubmittedAt time.Time `json:"submitted_at"`
				User        struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"user"`
			}
			if err = p.request(ctx, c, "GET", fmt.Sprintf("%s?per_page=100&page=%d", stream.path, page), nil, &rows); err != nil {
				return delivery.PullRequestReview{}, err
			}
			for _, row := range rows {
				// Exact GitHub identity, never infer reviewer identity from comment text.
				if row.User.Login != "chatgpt-codex-connector[bot]" || row.User.Type != "Bot" || row.Reply != 0 {
					continue
				}
				if row.UpdatedAt.IsZero() {
					row.UpdatedAt = row.SubmittedAt
				}
				out.Comments = append(out.Comments, delivery.ReviewComment{ID: row.ID, Kind: stream.kind, Author: row.User.Login, Body: row.Body, URL: row.URL, Path: row.Path, Line: row.Line, Commit: row.Commit, UpdatedAt: row.UpdatedAt})
			}
			if len(rows) < 100 {
				break
			}
			if page == 10 {
				return delivery.PullRequestReview{}, fmt.Errorf("review pagination limit reached; refusing partial sync")
			}
		}
	}
	return out, nil
}
