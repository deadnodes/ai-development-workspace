package application

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

var workerRequestKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{15,127}$`)

// ConfigureWorkers is startup-only; the authority has an immutable private config.
func (s *Service) ConfigureWorkers(a *workerconfig.Authority) { s.externalWorkers = a }
func workerJob(st *domain.State, id string) *domain.WorkerJob {
	for i := range st.WorkerJobs {
		if st.WorkerJobs[i].ID == id {
			return &st.WorkerJobs[i]
		}
	}
	return nil
}
func workerAttempt(st *domain.State, id string) *domain.WorkerAttempt {
	for i := range st.WorkerAttempts {
		if st.WorkerAttempts[i].ID == id {
			return &st.WorkerAttempts[i]
		}
	}
	return nil
}
func workerTerminal(status string) bool {
	return status == "SUCCEEDED" || status == "FAILED" || status == "CANCELLED" || status == "DEADLINE_EXCEEDED"
}
func workerAudit(st *domain.State, j *domain.WorkerJob, actor, action string, now time.Time) {
	j.UpdatedAt = now
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: actor, At: now, EntityID: j.ID, ProductID: j.ProductID, FeatureID: j.FeatureID, Data: domain.Command{Action: action, Actor: actor, ID: j.ID, ProductID: j.ProductID, FeatureID: j.FeatureID, Data: map[string]any{"status": j.Status, "fence": j.Fence, "input_hash": j.InputHash}}})
	if op := operationByID(st, j.OperationID); op != nil {
		op.Status = "RUNNING"
		if j.Status == "QUEUED" {
			op.Status = "PENDING"
		}
		if workerTerminal(j.Status) {
			op.Status = j.Status
			if j.Status == "DEADLINE_EXCEEDED" {
				op.Status = "FAILED"
			}
		}
		op.Attempts = int(j.Fence)
		op.Phase = "EXTERNAL_" + j.Status
		op.UpdatedAt = now
		op.LeaseOwner = ""
		op.LeaseUntil = time.Time{}
		op.Error, op.Detail = "", ""
		if j.Status == "RECONCILIATION_REQUIRED" {
			op.Error = "WORKER_RECONCILIATION_REQUIRED"
			op.Detail = "External execution requires explicit reconciliation; automatic replay is disabled"
		}
		if workerTerminal(j.Status) {
			t := now
			op.FinishedAt = &t
		} else {
			op.FinishedAt = nil
		}
	}
}
func (s *Service) executeWorkerCommand(ctx context.Context, c domain.Command) (any, error) {
	if s.externalWorkers == nil {
		return nil, invalid("external workers disabled")
	}
	if strings.TrimSpace(c.Actor) == "" {
		return nil, invalid("actor required")
	}
	var out any
	err := s.store.Update(ctx, func(st *domain.State) error {
		now := time.Now().UTC()
		if c.Action != "queue_worker_job" {
			if c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
				return invalid("unexpected worker command scope")
			}
			j := workerJob(st, c.ID)
			if j == nil {
				return missing("worker job", c.ID)
			}
			if c.ProductID != j.ProductID {
				return invalid("worker job outside product")
			}
			switch c.Action {
			case "cancel_worker_job":
				var empty struct{}
				if err := decode(c.Data, &empty); err != nil {
					return err
				}
				if workerTerminal(j.Status) {
					return invalid("job already terminal")
				}
				j.CancellationRequested = true
				if j.Status == "RUNNING" || j.Status == "RECONCILIATION_REQUIRED" {
					j.Status = "RECONCILIATION_REQUIRED"
				} else {
					j.Status = "CANCELLED"
				}
				if a := currentWorkerAttempt(st, j); a != nil {
					a.Status = j.Status
					a.UpdatedAt = now
				}
			case "reconcile_worker_job":
				var in struct {
					Outcome string `json:"outcome"`
				}
				if err := decode(c.Data, &in); err != nil {
					return err
				}
				if j.Status != "RECONCILIATION_REQUIRED" || in.Outcome != "FAILED" && in.Outcome != "CANCELLED" {
					return invalid("explicit reconciliation requires FAILED or CANCELLED")
				}
				j.Status = in.Outcome
				if a := currentWorkerAttempt(st, j); a != nil {
					a.Status = in.Outcome
					a.UpdatedAt = now
				}
			default:
				return invalid("unknown worker action")
			}
			workerAudit(st, j, c.Actor, c.Action, now)
			out = *j
			return nil
		}
		if c.ID != "" || c.FeatureID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
			return invalid("unexpected queue metadata")
		}
		var in struct {
			RequestKey     string         `json:"request_key"`
			RecipeID       string         `json:"recipe_id"`
			RecipeRevision string         `json:"recipe_revision"`
			RecipeHash     string         `json:"recipe_hash"`
			ConfigRevision string         `json:"config_revision"`
			RevisionID     string         `json:"revision_id"`
			SourceSHA      string         `json:"source_sha"`
			Parameters     map[string]any `json:"parameters"`
			Deadline       time.Time      `json:"deadline"`
			MaxAttempts    int            `json:"max_attempts"`
		}
		if err := decode(c.Data, &in); err != nil {
			return err
		}
		if !workerRequestKey.MatchString(in.RequestKey) {
			return invalid("request_key must contain 16..128 identifier characters")
		}
		requestHash := workerconfig.Fingerprint(struct {
			ProductID, IntegrationID, Actor string
			Request                         any
		}{c.ProductID, c.IntegrationID, c.Actor, in})
		// Retry lookup precedes current deadline/configuration checks: a lost
		// response must never turn into another execution after state changes.
		for _, previous := range st.WorkerJobs {
			if previous.ProductID == c.ProductID && previous.RequestKey == in.RequestKey {
				if previous.RequestHash != requestHash {
					return invalid("request_key already names different immutable inputs")
				}
				out = previous
				return nil
			}
		}
		recipe, ok := s.externalWorkers.Recipe(in.RecipeID)
		if !ok || in.RecipeRevision != recipe.Revision || in.RecipeHash != recipe.Hash || in.ConfigRevision != s.externalWorkers.Revision() || recipe.ProductID != c.ProductID {
			return invalid("recipe or configuration identity mismatch")
		}
		if err := workerconfig.ValidateParameters(recipe, in.Parameters); err != nil {
			return invalid("invalid typed recipe parameters")
		}
		if in.MaxAttempts < 1 || in.MaxAttempts > 10 || !in.Deadline.After(now) || in.Deadline.After(now.Add(time.Hour)) {
			return invalid("worker bounds: deadline within one hour, attempts 1..10")
		}
		integration := integration(st, c.IntegrationID)
		if integration == nil || integration.ProductID != c.ProductID {
			return invalid("integration outside product")
		}
		var rev *domain.IntegrationRevision
		for i := range st.IntegrationRevisions {
			if st.IntegrationRevisions[i].ID == in.RevisionID {
				rev = &st.IntegrationRevisions[i]
			}
		}
		if rev == nil || rev.ProductID != c.ProductID || rev.IntegrationID != integration.ID || rev.RepositoryID != recipe.RepositoryID || rev.HeadCommit != in.SourceSHA || !fullSHA.MatchString(in.SourceSHA) {
			return invalid("captured source identity mismatch")
		}
		matched := false
		for _, repo := range st.Repositories {
			if repo.ID == recipe.RepositoryID && repo.ProductID == c.ProductID && repo.URL == recipe.RepositoryURL {
				matched = true
			}
		}
		if !matched {
			return invalid("configured repository identity mismatch")
		}
		m := domain.Meta{ID: id(), ProductID: c.ProductID, FeatureID: integration.FeatureID, Actor: c.Actor, CreatedAt: now, UpdatedAt: now}
		inputs := domain.WorkerInputs{RecipeID: recipe.ID, RecipeRevision: recipe.Revision, RecipeHash: recipe.Hash, ConfigRevision: in.ConfigRevision, RepositoryID: recipe.RepositoryID, RepositoryURL: recipe.RepositoryURL, RevisionID: rev.ID, SourceSHA: rev.HeadCommit, Trust: recipe.Trust, Parameters: in.Parameters, Execution: recipe.Execution}
		op := domain.ExternalOperation{Meta: m, Kind: domain.WorkerOperationKind, IntegrationID: integration.ID, RepositoryID: recipe.RepositoryID, RequestedBy: c.Actor, Status: "PENDING", Phase: "EXTERNAL_QUEUED"}
		m.ID = id()
		j := domain.WorkerJob{Meta: m, OperationID: op.ID, RequestKey: in.RequestKey, RequestHash: requestHash, IntegrationID: integration.ID, Capability: recipe.Capability, Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: in.Deadline, MaxAttempts: in.MaxAttempts}
		st.Operations = append(st.Operations, op)
		st.WorkerJobs = append(st.WorkerJobs, j)
		workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], c.Actor, c.Action, now)
		out = j
		return nil
	})
	return out, err
}
func currentWorkerAttempt(st *domain.State, j *domain.WorkerJob) *domain.WorkerAttempt {
	for i := range st.WorkerAttempts {
		a := &st.WorkerAttempts[i]
		if a.JobID == j.ID && a.Fence == j.Fence {
			return a
		}
	}
	return nil
}
func (s *Service) workerConfigMatches(j *domain.WorkerJob) bool {
	r, ok := s.externalWorkers.Recipe(j.Inputs.RecipeID)
	return ok && r.Hash == j.Inputs.RecipeHash && r.Revision == j.Inputs.RecipeRevision && r.ProductID == j.ProductID && r.RepositoryID == j.Inputs.RepositoryID && r.RepositoryURL == j.Inputs.RepositoryURL && r.Trust == j.Inputs.Trust && r.Capability == j.Capability && s.externalWorkers.Revision() == j.Inputs.ConfigRevision && workerconfig.Fingerprint(j.Inputs) == j.InputHash
}
func recoverWorker(st *domain.State, j *domain.WorkerJob, now time.Time) {
	if workerTerminal(j.Status) || j.Status == "RECONCILIATION_REQUIRED" {
		return
	}
	a := currentWorkerAttempt(st, j)
	expired := (j.Status == "CLAIMED" || j.Status == "RUNNING") && a != nil && !now.Before(a.LeaseUntil)
	if !now.Before(j.Deadline) || expired {
		switch j.Status {
		case "RUNNING":
			j.Status = "RECONCILIATION_REQUIRED"
		case "CLAIMED":
			if now.Before(j.Deadline) && j.Fence < uint64(j.MaxAttempts) {
				j.Status = "QUEUED"
			} else {
				j.Status = "FAILED"
			}
		case "QUEUED":
			j.Status = "DEADLINE_EXCEEDED"
		}
		if a != nil {
			a.Status = "EXPIRED"
			a.UpdatedAt = now
		}
		workerAudit(st, j, "system/worker-recovery", "worker_expired", now)
	}
}
func (s *Service) ClaimWorker(ctx context.Context, p workerconfig.Principal, in domain.WorkerClaim) (*domain.WorkerLease, error) {
	if s.externalWorkers == nil {
		return nil, invalid("external workers disabled")
	}
	now := time.Now().UTC()
	if !p.Allows(in.ProductID, in.RepositoryID, in.Capability, now) {
		return nil, invalid("worker scope denied")
	}
	var out *domain.WorkerLease
	err := s.store.Update(ctx, func(st *domain.State) error {
		now = time.Now().UTC()
		if !p.Allows(in.ProductID, in.RepositoryID, in.Capability, now) {
			return invalid("worker scope expired")
		}
		for i := range st.WorkerJobs {
			j := &st.WorkerJobs[i]
			if j.ProductID != in.ProductID || j.Inputs.RepositoryID != in.RepositoryID || j.Capability != in.Capability {
				continue
			}
			recoverWorker(st, j, now)
			if j.Status != "QUEUED" || out != nil {
				continue
			}
			if !s.workerConfigMatches(j) {
				j.Status = "RECONCILIATION_REQUIRED"
				workerAudit(st, j, "system/worker-recovery", "worker_configuration_changed", now)
				continue
			}
			if j.Fence >= uint64(j.MaxAttempts) {
				j.Status = "FAILED"
				workerAudit(st, j, "system/worker-recovery", "worker_attempts_exhausted", now)
				continue
			}
			j.Fence++
			j.Status = "CLAIMED"
			until := now.Add(s.externalWorkers.Lease())
			if until.After(j.Deadline) {
				until = j.Deadline
			}
			a := domain.WorkerAttempt{Meta: domain.Meta{ID: id(), ProductID: j.ProductID, FeatureID: j.FeatureID, Actor: p.ID(), CreatedAt: now, UpdatedAt: now}, JobID: j.ID, Principal: p.ID(), Fence: j.Fence, InputHash: j.InputHash, LeaseUntil: until, Status: "CLAIMED"}
			st.WorkerAttempts = append(st.WorkerAttempts, a)
			workerAudit(st, j, p.ID(), "worker_claimed", now)
			out = &domain.WorkerLease{Job: *j, Attempt: a}
		}
		return nil
	})
	return out, err
}
func (s *Service) mutateWorker(ctx context.Context, p workerconfig.Principal, in domain.WorkerAttemptRequest, action string, report *domain.WorkerReport) (domain.WorkerLease, error) {
	var out domain.WorkerLease
	if s.externalWorkers == nil {
		return out, invalid("external workers disabled")
	}
	var rejected error
	err := s.store.Update(ctx, func(st *domain.State) error {
		now := time.Now().UTC()
		j := workerJob(st, in.JobID)
		a := workerAttempt(st, in.AttemptID)
		if j == nil || a == nil || a.JobID != j.ID || a.Fence != in.Fence || j.Fence != in.Fence || a.InputHash != in.InputHash || j.InputHash != in.InputHash || a.Principal != p.ID() || !p.Allows(j.ProductID, j.Inputs.RepositoryID, j.Capability, now) {
			return invalid("worker ownership or scope denied")
		}
		if report != nil && a.Report != nil {
			if reflect.DeepEqual(a.Report, report) {
				out = domain.WorkerLease{Job: *j, Attempt: *a}
				return nil
			}
			return invalid("conflicting report replay")
		}
		recoverWorker(st, j, now)
		if !s.workerConfigMatches(j) || !now.Before(a.LeaseUntil) || !now.Before(j.Deadline) || j.CancellationRequested || workerTerminal(j.Status) || j.Status == "RECONCILIATION_REQUIRED" {
			rejected = invalid("worker lease or immutable inputs invalid")
			return nil
		}
		switch action {
		case "start":
			if j.Status == "RUNNING" && a.Status == "RUNNING" {
				out = domain.WorkerLease{Job: *j, Attempt: *a}
				return nil
			}
			if j.Status != "CLAIMED" {
				return invalid("start requires CLAIMED")
			}
			j.Status = "RUNNING"
			a.Status = "RUNNING"
		case "heartbeat":
			if j.Status != "CLAIMED" && j.Status != "RUNNING" {
				return invalid("heartbeat requires active attempt")
			}
			until := now.Add(s.externalWorkers.Lease())
			if until.After(j.Deadline) {
				until = j.Deadline
			}
			a.LeaseUntil = until
		case "report":
			if j.Status != "RUNNING" {
				return invalid("report requires RUNNING")
			}
			if report.Result != "PASSED" && report.Result != "CHECK_FAILED" && report.Result != "EXECUTOR_FAILED" || report.EvidenceHash != "" && !workerconfig.ValidFingerprint(report.EvidenceHash) {
				return invalid("invalid bounded worker result")
			}
			if err := validateWorkerLocalOCI(j, a, report); err != nil {
				return err
			}
			j.Status = "FAILED"
			if report.Result == "PASSED" {
				j.Status = "SUCCEEDED"
			}
			a.Status = j.Status
			copy := *report
			a.Report = &copy
		default:
			return invalid("unknown worker action")
		}
		a.UpdatedAt = now
		workerAudit(st, j, p.ID(), "worker_"+action, now)
		out = domain.WorkerLease{Job: *j, Attempt: *a}
		return nil
	})
	if err == nil {
		err = rejected
	}
	return out, err
}

func validateWorkerLocalOCI(j *domain.WorkerJob, a *domain.WorkerAttempt, r *domain.WorkerReport) error {
	v := r.LocalOCI
	if r.Result != "PASSED" || j.Capability != "local_oci_build" {
		if v != nil {
			return invalid("unexpected local OCI result")
		}
		return nil
	}
	e := j.Inputs.Execution
	if v == nil || e == nil || v.JobID != j.ID || v.AttemptID != a.ID || v.InputHash != j.InputHash || v.SourceSHA != j.Inputs.SourceSHA || v.RecipeHash != j.Inputs.RecipeHash || v.ConfigRevision != j.Inputs.ConfigRevision || v.ToolchainImage != e.ToolchainImage || v.Platform != e.Platform || v.ArchiveBytes < 1 || v.ArchiveBytes > e.OutputBytes {
		return invalid("local OCI provenance mismatch")
	}
	for _, d := range []string{v.ArchiveHash, v.LayoutHash, v.IndexDigest, v.ManifestDigest} {
		if !workerconfig.ValidFingerprint(d) {
			return invalid("invalid local OCI identity")
		}
	}
	return nil
}
func (s *Service) StartWorker(ctx context.Context, p workerconfig.Principal, in domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	return s.mutateWorker(ctx, p, in, "start", nil)
}
func (s *Service) HeartbeatWorker(ctx context.Context, p workerconfig.Principal, in domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	return s.mutateWorker(ctx, p, in, "heartbeat", nil)
}
func (s *Service) ReportWorker(ctx context.Context, p workerconfig.Principal, in domain.WorkerReport) (domain.WorkerLease, error) {
	return s.mutateWorker(ctx, p, in.WorkerAttemptRequest, "report", &in)
}
