package delivery

import "context"

type GitOpsPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
}
type GitOpsPRProvider interface {
	ProposeGitOps(context.Context, Connection, GitOpsApply) (GitOpsPullRequest, error)
	ObserveGitOpsPR(context.Context, Connection, GitOpsApply, GitOpsPullRequest) (GitOpsPullRequest, error)
}

// GitOpsProposalLookup recovers an uncertain proposal without authorizing a
// retry. An empty result or error is unresolved, never permission to propose.
type GitOpsProposalLookup interface {
	LookupGitOpsProposal(context.Context, Connection, GitOpsApply) (GitOpsPullRequest, error)
}

// GitOpsGuardedPRProvider revalidates internal execution authority immediately
// before every mutating HTTP send within the compound proposal. The callback
// must use local state only; it must not perform provider IO.
type GitOpsGuardedPRProvider interface {
	ProposeGitOpsGuarded(context.Context, Connection, GitOpsApply, func(context.Context) error) (GitOpsPullRequest, error)
}
