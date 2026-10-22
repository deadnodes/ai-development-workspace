package delivery

import "context"

// GitOpsImageChange selects current image scalars and pins their desired image.
type GitOpsImageChange struct {
	ID              string        `json:"id"`
	Selector        GitOpsRequest `json:"selector"`
	ExpectedBlobSHA string        `json:"expected_blob_sha"`
	ImageRepository string        `json:"image_repository"`
	Digest          string        `json:"digest"`
}

type GitOpsBatchApply struct {
	Repository      string              `json:"repository"`
	Ref             string              `json:"ref"`
	ExpectedHeadSHA string              `json:"expected_head_sha"`
	OperationID     string              `json:"operation_id"`
	Message         string              `json:"message"`
	Changes         []GitOpsImageChange `json:"changes"`
}

// Recovery and observation never refresh authentication or mutate provider state.
type GitOpsBatchPRProvider interface {
	ProposeGitOpsBatchGuarded(context.Context, Connection, GitOpsBatchApply, func(context.Context) error) (GitOpsPullRequest, error)
	LookupGitOpsBatchProposal(context.Context, Connection, GitOpsBatchApply) (GitOpsPullRequest, error)
	ObserveGitOpsBatchPR(context.Context, Connection, GitOpsBatchApply, GitOpsPullRequest) (GitOpsPullRequest, error)
}
