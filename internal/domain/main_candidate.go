package domain

// MainCandidateRequest selects already merged source; it never authorizes a Git write.
type MainCandidateRequest struct {
	Name           string                `json:"name"`
	EnvironmentID  string                `json:"environment_id"`
	IdempotencyKey string                `json:"idempotency_key"`
	MainSources    []MainCandidateSource `json:"main_sources"`
}
type MainCandidateSource struct {
	ApplicationID string                  `json:"application_id"`
	MainSHA       string                  `json:"main_sha"`
	Native        NativeBinding           `json:"native"`
	Revisions     []MainCandidateRevision `json:"revisions"`
}
type MainCandidateRevision struct {
	RevisionID            string   `json:"revision_id"`
	PullRequestNumber     int      `json:"pull_request_number"`
	VerificationResultIDs []string `json:"verification_result_ids"`
}

// MainCandidateProof attributes existing RCP checks; it makes no claim about
// provider human review, squash policy, or ownership of every source change.
type MainCandidateProof struct {
	EvidenceKind      string                    `json:"evidence_kind"`
	Request           MainCandidateRequest      `json:"request"`
	ConfigurationHash string                    `json:"configuration_hash"`
	AuthorityHash     string                    `json:"authority_hash"`
	Sources           []MainCandidateRepository `json:"sources"`
}
type MainCandidateRepository struct {
	RepositoryID         string                        `json:"repository_id"`
	ProviderRepositoryID int64                         `json:"provider_repository_id"`
	MainRef              string                        `json:"main_ref"`
	MainSHA              string                        `json:"main_sha"`
	Revisions            []MainCandidateMergedRevision `json:"revisions"`
}
type MainCandidateMergedRevision struct {
	RevisionID        string `json:"revision_id"`
	PullRequestNumber int    `json:"pull_request_number"`
	HeadSHA           string `json:"head_sha"`
	HeadRef           string `json:"head_ref"`
	MergeSHA          string `json:"merge_sha"`
	// Fingerprint of minimal physical PR identity, never raw review/user payloads.
	ObservationHash string `json:"observation_hash"`
}
