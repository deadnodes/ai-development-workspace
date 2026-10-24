package application

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func mainCandidateInput(c domain.Command) (domain.MainCandidateRequest, string, error) {
	var in domain.MainCandidateRequest
	if err := exactMainCandidateKeys(c.Data); err != nil {
		return in, "", err
	}
	if err := decode(c.Data, &in); err != nil {
		return in, "", err
	}
	if c.ProductID == "" || strings.TrimSpace(c.Actor) == "" || c.ID != "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" || strings.TrimSpace(in.Name) == "" || len(in.Name) > 256 || !workerRequestKey.MatchString(in.IdempotencyKey) || len(in.MainSources) == 0 || len(in.MainSources) > 20 {
		return in, "", invalid("bounded main candidate input and actor/product required")
	}
	seen := map[string]bool{}
	for _, src := range in.MainSources {
		if src.ApplicationID == "" || len(src.ApplicationID) > 128 || seen[src.ApplicationID] || len(src.MainSHA) != 40 || !fullSHA.MatchString(src.MainSHA) || len(src.Revisions) == 0 || len(src.Revisions) > 20 {
			return in, "", invalid("invalid or duplicate main source")
		}
		seen[src.ApplicationID] = true
		seenRev := map[string]bool{}
		for _, rev := range src.Revisions {
			if rev.RevisionID == "" || len(rev.RevisionID) > 128 || seenRev[rev.RevisionID] || rev.PullRequestNumber < 1 || rev.PullRequestNumber > 2147483647 || len(rev.VerificationResultIDs) == 0 || len(rev.VerificationResultIDs) > 100 {
				return in, "", invalid("bounded unique revision and verification refs required")
			}
			seenRev[rev.RevisionID] = true
		}
	}
	return in, workerconfig.Fingerprint(struct {
		Product, Actor string
		Input          domain.MainCandidateRequest
	}{c.ProductID, c.Actor, in}), nil
}
func candidateReplay(st *domain.State, c domain.Command, in domain.MainCandidateRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.ProductID == c.ProductID && op.RequestKey == in.IdempotencyKey {
			if op.MainCandidate == nil || op.RequestHash != hash || op.RequestedBy != c.Actor {
				return nil, invalid("conflicting main candidate replay")
			}
			return op, nil
		}
	}
	return nil, nil
}

