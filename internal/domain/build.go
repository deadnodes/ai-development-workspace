package domain

import "time"

// Build is its own durable asynchronous handle, independent of delivery Operations.
type Build struct {
	AuthorityHash string `json:"authority_hash"`
	Meta
	ProfileID      string       `json:"profile_id"`
	PoolID         string       `json:"pool_id"`
	Repository     string       `json:"repository"`
	Workflow       string       `json:"workflow"`
	Branch         string       `json:"branch"`
	SourceSHA      string       `json:"source_sha"`
	RequestKey     string       `json:"request_key,omitempty"`
	Status         string       `json:"status"`
	ProviderRunID  int64        `json:"provider_run_id,omitempty"`
	RunURL         string       `json:"run_url,omitempty"`
	RunStatus      string       `json:"run_status,omitempty"`
	Images         []BuildImage `json:"images,omitempty"`
	Reused         string       `json:"reused,omitempty"`
	FailureCode    string       `json:"failure_code,omitempty"`
	FailureMessage string       `json:"failure_message,omitempty"`
	QueuedAt       time.Time    `json:"queued_at"`
	DispatchedAt   *time.Time   `json:"dispatched_at,omitempty"`
	StartedAt      *time.Time   `json:"started_at,omitempty"`
	FinishedAt     *time.Time   `json:"finished_at,omitempty"`
}
type QueueBuildRequest struct {
	ProductID  string `json:"product_id"`
	ProfileID  string `json:"profile_id"`
	Branch     string `json:"branch,omitempty"`
	SHA        string `json:"sha,omitempty"`
	RequestKey string `json:"request_key,omitempty"`
}
type BuildLookup struct {
	ProductID string `json:"product_id"`
	BuildID   string `json:"build_id"`
}
type ListBuildsRequest struct {
	ProductID string `json:"product_id"`
	ProfileID string `json:"profile_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type BuildPage struct {
	Items      []Build `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type BuildReceipt struct {
	Workflow string `json:"workflow"`
	Meta
	RequestKey string `json:"request_key"`
	BuildID    string `json:"build_id"`
	ProfileID  string `json:"profile_id"`
	Branch     string `json:"branch"`
	SourceSHA  string `json:"source_sha"`
}

type BuildImage struct {
	ID       string `json:"id"`
	ImageRef string `json:"image_ref"`
	Digest   string `json:"digest"`
}
