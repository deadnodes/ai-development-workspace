package domain

import (
	"fmt"
	"slices"
)

// ValidateStateStatuses checks Control Plane-owned state, including typed
// snapshots. Opaque provider responses and append-only event payloads retain
// their original vocabulary and are deliberately not interpreted as our state.
func ValidateStateStatuses(st State) error {
	catalog := StatusCatalog()
	check := func(kind, id, value string, optional bool) error {
		if value == "" && optional {
			return nil
		}
		if !slices.Contains(catalog[kind], value) {
			return fmt.Errorf("invalid %s status %q for entity %q", kind, value, id)
		}
		return nil
	}
	integration := func(v Integration, scope string) error {
		if err := check("integration", scope+v.ID, v.Status, false); err != nil {
			return err
		}
		for _, pr := range v.PullRequests {
			if err := check("pull_request", scope+v.ID+"/"+pr.ID, pr.Status, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, v := range st.Features {
		if err := check("feature", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.Integrations {
		if err := integration(v, ""); err != nil {
			return err
		}
	}
	for _, v := range st.Memories {
		if v.Kind == "blocker" {
			if err := check("blocker", v.ID, v.Status, false); err != nil {
				return err
			}
		} else if v.Status != "" {
			return fmt.Errorf("non-blocker memory entity %q cannot carry status %q", v.ID, v.Status)
		}
	}
	for _, v := range st.Findings {
		if err := check("finding", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.Results {
		if err := check("check_result", v.ID, v.Result, false); err != nil {
			return err
		}
	}
	for _, v := range st.Gates {
		if err := check("gate_result", v.ID, v.Result, false); err != nil {
			return err
		}
	}
	for _, v := range st.Compositions {
		if err := check("composition", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.Releases {
		if err := check("release", v.ID, v.Status, false); err != nil {
			return err
		}
		for _, snapshot := range v.Snapshots {
			if err := integration(snapshot, v.ID+"/snapshot/"); err != nil {
				return err
			}
		}
	}
	for _, v := range st.Operations {
		if err := check("operation", v.ID, v.Status, false); err != nil {
			return err
		}
		if err := check("deployment_state", v.ID, v.DeploymentState, true); err != nil {
			return err
		}
		if v.CompositionSnapshot != nil {
			if err := check("composition", v.ID+"/snapshot/"+v.CompositionSnapshot.ID, v.CompositionSnapshot.Status, false); err != nil {
				return err
			}
		}
	}
	for _, v := range st.OperationSteps {
		if err := check("operation_step", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.ReviewSyncs {
		if err := check("review_sync", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.GitObservations {
		if err := check("git_observation", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.DeliveryArtifacts {
		if err := check("artifact_availability", v.ID, v.Availability, false); err != nil {
			return err
		}
	}
	for _, v := range st.ScenarioRuns {
		if err := check("scenario_result", v.ID, v.Result, false); err != nil {
			return err
		}
	}
	return nil
}
