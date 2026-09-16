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
