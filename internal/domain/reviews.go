package domain

import "releasecontrol/internal/delivery"

type ReviewSource struct {
	RepositoryID string                 `json:"repository_id"`
	PullRequest  int                    `json:"pull_request"`
	Comment      delivery.ReviewComment `json:"comment"`
	Priority     string                 `json:"priority"`
}
type ReviewSync struct {
	Meta
	IntegrationID string                     `json:"integration_id"`
	RepositoryID  string                     `json:"repository_id"`
	Review        delivery.PullRequestReview `json:"review"`
	// NO_FINDINGS_OBSERVED is not review completion or approval.
	Status string `json:"status"`
}
