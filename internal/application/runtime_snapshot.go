package application

import (
	"context"
	"fmt"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

// Configure once at startup. Credentials remain inside provider adapters.
func (s *Service) SetRuntimeObservers(observers []delivery.RuntimeSnapshotObserver) {
	s.runtimeObservers = observers
}

func (s *Service) runtimeTargets(product, environment string) []map[string]string {
	out := []map[string]string{}
	seen := map[string]bool{}
	for _, o := range s.runtimeObservers {
		t := o.TargetIdentity()
		if t.ProductID == product && t.EnvironmentID == environment && !seen[t.ApplicationID] {
			out = append(out, map[string]string{"product_id": t.ProductID, "environment_id": t.EnvironmentID, "application_id": t.ApplicationID})
			seen[t.ApplicationID] = true
		}
	}
	return out
}
func (s *Service) tickRuntime(ctx context.Context, op domain.ExternalOperation, token string) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	env := environmentByID(&st, op.EnvironmentID)
	if env == nil || env.ProductID != op.ProductID {
		return s.finish(ctx, op, token, "FAILED", "environment no longer available", nil)
	}
	// Cache external reads per component; one component may own many workloads.
	expected := map[string]domain.RuntimeExpected{}
	actions := map[string][]delivery.ActionsRun{}
	actionsErrors := map[string]string{}
	snapshots := []domain.RuntimeSnapshot{}
	for _, observer := range s.runtimeObservers {
		target := observer.TargetIdentity()
		if target.ProductID != op.ProductID || target.EnvironmentID != op.EnvironmentID || (op.ApplicationID != "" && target.ApplicationID != op.ApplicationID) {
			continue
		}
		app := applicationByID(&st, target.ApplicationID)
		if app == nil || app.ProductID != op.ProductID {
			continue
		}
		if _, exists := expected[target.ApplicationID]; !exists {
			expected[target.ApplicationID] = s.runtimeExpected(ctx, &st, target)
			actions[target.ApplicationID], actionsErrors[target.ApplicationID] = s.runtimeActions(ctx, &st, target)
		}
		evidence := observer.Snapshot(ctx)
		// Scope is selected by trusted configuration, never supplied by remote data.
		evidence.ProductID = op.ProductID
		evidence.EnvironmentID = op.EnvironmentID
		evidence.ApplicationID = target.ApplicationID
		now := time.Now().UTC()
		v := domain.RuntimeSnapshot{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: "system/runtime-observer", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, EnvironmentID: op.EnvironmentID, ApplicationID: target.ApplicationID, Runtime: evidence, Expected: expected[target.ApplicationID], ArtifactMatches: []domain.DeliveryArtifact{}, ActionsRuns: actions[target.ApplicationID], ActionsError: actionsErrors[target.ApplicationID]}
		v.ReferenceComparison = runtimeReferenceComparison(v.Expected, evidence)
		v.Comparison = runtimeComparison(v.Expected, evidence)
		for _, a := range st.DeliveryArtifacts {
			if a.ProductID != op.ProductID || a.ApplicationID != target.ApplicationID || a.Digest == "" {
				continue
			}
			for _, pod := range evidence.Pods {
				if immutableDigest(pod.ImageID) == a.Digest && imageRepository(pod.Image) == a.ImageRepository {
					v.ArtifactMatches = append(v.ArtifactMatches, a)
					break
				}
			}
		}
		snapshots = append(snapshots, v)
	}
	if len(snapshots) == 0 {
		return s.finish(ctx, op, token, "FAILED", "no read-only runtime targets configured for this environment/component", nil)
	}
	err = s.store.Update(ctx, func(current *domain.State) error {
		active := operationByID(current, op.ID)
		if active == nil || active.LeaseOwner != token || active.Status == "CANCELLED" {
			return fmt.Errorf("runtime operation lease lost")
		}
		if env := environmentByID(current, op.EnvironmentID); env == nil || env.ProductID != op.ProductID {
			return fmt.Errorf("runtime target was removed during observation")
		}
		// Retry of a read is safe; replace no evidence and deduplicate saved step.
		for _, old := range current.RuntimeSnapshots {
			if old.OperationID == op.ID {
				return nil
			}
		}
		current.RuntimeSnapshots = append(current.RuntimeSnapshots, snapshots...)
		return nil
	})
	if err != nil {
		return err
	}
	status := "SUCCEEDED"
	failures := 0
	for _, v := range snapshots {
		if len(v.Runtime.Errors) > 0 {
			failures++
		}
	}
	if failures > 0 {
		status = "FAILED"
	}
	return s.finish(ctx, op, token, status, fmt.Sprintf("Read %d Kubernetes workloads; %d reads had errors. Snapshots preserve desired state, pod image IDs and provenance separately.", len(snapshots), failures), map[string]string{"snapshots": fmt.Sprint(len(snapshots)), "read_errors": fmt.Sprint(failures)})
}
func (s *Service) runtimeExpected(ctx context.Context, st *domain.State, t delivery.RuntimeTarget) domain.RuntimeExpected {
	b := environmentBinding(st, t.EnvironmentID, t.ApplicationID)
	if b == nil {
		return domain.RuntimeExpected{Error: "GitOps mapping not configured"}
	}
	if s.provider == nil {
		return domain.RuntimeExpected{Error: "GitOps provider unavailable"}
	}
	conn, err := scopedConnection(st, b.ConnectionID, t.ProductID)
	if err != nil {
		return domain.RuntimeExpected{Error: err.Error()}
	}
	rb := repositoryBinding(st, b.RepositoryID)
	if rb == nil {
		return domain.RuntimeExpected{Error: "GitOps repository binding missing"}
	}
	snap, err := s.provider.ReadGitOps(ctx, conn.Config, delivery.GitOpsRequest{Repository: repositoryLocator(st, *rb), Ref: b.Ref, Path: b.Path, ImageField: b.ImageField, DigestField: b.DigestField})
	if err != nil {
		return domain.RuntimeExpected{Error: err.Error()}
	}
	return domain.RuntimeExpected{ImageRepository: snap.ImageRepository, Value: snap.Digest, GitOpsCommit: snap.HeadSHA}
}
func (s *Service) runtimeActions(ctx context.Context, st *domain.State, t delivery.RuntimeTarget) ([]delivery.ActionsRun, string) {
	empty := []delivery.ActionsRun{}
	p, ok := s.provider.(delivery.ActionsInventoryProvider)
	if !ok {
		return empty, "Actions metadata reader unavailable"
	}
	a := applicationByID(st, t.ApplicationID)
	if a == nil {
		return empty, "component unavailable"
	}
	b := repositoryBinding(st, a.RepositoryID)
	if b == nil {
		return empty, "source repository connection unavailable"
	}
	c, err := scopedConnection(st, b.ConnectionID, t.ProductID)
	if err != nil {
		return empty, err.Error()
	}
	runs, err := p.ListRecentBuilds(ctx, c.Config, repositoryLocator(st, *b))
	if err != nil {
		return empty, err.Error()
	}
	return runs, ""
}
func immutableDigest(image string) string {
	if i := strings.LastIndex(image, "sha256:"); i >= 0 {
		v := image[i:]
		if len(v) == 71 {
			for _, c := range v[7:] {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
					return ""
				}
			}
			return v
		}
	}
	return ""
}
func imageRepository(image string) string {
	image = strings.SplitN(image, "@", 2)[0]
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image
}

