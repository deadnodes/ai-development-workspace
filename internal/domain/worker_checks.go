package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"releasecontrol/internal/delivery"
	"slices"
	"strings"
	"time"
)

func checkHash(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}
func ValidWorkerCheckBinding(c *WorkerCheckBinding) bool {
	return c != nil && regexp.MustCompile(`^RCP /[A-Za-z0-9 ._/-]{1,96}$`).MatchString(c.Name) && c.ExpectedAppID > 0 && c.FinalizeSeconds >= 60 && c.FinalizeSeconds <= 600
}
func validCheckPayload(w delivery.CheckRunWrite) bool {
	if !slices.Contains([]string{"queued", "in_progress", "completed"}, w.Status) || len(w.Output.Title) > 256 || len(w.Output.Summary) > 4096 || strings.ContainsAny(w.Output.Title, "\r\n\x00") || strings.ContainsAny(w.Output.Summary, "\r\x00") {
		return false
	}
	if !w.StartedAt.IsZero() && w.StartedAt.Nanosecond() != 0 || !w.CompletedAt.IsZero() && w.CompletedAt.Nanosecond() != 0 {
		return false
	}
	switch w.Status {
	case "queued":
		return w.Conclusion == "" && w.StartedAt.IsZero() && w.CompletedAt.IsZero()
	case "in_progress":
		return w.Conclusion == "" && !w.StartedAt.IsZero() && w.CompletedAt.IsZero()
	case "completed":
		return slices.Contains([]string{"success", "failure", "cancelled", "timed_out"}, w.Conclusion) && !w.StartedAt.IsZero() && !w.CompletedAt.IsZero() && !w.CompletedAt.Before(w.StartedAt)
	}
	return false
}

func safeCheckOutput(w delivery.CheckRunWrite, p *CheckPublication) bool {
	title := w.Status
	if w.Conclusion != "" {
		title = w.Conclusion
	}
	if w.Output.Title != "RCP checks/build: "+title {
		return false
	}
	for _, status := range []string{"QUEUED", "CLAIMED", "RUNNING", "RECONCILIATION_REQUIRED", "SUCCEEDED", "FAILED", "CANCELLED", "DEADLINE_EXCEEDED"} {
		if w.Output.Summary == fmt.Sprintf("Worker %s\nSource %s\nInputs %s\nRecipe %s\nResult %s", p.JobID, p.Inputs.SourceSHA, p.InputHash, p.Inputs.RecipeHash, status) {
			return true
		}
	}
	return false
}

func checkSuccessProvenance(st State, j WorkerJob) bool {
	if j.Status != "SUCCEEDED" || j.CancellationRequested {
		return false
	}
	for _, a := range st.WorkerAttempts {
		if a.JobID != j.ID || a.Fence != j.Fence {
			continue
		}
		r := a.Report
		if a.Status != "SUCCEEDED" || a.InputHash != j.InputHash || a.UpdatedAt.After(j.Deadline) || r == nil || r.Result != "PASSED" || r.JobID != j.ID || r.AttemptID != a.ID || r.Fence != a.Fence || r.InputHash != j.InputHash || r.LocalOCI == nil || r.Publication != nil || j.Inputs.Execution == nil {
			return false
		}
		v := r.LocalOCI
		e := j.Inputs.Execution
		if v.JobID != j.ID || v.AttemptID != a.ID || v.InputHash != j.InputHash || v.SourceSHA != j.Inputs.SourceSHA || v.RecipeHash != j.Inputs.RecipeHash || v.ConfigRevision != j.Inputs.ConfigRevision || v.ToolchainImage != e.ToolchainImage || v.Platform != e.Platform || v.ArchiveBytes < 1 || v.ArchiveBytes > e.OutputBytes || !reflect.DeepEqual(v.RuntimeRootFS, e.RuntimeRootFS) {
			return false
		}
		for _, hash := range []string{v.ArchiveHash, v.LayoutHash, v.IndexDigest, v.ManifestDigest} {
			if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(hash) {
				return false
			}
		}
		return true
	}
	return false
}

