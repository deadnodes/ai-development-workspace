package domain

// ScenarioVersion is an immutable behavior definition. ScenarioID is stable;
// each edit creates a new ID and monotonically increasing Version.
type ScenarioVersion struct {
	Meta
	ScenarioID       string   `json:"scenario_id"`
	Version          int      `json:"version"`
	IntegrationIDs   []string `json:"integration_ids"`
	Title            string   `json:"title"`
	Objective        string   `json:"objective"`
	Preconditions    []string `json:"preconditions"`
	Steps            []string `json:"steps"`
	ExpectedOutcomes []string `json:"expected_outcomes"`
	Mechanism        string   `json:"mechanism"`
	Blocking         bool     `json:"blocking"`
}

// ScenarioComponentEvidence records the exact tested output and its operation.
// DeploymentEvidence is an executor assertion/reference, not runtime observation
// manufactured from a successful GitOps write.
type ScenarioComponentEvidence struct {
	ApplicationID      string `json:"application_id"`
	OperationID        string `json:"operation_id"`
	SourceSHA          string `json:"source_sha"`
	ArtifactDigest     string `json:"artifact_digest"`
	DeploymentEvidence string `json:"deployment_evidence"`
}

type ScenarioRun struct {
	Meta
	ScenarioVersionID    string                      `json:"scenario_version_id"`
	CompositionID        string                      `json:"composition_id,omitempty"`
	CandidateOperationID string                      `json:"candidate_operation_id,omitempty"`
	EnvironmentID        string                      `json:"environment_id,omitempty"`
	Components           []ScenarioComponentEvidence `json:"components"`
	Result               string                      `json:"result"`
	Observations         []string                    `json:"observations"`
	Artifacts            []Artifact                  `json:"artifacts"`
	FindingIDs           []string                    `json:"finding_ids"`
}
