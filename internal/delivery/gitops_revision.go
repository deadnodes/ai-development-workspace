package delivery

import "context"

// GitOpsRevisionRequest uses the numeric repository identity. An empty HeadSHA
// pins the fresh branch HEAD; a supplied HeadSHA requires that branch still there.
type GitOpsRevisionRequest struct {
	Repository   string   `json:"repository"`
	Ref          string   `json:"ref"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	AncestorSHAs []string `json:"ancestor_shas"`
}
type GitOpsRevisionProof struct {
	Repository   string   `json:"repository"`
	Ref          string   `json:"ref"`
	HeadSHA      string   `json:"head_sha"`
	AncestorSHAs []string `json:"ancestor_shas"`
}

// GitOpsRevisionVerifier performs bounded GET-only numeric identity and ancestry
// checks. It cannot refresh authentication or mutate a repository.
type GitOpsRevisionVerifier interface {
	VerifyGitOpsRevision(context.Context, Connection, GitOpsRevisionRequest) (GitOpsRevisionProof, error)
}