// Candidate-local evidence uses durable append order, independently of wall-clock
// projections used elsewhere. Freezing the definition AND latest result prevents
// a later old-clock PASS from refreshing a previously accepted source reference.
func mainCandidateAuthority(st *domain.State, product string, in domain.MainCandidateRequest) (string, error) {
	type evidence struct {
		Revision    domain.IntegrationRevision
		Integration domain.Integration
		Gates       []domain.Gate
		Checks      []domain.Check
		Results     []struct{ ID, Hash string }
	}
	out := []evidence{}
	for _, src := range in.MainSources {
		app := applicationByID(st, src.ApplicationID)
		if app == nil || app.ProductID != product {
			return "", invalid("main source component outside product")
		}
		for _, chosen := range src.Revisions {
			rev := revisionByID(st, chosen.RevisionID)
			if rev == nil || rev.ProductID != product || rev.RepositoryID != app.RepositoryID || len(rev.HeadCommit) != 40 || !fullSHA.MatchString(rev.HeadCommit) {
				return "", invalid("main revision identity mismatch")
			}
			integ := integration(st, rev.IntegrationID)
			if integ == nil || integ.ProductID != product || integ.FeatureID != rev.FeatureID || (integ.Status != "ready" && integ.Status != "released") {
				return "", invalid("main source integration must be ready")
			}
			causal := mainCandidateCausalState(st)
			if err := readiness(&causal, *integ); err != nil {
				return "", err
			}
			e := evidence{Revision: *rev, Integration: *integ}
			supplied := map[string]bool{}
			for _, rid := range chosen.VerificationResultIDs {
				if supplied[rid] || rid == "" || len(rid) > 128 {
					return "", invalid("duplicate source verification result")
				}
				supplied[rid] = true
			}
			used := map[string]bool{}
			sourceGates := 0
			for _, g := range st.Gates {
				if g.ProductID != product || g.FeatureID != integ.FeatureID || !g.Blocking || !slices.Contains(g.IntegrationIDs, integ.ID) {
					continue
				}
				frozen := g
				frozen.Result = ""
				e.Gates = append(e.Gates, frozen)
				count := 0
				selectedSourceGate := false
				for _, chk := range st.Checks {
					if chk.GateID == g.ID {
						for _, r := range st.Results {
							if r.CheckID == chk.ID && supplied[r.ID] {
								selectedSourceGate = true
							}
						}
					}
				}
				if selectedSourceGate {
					sourceGates++
				}
				for _, chk := range st.Checks {
					if chk.GateID != g.ID {
						continue
					}
					count++
					if chk.ProductID != product || chk.FeatureID != g.FeatureID {
						return "", invalid("source check scope mismatch")
					}
					var last *domain.CheckResult
					for i := range st.Results {
						if st.Results[i].CheckID == chk.ID {
							last = &st.Results[i]
						}
					}
					if last == nil || last.Result != "passed" || last.ProductID != product || last.FeatureID != g.FeatureID || selectedSourceGate && (!supplied[last.ID] || last.Commit != rev.HeadCommit) || g.Commit != "" && last.Commit != g.Commit || g.EnvironmentID != "" && last.EnvironmentID != g.EnvironmentID || g.Deployment != "" && last.Deployment != g.Deployment {
						return "", invalid("complete latest exact-head source gate verification required")
					}
					if selectedSourceGate {
						used[last.ID] = true
					}
					e.Checks = append(e.Checks, chk)
					e.Results = append(e.Results, struct{ ID, Hash string }{last.ID, workerconfig.Fingerprint(last)})
				}
				if count == 0 {
					return "", invalid("source blocking gate has no checks")
				}
			}
			if sourceGates == 0 || len(used) != len(supplied) {
				return "", invalid("whole blocking source gate coverage required")
			}
			out = append(out, e)
		}
	}
	return workerconfig.Fingerprint(out), nil
}

