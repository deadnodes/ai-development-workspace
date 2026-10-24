package application

import (
	"encoding/json"
	"reflect"
	"slices"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

type buildFailureAgentAction struct {
	Type          string                     `json:"type"`
	Status        string                     `json:"status"`
	Reason        string                     `json:"reason,omitempty"`
	ProductID     string                     `json:"product_id,omitempty"`
	OperationID   string                     `json:"operation_id,omitempty"`
	CompositionID string                     `json:"composition_id,omitempty"`
	Failures      []buildFailureAgentContext `json:"failures"`
	Instructions  []string                   `json:"instructions,omitempty"`
}

type buildFailureAgentContext struct {
	agentSourceContext
	ComponentID          string `json:"component_id"`
	DeliveryID           string `json:"delivery_id"`
	WorkerOperationID    string `json:"worker_operation_id"`
	JobID                string `json:"job_id"`
	AttemptID            string `json:"attempt_id"`
	Fence                uint64 `json:"fence"`
	InputHash            string `json:"input_hash"`
	SourceSHA            string `json:"source_sha"`
	RecipeID             string `json:"recipe_id"`
	RecipeRevision       string `json:"recipe_revision"`
	RecipeHash           string `json:"recipe_hash"`
	ConfigRevision       string `json:"config_revision"`
	ReportedResult       string `json:"reported_result"`
	ResolutionConflictID string `json:"selected_resolution_conflict_id,omitempty"`
	ResolutionSHA        string `json:"selected_resolution_sha,omitempty"`
}

func unavailableBuildFailureAction(op domain.ExternalOperation, reason string) *buildFailureAgentAction {
	v := &buildFailureAgentAction{Type: "INVESTIGATE_BUILD_FAILURE", Status: "UNAVAILABLE", Reason: reason, Failures: []buildFailureAgentContext{}}
	if conflictLabel(op.ProductID, 128) && conflictLabel(op.ID, 128) {
		v.ProductID, v.OperationID = op.ProductID, op.ID
	}
	return v
}

func uniqueBuildActionOperation(st *domain.State, id string) *domain.ExternalOperation {
	var found *domain.ExternalOperation
	for i := range st.Operations {
		if st.Operations[i].ID == id {
			if found != nil {
				return nil
			}
			found = &st.Operations[i]
		}
	}
	return found
}

func uniqueBuildActionJob(st *domain.State, id string) *domain.WorkerJob {
	var found *domain.WorkerJob
	for i := range st.WorkerJobs {
		if st.WorkerJobs[i].ID == id {
			if found != nil {
				return nil
			}
			found = &st.WorkerJobs[i]
		}
	}
	return found
}

// Report admission records the original input hash separately from the mutable
// job projection. Read that history without consulting current worker authority.
func buildFailureReportAdmission(st *domain.State, job domain.WorkerJob) (found, valid bool) {
	seen := false
	for _, event := range st.Events {
		if event.Action != "worker_report" || event.EntityID != job.ID {
			continue
		}
		var recorded struct {
			Status    string `json:"status"`
			Fence     uint64 `json:"fence"`
			InputHash string `json:"input_hash"`
		}
		data, err := json.Marshal(event.Data.Data)
		if err != nil || json.Unmarshal(data, &recorded) != nil || recorded.Fence == 0 {
			return true, false
		}
		if recorded.Fence != job.Fence {
			// An accepted report terminates the job. A later claim fence cannot
			// supersede that history; inconsistent projections invalidate the set.
			return true, false
		}
		if seen || event.ProductID != job.ProductID || event.FeatureID != job.FeatureID || event.Data.Action != event.Action || event.Data.ID != job.ID || event.Data.ProductID != job.ProductID || event.Data.FeatureID != job.FeatureID || (recorded.Status != "FAILED" && recorded.Status != "SUCCEEDED") || recorded.Status != job.Status || recorded.InputHash != job.InputHash || !workerconfig.ValidFingerprint(recorded.InputHash) {
			return true, false
		}
		seen, found = true, recorded.Status == "FAILED"
	}
	return found, true
}

// A failed status is insufficient: expiry and cancellation can lack an accepted
// report. This projection joins historical records; it never renews authority.
func operationBuildFailureAction(st *domain.State, op domain.ExternalOperation, now time.Time) *buildFailureAgentAction {
	if op.Kind != "COMPOSE" || op.Status == "CANCELLED" {
		return nil
	}
	unavailable := func() *buildFailureAgentAction {
		return unavailableBuildFailureAction(op, "BUILD_FAILURE_CONTEXT_UNAVAILABLE")
	}
	if uniqueBuildActionOperation(st, op.ID) == nil || len(op.ChildIDs) > 100 || len(op.PreparedDeployments) > 100 {
		return unavailable()
	}
	out := &buildFailureAgentAction{Type: "INVESTIGATE_BUILD_FAILURE", Status: "ACTION_REQUIRED", ProductID: op.ProductID, OperationID: op.ID, Failures: []buildFailureAgentContext{}}
	seen := map[string]bool{}
	seenJobs := map[string]bool{}
	for _, childID := range op.ChildIDs {
		child := uniqueBuildActionOperation(st, childID)
		if seen[childID] || child == nil || child.ParentID != op.ID || child.ProductID != op.ProductID {
			return unavailable()
		}
		seen[childID] = true
		if child.Native == nil || child.Native.BuildJobID == "" {
			continue
		}
		job := uniqueBuildActionJob(st, child.Native.BuildJobID)
		if job == nil {
			return unavailable()
		}
		seenJobs[job.ID] = true
		admitted, valid := buildFailureReportAdmission(st, *job)
		if !valid || admitted && (job.CancellationRequested || child.Status == "CANCELLED") {
			return unavailable()
		}
		if job.Status != "FAILED" || job.CancellationRequested || child.Status == "CANCELLED" {
			continue
		}
		var attempt *domain.WorkerAttempt
		for i := range st.WorkerAttempts {
			a := &st.WorkerAttempts[i]
			if a.JobID == job.ID && a.Fence == job.Fence {
				if attempt != nil {
					return unavailable()
				}
				attempt = a
			}
		}
		if attempt == nil || attempt.Report == nil {
			if admitted {
				return unavailable()
			}
			continue
		}
		if !admitted {
			return unavailable()
		}
		v, ok := buildFailureAgentDetails(st, op, *child, *job, *attempt, now)
		if !ok {
			return unavailable()
		}
		out.Failures = append(out.Failures, v)
	}
	// A listed-child join must not silently discard another failed native child.
	for _, child := range st.Operations {
		if child.ParentID == op.ID && child.ProductID == op.ProductID && child.Native != nil && !seen[child.ID] {
			if job := uniqueBuildActionJob(st, child.Native.BuildJobID); job != nil && job.Status == "FAILED" {
				return unavailable()
			}
		}
	}
	for _, job := range st.WorkerJobs {
		if job.ProductID == op.ProductID && job.Capability == "local_oci_build" && job.Status == "FAILED" && job.Inputs.SourceProof != nil && job.Inputs.SourceProof.ParentID == op.ID && !seenJobs[job.ID] {
			return unavailable()
		}
	}
	if len(out.Failures) == 0 {
		return nil
	}
	out.CompositionID = op.CompositionSnapshot.ID
	out.Instructions = []string{
		"The existing requesting agent investigates this accepted build report; RCP does not launch another agent.",
		"Cause is unclassified. CHECK_FAILED can include Docker execution errors; EXECUTOR_FAILED is a distinct reported outcome. Neither proves a semantic conflict or authorizes code edits.",
		"Inspect the exact job/attempt through get_worker_job and the delivery/worker operation IDs through get_operation. Use the frozen source SHA and ordered input SHAs, not current branch tips.",
		"Use resume and get_integration_context for every affected Feature/Integration. Those semantic goals, acceptance criteria and decisions are current context, not frozen execution snapshots. Preserve every side; ask the human when intent is incompatible.",
		"Retained refs are dated observations. This historical report, old lease and actor labels grant no new execution permission. Do not retry, reconcile, publish, change source or prepare GitOps merely because this response exists.",
		"Record the investigation and exact evidence through existing progress/discovery commands. Semantic-conflict registration and an independently reviewed resolution remain separate work; do not use textual conflict commands without a real conflict record.",
	}
	b, err := json.Marshal(out)
	if err != nil || len(b) > conflictAgentActionBytes {
		return unavailableBuildFailureAction(op, "BUILD_FAILURE_CONTEXT_TOO_LARGE")
	}
	return out
}

func buildFailureAgentDetails(st *domain.State, op, child domain.ExternalOperation, job domain.WorkerJob, attempt domain.WorkerAttempt, now time.Time) (buildFailureAgentContext, bool) {
	v := buildFailureAgentContext{}
	if op.CompositionSnapshot == nil || child.Kind != "DEPLOY" || child.BuildOnly || child.Snapshot == nil || child.Native == nil || job.Inputs.SourceProof == nil || job.Capability != "local_oci_build" || job.Inputs.Execution == nil || job.Inputs.Execution.Kind != "local_oci_build" || job.Inputs.Publication != nil || job.Inputs.Registry != nil || job.ProductID != op.ProductID || job.FeatureID != child.FeatureID || job.IntegrationID != child.IntegrationID || attempt.ProductID != job.ProductID || attempt.FeatureID != job.FeatureID || attempt.Status != "FAILED" || job.Fence == 0 || !workerconfig.ValidFingerprint(job.InputHash) || workerconfig.Fingerprint(job.Inputs) != job.InputHash || job.RequestHash != job.InputHash || nativeWorkerOrigin(st, &job) != nil {
		return v, false
	}
	workerOp := uniqueBuildActionOperation(st, job.OperationID)
	if workerOp == nil || workerOp.Kind != domain.WorkerOperationKind || workerOp.Status != "FAILED" || workerOp.Attempts <= 0 || uint64(workerOp.Attempts) != job.Fence || workerOp.ParentID != child.ID || workerOp.ProductID != op.ProductID || workerOp.FeatureID != job.FeatureID || workerOp.IntegrationID != job.IntegrationID || workerOp.RepositoryID != job.Inputs.RepositoryID {
		return v, false
	}
	for _, a := range st.WorkerAttempts {
		if a.ID == attempt.ID && (a.JobID != attempt.JobID || a.Fence != attempt.Fence) {
			return v, false
		}
	}
	report := attempt.Report
	want := domain.WorkerAttemptRequest{JobID: job.ID, AttemptID: attempt.ID, Fence: job.Fence, InputHash: job.InputHash}
	if report == nil || !reflect.DeepEqual(report.WorkerAttemptRequest, want) || attempt.InputHash != job.InputHash || (report.Result != "CHECK_FAILED" && report.Result != "EXECUTOR_FAILED") || report.LocalOCI != nil || report.Publication != nil || report.Registry != nil || report.EvidenceHash != "" && !workerconfig.ValidFingerprint(report.EvidenceHash) {
		return v, false
	}
	var prepared *domain.DeliverySnapshot
	for i := range op.PreparedDeployments {
		p := &op.PreparedDeployments[i]
		if p.Application.ID == child.ApplicationID {
			if prepared != nil {
				return v, false
			}
			prepared = p
		}
	}
	if prepared == nil || prepared.Native == nil || prepared.Build.Native == nil || prepared.Build.Mode != "NATIVE" || child.EnvironmentID != op.EnvironmentID || child.Snapshot.Application.ID != child.ApplicationID || job.Inputs.RecipeID != prepared.Build.Native.BuildRecipeID || job.Inputs.RecipeHash != prepared.Native.BuildRecipeHash || job.Inputs.RecipeRevision != prepared.Native.BuildRecipeRevision || job.Inputs.ConfigRevision != prepared.Native.ConfigRevision || !reflect.DeepEqual(job.Inputs.Parameters, prepared.Build.Native.Parameters) {
		return v, false
	}
	// Only generated revision/build-request fields differ from the frozen parent.
	snapshot := *child.Snapshot
	snapshot.Revision, snapshot.BuildRequest = prepared.Revision, prepared.BuildRequest
	native := *child.Native
	native.ParentHash, native.BuildJobID, native.PublicationJobID = "", "", ""
	if !reflect.DeepEqual(snapshot, *prepared) || !reflect.DeepEqual(&native, prepared.Native) {
		return v, false
	}
	var source *domain.FlowSource
	for i := range op.Sources {
		if op.Sources[i].RepositoryID == job.Inputs.RepositoryID {
			if source != nil {
				return v, false
			}
			source = &op.Sources[i]
		}
	}
	if source == nil || source.Result == nil || source.Result.Conflict || source.Result.SHA != job.Inputs.SourceSHA || child.Snapshot.Revision.Branch != source.Result.Branch || child.Snapshot.Revision.BaseCommit != source.Plan.BaseSHA || child.Snapshot.Application.RepositoryID != source.RepositoryID {
		return v, false
	}
	var ok bool
	v.agentSourceContext, ok = frozenAgentSourceDetails(op, *source, now)
	if !ok || (source.Plan.ResolutionSHA == "") != (source.Plan.ResolutionConflictID == "") {
		return v, false
	}
	if source.Plan.ResolutionSHA != "" && (!fullSHA.MatchString(source.Plan.ResolutionSHA) || !conflictLabel(source.Plan.ResolutionConflictID, 128) || source.Plan.ResolutionSHA != source.Result.SHA) {
		return v, false
	}
	for _, id := range []string{child.ApplicationID, child.ID, workerOp.ID, job.ID, attempt.ID, job.Inputs.RecipeID, job.Inputs.RecipeRevision, job.Inputs.ConfigRevision} {
		if !conflictLabel(id, 128) {
			return v, false
		}
	}
	if !workerconfig.ValidFingerprint(job.Inputs.RecipeHash) || !slices.Contains(op.ChildIDs, child.ID) {
		return v, false
	}
	v.ComponentID, v.DeliveryID, v.WorkerOperationID = child.ApplicationID, child.ID, workerOp.ID
	v.JobID, v.AttemptID, v.Fence, v.InputHash = job.ID, attempt.ID, job.Fence, job.InputHash
	v.SourceSHA, v.RecipeID, v.RecipeRevision, v.RecipeHash, v.ConfigRevision = job.Inputs.SourceSHA, job.Inputs.RecipeID, job.Inputs.RecipeRevision, job.Inputs.RecipeHash, job.Inputs.ConfigRevision
	v.ReportedResult = report.Result
	v.ResolutionConflictID, v.ResolutionSHA = source.Plan.ResolutionConflictID, source.Plan.ResolutionSHA
	return v, true
}
