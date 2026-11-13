package domain

import "time"

// Deployment records GitOps completion only, never runtime health.
type Deployment struct {
	GitOpsRepository string   `json:"gitops_repository"`
	TargetFiles      []string `json:"target_files"`
	Meta
	BuildID           string            `json:"build_id"`
	ProfileID         string            `json:"profile_id"`
	EnvironmentID     string            `json:"environment_id"`
	RequestKey        string            `json:"request_key,omitempty"`
	AuthorityHash     string            `json:"authority_hash"`
	Identity          string            `json:"identity"`
	Status            string            `json:"status"`
	FailureCode       string            `json:"failure_code,omitempty"`
	Images            []BuildImage      `json:"images"`
	SourceSHA         string            `json:"source_sha"`
	RunURL            string            `json:"run_url,omitempty"`
	CIState           string            `json:"ci_state,omitempty"`
	CIContexts        map[string]string `json:"ci_contexts,omitempty"`
	CheckedAt         *time.Time        `json:"checked_at,omitempty"`
	RegistryCheckedAt *time.Time        `json:"registry_checked_at,omitempty"`
	Deadline          time.Time         `json:"deadline"`
	Branch            string            `json:"branch"`
	PRNumber          int               `json:"pr_number,omitempty"`
	PRURL             string            `json:"pr_url,omitempty"`
	HeadSHA           string            `json:"head_sha,omitempty"`
	BaseSHA           string            `json:"base_sha,omitempty"`
	ProposalBaseSHA   string            `json:"proposal_base_sha,omitempty"`
	MergeCommitSHA    string            `json:"merge_commit_sha,omitempty"`
	AlreadyApplied    bool              `json:"already_applied,omitempty"`
	RetryCount        int               `json:"retry_count"`
	UncertainAction   string            `json:"uncertain_action,omitempty"`
	FinishedAt        *time.Time        `json:"finished_at,omitempty"`
}
type DeployBuildRequest struct {
	ProductID     string `json:"product_id"`
	BuildID       string `json:"build_id"`
	EnvironmentID string `json:"environment_id"`
	RequestKey    string `json:"request_key,omitempty"`
}
type DeploymentLookup struct {
	ProductID    string `json:"product_id"`
	DeploymentID string `json:"deployment_id"`
}
type ListDeploymentsRequest struct {
	ProductID     string `json:"product_id"`
	EnvironmentID string `json:"environment_id,omitempty"`
	ProfileID     string `json:"profile_id,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}
type DeploymentPage struct {
	Items      []Deployment `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}
type DeploymentReceipt struct {
	Meta
	RequestKey    string `json:"request_key"`
	BuildID       string `json:"build_id"`
	EnvironmentID string `json:"environment_id"`
	DeploymentID  string `json:"deployment_id"`
}
