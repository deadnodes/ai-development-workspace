package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	"releasecontrol/internal/delivery"
)

var _ delivery.ActionsInventoryProvider = (*Provider)(nil)

func (p *Provider) ListRecentBuilds(ctx context.Context, c delivery.Connection, repository string) ([]delivery.ActionsRun, error) {
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return nil, err
	}
	var response struct {
		Runs []struct {
			ID         int64     `json:"id"`
			WorkflowID int64     `json:"workflow_id"`
			Name       string    `json:"name"`
			HeadSHA    string    `json:"head_sha"`
			HeadBranch string    `json:"head_branch"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			URL        string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	// One page by design: this is a recent observation, not complete build history.
	if err := p.request(ctx, c, http.MethodGet, "/repos/"+repo+"/actions/runs?status=success&per_page=20&page=1", nil, &response); err != nil {
		return nil, err
	}
	if len(response.Runs) > 20 {
		return nil, errors.New("workflow inventory exceeded requested bound")
	}
	out := make([]delivery.ActionsRun, 0, len(response.Runs))
	for _, r := range response.Runs {
		if r.Status != "completed" || r.Conclusion != "success" {
			continue
		}
		if r.ID <= 0 || !fullSHA.MatchString(r.HeadSHA) || r.CreatedAt.IsZero() {
			return nil, errors.New("workflow inventory missing run provenance")
		}
		out = append(out, delivery.ActionsRun{ID: r.ID, WorkflowID: r.WorkflowID, Name: r.Name, HeadSHA: r.HeadSHA, HeadBranch: r.HeadBranch, Status: r.Status, Conclusion: r.Conclusion, URL: r.URL, CreatedAt: r.CreatedAt})
	}
	return out, nil
}
