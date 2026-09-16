package delivery

import "context"

// SourcePlan pins every input. Generated branches are disposable outputs, never inputs.
type SourcePlan struct {
	Repository   string   `json:"repository"`
	BaseRef      string   `json:"base_ref"`
	BaseSHA      string   `json:"base_sha"`
	TargetBranch string   `json:"target_branch"`
	OperationID  string   `json:"operation_id"`
	HeadSHAs     []string `json:"head_shas"`
}
type SourceResult struct {
	Branch   string `json:"branch"`
	SHA      string `json:"sha"`
	Conflict bool   `json:"conflict"`
	Detail   string `json:"detail,omitempty"`
}

// SourceProvider is optional: source orchestration does not require every provider
// to support writes. Callers must apply policy and retain operation provenance.
type SourceProvider interface {
	ComposeSource(context.Context, Connection, SourcePlan) (SourceResult, error)
	AdvanceMain(context.Context, Connection, string, string, string, string) (SourceResult, error)
}