func (s *Service) executeMainCandidate(ctx context.Context, c domain.Command) (any, error) {
	in, hash, err := mainCandidateInput(c)
	if err != nil {
		return nil, err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	if old, e := candidateReplay(&st, c, in, hash); e != nil {
		return nil, e
	} else if old != nil {
		return *old, nil
	}
	op, err := s.prepareMainCandidate(&st, c, in, hash)
	if err != nil {
		return nil, err
	}
	ioctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err = s.observeMainCandidate(ioctx, &op, true); err != nil {
		return nil, err
	}
	var out domain.ExternalOperation
	err = s.store.Update(ctx, func(current *domain.State) error {
		if old, e := candidateReplay(current, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		if err := s.mainCandidateAllowed(current, &op); err != nil {
			return err
		}
		if err := reservationFree(current, op.EnvironmentID); err != nil {
			return err
		}
		if e := environmentByID(current, op.EnvironmentID); e != nil {
			op.ReservationEpoch = e.ReservationEpoch
		}
		current.Compositions = append(current.Compositions, *op.CompositionSnapshot)
		current.Operations = append(current.Operations, op)
		current.Events = append(current.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: op.CreatedAt, EntityID: op.ID, ProductID: c.ProductID, Data: c})
		out = op
		return nil
	})
	return out, err
}
func (s *Service) prepareMainCandidate(st *domain.State, c domain.Command, in domain.MainCandidateRequest, hash string) (domain.ExternalOperation, error) {
	var empty domain.ExternalOperation
	if s.externalWorkers == nil || s.provider == nil || nativeQuarantined(st, c.ProductID) {
		return empty, invalid("main candidate authority unavailable")
	}
	authority, err := mainCandidateAuthority(st, c.ProductID, in)
	if err != nil {
		return empty, err
	}
	m := meta(c)
	cm := m
	cm.ID = id()
	components := []domain.CompositionComponent{}
	repoInputs := map[string]string{}
	for _, src := range in.MainSources {
		app := applicationByID(st, src.ApplicationID)
		binding := repositoryBinding(st, app.RepositoryID)
		build := componentBuild(st, app.ID)
		target := environmentBinding(st, in.EnvironmentID, app.ID)
		if binding == nil || binding.ProductID != c.ProductID || !domain.IsSourceRole(binding.Role) || build == nil || build.Mode != "NATIVE" || target == nil || target.ProductID != c.ProductID || target.Purpose != "PROD" || !target.AllowDeploy || target.GitOpsMode != "PR" {
			return empty, invalid("main candidate requires configured native component and PR-only PROD target")
		}
		if configuredBase(*binding) != "main" && configuredBase(*binding) != "master" {
			return empty, invalid("configured main/master required")
		}
		if num, e := strconv.ParseInt(repositoryLocator(st, *binding), 10, 64); e != nil || num <= 0 {
			return empty, invalid("physical numeric repository identity required")
		}
		shared := workerconfig.Fingerprint(struct {
			SHA       string
			Revisions []domain.MainCandidateRevision
		}{src.MainSHA, src.Revisions})
		if prev, ok := repoInputs[app.RepositoryID]; ok && prev != shared {
			return empty, invalid("shared repository candidate proof mismatch")
		}
		repoInputs[app.RepositoryID] = shared
		rids := []string{}
		for _, r := range src.Revisions {
			rids = append(rids, r.RevisionID)
		}
		components = append(components, domain.CompositionComponent{ApplicationID: src.ApplicationID, BaseRef: configuredBase(*binding), BaseCommit: src.MainSHA, TargetBranch: "generated/" + in.EnvironmentID + "/" + m.ID + "/" + app.RepositoryID, RevisionIDs: rids})
	}
	result, _, err := applyComposition(st, domain.Command{Action: "plan_composition", ProductID: c.ProductID, Actor: c.Actor, Data: map[string]any{"name": in.Name, "environment_id": in.EnvironmentID, "components": components}}, cm)
	if err != nil {
		return empty, err
	}
	comp := result.(domain.Composition)
	// The preliminary state is a private Read clone. Publication appends exactly once.
	st.Compositions = st.Compositions[:len(st.Compositions)-1]
	causal := mainCandidateCausalState(st)
	if err = releaseWorkReady(&causal, comp); err != nil {
		return empty, err
	}
	snaps, err := prepareFlowDeployments(st, comp, m)
	if err != nil {
		return empty, err
	}
	proof := &domain.MainCandidateProof{EvidenceKind: "ATTRIBUTED_RCP_VERIFICATION", Request: in, AuthorityHash: authority, Sources: []domain.MainCandidateRepository{}}
	for i := range snaps {
		snap := &snaps[i]
		src := in.MainSources[i]
		baseline := snap.Build
		snap.BuildBaseline = &baseline
		pub, ok := s.externalWorkers.Publication(src.Native.PublicationRecipeID)
		if !ok {
			return empty, invalid("candidate publication recipe missing")
		}
		effective := snap.Build
		effective.Native = &src.Native
		effective.Profile = pub.Profile
		effective.ImageRepository = nativeImage(pub)
		effective.RebuildMissing = false
		if _, _, err = s.nativeRecipes(c.ProductID, snap.Application, effective.Profile, effective.ImageRepository, effective.Native); err != nil {
			return empty, err
		}
		snap.Build = effective
		snap.GitOpsRequest.ImageRepository = effective.ImageRepository
		snap.BuildRequest.Inputs["image_repository"] = effective.ImageRepository
		snap.Native, err = s.freezeNative(c.ProductID, snap.Application, effective, m.CreatedAt)
		if err != nil {
			return empty, err
		}
		binding := snap.Source
		n, _ := strconv.ParseInt(snap.BuildRequest.Repository, 10, 64)
		rp := domain.MainCandidateRepository{RepositoryID: snap.Application.RepositoryID, ProviderRepositoryID: n, MainRef: configuredBase(binding), MainSHA: src.MainSHA, Revisions: []domain.MainCandidateMergedRevision{}}
		for _, r := range src.Revisions {
			rev := revisionByID(st, r.RevisionID)
			rp.Revisions = append(rp.Revisions, domain.MainCandidateMergedRevision{RevisionID: rev.ID, PullRequestNumber: r.PullRequestNumber, HeadSHA: rev.HeadCommit, HeadRef: rev.Branch})
		}
		proof.Sources = append(proof.Sources, rp)
	}
	op := domain.ExternalOperation{ReservationEpoch: comp.EnvironmentSnapshot.ReservationEpoch, Meta: m, MainCandidate: proof, Kind: "RELEASE_CANDIDATE", EnvironmentID: in.EnvironmentID, Status: "PENDING", Phase: "QUEUE_BUILDS", RequestedBy: c.Actor, RequestKey: in.IdempotencyKey, RequestHash: hash, CompositionSnapshot: &comp, PreparedDeployments: snaps, IntegrationIDs: selectedIntegrations(comp), ChildIDs: []string{}, NextAttemptAt: m.CreatedAt}
	return op, nil
}

func (s *Service) observeMainCandidate(ctx context.Context, op *domain.ExternalOperation, initial bool) error {
	observer, ok := s.provider.(delivery.PullRequestObserver)
	if !ok || op.MainCandidate == nil {
		return invalid("merged pull request observer required")
	}
	for i := range op.MainCandidate.Sources {
		src := &op.MainCandidate.Sources[i]
		snap := op.PreparedDeployments[i]
		branches, err := s.provider.Branches(ctx, snap.SourceConnection, strconv.FormatInt(src.ProviderRepositoryID, 10))
		if err != nil {
			return invalid("candidate main observation failed")
		}
		tip := ""
		for _, b := range branches {
			if b.Name == src.MainRef {
				tip = b.SHA
			}
		}
		if len(tip) != 40 || !fullSHA.MatchString(tip) {
			return invalid("configured main missing")
		}
		ancestor := func(base, head string) error {
			if base == head {
				return nil
			}
			v, e := s.provider.Compare(ctx, snap.SourceConnection, snap.BuildRequest.Repository, base, head)
			if e != nil || v.BaseSHA != base || v.HeadSHA != head || v.Behind != 0 {
				return invalid("candidate source is not current main ancestry")
			}
			return nil
		}
		if err = ancestor(src.MainSHA, tip); err != nil {
			return err
		}
		selected := false
		for j := range src.Revisions {
			r := &src.Revisions[j]
			pr, e := observer.ObservePullRequest(ctx, snap.SourceConnection, snap.BuildRequest.Repository, r.PullRequestNumber)
			if e != nil || pr.Number != r.PullRequestNumber || pr.BaseRepositoryID != src.ProviderRepositoryID || pr.Base != src.MainRef || pr.HeadSHA != r.HeadSHA || pr.Head != r.HeadRef || !strings.EqualFold(pr.State, "merged") || pr.MergedAt.IsZero() || len(pr.MergeSHA) != 40 || !fullSHA.MatchString(pr.MergeSHA) {
				return invalid("merged main pull request identity mismatch")
			}
			minimal := struct {
				BaseID, HeadID                int64
				Number                        int
				Base, Head, HeadSHA, MergeSHA string
				MergedAt                      time.Time
			}{pr.BaseRepositoryID, pr.HeadRepositoryID, pr.Number, pr.Base, pr.Head, pr.HeadSHA, pr.MergeSHA, pr.MergedAt}
			observed := workerconfig.Fingerprint(minimal)
			if initial {
				r.MergeSHA = pr.MergeSHA
				r.ObservationHash = observed
			} else if r.MergeSHA != pr.MergeSHA || r.ObservationHash != observed {
				return invalid("merged pull request identity changed")
			}
			if pr.MergeSHA == src.MainSHA {
				selected = true
			}
			if err = ancestor(pr.MergeSHA, src.MainSHA); err != nil {
				return err
			}
		}
		if !selected {
			return invalid("pinned main must equal a selected pull request merge")
		}
	}
	if initial {
		op.MainCandidate.ConfigurationHash = mainCandidateSnapshotHash(op)
		for i := range op.PreparedDeployments {
			op.PreparedDeployments[i].MainCandidate = op.MainCandidate
		}
	}
	return nil
}

func mainCandidateGuard(st *domain.State, op *domain.ExternalOperation) error {
	if op == nil || op.Kind != "RELEASE_CANDIDATE" || op.MainCandidate == nil || op.CompositionSnapshot == nil || op.MainCandidate.EvidenceKind != "ATTRIBUTED_RCP_VERIFICATION" || op.Status == "CANCELLED" || op.Status == "FAILED" {
		return invalid("main candidate unavailable")
	}
	if !slices.Contains([]string{"QUEUE_BUILDS", "WAIT_CHILDREN"}, op.Phase) || len(op.Sources) != 0 || op.ParentID != "" || op.ReleaseCandidateID != "" || op.GitOpsPR != nil || op.GitOpsResult != nil || op.Run != nil {
		return invalid("main candidate build-only phase policy denied")
	}
	p := op.MainCandidate
	if p.ConfigurationHash == "" || mainCandidateSnapshotHash(op) != p.ConfigurationHash {
		return invalid("main candidate frozen parent snapshot changed")
	}
	if op.EnvironmentID != p.Request.EnvironmentID || op.RequestKey != p.Request.IdempotencyKey || op.RequestHash != workerconfig.Fingerprint(struct {
		Product, Actor string
		Input          domain.MainCandidateRequest
	}{op.ProductID, op.RequestedBy, p.Request}) {
		return invalid("main candidate request proof changed")
	}
	hash, err := mainCandidateAuthority(st, op.ProductID, p.Request)
	if err != nil {
		return err
	}
	if hash != p.AuthorityHash {
		return invalid("main candidate source authority changed")
	}
	if len(op.PreparedDeployments) != len(p.Request.MainSources) || len(p.Sources) != len(op.PreparedDeployments) {
		return invalid("main candidate component proof incomplete")
	}
	causal := mainCandidateCausalState(st)
	if err = releaseWorkReady(&causal, *op.CompositionSnapshot); err != nil {
		return err
	}
	for i, snap := range op.PreparedDeployments {
		selected := p.Request.MainSources[i]
		sourceProof := p.Sources[i]
		if sourceProof.RepositoryID != snap.Application.RepositoryID || len(sourceProof.Revisions) != len(selected.Revisions) || snap.Build.Mode != "NATIVE" || snap.Target.Purpose != "PROD" || snap.Target.GitOpsMode != "PR" || !snap.Target.AllowDeploy || snap.Build.RebuildMissing || !reflect.DeepEqual(snap.Build.Native, &selected.Native) {
			return invalid("main candidate effective binding changed")
		}
		for j, chosen := range selected.Revisions {
			frozen := sourceProof.Revisions[j]
			rev := revisionByID(st, chosen.RevisionID)
			if rev == nil || frozen.RevisionID != rev.ID || frozen.HeadSHA != rev.HeadCommit || frozen.HeadRef != rev.Branch || frozen.PullRequestNumber != chosen.PullRequestNumber || len(frozen.MergeSHA) != 40 || !fullSHA.MatchString(frozen.MergeSHA) || frozen.ObservationHash == "" {
				return invalid("main candidate merged revision proof changed")
			}
		}
		baseline := componentBuild(st, snap.Application.ID)
		target := environmentBinding(st, op.EnvironmentID, snap.Application.ID)
		source := repositoryBinding(st, snap.Source.RepositoryID)
		gitops := repositoryBinding(st, snap.GitOps.RepositoryID)
		env := environmentByID(st, op.EnvironmentID)
		app := applicationByID(st, snap.Application.ID)
		if baseline == nil || snap.BuildBaseline == nil || !reflect.DeepEqual(*baseline, *snap.BuildBaseline) || target == nil || !reflect.DeepEqual(*target, snap.Target) || source == nil || !reflect.DeepEqual(*source, snap.Source) || gitops == nil || !reflect.DeepEqual(*gitops, snap.GitOps) || env == nil || env.ProductID != op.ProductID || env.Cluster != snap.Environment.Cluster || env.Namespace != snap.Environment.Namespace || app == nil || !reflect.DeepEqual(*app, snap.Application) {
			return invalid("main candidate frozen configuration changed")
		}
		for _, pair := range []struct {
			ID     string
			Config delivery.Connection
		}{{snap.Source.ConnectionID, snap.SourceConnection}, {snap.GitOps.ConnectionID, snap.GitOpsConnection}} {
			conn, e := scopedConnection(st, pair.ID, op.ProductID)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(conn.Config, pair.Config) {
				return invalid("main candidate provider authority changed")
			}
		}
		physical, parseErr := strconv.ParseInt(repositoryLocator(st, snap.Source), 10, 64)
		if parseErr != nil || physical != p.Sources[i].ProviderRepositoryID || configuredBase(snap.Source) != p.Sources[i].MainRef {
			return invalid("main candidate physical repository changed")
		}
		if snap.MainCandidate == nil || !reflect.DeepEqual(*snap.MainCandidate, *p) || p.Sources[i].MainSHA != p.Request.MainSources[i].MainSHA || snap.Application.ID != p.Request.MainSources[i].ApplicationID {
			return invalid("main candidate frozen proof mismatch")
		}
	}
	return nil
}
func (s *Service) mainCandidateAllowed(st *domain.State, op *domain.ExternalOperation) error {
	if op != nil {
		if err := reservationOperationAllowed(st, *op); err != nil {
			return err
		}
	}
	if err := mainCandidateGuard(st, op); err != nil {
		return err
	}
	if nativeQuarantined(st, op.ProductID) {
		return invalid("main candidate quarantined")
	}
	for _, snap := range op.PreparedDeployments {
		recipe, _, err := s.nativeRecipes(op.ProductID, snap.Application, snap.Build.Profile, snap.Build.ImageRepository, snap.Build.Native)
		if err != nil {
			return err
		}
		matched := false
		for _, repo := range st.Repositories {
			if repo.ID == snap.Application.RepositoryID && repo.ProductID == op.ProductID && repo.URL == recipe.RepositoryURL {
				matched = true
			}
		}
		if !matched {
			return invalid("main candidate repository URL changed")
		}
		if err := s.validateFrozenNative(op.ProductID, snap, op.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) nativeMainChildAllowed(st *domain.State, child *domain.ExternalOperation) error {
	if child == nil || !child.BuildOnly || child.Snapshot == nil || child.Snapshot.MainCandidate == nil || child.ParentID == "" || child.ReleaseCandidateID != "" || child.Snapshot.Target.Purpose != "PROD" {
		return invalid("main candidate child policy denied")
	}
	if !slices.Contains([]string{"NATIVE_BUILD", "INSPECT", "APPLY"}, child.Phase) || child.GitOpsPR != nil || child.GitOpsResult != nil || child.GitOpsBase != nil || child.Native == nil || child.Run != nil {
		return invalid("main candidate child phase cannot write GitOps or dispatch")
	}
	parent := operationByID(st, child.ParentID)
	if parent == nil || !slices.Contains(parent.ChildIDs, child.ID) {
		return invalid("main candidate child link missing")
	}
	if err := s.mainCandidateAllowed(st, parent); err != nil {
		return err
	}
	if !reflect.DeepEqual(parent.MainCandidate, child.Snapshot.MainCandidate) {
		return invalid("main candidate child proof changed")
	}
	found := false
	for _, snap := range parent.PreparedDeployments {
		if snap.Application.ID != child.ApplicationID {
			continue
		}
		found = true
		if !reflect.DeepEqual(snap.Build, child.Snapshot.Build) || !reflect.DeepEqual(snap.BuildBaseline, child.Snapshot.BuildBaseline) || !reflect.DeepEqual(snap.Application, child.Snapshot.Application) || !reflect.DeepEqual(snap.Source, child.Snapshot.Source) || !reflect.DeepEqual(snap.Target, child.Snapshot.Target) || child.Snapshot.Revision.HeadCommit != parent.MainCandidate.Request.MainSources[slices.IndexFunc(parent.PreparedDeployments, func(v domain.DeliverySnapshot) bool { return v.Application.ID == child.ApplicationID })].MainSHA {
			return invalid("main candidate child snapshot changed")
		}
	}
	if !found {
		return invalid("main candidate component missing")
	}
	return nil
}

// Disallow case aliases accepted by encoding/json's struct field matching. Data
// enters through several transports; the application enforces one input spelling.
func exactMainCandidateKeys(data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return invalid("invalid main candidate data")
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return invalid("invalid main candidate data")
	}
	keys := func(obj map[string]any, names ...string) error {
		for k := range obj {
			if !slices.Contains(names, k) {
				return invalid("unknown main candidate field %s", k)
			}
		}
		return nil
	}
	if err = keys(root, "name", "environment_id", "idempotency_key", "main_sources"); err != nil {
		return err
	}
	sources, ok := root["main_sources"].([]any)
	if !ok {
		return invalid("main_sources must be an array")
	}
	for _, v := range sources {
		src, ok := v.(map[string]any)
		if !ok {
			return invalid("invalid main source")
		}
		if err = keys(src, "application_id", "main_sha", "native", "revisions"); err != nil {
			return err
		}
		native, ok := src["native"].(map[string]any)
		if !ok {
			return invalid("explicit native binding required")
		}
		if _, ok := native["parameters"].(map[string]any); !ok {
			return invalid("explicit native parameters object required")
		}
		if err = keys(native, "build_recipe_id", "publication_recipe_id", "parameters"); err != nil {
			return err
		}
		revisions, ok := src["revisions"].([]any)
		if !ok {
			return invalid("revisions must be an array")
		}
		for _, v := range revisions {
			rev, ok := v.(map[string]any)
			if !ok {
				return invalid("invalid revision")
			}
			if err = keys(rev, "revision_id", "pull_request_number", "verification_result_ids"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reuse all existing readiness/applicability rules with a candidate-local causal
// projection. It does not mutate stored history or the global gate engine.
func mainCandidateCausalState(st *domain.State) domain.State {
	out := *st
	last := map[string]int{}
	for i, r := range st.Results {
		last[r.CheckID] = i
	}
	out.Results = []domain.CheckResult{}
	for i, r := range st.Results {
		if last[r.CheckID] == i {
			out.Results = append(out.Results, r)
		}
	}
	return out
}

func mainCandidateSnapshotHash(op *domain.ExternalOperation) string {
	snapshots := append([]domain.DeliverySnapshot(nil), op.PreparedDeployments...)
	for i := range snapshots {
		snapshots[i].MainCandidate = nil
	}
	return workerconfig.Fingerprint(struct {
		Composition *domain.Composition
		Snapshots   []domain.DeliverySnapshot
	}{op.CompositionSnapshot, snapshots})
}
