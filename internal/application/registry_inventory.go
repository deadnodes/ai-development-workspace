package application

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func observationScope(r workerconfig.RegistryObserveRecipe) domain.RegistryScope {
	u, _ := url.Parse(r.Origin)
	image := ""
	if u != nil {
		image, _ = domain.CanonicalImageRepository(u.Host + "/" + r.Repository)
	}
	return domain.RegistryScope{ProductID: r.ProductID, ComponentID: r.ComponentID, RepositoryID: r.RepositoryID, Origin: r.Origin, Repository: r.Repository, ImageRepository: image}
}
func registryScopeCurrent(st *domain.State, scope domain.RegistryScope, repositoryURL string) bool {
	if nativeQuarantined(st, scope.ProductID) {
		return false
	}
	app := applicationByID(st, scope.ComponentID)
	if app == nil || app.ProductID != scope.ProductID || app.RepositoryID != scope.RepositoryID || domain.ComponentKind(*app) != "APPLICATION" {
		return false
	}
	for _, repo := range st.Repositories {
		if repo.ID == scope.RepositoryID && repo.ProductID == scope.ProductID && (repositoryURL == "" || repo.URL == repositoryURL) {
			return true
		}
	}
	return false
}
func (s *Service) registryAuthorities(st *domain.State, now time.Time) []domain.RegistryObservationAuthority {
	if s.externalWorkers == nil {
		return nil
	}
	all := s.externalWorkers.RegistryObservations(now)
	for i := range all {
		all[i].Current = all[i].Current && registryScopeCurrent(st, all[i].Scope, "")
	}
	return all
}
func (s *Service) evaluateRetention(st domain.State, product string, now time.Time) domain.RetentionReport {
	return domain.EvaluateRetention(st, product, now, s.registryAuthorities(&st, now))
}
func (s *Service) registryConfigMatches(j *domain.WorkerJob) bool {
	if s.externalWorkers == nil || j == nil || j.Inputs.Registry == nil {
		return false
	}
	r, ok := s.externalWorkers.Observation(j.Inputs.RecipeID)
	p := j.Inputs.Registry
	return ok && j.Capability == "registry_observe" && r.Hash == j.Inputs.RecipeHash && r.Revision == j.Inputs.RecipeRevision && s.externalWorkers.Revision() == j.Inputs.ConfigRevision && reflect.DeepEqual(p.Scope, observationScope(r)) && reflect.DeepEqual(p.Limits, r.Limits) && j.InputHash == workerconfig.Fingerprint(j.Inputs)
}
func (s *Service) registryWorkerAllowed(st *domain.State, j *domain.WorkerJob) error {
	if !s.registryConfigMatches(j) || !registryScopeCurrent(st, j.Inputs.Registry.Scope, j.Inputs.RepositoryURL) {
		return invalid("registry observation authority changed or quarantined")
	}
	return nil
}
func (s *Service) refreshRegistryInventory(ctx context.Context, c domain.Command) (any, error) {
	if s.externalWorkers == nil || strings.TrimSpace(c.Actor) == "" || c.ProductID == "" || c.ID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return nil, invalid("explicit registry observation scope required")
	}
	var in struct {
		ApplicationID  string    `json:"application_id"`
		RecipeID       string    `json:"recipe_id"`
		RecipeRevision string    `json:"recipe_revision"`
		RecipeHash     string    `json:"recipe_hash"`
		ConfigRevision string    `json:"config_revision"`
		RequestKey     string    `json:"request_key"`
		Deadline       time.Time `json:"deadline"`
	}
	if e := decode(c.Data, &in); e != nil {
		return nil, e
	}
	if !workerRequestKey.MatchString(in.RequestKey) {
		return nil, invalid("bounded registry observation request key required")
	}
	requestHash := workerconfig.Fingerprint(struct {
		Product, Feature, Integration, Actor string
		Request                              any
	}{c.ProductID, c.FeatureID, c.IntegrationID, c.Actor, in})
	var out domain.WorkerJob
	e := s.store.Update(ctx, func(st *domain.State) error {
		for _, previous := range st.WorkerJobs {
			if previous.ProductID == c.ProductID && previous.RequestKey == in.RequestKey {
				if previous.Capability != "registry_observe" || previous.RequestHash != requestHash {
					return invalid("conflicting registry observation request replay")
				}
				out = previous
				return nil
			}
		}
		now := time.Now().UTC()
		r, ok := s.externalWorkers.Observation(in.RecipeID)
		if !ok || r.ProductID != c.ProductID || r.ComponentID != in.ApplicationID || r.Revision != in.RecipeRevision || r.Hash != in.RecipeHash || s.externalWorkers.Revision() != in.ConfigRevision || !in.Deadline.After(now) || in.Deadline.After(now.Add(time.Hour)) {
			return invalid("registry recipe/configuration/deadline mismatch")
		}
		scope := observationScope(r)
		if !registryScopeCurrent(st, scope, "") {
			return invalid("registry component/repository scope unavailable")
		}
		eligible := false
		for _, authority := range s.registryAuthorities(st, now) {
			eligible = eligible || authority.Current && authority.RecipeID == r.ID && authority.RecipeHash == r.Hash
		}
		if !eligible {
			return invalid("no current registry observer grant")
		}
		featureID := c.FeatureID
		if featureID != "" {
			f := feature(st, featureID)
			if f == nil || f.ProductID != c.ProductID {
				return invalid("registry feature outside product")
			}
		}
		if c.IntegrationID != "" {
			integration := integration(st, c.IntegrationID)
			if integration == nil || integration.ProductID != c.ProductID || featureID != "" && integration.FeatureID != featureID {
				return invalid("registry integration outside feature/product")
			}
			featureID = integration.FeatureID
		}
		repoURL := ""
		for _, repo := range st.Repositories {
			if repo.ID == scope.RepositoryID && repo.ProductID == c.ProductID {
				repoURL = repo.URL
			}
		}
		retention := s.evaluateRetention(*st, c.ProductID, now)
		roots := domain.RegistryKnownRoots(retention, scope.ImageRepository)
		if len(roots) > r.Limits.MaxNodes || len(roots) > 256 {
			return invalid("known roots exceed registry observation bounds")
		}
		registry := &domain.WorkerRegistryInputs{Scope: scope, Roots: roots, RootSetHash: domain.RegistryRootSetHash(scope, r.ID, r.Revision, r.Hash, in.ConfigRevision, roots), RequestRetentionInputHash: retention.InputHash, Limits: r.Limits}
		inputs := domain.WorkerInputs{RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, ConfigRevision: in.ConfigRevision, RepositoryID: r.RepositoryID, RepositoryURL: repoURL, Trust: "registry_observation", Parameters: map[string]any{}, Registry: registry}
		m := domain.Meta{ID: id(), ProductID: c.ProductID, FeatureID: featureID, Actor: c.Actor, CreatedAt: now, UpdatedAt: now}
		op := domain.ExternalOperation{Meta: m, Kind: domain.RegistryObserveOperationKind, IntegrationID: c.IntegrationID, RepositoryID: r.RepositoryID, ApplicationID: r.ComponentID, Status: "PENDING", Phase: "EXTERNAL_QUEUED", RequestedBy: c.Actor, RegistryObservation: &inputs}
		m.ID = id()
		out = domain.WorkerJob{Meta: m, OperationID: op.ID, RequestKey: in.RequestKey, RequestHash: requestHash, IntegrationID: c.IntegrationID, Capability: "registry_observe", Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: in.Deadline, MaxAttempts: 1}
		st.Operations = append(st.Operations, op)
		st.WorkerJobs = append(st.WorkerJobs, out)
		workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], c.Actor, c.Action, now)
		return nil
	})
	return out, e
}
func admitRegistryInventory(st *domain.State, j *domain.WorkerJob, a *domain.WorkerAttempt, r *domain.WorkerReport, now time.Time) {
	m := domain.Meta{ID: id(), ProductID: j.ProductID, FeatureID: j.FeatureID, Actor: a.Principal, CreatedAt: now, UpdatedAt: now}
	st.RegistryInventories = append(st.RegistryInventories, domain.RegistryInventory{Meta: m, JobID: j.ID, AttemptID: a.ID, Fence: a.Fence, InputHash: j.InputHash, Inputs: j.Inputs, Report: *r.Registry, ReceivedAt: now})
}
