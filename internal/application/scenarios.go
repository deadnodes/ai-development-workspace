package application

import (
	"regexp"
	"slices"
	"strings"

	"releasecontrol/internal/domain"
)

var scenarioDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func latestScenario(st *domain.State, id string) *domain.ScenarioVersion {
	var latest *domain.ScenarioVersion
	for i := range st.ScenarioVersions {
		v := &st.ScenarioVersions[i]
		if v.ScenarioID == id && (latest == nil || v.Version > latest.Version) {
			latest = v
		}
	}
	return latest
}
func scenarioVersion(st *domain.State, id string) *domain.ScenarioVersion {
	for i := range st.ScenarioVersions {
		if st.ScenarioVersions[i].ID == id {
			return &st.ScenarioVersions[i]
		}
	}
	return nil
}
func applyScenario(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "create_test_scenario", "revise_test_scenario":
		var input struct {
			ScenarioID       string   `json:"scenario_id"`
			IntegrationIDs   []string `json:"integration_ids"`
			Title            string   `json:"title"`
			Objective        string   `json:"objective"`
			Preconditions    []string `json:"preconditions"`
			Steps            []string `json:"steps"`
			ExpectedOutcomes []string `json:"expected_outcomes"`
			Mechanism        string   `json:"mechanism"`
			Blocking         bool     `json:"blocking"`
		}
		if err := decode(c.Data, &input); err != nil {
			return nil, m, err
		}
		if !productExists(st, c.ProductID) {
			return nil, m, missing("product", c.ProductID)
		}
		version := 1
		if c.Action == "revise_test_scenario" {
			previous := latestScenario(st, input.ScenarioID)
			if previous == nil || previous.ProductID != c.ProductID {
				return nil, m, invalid("scenario must belong to product")
			}
			version = previous.Version + 1
		} else {
			if input.ScenarioID != "" {
				return nil, m, invalid("create assigns scenario_id from command id")
			}
			input.ScenarioID = m.ID
		}
		if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Objective) == "" || strings.TrimSpace(input.Mechanism) == "" || len(input.Steps) == 0 || len(input.ExpectedOutcomes) == 0 {
			return nil, m, invalid("title, objective, mechanism, steps and expected_outcomes required")
		}
		for _, list := range [][]string{input.Steps, input.ExpectedOutcomes} {
			for _, value := range list {
				if strings.TrimSpace(value) == "" {
					return nil, m, invalid("steps and outcomes cannot be empty")
				}
			}
		}
		seen := map[string]bool{}
		for _, id := range input.IntegrationIDs {
			in := integration(st, id)
			if in == nil || in.ProductID != c.ProductID || seen[id] {
				return nil, m, invalid("unique product-scoped integration_ids required")
			}
			seen[id] = true
		}
		v := domain.ScenarioVersion{Meta: m, ScenarioID: input.ScenarioID, Version: version, IntegrationIDs: input.IntegrationIDs, Title: input.Title, Objective: input.Objective, Preconditions: input.Preconditions, Steps: input.Steps, ExpectedOutcomes: input.ExpectedOutcomes, Mechanism: input.Mechanism, Blocking: input.Blocking}
		st.ScenarioVersions = append(st.ScenarioVersions, v)
		return v, m, nil
	case "record_scenario_run":
		var input struct {
			ScenarioVersionID    string                             `json:"scenario_version_id"`
			CompositionID        string                             `json:"composition_id"`
			CandidateOperationID string                             `json:"candidate_operation_id"`
			EnvironmentID        string                             `json:"environment_id"`
			Components           []domain.ScenarioComponentEvidence `json:"components"`
			Result               string                             `json:"result"`
			Observations         []string                           `json:"observations"`
			Artifacts            []domain.Artifact                  `json:"artifacts"`
			FindingIDs           []string                           `json:"finding_ids"`
		}
		if err := decode(c.Data, &input); err != nil {
			return nil, m, err
		}
		definition := scenarioVersion(st, input.ScenarioVersionID)
		if definition == nil || definition.ProductID != c.ProductID {
			return nil, m, invalid("scenario version must belong to product")
		}
		if !slices.Contains([]string{"passed", "failed", "blocked"}, input.Result) {
			return nil, m, invalid("result must be passed, failed or blocked")
		}
		if len(input.Observations) == 0 && len(input.Artifacts) == 0 {
			return nil, m, invalid("execution observations or artifacts required")
		}
		for _, observation := range input.Observations {
			if strings.TrimSpace(observation) == "" {
				return nil, m, invalid("observations cannot be blank")
			}
		}
		for _, artifact := range input.Artifacts {
			if strings.TrimSpace(artifact.Kind) == "" || strings.TrimSpace(artifact.URL) == "" {
				return nil, m, invalid("artifact kind and reference required")
			}
		}
		run := domain.ScenarioRun{Meta: m, ScenarioVersionID: input.ScenarioVersionID, CompositionID: input.CompositionID, CandidateOperationID: input.CandidateOperationID, EnvironmentID: input.EnvironmentID, Components: input.Components, Result: input.Result, Observations: input.Observations, Artifacts: input.Artifacts, FindingIDs: input.FindingIDs}
		if err := validateScenarioTarget(st, definition, &run); err != nil {
			return nil, m, err
		}
		seen := map[string]bool{}
		for _, id := range input.FindingIDs {
			found := false
			for _, finding := range st.Findings {
				if finding.ID == id && finding.ProductID == m.ProductID {
					found = true
				}
			}
			if !found || seen[id] {
				return nil, m, invalid("unique product-scoped finding_ids required")
			}
			seen[id] = true
		}
		st.ScenarioRuns = append(st.ScenarioRuns, run)
		return run, m, nil
	}
	return nil, m, invalid("unknown scenario action")
}

