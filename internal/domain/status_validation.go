package domain

import (
	"fmt"
	"slices"
)

// ValidateStateStatuses checks Control Plane-owned state, including typed
// snapshots. Opaque provider responses and append-only event payloads retain
// their original vocabulary and are deliberately not interpreted as our state.
func ValidateStateStatuses(st State) error {
	if err := ValidateExecutionRecords(st); err != nil {
		return err
	}
	if err := ValidateRegistryInventories(st); err != nil {
		return err
	}
	if err := ValidateArtifactPins(st); err != nil {
		return err
	}
	if err := ValidateWorkerChecks(st); err != nil {
		return err
	}
	if err := ValidateRepositoryKinds(st); err != nil {
		return err
	}
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
	for _, v := range st.Deployments {
		if err := check("deployment", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.ArtifactSignatures {
		if err := check("signing", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.Builds {
		if err := check("signing", v.ID, v.SigningStatus, true); err != nil {
			return err
		}
		if err := check("build", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.WorkerJobs {
		if err := check("worker_job", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.WorkerAttempts {
		if err := check("worker_attempt", v.ID, v.Status, false); err != nil {
			return err
		}
	}
	for _, v := range st.RuntimeSnapshots {
		if err := check("runtime_comparison", v.ID, v.ReferenceComparison, true); err != nil {
			return err
		}
		if err := check("runtime_comparison", v.ID, v.Comparison, false); err != nil {
			return err
		}
		if err := check("runtime_health", v.ID, v.Runtime.Health, false); err != nil {
			return err
		}
	}
	for _, c := range st.CompositionConflicts {
		if err := check("composition_conflict", c.ID, c.Status, false); err != nil {
			return err
		}
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

func ValidateExecutionRecords(st State) error {
	known := map[string]Integration{}
	products := map[string]bool{}
	for _, p := range st.Products {
		products[p.ID] = true
	}
	for _, in := range st.Integrations {
		known[in.ID] = in
	}
	seen := map[string]bool{}
	states := map[string]bool{"unassigned": true, "running": true, "waiting_external": true, "blocked": true, "paused": true, "cancelled": true, "exited_incomplete": true, "lost": true, "stalled": true, "completed": true}
	for _, e := range st.IntegrationExecutions {
		in, ok := known[e.IntegrationID]
		if e.ID == "" || !ok || in.ProductID != e.ProductID || !products[e.ProductID] {
			return fmt.Errorf("execution %q has invalid Integration scope", e.IntegrationID)
		}
		if seen[e.IntegrationID] {
			return fmt.Errorf("duplicate execution for Integration %q", e.IntegrationID)
		}
		seen[e.IntegrationID] = true
		if !states[e.State] || e.DesiredState != "ready" && e.DesiredState != "released" {
			return fmt.Errorf("invalid execution state or desired state for %q", e.IntegrationID)
		}
		if e.LeaseTTLSeconds < 0 {
			return fmt.Errorf("invalid execution lease or fence for %q", e.IntegrationID)
		}
		if e.StalledTimeoutSeconds != nil && *e.StalledTimeoutSeconds < 0 {
			return fmt.Errorf("invalid stalled timeout for %q", e.IntegrationID)
		}
	}
	groups := map[string]bool{}
	for _, g := range st.ExecutionGroups {
		if g.ID == "" || groups[g.ID] || !products[g.ProductID] {
			return fmt.Errorf("duplicate or missing execution group id %q", g.ID)
		}
		groups[g.ID] = true
		members := map[string]bool{}
		for _, iid := range g.MemberIntegrationIDs {
			in, ok := known[iid]
			if !ok || in.ProductID != g.ProductID || members[iid] {
				return fmt.Errorf("execution group %q has invalid member scope", g.ID)
			}
			members[iid] = true
		}
		if len(g.Revisions) == 0 {
			return fmt.Errorf("execution group %q has no membership history", g.ID)
		}
		for i, revision := range g.Revisions {
			if revision.Revision != uint64(i+1) {
				return fmt.Errorf("execution group %q revision sequence mismatch", g.ID)
			}
			revisionMembers := map[string]bool{}
			for _, iid := range revision.MemberIntegrationIDs {
				in, ok := known[iid]
				if !ok || in.ProductID != g.ProductID || revisionMembers[iid] {
					return fmt.Errorf("execution group %q revision has invalid member scope", g.ID)
				}
				revisionMembers[iid] = true
			}
		}
		last := g.Revisions[len(g.Revisions)-1]
		if !slices.Equal(last.MemberIntegrationIDs, g.MemberIntegrationIDs) {
			return fmt.Errorf("execution group %q revision membership mismatch", g.ID)
		}
	}
	return nil
}
