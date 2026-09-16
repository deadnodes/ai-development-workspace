package domain

import "time"

type Meta struct {
	ID        string    `json:"id"`
	ProductID string    `json:"product_id,omitempty"`
	FeatureID string    `json:"feature_id,omitempty"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type Product struct {
	Meta
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Feature struct {
	Meta
	Title        string   `json:"title"`
	Problem      string   `json:"problem"`
	Goal         string   `json:"goal"`
	Requirements []string `json:"requirements"`
	Constraints  []string `json:"constraints"`
	Context      string   `json:"context"`
	Repositories []string `json:"repositories"`
	Owner        string   `json:"owner"`
	Status       string   `json:"status"`
}
type Branch struct {
	RepositoryID string `json:"repository_id"`
	Name         string `json:"name"`
}
type Commit struct {
	RepositoryID string `json:"repository_id"`
	SHA          string `json:"sha"`
}
type PullRequest struct {
	RepositoryID string `json:"repository_id"`
	ID           string `json:"id"`
	URL          string `json:"url"`
	Status       string `json:"status"`
}
type Integration struct {
	Kind           string `json:"kind,omitempty"`
	FixesFindingID string `json:"fixes_finding_id,omitempty"`
	Meta
	Title              string        `json:"title"`
	Objective          string        `json:"objective"`
	Rationale          string        `json:"rationale"`
	Dependencies       []string      `json:"dependencies"`
	AcceptanceCriteria []string      `json:"acceptance_criteria"`
	Status             string        `json:"status"`
	Owner              string        `json:"owner"`
	Repositories       []string      `json:"repositories"`
	Branches           []Branch      `json:"branches"`
	Commits            []Commit      `json:"commits"`
	PullRequests       []PullRequest `json:"pull_requests"`
	Completed          []string      `json:"completed"`
	Remaining          []string      `json:"remaining"`
	WorkingAreas       []string      `json:"working_areas"`
	Position           int           `json:"position"`
}
type Memory struct {
	Meta
	IntegrationID string   `json:"integration_id,omitempty"`
	Kind          string   `json:"kind"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	Reason        string   `json:"reason"`
	Status        string   `json:"status,omitempty"`
	Completed     []string `json:"completed"`
	Current       string   `json:"current"`
	Remaining     []string `json:"remaining"`
	Next          []string `json:"next"`
	Warnings      []string `json:"warnings"`
	Commit        string   `json:"commit"`
	Deployment    string   `json:"deployment"`
	Session       string   `json:"session"`
	Resolution    string   `json:"resolution,omitempty"`
}
type Gate struct {
	Meta
	Title          string   `json:"title"`
	Reason         string   `json:"reason"`
	IntegrationIDs []string `json:"integration_ids"`
	Position       int      `json:"position"`
	Blocking       bool     `json:"blocking"`
	EnvironmentID  string   `json:"environment_id"`
	Commit         string   `json:"commit"`
	Deployment     string   `json:"deployment"`
	Result         string   `json:"result"`
}
type Check struct {
	Meta
	GateID       string `json:"gate_id"`
	Title        string `json:"title"`
	Mechanism    string `json:"mechanism"`
	Instructions string `json:"instructions"`
}
type Artifact struct {
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Label string `json:"label"`
}
type CheckResult struct {
	Meta
	CheckID       string     `json:"check_id"`
	Result        string     `json:"result"`
	Commit        string     `json:"commit"`
	EnvironmentID string     `json:"environment_id"`
	Deployment    string     `json:"deployment"`
	Steps         []string   `json:"steps"`
	Observations  string     `json:"observations"`
	Logs          string     `json:"logs"`
	Artifacts     []Artifact `json:"artifacts"`
}
type Finding struct {
	ReviewSource *ReviewSource `json:"review_source,omitempty"`
	Meta
	ResultID       string   `json:"result_id"`
	Title          string   `json:"title"`
	Severity       string   `json:"severity"`
	Body           string   `json:"body"`
	Status         string   `json:"status"`
	IntegrationIDs []string `json:"integration_ids"`
	GateIDs        []string `json:"gate_ids"`
	BlocksRelease  bool     `json:"blocks_release"`
	Resolution     string   `json:"resolution"`
	FixCommit      string   `json:"fix_commit"`
}
type Environment struct {
	Parameters           map[string]string `json:"parameters,omitempty"`
	DesiredOperationID   string            `json:"desired_operation_id,omitempty"`
	Cluster              string            `json:"cluster"`
	Namespace            string            `json:"namespace"`
	DesiredCompositionID string            `json:"desired_composition_id"`
	Meta
	Name        string         `json:"name"`
	Desired     map[string]any `json:"desired"`
	Reconciled  map[string]any `json:"reconciled"`
	Runtime     map[string]any `json:"runtime"`
	Composition map[string]any `json:"composition"`
}
type Application struct {
	Parameters map[string]string `json:"parameters,omitempty"`
	Kind       string            `json:"kind"`
	Meta
	Name         string `json:"name"`
	RepositoryID string `json:"repository_id"`
	Path         string `json:"path"`
}
type IntegrationRevision struct {
	Meta
	IntegrationID string   `json:"integration_id"`
	RepositoryID  string   `json:"repository_id"`
	Branch        string   `json:"branch"`
	BaseCommit    string   `json:"base_commit"`
	HeadCommit    string   `json:"head_commit"`
	Commits       []string `json:"commits"`
}
type CompositionComponent struct {
	ApplicationID string   `json:"application_id"`
	BaseRef       string   `json:"base_ref"`
	BaseCommit    string   `json:"base_commit"`
	TargetBranch  string   `json:"target_branch"`
	RevisionIDs   []string `json:"revision_ids"`
}
type Composition struct {
	Meta
	EnvironmentID        string                 `json:"environment_id"`
	Name                 string                 `json:"name"`
	Status               string                 `json:"status"`
	Components           []CompositionComponent `json:"components"`
	ApplicationSnapshots []Application          `json:"application_snapshots"`
	RevisionSnapshots    []IntegrationRevision  `json:"revision_snapshots"`
	EnvironmentSnapshot  Environment            `json:"environment_snapshot"`
}
type Repository struct {
	Role                   string `json:"role"`
	RegisteredRepositoryID string `json:"registered_repository_id,omitempty"`
	Meta
	Name     string `json:"name"`
	URL      string `json:"url"`
	Provider string `json:"provider"`
}
type Release struct {
	ExecutionOperationID string            `json:"execution_operation_id,omitempty"`
	CandidateOperationID string            `json:"candidate_operation_id,omitempty"`
	SourceCommits        map[string]string `json:"source_commits,omitempty"`
	ArtifactDigests      map[string]string `json:"artifact_digests,omitempty"`
	GitOpsCommits        map[string]string `json:"gitops_commits,omitempty"`
	Status               string            `json:"status"`
	Snapshots            []Integration     `json:"snapshots"`
	Meta
	Name                   string   `json:"name"`
	IntegrationIDs         []string `json:"integration_ids"`
	ExcludedIntegrationIDs []string `json:"excluded_integration_ids"`
	Notes                  string   `json:"notes"`
}
type Event struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	EntityID  string    `json:"entity_id"`
	ProductID string    `json:"product_id,omitempty"`
	FeatureID string    `json:"feature_id,omitempty"`
	Data      Command   `json:"data"`
}
type State struct {
	ProjectKnowledge       []ProjectKnowledge     `json:"project_knowledge"`
	LocalCheckouts         []LocalCheckout        `json:"local_checkouts"`
	WorkspaceDocuments     []WorkspaceDocument    `json:"workspace_documents"`
	PublicationTargets     []PublicationTarget    `json:"publication_targets"`
	PackageArtifacts       []PackageArtifact      `json:"package_artifacts"`
	ScenarioVersions       []ScenarioVersion      `json:"scenario_versions"`
	ScenarioRuns           []ScenarioRun          `json:"scenario_runs"`
	RuntimeObservations    []RuntimeObservation   `json:"runtime_observations"`
	ReviewSyncs            []ReviewSync           `json:"review_syncs"`
	DeliveryArtifacts      []DeliveryArtifact     `json:"delivery_artifacts"`
	DeliveryBuildRuns      []DeliveryBuildRun     `json:"delivery_build_runs"`
	ExternalSystems        []ExternalSystem       `json:"external_systems"`
	SystemRelationships    []SystemRelationship   `json:"system_relationships"`
	ExternalScopes         []ExternalScope        `json:"external_scopes"`
	RegisteredRepositories []RegisteredRepository `json:"registered_repositories"`
	ConnectionGrants       []ConnectionGrant      `json:"connection_grants"`
	GitHubConnections      []GitHubConnection     `json:"github_connections"`
	RepositoryBindings     []RepositoryBinding    `json:"repository_bindings"`
	ComponentBuilds        []ComponentBuild       `json:"component_builds"`
	EnvironmentBindings    []EnvironmentBinding   `json:"environment_bindings"`
	Operations             []ExternalOperation    `json:"operations"`
	OperationSteps         []OperationStep        `json:"operation_steps"`
	GitObservations        []GitObservation       `json:"git_observations"`
	Applications           []Application          `json:"applications"`
	IntegrationRevisions   []IntegrationRevision  `json:"integration_revisions"`
	Compositions           []Composition          `json:"compositions"`
	Products               []Product              `json:"products"`
	Features               []Feature              `json:"features"`
	Integrations           []Integration          `json:"integrations"`
	Gates                  []Gate                 `json:"gates"`
	Checks                 []Check                `json:"checks"`
	Results                []CheckResult          `json:"results"`
	Findings               []Finding              `json:"findings"`
	Memories               []Memory               `json:"memories"`
	Environments           []Environment          `json:"environments"`
	Repositories           []Repository           `json:"repositories"`
	Releases               []Release              `json:"releases"`
	Events                 []Event                `json:"events"`
}