func validateScenarioTarget(st *domain.State, definition *domain.ScenarioVersion, run *domain.ScenarioRun) error {
	if (run.CompositionID == "") == (run.CandidateOperationID == "") {
		return invalid("exactly one composition_id or candidate_operation_id required")
	}
	var parent *domain.ExternalOperation
	var applications []string
	var integrations []string
	if run.CandidateOperationID != "" {
		parent = operationByID(st, run.CandidateOperationID)
		if parent == nil || parent.Kind != "RELEASE_CANDIDATE" || parent.ProductID != run.ProductID || parent.Status != "SUCCEEDED" || parent.DeploymentState != "READY_FOR_VERIFICATION" {
			return invalid("successful product release candidate required")
		}
		integrations = parent.IntegrationIDs
		if run.EnvironmentID != "" {
			env := environmentByID(st, run.EnvironmentID)
			if env == nil || env.ProductID != run.ProductID {
				return invalid("test environment outside product")
			}
		}
	} else {
		var composition *domain.Composition
		for i := range st.Compositions {
			if st.Compositions[i].ID == run.CompositionID {
				composition = &st.Compositions[i]
			}
		}
		if composition == nil || composition.ProductID != run.ProductID {
			return invalid("composition must belong to product")
		}
		if run.EnvironmentID != "" && run.EnvironmentID != composition.EnvironmentID {
			return invalid("composition environment mismatch")
		}
		run.EnvironmentID = composition.EnvironmentID
		for _, component := range composition.Components {
			applications = append(applications, component.ApplicationID)
		}
		for _, revision := range composition.RevisionSnapshots {
			integrations = append(integrations, revision.IntegrationID)
		}
		// Identify a complete execution by the submitted child operation IDs. This
		// allows recording historical runs after a newer composition is selected.
		for i := range st.Operations {
			op := &st.Operations[i]
			if op.ProductID != run.ProductID || op.Kind != "COMPOSE" || op.Status != "SUCCEEDED" || op.DeploymentState != "DEPLOYED" || op.CompositionSnapshot == nil || op.CompositionSnapshot.ID != run.CompositionID {
				continue
			}
			all := true
			for _, evidence := range run.Components {
				if !slices.Contains(op.ChildIDs, evidence.OperationID) {
					all = false
				}
			}
			if all {
				parent = op
				break
			}
		}
		if parent == nil {
			return invalid("composition needs a successful execution matching component evidence")
		}
	}
	for _, id := range definition.IntegrationIDs {
		if !slices.Contains(integrations, id) {
			return invalid("scenario integration is absent from tested target")
		}
	}
	if len(run.Components) == 0 || len(run.Components) != len(parent.ChildIDs) {
		return invalid("complete component evidence required")
	}
	seen := map[string]bool{}
	for _, evidence := range run.Components {
		child := operationByID(st, evidence.OperationID)
		if child == nil || !slices.Contains(parent.ChildIDs, child.ID) || child.ProductID != run.ProductID || child.Status != "SUCCEEDED" || child.Snapshot == nil || child.Artifact == nil || !child.Artifact.Available {
			return invalid("successful target child with available artifact required")
		}
		if seen[evidence.ApplicationID] || evidence.ApplicationID != child.Snapshot.Application.ID || evidence.SourceSHA != child.Snapshot.Revision.HeadCommit || evidence.ArtifactDigest != child.Artifact.Digest || !fullSHA.MatchString(evidence.SourceSHA) || !scenarioDigest.MatchString(evidence.ArtifactDigest) {
			return invalid("component source or artifact does not match immutable execution")
		}
		if run.CompositionID != "" && (!slices.Contains(applications, evidence.ApplicationID) || strings.TrimSpace(evidence.DeploymentEvidence) == "") {
			return invalid("composition component deployment evidence required")
		}
		seen[evidence.ApplicationID] = true
	}
	if len(applications) > 0 && len(seen) != len(applications) {
		return invalid("evidence must cover complete composition")
	}
	return nil
}

// scenarioCandidateReady never transfers shared-environment evidence to main.
// A new definition or failed rerun invalidates readiness, retaining old history.
func scenarioCandidateReady(st *domain.State, candidateID string) error {
	candidate := operationByID(st, candidateID)
	if candidate == nil || candidate.Kind != "RELEASE_CANDIDATE" || candidate.Status != "SUCCEEDED" {
		return invalid("successful release candidate required")
	}
	covered := map[string]bool{}
	for i := range st.ScenarioVersions {
		definition := &st.ScenarioVersions[i]
		if definition.ProductID != candidate.ProductID || !definition.Blocking || latestScenario(st, definition.ScenarioID).ID != definition.ID {
			continue
		}
		relevant := true
		for _, id := range definition.IntegrationIDs {
			if !slices.Contains(candidate.IntegrationIDs, id) {
				relevant = false
			}
		}
		if !relevant {
			continue
		}
		var latest *domain.ScenarioRun
		for j := range st.ScenarioRuns {
			run := &st.ScenarioRuns[j]
			if run.ScenarioVersionID == definition.ID && run.CandidateOperationID == candidateID && (latest == nil || !run.CreatedAt.Before(latest.CreatedAt)) {
				latest = run
			}
		}
		if latest == nil || latest.Result != "passed" {
			return invalid("blocking scenario %s needs passing candidate evidence", definition.Title)
		}
		candidateEvidence := *latest
		if err := validateScenarioTarget(st, definition, &candidateEvidence); err != nil {
			return err
		}
		for _, id := range definition.IntegrationIDs {
			covered[id] = true
		}
	}
	for _, id := range candidate.IntegrationIDs {
		if !covered[id] {
			return invalid("integration %s requires explicit blocking scenario coverage", id)
		}
	}
	return nil
}