// ValidateWorkerChecks protects typed references and durable provider intents
// on every store update/reopen and backup import. It grants no execution rights.
func ValidateWorkerChecks(st State) error {
	fail := func() error { return fmt.Errorf("invalid worker check publication references or intent") }
	jobs := map[string]WorkerJob{}
	ops := map[string]ExternalOperation{}
	for _, j := range st.WorkerJobs {
		jobs[j.ID] = j
	}
	for _, o := range st.Operations {
		ops[o.ID] = o
	}
	for _, j := range st.WorkerJobs {
		if j.Inputs.Check == nil {
			if j.CheckOperationID != "" {
				return fail()
			}
			continue
		}
		if !ValidWorkerCheckBinding(j.Inputs.Check) || j.Capability != "local_oci_build" || j.Inputs.Trust != "trusted_source" || len(j.Inputs.Parameters) != 0 || j.CheckOperationID == "" {
			return fail()
		}
		child, ok := ops[j.CheckOperationID]
		if !ok || child.Kind != "CHECK_PUBLICATION" || child.CheckPublication == nil || child.CheckPublication.JobID != j.ID {
			return fail()
		}
	}
	for _, o := range st.Operations {
		p := o.CheckPublication
		if o.Kind != "CHECK_PUBLICATION" {
			if p != nil {
				return fail()
			}
			continue
		}
		if p == nil {
			return fail()
		}
		j, ok := jobs[p.JobID]
		if !ok || j.CheckOperationID != o.ID || j.OperationID != p.WorkerOperationID || o.ParentID != p.WorkerOperationID || j.ProductID != o.ProductID || j.FeatureID != o.FeatureID || j.InputHash != p.InputHash || checkHash(p.Inputs) != p.InputHash || !reflect.DeepEqual(j.Inputs, p.Inputs) || !ValidWorkerCheckBinding(p.Inputs.Check) {
			return fail()
		}
		parent, ok := ops[p.WorkerOperationID]
		if !ok || parent.Kind != WorkerOperationKind || parent.ProductID != o.ProductID || !slices.Contains(parent.ChildIDs, o.ID) {
			return fail()
		}
		identity := p.Identity
		if !regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(identity.HeadSHA) || identity.RepositoryID < 1 || p.ProviderRepositoryID != identity.RepositoryID || identity.Repository != p.Source.FullName || identity.AppID != p.Connection.AppID || identity.AppID != p.Inputs.Check.ExpectedAppID || identity.HeadSHA != p.Inputs.SourceSHA || identity.Name != p.Inputs.Check.Name || identity.ExternalID != "rcp:"+o.ID || p.Source.ProductID != o.ProductID || p.Source.RepositoryID != p.Inputs.RepositoryID || p.Source.ConnectionID != p.Connection.ID || p.Observations < 0 || p.Observations > 64 || !p.FinalizeDeadline.Equal(j.Deadline.Add(time.Duration(p.Inputs.Check.FinalizeSeconds)*time.Second)) {
			return fail()
		}
		if !slices.Contains([]string{"CHECK_CREATE_READY", "CHECK_CREATE_UNCERTAIN", "CHECK_OBSERVE", "CHECK_UPDATE_UNCERTAIN"}, o.Phase) {
			return fail()
		}
		if p.Intent != nil {
			if p.IntentHash != checkHash(*p.Intent) || !reflect.DeepEqual(p.Intent.Identity, identity) || !validCheckPayload(*p.Intent) || !safeCheckOutput(*p.Intent, p) {
				return fail()
			}
		} else if p.IntentHash != "" || o.Phase != "CHECK_CREATE_READY" {
			return fail()
		}
		if p.Observed != nil {
			v := p.Observed
			if !safeCheckOutput(delivery.CheckRunWrite{Status: v.Status, Conclusion: v.Conclusion, Output: v.Output}, p) {
				return fail()
			}
			if !v.Found || v.ID < 1 || !reflect.DeepEqual(v.Identity, identity) || !validCheckPayload(delivery.CheckRunWrite{Identity: v.Identity, ID: v.ID, Status: v.Status, Conclusion: v.Conclusion, StartedAt: v.StartedAt, CompletedAt: v.CompletedAt, Output: v.Output}) {
				return fail()
			}
		}
		if (p.Intent != nil && p.Intent.Conclusion == "success" || p.Observed != nil && p.Observed.Conclusion == "success") && !checkSuccessProvenance(st, j) {
			return fail()
		}
		if o.Phase == "CHECK_CREATE_READY" && (p.Intent != nil || p.Observed != nil || p.Observations != 0) {
			return fail()
		}
		if o.Phase == "CHECK_CREATE_UNCERTAIN" && (p.Intent == nil || p.Intent.Status != "queued" || p.Intent.ID != 0 || p.Observed != nil) {
			return fail()
		}
		if (o.Phase == "CHECK_OBSERVE" || o.Phase == "CHECK_UPDATE_UNCERTAIN") && (p.Intent == nil || p.Observed == nil) {
			return fail()
		}
		if o.Phase == "CHECK_UPDATE_UNCERTAIN" && p.Intent.ID != p.Observed.ID {
			return fail()
		}
		if o.Status == "SUCCEEDED" && (p.Observed == nil || p.Intent == nil || p.Observed.Status != "completed" || p.Intent.Status != "completed" || p.Observed.Conclusion != p.Intent.Conclusion || p.Observed.Output != p.Intent.Output || !p.Observed.StartedAt.Equal(p.Intent.StartedAt) || !p.Observed.CompletedAt.Equal(p.Intent.CompletedAt)) {
			return fail()
		}
	}
	return nil
}
