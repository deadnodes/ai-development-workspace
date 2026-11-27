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
	Name                  string               `json:"name"`
	Description           string               `json:"description"`
	Visibility            string               `json:"visibility,omitempty"`
	Owner                 string               `json:"owner,omitempty"`
	Members               []string             `json:"members,omitempty"`
	IdeaPromotion         string               `json:"idea_promotion,omitempty"`
	StalledTimeoutSeconds *int                 `json:"stalled_timeout_seconds,omitempty"`
	IdlePolicy            *ExecutionIdlePolicy `json:"idle_policy,omitempty"`
	AgentRoles            *AgentRoles          `json:"agent_roles,omitempty"`
}
type Feature struct {
	Meta
	Revision int64 `json:"revision,omitempty"`

	Title        string     `json:"title"`
	Problem      string     `json:"problem"`
	Goal         string     `json:"goal"`
	Requirements []string   `json:"requirements"`
	Constraints  []string   `json:"constraints"`
	Context      string     `json:"context"`
	Repositories []string   `json:"repositories"`
	Owner        string     `json:"owner"`
	Status       string     `json:"status"`
	ArchivedFrom string     `json:"archived_from,omitempty"`
	Priority     *int       `json:"priority,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
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
	Assignee           string        `json:"assignee,omitempty"`
	Repositories       []string      `json:"repositories"`
	Branches           []Branch      `json:"branches"`
	Commits            []Commit      `json:"commits"`
	PullRequests       []PullRequest `json:"pull_requests"`
	Completed          []string      `json:"completed"`
	Remaining          []string      `json:"remaining"`
	WorkingAreas       []string      `json:"working_areas"`
	Position           int           `json:"position"`
}

type ExecutionHarness struct {
	Kind       string `json:"kind"`
	SessionRef string `json:"session_ref"`
	TmuxTarget string `json:"tmux_target,omitempty"`
	CWD        string `json:"cwd"`
	Host       string `json:"host"`
}
type ExecutionReconciliation struct {
	Enabled        bool                `json:"enabled"`
	ActionRequired bool                `json:"action_required"`
	Reason         string              `json:"reason,omitempty"`
	Cause          ExecutionEventCause `json:"cause,omitempty"`
	Since          *time.Time          `json:"since,omitempty"`
	Generation     uint64              `json:"generation"`
}
type ExecutionEventDelivery struct {
	Attempts       int        `json:"attempts"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
	DeliveryID     string     `json:"delivery_id,omitempty"`
}
type ExecutionEventResolution struct {
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	Evidence   string     `json:"evidence,omitempty"`
}
type ExecutionEventCause string

const (
	ExecutionCauseBlockerResolved      ExecutionEventCause = "blocker_resolved"
	ExecutionCauseQueueAvailable       ExecutionEventCause = "queue_available"
	ExecutionCauseQuestionContinuation ExecutionEventCause = "question_continuation"
)