func EmptyState() State {
	return State{ProjectKnowledge: []ProjectKnowledge{}, LocalCheckouts: []LocalCheckout{}, WorkspaceDocuments: []WorkspaceDocument{}, PublicationTargets: []PublicationTarget{}, PackageArtifacts: []PackageArtifact{}, ScenarioVersions: []ScenarioVersion{}, ScenarioRuns: []ScenarioRun{}, RuntimeObservations: []RuntimeObservation{}, ReviewSyncs: []ReviewSync{}, DeliveryArtifacts: []DeliveryArtifact{}, DeliveryBuildRuns: []DeliveryBuildRun{}, ExternalSystems: []ExternalSystem{}, SystemRelationships: []SystemRelationship{}, ExternalScopes: []ExternalScope{}, RegisteredRepositories: []RegisteredRepository{}, ConnectionGrants: []ConnectionGrant{}, GitHubConnections: []GitHubConnection{}, RepositoryBindings: []RepositoryBinding{}, ComponentBuilds: []ComponentBuild{}, EnvironmentBindings: []EnvironmentBinding{}, Operations: []ExternalOperation{}, OperationSteps: []OperationStep{}, GitObservations: []GitObservation{}, Applications: []Application{}, IntegrationRevisions: []IntegrationRevision{}, Compositions: []Composition{}, Products: []Product{}, Features: []Feature{}, Integrations: []Integration{}, Gates: []Gate{}, Checks: []Check{}, Results: []CheckResult{}, Findings: []Finding{}, Memories: []Memory{}, Environments: []Environment{}, Repositories: []Repository{}, Releases: []Release{}, Events: []Event{}}
}

