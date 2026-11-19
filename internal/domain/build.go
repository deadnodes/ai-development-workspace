package domain

import "time"

// Build is its own durable asynchronous handle, independent of delivery Operations.
type Build struct {
	SecurityStatus       string               `json:"security_status,omitempty"`
	Security             []BuildImageSecurity `json:"security,omitempty"`
	SigningStatus        string               `json:"signing_status"`
	Signatures           []BuildSignature     `json:"signatures,omitempty"`
	SigningAttempts      int                  `json:"signing_attempts,omitempty"`
	SigningStartedAt     time.Time            `json:"signing_started_at,omitempty"`
	SigningNextAttemptAt time.Time            `json:"signing_next_attempt_at,omitempty"`
	Trigger              string               `json:"trigger"`
	SupersededBy         string               `json:"superseded_by,omitempty"`
	WaitingForBuildID    string               `json:"waiting_for_build_id,omitempty"`
	LastError            string               `json:"last_error,omitempty"`
	ProviderUnavailable  bool                 `json:"provider_unavailable,omitempty"`
	RestoreQuarantined   bool                 `json:"restore_quarantined,omitempty"`
	AuthorityHash        string               `json:"authority_hash"`
	Meta
	ConnectionID      string            `json:"connection_id"`
	ConnectionAPIURL  string            `json:"connection_api_url,omitempty"`
	ProfileID         string            `json:"profile_id"`
	PoolID            string            `json:"pool_id"`
	Repository        string            `json:"repository"`
	Workflow          string            `json:"workflow"`
	Branch            string            `json:"branch"`
	SourceSHA         string            `json:"source_sha"`
	RequestKey        string            `json:"request_key,omitempty"`
	Status            string            `json:"status"`
	ProviderRunID     int64             `json:"provider_run_id,omitempty"`
	RunURL            string            `json:"run_url,omitempty"`
	RunStatus         string            `json:"run_status,omitempty"`
	Images            []BuildImage      `json:"images,omitempty"`
	Reused            string            `json:"reused,omitempty"`
	FailureCode       string            `json:"failure_code,omitempty"`
	FailureMessage    string            `json:"failure_message,omitempty"`
	QueuedAt          time.Time         `json:"queued_at"`
	DispatchedAt      *time.Time        `json:"dispatched_at,omitempty"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	FinishedAt        *time.Time        `json:"finished_at,omitempty"`
	CIState           string            `json:"ci_state,omitempty"`
	CIContexts        map[string]string `json:"ci_contexts,omitempty"`
	CIFailingContexts map[string]string `json:"ci_failing_contexts,omitempty"`
	CICheckedAt       *time.Time        `json:"ci_checked_at,omitempty"`
	CIFailedAt        *time.Time        `json:"ci_failed_at,omitempty"`
	CITriage          *BuildCITriage    `json:"ci_triage,omitempty"`
}

type BuildCITriage struct {
	BaseBuildID       string           `json:"base_build_id,omitempty"`
	BaseSHA           string           `json:"base_sha,omitempty"`
	Commits           []BuildCICommit  `json:"commits,omitempty"`
	CommitsTruncated  bool             `json:"commits_truncated,omitempty"`
	Features          []BuildCIFeature `json:"features,omitempty"`
	Handoffs          []BuildCIHandoff `json:"handoffs,omitempty"`
	BuildActor        string           `json:"build_actor,omitempty"`
	BuildTrigger      string           `json:"build_trigger,omitempty"`
	ComputedAt        time.Time        `json:"computed_at,omitempty"`
	UnavailableReason string           `json:"unavailable_reason,omitempty"`
}
type BuildCICommit struct {
	SHA         string    `json:"sha"`
	Title       string    `json:"title"`
	Author      string    `json:"author,omitempty"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
	PRNumber    int       `json:"pr_number,omitempty"`
}
type BuildCIFeature struct {
	FeatureID      string   `json:"feature_id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	IntegrationIDs []string `json:"integration_ids,omitempty"`
	MatchedBy      []string `json:"matched_by,omitempty"`
}
type BuildCIHandoff struct {
	MemoryID      string    `json:"memory_id"`
	FeatureID     string    `json:"feature_id,omitempty"`
	IntegrationID string    `json:"integration_id,omitempty"`
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	Session       string    `json:"session,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}
type BuildImageSecurity struct {
	ImageID          string    `json:"image_id"`
	Digest           string    `json:"digest"`
	Verdict          string    `json:"verdict"`
	SummaryDigest    string    `json:"summary_digest,omitempty"`
	SBOMDigest       string    `json:"sbom_digest,omitempty"`
	SourceSHA        string    `json:"source_sha,omitempty"`
	GitleaksFindings int       `json:"gitleaks_findings,omitempty"`
	Critical         int       `json:"critical,omitempty"`
	HighFixable      int       `json:"high_fixable,omitempty"`
	Blocking         int       `json:"blocking,omitempty"`
	BlockingIDs      []string  `json:"blocking_ids,omitempty"`
	Reasons          []string  `json:"reasons,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
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

// CloseBuildRequest closes only the RCP handle; it never cancels provider execution.
type CloseBuildRequest struct {
	ProductID  string `json:"product_id"`
	BuildID    string `json:"build_id"`
	Reason     string `json:"reason"`
	RequestKey string `json:"request_key,omitempty"`
}