type ExecutionEvent struct {
	Owner               string                     `json:"owner,omitempty"`
	RegistrationID      string                     `json:"registration_id,omitempty"`
	ID                  string                     `json:"id"`
	ProductID           string                     `json:"product_id"`
	FeatureID           string                     `json:"feature_id,omitempty"`
	QuestionIDs         []string                   `json:"question_ids,omitempty"`
	IntegrationID       string                     `json:"integration_id"`
	GroupIDs            []string                   `json:"group_ids"`
	Type                string                     `json:"type"`
	Cause               ExecutionEventCause        `json:"cause,omitempty"`
	Fence               uint64                     `json:"fence"`
	ConditionGeneration uint64                     `json:"condition_generation"`
	Reason              string                     `json:"reason"`
	DedupeKey           string                     `json:"dedupe_key"`
	SessionRef          string                     `json:"session_ref"`
	HarnessKind         string                     `json:"harness_kind"`
	TmuxTarget          string                     `json:"tmux_target,omitempty"`
	WakeTarget          ExecutionWakeTarget        `json:"wake_target"`
	OrchestratorSkill   string                     `json:"orchestrator_skill,omitempty"`
	GroupIdle           *ExecutionGroupIdlePayload `json:"group_idle,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
	Delivery            ExecutionEventDelivery     `json:"delivery"`
	Resolution          ExecutionEventResolution   `json:"resolution"`
	EscalationRequired  bool                       `json:"escalation_required,omitempty"`
	Acknowledgements    []ExecutionEventAck        `json:"acknowledgements,omitempty"`
}
type ExecutionWakeTarget struct {
	GroupID         string `json:"group_id,omitempty"`
	HarnessRevision uint64 `json:"harness_revision,omitempty"`
	Kind            string `json:"kind,omitempty"`
	SessionRef      string `json:"session_ref,omitempty"`
	TmuxTarget      string `json:"tmux_target,omitempty"`
	Source          string `json:"source,omitempty"`
}
type ExecutionEventAck struct {
	DeliveryID   string    `json:"delivery_id"`
	SupervisorID string    `json:"supervisor_id"`
	Outcome      string    `json:"outcome"`
	Detail       string    `json:"detail,omitempty"`
	At           time.Time `json:"at"`
}
type IntegrationExecution struct {
	LeasePrincipal        string                  `json:"lease_principal,omitempty"`
	ID                    string                  `json:"id"`
	IntegrationID         string                  `json:"integration_id"`
	ProductID             string                  `json:"product_id"`
	DesiredState          string                  `json:"desired_state"`
	State                 string                  `json:"state"`
	OrchestratorID        string                  `json:"orchestrator_id,omitempty"`
	AgentID               string                  `json:"agent_id,omitempty"`
	Harness               ExecutionHarness        `json:"harness"`
	Fence                 uint64                  `json:"fence"`
	LeaseTTLSeconds       int                     `json:"lease_ttl_seconds"`
	LeaseUntil            *time.Time              `json:"lease_until,omitempty"`
	LastHeartbeat         *time.Time              `json:"last_heartbeat,omitempty"`
	LastProgress          *time.Time              `json:"last_progress,omitempty"`
	LastRecordedProgress  *time.Time              `json:"last_recorded_progress,omitempty"`
	ClaimedAt             *time.Time              `json:"claimed_at,omitempty"`
	ExitReason            string                  `json:"exit_reason,omitempty"`
	WaitingRef            string                  `json:"waiting_ref,omitempty"`
	BlockerRef            string                  `json:"blocker_ref,omitempty"`
	BlockerFence          *uint64                 `json:"blocker_fence,omitempty"`
	StalledTimeoutSeconds *int                    `json:"stalled_timeout_seconds,omitempty"`
	Reconciliation        ExecutionReconciliation `json:"reconciliation"`
	TurnEndAt             *time.Time              `json:"turn_end_at,omitempty"`
	TurnEndID             string                  `json:"turn_end_id,omitempty"`
}
type ExecutionGroupRevision struct {
	Revision             uint64    `json:"revision"`
	MemberIntegrationIDs []string  `json:"member_integration_ids"`
	CreatedAt            time.Time `json:"created_at"`
}
type ExecutionGroup struct {
	ID                   string                   `json:"id"`
	ProductID            string                   `json:"product_id"`
	Owner                string                   `json:"owner,omitempty"`
	Name                 string                   `json:"name"`
	OrchestratorID       string                   `json:"orchestrator_id"`
	OrchestratorHarness  ExecutionHarness         `json:"orchestrator_harness,omitempty"`
	HarnessRevision      uint64                   `json:"harness_revision,omitempty"`
	HarnessChangedAt     *time.Time               `json:"harness_changed_at,omitempty"`
	TurnEndAt            *time.Time               `json:"turn_end_at,omitempty"`
	TurnEndID            string                   `json:"turn_end_id,omitempty"`
	TurnStartAt          *time.Time               `json:"turn_start_at,omitempty"`
	TurnStartID          string                   `json:"turn_start_id,omitempty"`
	MemberIntegrationIDs []string                 `json:"member_integration_ids"`
	Revisions            []ExecutionGroupRevision `json:"revisions"`
	CreatedAt            time.Time                `json:"created_at"`
	ClosedAt             *time.Time               `json:"closed_at,omitempty"`
}
type Memory struct {
	Meta
	Owner         string           `json:"owner,omitempty"`
	Question      *QuestionDetails `json:"question,omitempty"`
	IntegrationID string           `json:"integration_id,omitempty"`
	Kind          string           `json:"kind"`
	Title         string           `json:"title"`
	Body          string           `json:"body"`
	Reason        string           `json:"reason"`
	Status        string           `json:"status,omitempty"`
	Completed     []string         `json:"completed"`
	Current       string           `json:"current"`
	Remaining     []string         `json:"remaining"`
	Next          []string         `json:"next"`
	Warnings      []string         `json:"warnings"`
	Commit        string           `json:"commit"`
	Deployment    string           `json:"deployment"`
	Session       string           `json:"session"`
	Resolution    string           `json:"resolution,omitempty"`
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
	// ExternalScopeID is the gate's external scope in force when the result was
	// recorded ("" for none); nil only on results recorded before it existed.
	ExternalScopeID *string `json:"external_scope_id,omitempty"`
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
	ReservationEpoch     uint64            `json:"reservation_epoch"`
	SelectionGeneration  uint64            `json:"selection_generation"`
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
	Retention  *ArtifactRetention `json:"retention,omitempty"`
	Parameters map[string]string  `json:"parameters,omitempty"`
	Kind       string             `json:"kind"`
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
	AutoRefresh bool `json:"auto_refresh"`
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
	EnvironmentID        string            `json:"environment_id,omitempty"`
	RecordedExternally   bool              `json:"recorded_externally,omitempty"`
	Evidence             []string          `json:"evidence,omitempty"`
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
	FeatureDependencies    []FeatureDependency    `json:"feature_dependencies"`
	UserOrchestrators      []UserOrchestrator     `json:"user_orchestrators"`
	ProductAgentSkills    []ProductAgentSkill    `json:"product_agent_skills"`
	ProdPromotions         []ProdPromotion        `json:"prod_promotions"`
	IntegrationExecutions  []IntegrationExecution `json:"integration_executions"`
	ExecutionEvents        []ExecutionEvent       `json:"execution_events"`
	ExecutionGroups        []ExecutionGroup       `json:"execution_groups"`
	ArtifactSignatures     []ArtifactSignature    `json:"artifact_signatures"`
	Deployments            []Deployment           `json:"deployments"`
	DeploymentReceipts     []DeploymentReceipt    `json:"deployment_receipts"`
	BuildReceipts          []BuildReceipt         `json:"build_receipts"`
	Builds                 []Build                `json:"builds"`
	RegistryInventories    []RegistryInventory    `json:"registry_inventories"`
	ArtifactPins           []ArtifactPin          `json:"artifact_pins"`
	NativeQuarantines      []NativeQuarantine     `json:"native_quarantines"`
	WorkerJobs             []WorkerJob            `json:"worker_jobs"`
	WorkerAttempts         []WorkerAttempt        `json:"worker_attempts"`
	CompositionConflicts   []CompositionConflict  `json:"composition_conflicts"`
	ProjectKnowledge       []ProjectKnowledge     `json:"project_knowledge"`
	LocalCheckouts         []LocalCheckout        `json:"local_checkouts"`
	WorkspaceDocuments     []WorkspaceDocument    `json:"workspace_documents"`
	PublicationTargets     []PublicationTarget    `json:"publication_targets"`
	PackageArtifacts       []PackageArtifact      `json:"package_artifacts"`
	ScenarioVersions       []ScenarioVersion      `json:"scenario_versions"`
	ScenarioRuns           []ScenarioRun          `json:"scenario_runs"`
	RuntimeSnapshots       []RuntimeSnapshot      `json:"runtime_snapshots"`
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
	return State{ProdPromotions: []ProdPromotion{}, IntegrationExecutions: []IntegrationExecution{}, ExecutionGroups: []ExecutionGroup{}, ArtifactSignatures: []ArtifactSignature{}, Deployments: []Deployment{}, DeploymentReceipts: []DeploymentReceipt{}, BuildReceipts: []BuildReceipt{}, Builds: []Build{}, RegistryInventories: []RegistryInventory{}, ArtifactPins: []ArtifactPin{}, WorkerJobs: []WorkerJob{}, WorkerAttempts: []WorkerAttempt{}, RuntimeSnapshots: []RuntimeSnapshot{}, ProjectKnowledge: []ProjectKnowledge{}, LocalCheckouts: []LocalCheckout{}, WorkspaceDocuments: []WorkspaceDocument{}, PublicationTargets: []PublicationTarget{}, PackageArtifacts: []PackageArtifact{}, ScenarioVersions: []ScenarioVersion{}, ScenarioRuns: []ScenarioRun{}, RuntimeObservations: []RuntimeObservation{}, ReviewSyncs: []ReviewSync{}, DeliveryArtifacts: []DeliveryArtifact{}, DeliveryBuildRuns: []DeliveryBuildRun{}, ExternalSystems: []ExternalSystem{}, SystemRelationships: []SystemRelationship{}, ExternalScopes: []ExternalScope{}, RegisteredRepositories: []RegisteredRepository{}, ConnectionGrants: []ConnectionGrant{}, GitHubConnections: []GitHubConnection{}, RepositoryBindings: []RepositoryBinding{}, ComponentBuilds: []ComponentBuild{}, EnvironmentBindings: []EnvironmentBinding{}, Operations: []ExternalOperation{}, OperationSteps: []OperationStep{}, GitObservations: []GitObservation{}, Applications: []Application{}, IntegrationRevisions: []IntegrationRevision{}, Compositions: []Composition{}, Products: []Product{}, Features: []Feature{}, Integrations: []Integration{}, Gates: []Gate{}, Checks: []Check{}, Results: []CheckResult{}, Findings: []Finding{}, Memories: []Memory{}, Environments: []Environment{}, Repositories: []Repository{}, Releases: []Release{}, Events: []Event{}}
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

func init() { Actions = append(Actions, "upsert_code_index", "set_product_idle_policy") }

var Actions = append([]string{"adopt_product_ownership", "set_product_visibility", "add_product_member", "remove_product_member", "assign_integration", "unassign_integration", "claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_execution_group_harness", "set_product_stalled_timeout", "queue_intelligence_task", "cancel_intelligence_task", "request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration", "capture_candidate_dev_baseline", "verify_candidate_dev_runtime", "reserve_candidate_dev", "release_candidate_dev", "refresh_registry_inventory", "pin_artifact", "release_artifact_pin", "cancel_native_delivery", "queue_worker_publication", "reconcile_worker_publication", "queue_worker_job", "cancel_worker_job", "reconcile_worker_job", "refresh_environment_runtime", "create_integration_branch", "protect_integration_revision", "claim_composition_conflict", "record_conflict_resolution", "verify_conflict_resolution", "configure_artifact_retention", "delete_product", "classify_repository", "configure_publication", "record_package_artifact", "update_environment_feature", "assemble_environment", "reconcile_composition", "prepare_release_candidate", "promote_release_candidate", "create_hotfix", "record_runtime_observation", "create_test_scenario", "revise_test_scenario", "record_scenario_run", "create_external_system", "update_external_system", "create_system_relationship", "set_external_scope", "grant_connection", "create_github_connection", "import_repository", "configure_component", "configure_environment", "refresh_integration_git", "refresh_repository_git", "deploy_integration", "deploy_existing_artifact", "create_application", "record_integration_revision", "plan_composition", "select_composition", "create_product", "create_feature", "update_feature", "create_integration", "update_integration", "start_integration", "transition_integration", "complete_integration", "record_progress", "record_decision", "record_discovery", "add_blocker", "resolve_blocker", "handoff", "create_gate", "add_check", "record_check_result", "record_finding", "resolve_finding", "create_environment", "update_environment", "create_repository", "plan_release", "record_release"}, WorkActions...)
