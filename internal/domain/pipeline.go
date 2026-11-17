package domain

import "time"

type ListPipelinesRequest struct {
	ProductID     string `json:"product_id"`
	ProfileID     string `json:"profile_id,omitempty"`
	Status        string `json:"status,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
	Query         string `json:"q,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}

type Pipeline struct {
	Build           Build           `json:"build"`
	Deployments     []Deployment    `json:"deployments"`
	Status          string          `json:"status"`
	Stages          []PipelineStage `json:"stages"`
	CommitURL       string          `json:"commit_url,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	DurationSeconds int64           `json:"duration_seconds,omitempty"`
}

type PipelineStage struct {
	Kind           string   `json:"kind"`
	EnvironmentID  string   `json:"environment_id,omitempty"`
	EntityID       string   `json:"entity_id,omitempty"`
	RawStatus      string   `json:"raw_status"`
	FailureCode    string   `json:"failure_code,omitempty"`
	Status         string   `json:"status"`
	Label          string   `json:"label"`
	Hint           string   `json:"hint,omitempty"`
	AllowedActions []string `json:"allowed_actions,omitempty"`
}

type PipelinePage struct {
	Items        []Pipeline `json:"items"`
	NextCursor   string     `json:"next_cursor,omitempty"`
	Profiles     []string   `json:"profiles"`
	Environments []string   `json:"environments"`
}