type Command struct {
	Action        string         `json:"action"`
	Actor         string         `json:"actor"`
	ID            string         `json:"id,omitempty"`
	ProductID     string         `json:"product_id,omitempty"`
	FeatureID     string         `json:"feature_id,omitempty"`
	IntegrationID string         `json:"integration_id,omitempty"`
	GateID        string         `json:"gate_id,omitempty"`
	CheckID       string         `json:"check_id,omitempty"`
	ResultID      string         `json:"result_id,omitempty"`
	Data          map[string]any `json:"data"`
}

var Actions = []string{"classify_repository", "configure_publication", "record_package_artifact", "reconcile_composition", "prepare_release_candidate", "promote_release_candidate", "create_hotfix", "record_runtime_observation", "create_test_scenario", "revise_test_scenario", "record_scenario_run", "create_external_system", "update_external_system", "create_system_relationship", "set_external_scope", "grant_connection", "create_github_connection", "import_repository", "configure_component", "configure_environment", "refresh_integration_git", "deploy_integration", "create_application", "record_integration_revision", "plan_composition", "select_composition", "create_product", "create_feature", "update_feature", "create_integration", "update_integration", "start_integration", "transition_integration", "complete_integration", "record_progress", "record_decision", "record_discovery", "add_blocker", "resolve_blocker", "handoff", "create_gate", "add_check", "record_check_result", "record_finding", "resolve_finding", "create_environment", "update_environment", "create_repository", "plan_release"}