// Reference comparison is string/canonical-reference evidence, not proof of the
// immutable bytes behind a mutable tag. Missing pods/read errors stay unknown.
func runtimeReferenceComparison(expected domain.RuntimeExpected, actual delivery.RuntimeSnapshot) string {
	if expected.Error != "" || expected.ImageRepository == "" || actual.WorkloadImage == "" {
		return "UNKNOWN"
	}
	desired := expected.ImageRepository
	if strings.HasPrefix(expected.Value, "sha256:") {
		desired += "@" + expected.Value
	} else if expected.Value != "" {
		desired += ":" + expected.Value
	}
	equivalent := func(a, b string) bool {
		da, db := immutableDigest(a), immutableDigest(b)
		if da != "" && db != "" {
			return imageRepository(a) == imageRepository(b) && da == db
		}
		return a == b
	}
	if !equivalent(actual.WorkloadImage, desired) {
		return "DRIFT"
	}
	for _, p := range actual.Pods {
		if p.Image != "" && !equivalent(p.Image, actual.WorkloadImage) {
			return "DRIFT"
		}
	}
	if len(actual.Errors) > 0 || len(actual.Pods) == 0 {
		return "UNKNOWN"
	}
	for _, p := range actual.Pods {
		if p.Image == "" {
			return "UNKNOWN"
		}
	}
	return "MATCH"
}

// A pod imageID may be an architecture-specific manifest while desired digest
// identifies an OCI index. Only exact equality proves a match; inequality alone
// cannot prove drift without registry platform metadata.
func runtimeComparison(expected domain.RuntimeExpected, actual delivery.RuntimeSnapshot) string {
	reference := runtimeReferenceComparison(expected, actual)
	if reference != "MATCH" {
		return reference
	}
	digest := immutableDigest(expected.Value)
	if digest == "" {
		return "UNKNOWN"
	}
	for _, p := range actual.Pods {
		if immutableDigest(p.ImageID) != digest {
			return "UNKNOWN"
		}
	}
	return "MATCH"
}
