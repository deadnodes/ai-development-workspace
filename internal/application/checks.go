package application

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"time"
)

const checkObservationBudget = 64

// checkAPIIdentity is deliberately local to check ownership. Frozen connection
// snapshots still require exact equality at every admission and I/O boundary.
func checkAPIIdentity(raw string) (string, error) {
	if raw == "" {
		raw = "https://api.github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", invalid("invalid check provider API identity")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" && u.Scheme == "https" || port == "80" && u.Scheme == "http" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
func checkAPISame(a, b string) bool {
	x, e := checkAPIIdentity(a)
	if e != nil {
		return false
	}
	y, e := checkAPIIdentity(b)
	return e == nil && x == y
}
func checkRegisteredConnection(r domain.RegisteredRepository, connectionID string) bool {
	if r.ConnectionID == connectionID {
		return true
	}
	for _, id := range r.ConnectionIDs {
		if id == connectionID {
			return true
		}
	}
	return false
}
func checkPhysical(st *domain.State, product, repository string) (*domain.RepositoryBinding, *domain.GitHubConnection, int64, error) {
	source := repositoryBinding(st, repository)
	if source == nil || source.ProductID != product || source.Role != "APPLICATION" || !repositoryName.MatchString(source.FullName) {
		return nil, nil, 0, invalid("check source repository binding unavailable")
	}
	conn, e := scopedConnection(st, source.ConnectionID, product)
	if e != nil {
		return nil, nil, 0, e
	}
	for _, repo := range st.Repositories {
		if repo.ID != repository || repo.ProductID != product {
			continue
		}
		for _, registered := range st.RegisteredRepositories {
			if registered.ID == repo.RegisteredRepositoryID && registered.ProviderRepositoryID > 0 && strings.EqualFold(registered.FullName, source.FullName) && checkAPISame(registered.ProviderAPIURL, conn.Config.APIURL) && checkRegisteredConnection(registered, source.ConnectionID) {
				return source, conn, registered.ProviderRepositoryID, nil
			}
		}
	}
	return nil, nil, 0, invalid("check requires registered physical repository identity")
}
func (s *Service) checkOwnership(st *domain.State, j *domain.WorkerJob, source domain.RepositoryBinding, conn delivery.Connection, physical int64) error {
	for _, r := range s.externalWorkers.CheckRecipes() {
		if r.ID == j.Inputs.RecipeID && r.ProductID == j.ProductID {
			continue
		}
		if r.Check.Name != j.Inputs.Check.Name || r.Check.ExpectedAppID != j.Inputs.Check.ExpectedAppID {
			continue
		}
		other := repositoryBinding(st, r.RepositoryID)
		if other == nil {
			continue
		}
		oc := connection(st, other.ConnectionID)
		if oc == nil || !checkAPISame(oc.Config.APIURL, conn.APIURL) {
			continue
		}
		number := int64(0)
		for _, repo := range st.Repositories {
			if repo.ID == r.RepositoryID && repo.ProductID == r.ProductID {
				for _, registered := range st.RegisteredRepositories {
					if registered.ID == repo.RegisteredRepositoryID && checkAPISame(registered.ProviderAPIURL, conn.APIURL) {
						number = registered.ProviderRepositoryID
					}
				}
			}
		}
		if number == physical || strings.EqualFold(other.FullName, source.FullName) {
			return invalid("physical repository check name owned by another configured recipe")
		}
	}
	return nil
}
func (s *Service) queueWorkerCheck(st *domain.State, jobID string) error {
	j := workerJob(st, jobID)
	if j == nil {
		return invalid("check worker job missing")
	}
	if j.Inputs.Check == nil {
		return nil
	}
	if _, ok := s.provider.(delivery.CheckRunProvider); !ok {
		return invalid("check publication provider unavailable")
	}
	source, conn, physical, e := checkPhysical(st, j.ProductID, j.Inputs.RepositoryID)
	if e != nil {
		return e
	}
	if conn.Config.AppID != j.Inputs.Check.ExpectedAppID {
		return invalid("check GitHub App identity mismatch")
	}
	if e = s.checkOwnership(st, j, *source, conn.Config, physical); e != nil {
		return e
	}
	if proof := j.Inputs.SourceProof; proof != nil {
		origin := operationByID(st, proof.DeliveryID)
		if origin == nil || origin.Snapshot == nil || !reflect.DeepEqual(origin.Snapshot.Source, *source) || !reflect.DeepEqual(origin.Snapshot.SourceConnection, conn.Config) {
			return invalid("check native source authority mismatch")
		}
	}
	parent := operationByID(st, j.OperationID)
	if parent == nil || parent.Kind != domain.WorkerOperationKind || parent.ProductID != j.ProductID {
		return invalid("check worker operation missing")
	}
	now := time.Now().UTC()
	m := domain.Meta{ID: id(), ProductID: j.ProductID, FeatureID: j.FeatureID, Actor: "system/check-publication", CreatedAt: now, UpdatedAt: now}
	identity := delivery.CheckRunIdentity{Repository: source.FullName, RepositoryID: physical, HeadSHA: j.Inputs.SourceSHA, Name: j.Inputs.Check.Name, ExternalID: "rcp:" + m.ID, AppID: conn.Config.AppID}
	cp := &domain.CheckPublication{JobID: j.ID, WorkerOperationID: j.OperationID, Inputs: j.Inputs, InputHash: j.InputHash, Source: *source, Connection: conn.Config, ProviderRepositoryID: physical, Identity: identity, FinalizeDeadline: j.Deadline.Add(time.Duration(j.Inputs.Check.FinalizeSeconds) * time.Second)}
	child := domain.ExternalOperation{Meta: m, Kind: "CHECK_PUBLICATION", ParentID: parent.ID, IntegrationID: j.IntegrationID, RepositoryID: j.Inputs.RepositoryID, RequestedBy: parent.RequestedBy, Status: "PENDING", Phase: "CHECK_CREATE_READY", CurrentStep: "CHECK_CREATE_READY", CheckPublication: cp, NextAttemptAt: now}
	j.CheckOperationID = child.ID
	parent.ChildIDs = append(parent.ChildIDs, child.ID)
	st.Operations = append(st.Operations, child)
	return nil
}
func checkIdentityEqual(a, b delivery.CheckRunIdentity) bool { return reflect.DeepEqual(a, b) }
func checkObservationMatches(o delivery.CheckRunObservation, w delivery.CheckRunWrite) bool {
	return o.Found && o.ID > 0 && (w.ID == 0 || o.ID == w.ID) && checkIdentityEqual(o.Identity, w.Identity) && o.Status == w.Status && o.Conclusion == w.Conclusion && o.StartedAt.Equal(w.StartedAt) && o.CompletedAt.Equal(w.CompletedAt) && o.Output == w.Output
}
func checkInitialReady(st *domain.State, j *domain.WorkerJob) bool {
	if j.Inputs.Check == nil {
		return true
	}
	op := operationByID(st, j.CheckOperationID)
	return op != nil && op.Kind == "CHECK_PUBLICATION" && op.Status != "FAILED" && op.Status != "CANCELLED" && op.CheckPublication != nil && op.CheckPublication.JobID == j.ID && op.CheckPublication.Observed != nil && op.CheckPublication.Observed.Found && (op.CheckPublication.Observed.Status == "queued" || op.CheckPublication.Observed.Status == "in_progress") && checkIdentityEqual(op.CheckPublication.Observed.Identity, op.CheckPublication.Identity)
}
func workerCheckSucceeded(st *domain.State, j *domain.WorkerJob) bool {
	if j == nil {
		return false
	}
	if j.Inputs.Check == nil {
		return true
	}
	op := operationByID(st, j.CheckOperationID)
	return op != nil && op.Status == "SUCCEEDED" && op.CheckPublication != nil && op.CheckPublication.JobID == j.ID && op.CheckPublication.Intent != nil && op.CheckPublication.Observed != nil && op.CheckPublication.Observed.Status == "completed" && op.CheckPublication.Observed.Conclusion == "success" && checkObservationMatches(*op.CheckPublication.Observed, *op.CheckPublication.Intent)
}

func (s *Service) workerCheckGate(st *domain.State, j *domain.WorkerJob) bool {
	if !workerCheckSucceeded(st, j) {
		return false
	}
	if j.Inputs.Check == nil {
		return true
	}
	return s.checkAuthority(st, operationByID(st, j.CheckOperationID), false) == nil
}
func (s *Service) checkAuthority(st *domain.State, op *domain.ExternalOperation, success bool) error {
	if op == nil || op.CheckPublication == nil {
		return invalid("check publication snapshot missing")
	}
	cp := op.CheckPublication
	j := workerJob(st, cp.JobID)
	if j == nil || j.CheckOperationID != op.ID || j.OperationID != cp.WorkerOperationID || j.ProductID != op.ProductID || j.InputHash != cp.InputHash || !reflect.DeepEqual(j.Inputs, cp.Inputs) || j.Inputs.Check == nil || !s.workerConfigMatches(j) || nativeQuarantined(st, j.ProductID) {
		return invalid("check worker authority changed or quarantined")
	}
	source, conn, physical, e := checkPhysical(st, j.ProductID, j.Inputs.RepositoryID)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(*source, cp.Source) || !reflect.DeepEqual(conn.Config, cp.Connection) || physical != cp.ProviderRepositoryID || conn.Config.AppID != j.Inputs.Check.ExpectedAppID {
		return invalid("check source connection authority changed")
	}
	if e = s.checkOwnership(st, j, *source, conn.Config, physical); e != nil {
		return e
	}
	if j.Inputs.SourceProof != nil {
		if e = nativeWorkerOrigin(st, j); e != nil {
			return e
		}
		if success {
			return s.managedWorkerAllowed(st, j)
		}
	} else {
		valid := false
		for _, r := range st.IntegrationRevisions {
			if r.ID == j.Inputs.RevisionID && r.ProductID == j.ProductID && r.IntegrationID == j.IntegrationID && r.RepositoryID == j.Inputs.RepositoryID && r.HeadCommit == j.Inputs.SourceSHA {
				valid = true
			}
		}
		if !valid {
			return invalid("check captured revision identity changed")
		}
	}
	return nil
}
func (s *Service) desiredCheck(st *domain.State, op *domain.ExternalOperation, now time.Time) (delivery.CheckRunWrite, error) {
	cp := op.CheckPublication
	j := workerJob(st, cp.JobID)
	recoverWorker(st, j, now)
	w := delivery.CheckRunWrite{Identity: cp.Identity, Status: "queued"}
	if cp.Observed != nil {
		w.ID = cp.Observed.ID
	}
	conclusion := ""
	terminal := false
	switch j.Status {
	case "CLAIMED", "RUNNING", "RECONCILIATION_REQUIRED":
		w.Status = "in_progress"
	case "SUCCEEDED":
		a := currentWorkerAttempt(st, j)
		if a == nil || a.Report == nil || a.Report.Result != "PASSED" || a.UpdatedAt.After(j.Deadline) || validateWorkerLocalOCI(j, a, a.Report) != nil {
			return w, invalid("check success requires accepted on-time typed build receipt")
		}
		terminal = true
		conclusion = "success"
	case "FAILED":
		terminal = true
		conclusion = "failure"
	case "CANCELLED":
		terminal = true
		conclusion = "cancelled"
	case "DEADLINE_EXCEEDED":
		terminal = true
		conclusion = "timed_out"
	}
	// Existing worker recovery records a never-started CLAIMED attempt as FAILED
	// when first discovering its expired execution deadline. Preserve that job
	// provenance while reporting the precise timeout outcome. An earlier lease
	// failure or a genuine accepted failure report remains a failure.
	if j.Status == "FAILED" && !now.Before(j.Deadline) && !j.UpdatedAt.Before(j.Deadline) {
		if a := currentWorkerAttempt(st, j); a != nil && a.Status == "EXPIRED" && a.Report == nil {
			terminal = true
			conclusion = "timed_out"
		}
	}
	if !now.Before(j.Deadline) && !terminal {
		terminal = true
		conclusion = "timed_out"
	}
	if j.CancellationRequested {
		terminal = true
		conclusion = "cancelled"
	}
	if j.Inputs.SourceProof != nil && (!terminal || conclusion == "success") && s.managedWorkerAllowed(st, j) != nil {
		terminal = true
		conclusion = "cancelled"
	}
	if cp.Observed != nil && cp.Observed.Status == "in_progress" && w.Status == "queued" {
		w.Status = "in_progress"
	}
	if terminal {
		w.Status = "completed"
		w.Conclusion = conclusion
		w.CompletedAt = j.UpdatedAt.UTC().Truncate(time.Second)
		if conclusion == "timed_out" {
			w.CompletedAt = j.Deadline.UTC().Truncate(time.Second)
		}
	}
	if w.Status != "queued" {
		w.StartedAt = j.CreatedAt.UTC().Truncate(time.Second)
	}
	w.Output = delivery.CheckRunOutput{Title: "RCP checks/build: " + w.Status, Summary: fmt.Sprintf("Worker %s\nSource %s\nInputs %s\nRecipe %s\nResult %s", j.ID, j.Inputs.SourceSHA, j.InputHash, j.Inputs.RecipeHash, j.Status)}
	if w.Conclusion != "" {
		w.Output.Title = "RCP checks/build: " + w.Conclusion
	}
	return w, nil
}
func (s *Service) checkUpdate(ctx context.Context, op domain.ExternalOperation, token string, fn func(*domain.State, *domain.ExternalOperation) error) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) {
			return invalid("check operation lease lost")
		}
		return fn(st, current)
	})
}
func checkStep(st *domain.State, op *domain.ExternalOperation, detail string, terminal bool) {
	now := time.Now().UTC()
	op.UpdatedAt = now
	op.Detail = detail
	op.CurrentStep = op.Phase
	op.NextAttemptAt = now.Add(2 * time.Second)
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	if terminal {
		op.FinishedAt = &now
	}
	st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: "system/check-publication", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: op.Phase, Status: op.Status, Detail: detail})
}
func (s *Service) tickCheck(ctx context.Context, op domain.ExternalOperation, token string) error {
	provider, ok := s.provider.(delivery.CheckRunProvider)
	if !ok {
		return s.checkUpdate(ctx, op, token, func(st *domain.State, current *domain.ExternalOperation) error {
			current.Status = "FAILED"
			current.Error = "CHECK_PROVIDER_UNAVAILABLE"
			checkStep(st, current, "Check provider unavailable", true)
			return nil
		})
	}
	var action string
	var cp domain.CheckPublication
	err := s.checkUpdate(ctx, op, token, func(st *domain.State, current *domain.ExternalOperation) error {
		now := time.Now().UTC()
		p := current.CheckPublication
		fail := func(reason string) {
			current.Status = "FAILED"
			current.Error = reason
			checkStep(st, current, reason, true)
		}
		if p == nil {
			fail("CHECK_SNAPSHOT_INVALID")
			return nil
		}
		if !now.Before(p.FinalizeDeadline) || p.Observations >= checkObservationBudget {
			fail("CHECK_RECONCILIATION_REQUIRED: finalization deadline or observation budget exhausted")
			return nil
		}
		if e := s.checkAuthority(st, current, false); e != nil {
			fail("CHECK_AUTHORITY_CHANGED")
			return nil
		}
		desired, e := s.desiredCheck(st, current, now)
		if e != nil {
			fail("CHECK_RESULT_INVALID")
			return nil
		}
		if desired.Conclusion == "success" {
			if e = s.checkAuthority(st, current, true); e != nil {
				fail("CHECK_SUCCESS_AUTHORITY_CHANGED")
				return nil
			}
		}
		switch current.Phase {
		case "CHECK_CREATE_READY":
			// Initial registration is always queued, even if timeout was first observed
			// after the execution deadline. Finalization remains a separate intent.
			desired.Status = "queued"
			desired.Conclusion = ""
			desired.StartedAt = time.Time{}
			desired.CompletedAt = time.Time{}
			desired.ID = 0
			desired.Output = delivery.CheckRunOutput{Title: "RCP checks/build: queued", Summary: fmt.Sprintf("Worker %s\nSource %s\nInputs %s\nRecipe %s\nResult QUEUED", p.JobID, p.Inputs.SourceSHA, p.InputHash, p.Inputs.RecipeHash)}
			p.Intent = &desired
			p.IntentHash = workerconfig.Fingerprint(desired)
			current.Phase = "CHECK_CREATE_UNCERTAIN"
			action = "create"
		case "CHECK_CREATE_UNCERTAIN":
			action = "find"
		case "CHECK_UPDATE_UNCERTAIN":
			action = "read"
		case "CHECK_OBSERVE":
			if p.Observed == nil {
				fail("CHECK_OBSERVATION_MISSING")
				return nil
			}
			if p.Observed.Status == "completed" {
				fail("CHECK_TERMINAL_OUTCOME_MISMATCH")
				return nil
			}
			if checkObservationMatches(*p.Observed, desired) {
				current.Status = "RUNNING"
				checkStep(st, current, "Check state observed; waiting for accepted worker result", false)
				return nil
			}
			p.Intent = &desired
			p.IntentHash = workerconfig.Fingerprint(desired)
			current.Phase = "CHECK_UPDATE_UNCERTAIN"
			action = "update"
		default:
			fail("CHECK_PHASE_INVALID")
			return nil
		}
		if p.Intent == nil || p.IntentHash != workerconfig.Fingerprint(*p.Intent) {
			fail("CHECK_INTENT_INVALID")
			return nil
		}
		p.Observations++
		current.CurrentStep = current.Phase
		current.UpdatedAt = now
		cp = *p
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: current.ProductID, FeatureID: current.FeatureID, Actor: "system/check-publication", CreatedAt: now, UpdatedAt: now}, OperationID: current.ID, Phase: current.Phase, Status: "RUNNING", Detail: "Durable bounded check intent/observation"})
		return nil
	})
	if err != nil || action == "" {
		return err
	}
	var observed delivery.CheckRunObservation
	var callErr error
	switch action {
	case "create":
		observed, callErr = provider.CreateCheckRun(ctx, cp.Connection, *cp.Intent)
	case "find":
		observed, callErr = provider.FindCheckRun(ctx, cp.Connection, cp.Identity)
	case "read":
		observed, callErr = provider.ReadCheckRun(ctx, cp.Connection, cp.Identity, cp.Intent.ID)
	case "update":
		observed, callErr = provider.UpdateCheckRun(ctx, cp.Connection, *cp.Intent)
	default:
		return errors.New("invalid check provider action")
	}
	return s.checkUpdate(ctx, op, token, func(st *domain.State, current *domain.ExternalOperation) error {
		p := current.CheckPublication
		if p == nil || p.IntentHash != cp.IntentHash || p.Observations != cp.Observations {
			return invalid("check intent changed during provider I/O")
		}
		if e := s.checkAuthority(st, current, cp.Intent.Conclusion == "success"); e != nil {
			current.Status = "FAILED"
			current.Error = "CHECK_POST_IO_AUTHORITY_CHANGED"
			checkStep(st, current, current.Error, true)
			return nil
		}
		if !time.Now().UTC().Before(p.FinalizeDeadline) {
			current.Status = "FAILED"
			current.Error = "CHECK_FINALIZATION_EXPIRED"
			checkStep(st, current, current.Error, true)
			return nil
		}
		if callErr == nil && observed.Found && !checkIdentityEqual(observed.Identity, p.Identity) {
			current.Status = "FAILED"
			current.Error = "CHECK_PROVIDER_IDENTITY_MISMATCH"
			checkStep(st, current, current.Error, true)
			return nil
		}
		if callErr != nil || !checkObservationMatches(observed, *p.Intent) {
			current.Status = "RUNNING"
			checkStep(st, current, "Check write outcome uncertain; read-only reconciliation pending", false)
			current.NextAttemptAt = time.Now().UTC().Add(time.Duration(2+p.Observations/4) * time.Second)
			return nil
		}
		p.Observed = &observed
		current.Phase = "CHECK_OBSERVE"
		current.Status = "RUNNING"
		if observed.Status == "completed" {
			current.Status = "SUCCEEDED"
		}
		checkStep(st, current, "Exact check state observed: "+observed.Status+" "+observed.Conclusion, current.Status == "SUCCEEDED")
		return nil
	})
}
