package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"slices"
	"sort"
	"time"
)

func (s *Service) queueRestorationRegistry(st *domain.State, op *domain.ExternalOperation) error {
	p := *op.DevRestoration
	current := operationByID(st, op.ID)
	for i, c := range p.Components {
		if c.ArtifactID != "" {
			continue
		}
		r, err := s.baselineObservationAuthority(st, op.ProductID, c)
		if err != nil {
			return err
		}
		scope := observationScope(r)
		retention := s.evaluateRetention(*st, op.ProductID, time.Now().UTC())
		roots := domain.RegistryKnownRoots(retention, scope.ImageRepository)
		if !slices.Contains(roots, c.Digest) || len(roots) > r.Limits.MaxNodes || len(roots) > 256 {
			return invalid("baseline protected registry roots missing or exceed limits")
		}
		sort.Strings(roots)
		registry := &domain.WorkerRegistryInputs{Scope: scope, Roots: roots, RootSetHash: domain.RegistryRootSetHash(scope, r.ID, r.Revision, r.Hash, s.externalWorkers.Revision(), roots), RequestRetentionInputHash: retention.InputHash, Limits: r.Limits}
		repoURL := ""
		for _, repo := range st.Repositories {
			if repo.ID == c.RepositoryID && repo.ProductID == op.ProductID {
				repoURL = repo.URL
			}
		}
		inputs := domain.WorkerInputs{RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, ConfigRevision: s.externalWorkers.Revision(), RepositoryID: c.RepositoryID, RepositoryURL: repoURL, Trust: "registry_observation", Parameters: map[string]any{}, Registry: registry}
		now := time.Now().UTC()
		m := domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}
		child := domain.ExternalOperation{Meta: m, Kind: domain.RegistryObserveOperationKind, ParentID: op.ID, ApplicationID: c.ApplicationID, RepositoryID: c.RepositoryID, RequestedBy: op.RequestedBy, RegistryObservation: &inputs, Status: "PENDING", Phase: "EXTERNAL_QUEUED"}
		m.ID = id()
		job := domain.WorkerJob{Meta: m, OperationID: child.ID, RequestKey: "restoration-registry-" + workerconfig.Fingerprint(struct{ Capture, App string }{op.ID, c.ApplicationID})[7:], RequestHash: workerconfig.Fingerprint(inputs), Capability: "registry_observe", Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: now.Add(time.Hour), MaxAttempts: 1}
		c.RegistryJobID = job.ID
		c.RegistryInputHash = job.InputHash
		c.RegistryRootSetHash = registry.RootSetHash
		p.Components[i] = c
		current.ChildIDs = append(current.ChildIDs, child.ID)
		st.Operations = append(st.Operations, child)
		st.WorkerJobs = append(st.WorkerJobs, job)
		workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], op.RequestedBy, "baseline_registry_queued", now)
		current = operationByID(st, op.ID)
	}

	p.SnapshotHash = restorationProofHash(p)
	current.DevRestoration = &p
	if !restorationFits(current) {
		return invalid("recovery registry authority exceeds bound")
	}
	return nil
}

// Reconciliation never runs candidate source/auth eligibility or refreshes a token.
// A missing lookup is UNKNOWN, including an absent proposal after a lost send.
func (s *Service) reconcileRestorationEffects(ctx context.Context, st *domain.State, op domain.ExternalOperation, includeOwn bool) (string, error) {
	provider, ok := s.provider.(delivery.GitOpsBatchPRProvider)
	if !ok {
		return "", invalid("grouped recovery provider required")
	}
	effects := append([]domain.DevRestorationEffect(nil), op.DevRestoration.Effects...)
	if includeOwn && op.DevRestorationPlan != nil {
		for _, g := range op.DevRestorationPlan.Groups {
			if g.Batch != nil && g.SendStarted {
				effects = append(effects, domain.DevRestorationEffect{OperationID: g.LeaderID, Connection: g.Connection, Batch: g.Batch, PullRequest: g.PullRequest})
			}
		}
	}
	if len(effects) > 256 || !restorationFits(effects) {
		return "", invalid("recovery effect history exceeds bound")
	}
	settled := []domain.DevRestorationEffect{}
	for _, effect := range effects {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		if effect.NoIntent {
			current := operationByID(st, effect.OperationID)
			if current == nil || current.DevActivationBatch != nil || current.DevActivationIntentHash != "" || current.GitOpsPR != nil || current.GitOpsResult != nil || current.ReservationMutationIntent != "" {
				return "", invalid("original no-intent proof changed")
			}
			settled = append(settled, effect)
			continue
		}
		if effect.Batch == nil {
			return "", invalid("unknown durable recovery intent")
		}
		var pr delivery.GitOpsPullRequest
		var e error
		if effect.PullRequest != nil {
			pr, e = provider.ObserveGitOpsBatchPR(delivery.WithCachedAuthentication(ctx), effect.Connection, *effect.Batch, *effect.PullRequest)
		} else {
			pr, e = provider.LookupGitOpsBatchProposal(delivery.WithCachedAuthentication(ctx), effect.Connection, *effect.Batch)
		}
		if e != nil || pr.Number <= 0 || pr.URL == "" || !fullSHA.MatchString(pr.HeadSHA) {
			return "", invalid("UNKNOWN exact original/recovery effect; hold retained")
		}
		if effect.PullRequest != nil && activationPRHash(pr) != activationPRHash(*effect.PullRequest) {
			return "", invalid("effect PR/head changed")
		}
		if !pr.Merged && pr.State != "closed" {
			return "", invalid("open or reopened original/recovery proposal blocks restoration")
		}
		effect.PullRequest = &pr
		settled = append(settled, effect)
	}
	return workerconfig.Fingerprint(settled), nil
}
func (s *Service) restorationAvailability(ctx context.Context, op domain.ExternalOperation) ([]domain.DevBaselineComponentResult, error) {
	st, e := s.store.Read(ctx)
	if e != nil {
		return nil, e
	}
	results := []domain.DevBaselineComponentResult{}
	for _, c := range op.DevRestoration.Components {
		cr := domain.DevBaselineComponentResult{ApplicationID: c.ApplicationID, ImageRepository: c.ImageRepository, Digest: c.Digest, GitOps: *c.ObservedGitOps, ArtifactID: c.ArtifactID, ArtifactHash: c.ArtifactHash}
		if c.ArtifactID != "" {
			a, e := s.inspectBaselineArtifact(ctx, op, c)
			if e != nil || !a.Available || a.Digest != c.Digest || a.ObservedAt.Before(op.CreatedAt) {
				return nil, invalid("original image bytes unavailable")
			}
			cr.Availability = &a
		} else {
			receipt, e := s.baselineRegistryReceipt(&st, op, c, time.Now().UTC())
			if e != nil {
				return nil, e
			}
			cr.Registry = receipt
		}
		results = append(results, cr)
	}
	return results, nil
}

// Output repositories can only follow this component's exact admitted mapping.
// A sibling's same-file image selector is never a substitute for this mapping.
func restorationCurrentRepositories(st *domain.State, op domain.ExternalOperation, c domain.DevBaselineComponent, connection delivery.Connection) ([]string, error) {
	out := []string{c.ImageRepository}
	for _, effect := range op.DevRestoration.Effects {
		if effect.NoIntent || effect.Batch == nil || activationPhysicalKey(effect.Connection, effect.Batch.Repository, effect.Batch.Ref) != activationPhysicalKey(connection, c.GitOps.Repository, c.GitOps.Ref) {
			continue
		}
		for _, change := range effect.Batch.Changes {
			same := change.ID == c.ApplicationID
			if child := operationByID(st, change.ID); child != nil && child.ApplicationID == c.ApplicationID && child.ProductID == op.ProductID {
				same = true
			}
			if !same || change.Selector.Path != c.GitOps.Path || change.Selector.ImageField != c.GitOps.ImageField || change.Selector.DigestField != c.GitOps.DigestField {
				continue
			}
			if !slices.Contains(out, change.Selector.ImageRepository) {
				return nil, invalid("admitted component image mapping chain is discontinuous")
			}
			if !slices.Contains(out, change.ImageRepository) {
				out = append(out, change.ImageRepository)
			}
		}
	}
	if len(out) > 2 {
		return nil, invalid("restoration component image mapping exceeds immutable candidate/original bound")
	}
	return out, nil
}
func (s *Service) knownRestorationBlob(ctx context.Context, op domain.ExternalOperation, c domain.DevBaselineComponent, connection delivery.Connection, actual delivery.GitOpsSnapshot) error {
	if c.ObservedGitOps != nil && actual.BlobSHA == c.ObservedGitOps.BlobSHA && actual.ImageRepository == c.ImageRepository && actual.Digest == c.Digest && workerconfig.Fingerprint(actual.Content) == c.ObservedGitOps.ContentHash {
		return nil
	}
	verifier := s.provider.(delivery.GitOpsRevisionVerifier)
	provider := s.provider.(delivery.GitOpsBatchPRProvider)
	for _, effect := range op.DevRestoration.Effects {
		if effect.NoIntent || effect.Batch == nil || activationPhysicalKey(effect.Connection, effect.Batch.Repository, effect.Batch.Ref) != activationPhysicalKey(connection, c.GitOps.Repository, c.GitOps.Ref) {
			continue
		}
		sameFile := false
		for _, change := range effect.Batch.Changes {
			if change.Selector.Path == c.GitOps.Path {
				sameFile = true
			}
		}
		if !sameFile {
			continue
		}
		var pr delivery.GitOpsPullRequest
		var e error
		if effect.PullRequest != nil {
			pr, e = provider.ObserveGitOpsBatchPR(ctx, effect.Connection, *effect.Batch, *effect.PullRequest)
		} else {
			pr, e = provider.LookupGitOpsBatchProposal(ctx, effect.Connection, *effect.Batch)
		}
		if e != nil || !pr.Merged {
			continue
		}
		// Read this component's exact scalar selector at the proven full-file commit.
		// This also covers untouched C sharing a file changed only by selected A/B.
		req := c.GitOps
		req.Ref = pr.HeadSHA
		req.ExpectedHeadSHA = pr.HeadSHA
		req.ImageRepository = actual.ImageRepository
		known, e := s.readRestorationCommit(ctx, effect.Connection, req, pr.HeadSHA)
		if e != nil || known.HeadSHA != pr.HeadSHA || known.ImageRepository != actual.ImageRepository || known.Digest != actual.Digest {
			continue
		}
		ancestry := delivery.GitOpsRevisionRequest{Repository: c.GitOps.Repository, Ref: c.GitOps.Ref, HeadSHA: actual.HeadSHA, AncestorSHAs: []string{pr.HeadSHA}}
		proof, e := verifier.VerifyGitOpsRevision(ctx, connection, ancestry)
		if e == nil && validRevisionProof(ancestry, proof) && known.BlobSHA == actual.BlobSHA && workerconfig.Fingerprint(known.Content) == workerconfig.Fingerprint(actual.Content) {
			return nil
		}
	}
	return invalid("selected file is neither original nor exact admitted effect blob")
}
func (s *Service) readCurrentRestoration(ctx context.Context, st *domain.State, op domain.ExternalOperation, c domain.DevBaselineComponent, connection delivery.Connection, head string) (delivery.GitOpsSnapshot, error) {
	repositories, e := restorationCurrentRepositories(st, op, c, connection)
	if e != nil {
		return delivery.GitOpsSnapshot{}, e
	}
	var found *delivery.GitOpsSnapshot
	for _, repository := range repositories {
		selector := c.GitOps
		selector.ExpectedHeadSHA = head
		selector.ImageRepository = repository
		actual, e := s.provider.ReadGitOps(ctx, connection, selector)
		if e != nil || actual.HeadSHA != head || actual.ImageRepository != repository || !fullSHA.MatchString(actual.BlobSHA) || actual.Content == "" {
			continue
		}
		if e = s.knownRestorationBlob(ctx, op, c, connection, actual); e != nil {
			continue
		}
		if found != nil {
			return delivery.GitOpsSnapshot{}, invalid("ambiguous original/admitted component image selector")
		}
		copy := actual
		found = &copy
	}
	if found == nil {
		return delivery.GitOpsSnapshot{}, invalid("exact original/admitted current component selector unavailable")
	}
	return *found, nil
}
func (s *Service) prepareRestorationPlan(ctx context.Context, op domain.ExternalOperation, token, settlement string) (domain.DevRestorationPlan, error) {
	plan := domain.DevRestorationPlan{ExpectedHash: op.DevRestoration.SnapshotHash, SettlementHash: settlement}
	st, e := s.store.Read(ctx)
	if e != nil {
		return plan, e
	}
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return plan, invalid("recovery ancestry verifier required")
	}
	groups := map[string]int{}
	for _, c := range op.DevRestoration.Components {
		if e = s.validateRestorationIO(ctx, op, token, true); e != nil {
			return plan, e
		}
		conn, e := scopedConnection(&st, c.ConnectionID, op.ProductID)
		if e != nil {
			return plan, e
		}
		key := activationPhysicalKey(conn.Config, c.GitOps.Repository, c.GitOps.Ref)
		gi, ok := groups[key]
		if !ok {
			gi = len(plan.Groups)
			groups[key] = gi
			plan.Groups = append(plan.Groups, domain.DevRestorationGroup{ID: workerconfig.Fingerprint(key), LeaderID: op.ID + "-" + workerconfig.Fingerprint(key)[7:23], Connection: conn.Config, Repository: c.GitOps.Repository, Ref: c.GitOps.Ref})
		}
		g := &plan.Groups[gi]
		if !reflect.DeepEqual(g.Connection, conn.Config) {
			return plan, invalid("physical recovery aliases require identical connection")
		}
		head := ""
		for _, m := range plan.Components {
			if m.GroupID == g.ID {
				head = m.Current.HeadSHA
				break
			}
		}
		ancestry := delivery.GitOpsRevisionRequest{Repository: g.Repository, Ref: g.Ref, HeadSHA: head, AncestorSHAs: []string{c.ObservedGitOps.HeadSHA}}
		revision, e := verifier.VerifyGitOpsRevision(ctx, conn.Config, ancestry)
		if e != nil || !validRevisionProof(ancestry, revision) {
			return plan, invalid("original recovery ancestry unavailable")
		}
		actual, e := s.readCurrentRestoration(ctx, &st, op, c, conn.Config, revision.HeadSHA)
		if e != nil {
			return plan, e
		}
		selector := c.GitOps
		current := runtimeGitOpsObservation(actual)
		no := current.ImageRepository == c.ImageRepository && current.Digest == c.Digest
		selector = c.GitOps
		selector.ImageRepository = current.ImageRepository
		selector.ExpectedHeadSHA = ""
		plan.Components = append(plan.Components, domain.DevRestorationMember{ApplicationID: c.ApplicationID, GroupID: g.ID, Original: *c.ObservedGitOps, Current: current, Selector: selector, NoWrite: no})
		if !no {
			if g.Batch == nil {
				g.Batch = &delivery.GitOpsBatchApply{Repository: g.Repository, Ref: g.Ref, ExpectedHeadSHA: revision.HeadSHA, OperationID: g.LeaderID, Message: "release-control: " + g.LeaderID}
			}
			g.Batch.Changes = append(g.Batch.Changes, delivery.GitOpsImageChange{ID: c.ApplicationID, Selector: selector, ExpectedBlobSHA: current.BlobSHA, ImageRepository: c.ImageRepository, Digest: c.Digest})
		}
	}
	if len(op.DevRestoration.Effects)+len(plan.Groups) > 256 {
		return plan, invalid("complete recovery effect set exceeds256")
	}
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Batch != nil {
			g.IntentHash = workerconfig.Fingerprint(g.Batch)
		}
	}
	for _, m := range plan.Components {
		g := plan.Groups[groups[activationPhysicalKey(matchingRestorationConnection(plan, m.GroupID), m.Selector.Repository, m.Selector.Ref)]]
		req := m.Selector
		req.ExpectedHeadSHA = m.Current.HeadSHA
		if e = s.validateRestorationIO(ctx, op, token, true); e != nil {
			return plan, e
		}
		v, e := s.provider.ReadGitOps(ctx, g.Connection, req)
		if e != nil || runtimeGitOpsObservation(v) != m.Current {
			return plan, invalid("recovery target moved during preparation")
		}
	}
	if !restorationFits(struct {
		Proof *domain.DevRestorationProof
		Plan  domain.DevRestorationPlan
	}{op.DevRestoration, plan}) {
		return plan, invalid("complete recovery plan exceeds bound")
	}
	return plan, nil
}
func matchingRestorationConnection(plan domain.DevRestorationPlan, id string) delivery.Connection {
	for _, g := range plan.Groups {
		if g.ID == id {
			return g.Connection
		}
	}
	return delivery.Connection{}
}
func (s *Service) saveRestoration(ctx context.Context, op domain.ExternalOperation, token, phase string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) || ctx.Err() != nil {
			return invalid("recovery final save lease lost")
		}
		if e := s.restorationAllowed(st, op, false); e != nil {
			return e
		}
		if e := validateRestorationPlan(op); e != nil {
			return e
		}
		if !restorationFits(op) {
			return invalid("recovery final proof exceeds bound")
		}
		current.DevRestorationPlan = op.DevRestorationPlan
		current.DevRestorationAvailability = op.DevRestorationAvailability
		current.Phase = phase
		current.CurrentStep = phase
		current.Status = "RUNNING"
		if phase == "GITOPS_APPLIED" {
			current.Status = "SUCCEEDED"
			current.DeploymentState = phase
			now := time.Now().UTC()
			current.FinishedAt = &now
		}
		current.UpdatedAt = time.Now().UTC()
		current.NextAttemptAt = current.UpdatedAt.Add(time.Second)
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		return nil
	})
}
func (s *Service) tickDevRestoration(ctx context.Context, op domain.ExternalOperation, token string) error {
	ctx = delivery.WithCachedAuthentication(ctx)
	if e := s.validateRestorationIO(ctx, op, token, false); e != nil {
		return s.retryRestoration(ctx, op, token, "Recovery authority unavailable; hold retained")
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	if op.Phase == "RECONCILE_EFFECTS" {
		settlement, e := s.reconcileRestorationEffects(ctx, &st, op, false)
		if e != nil {
			return s.retryRestoration(ctx, op, token, "Unknown/open original effects; hold retained")
		}
		availability, e := s.restorationAvailability(ctx, op)
		if e != nil {
			return s.retryRestoration(ctx, op, token, "Waiting for fresh complete original image availability")
		}
		plan, e := s.prepareRestorationPlan(ctx, op, token, settlement)
		if e != nil {
			return s.retryRestoration(ctx, op, token, "Conservative recovery plan unavailable; hold retained")
		}
		op.DevRestorationAvailability = availability
		op.DevRestorationPlan = &plan
		// Persist complete intent before the first mutating HTTP request, including auth.
		return s.saveRestoration(ctx, op, token, "PREPARE_PROPOSALS")
	}
	if op.DevRestorationPlan == nil {
		return s.retryRestoration(ctx, op, token, "Recovery intent missing; hold retained")
	}
	if op.Phase == "PREPARE_PROPOSALS" {
		provider := s.provider.(delivery.GitOpsBatchPRProvider)
		for i, g := range op.DevRestorationPlan.Groups {
			if g.Batch == nil || g.PullRequest != nil {
				continue
			}
			if e = s.validateRestorationIO(ctx, op, token, false); e != nil {
				return e
			}
			// A separate persisted send marker distinguishes cold intent from ambiguity.
			if g.SendStarted {
				pr, e := provider.LookupGitOpsBatchProposal(ctx, g.Connection, *g.Batch)
				if e != nil || pr.Number <= 0 || !fullSHA.MatchString(pr.HeadSHA) {
					return s.retryRestoration(ctx, op, token, "UNKNOWN proposal; cached GET-only reconciliation; no recreation")
				}
				g.PullRequest = &pr
				op.DevRestorationPlan.Groups[i] = g
				return s.saveRestoration(ctx, op, token, "PREPARE_PROPOSALS")
			}
			if e = s.validateRestorationIO(ctx, op, token, true); e != nil {
				return s.retryRestoration(ctx, op, token, "Restoration-only write authority expired; hold retained")
			}
			// Empty non-nil result is a durable send latch, never deployment proof.
			g.SendStarted = true
			op.DevRestorationPlan.Groups[i] = g
			if e = s.latchRestorationSend(ctx, op, token); e != nil {
				return e
			}
			guard := func(c context.Context) error {
				if e := s.validateRestorationIO(c, op, token, true); e != nil {
					return e
				}
				if _, e := s.restorationAvailability(c, op); e != nil {
					return e
				}
				state, e := s.store.Read(c)
				if e != nil {
					return e
				}
				settlement, e := s.reconcileRestorationEffects(c, &state, op, false)
				if e != nil || settlement != op.DevRestorationPlan.SettlementHash {
					return invalid("original effect settlement changed before physical recovery send")
				}
				return s.validateRestorationIO(c, op, token, true)
			}
			pr, e := provider.ProposeGitOpsBatchGuarded(ctx, g.Connection, *g.Batch, guard)
			if e != nil {
				return s.retryRestoration(ctx, op, token, "UNKNOWN proposal send; hold retained")
			}
			proof, e := provider.LookupGitOpsBatchProposal(ctx, g.Connection, *g.Batch)
			if e != nil || proof.Number <= 0 || activationPRHash(pr) != activationPRHash(proof) {
				return s.retryRestoration(ctx, op, token, "UNKNOWN initial proposal proof; hold retained")
			}
			g.PullRequest = &proof
			op.DevRestorationPlan.Groups[i] = g
			return s.saveRestoration(ctx, op, token, "PREPARE_PROPOSALS")
		}
		// Stable complete proposal set. An early merge cannot receive confirmation.
		if e = s.observeRestorationPlan(ctx, op, false); e != nil {
			if e == errRestorationUnapprovedMerge {
				return s.markRestorationUnapproved(ctx, op, token)
			}
			return s.retryRestoration(ctx, op, token, "Proposal set unstable or early external merge; hold retained")
		}
		op.DevRestorationPlan.Hash = restorationPlanHash(*op.DevRestorationPlan)
		return s.saveRestoration(ctx, op, token, "WAIT_EXTERNAL_MERGE")
	}
	if op.Phase == "WAIT_EXTERNAL_MERGE" {
		confirmation := restorationConfirmation(&st, op)
		if confirmation == nil {
			return s.retryRestoration(ctx, op, token, "Separate attributed control confirmation required; external human-only merge gate retained")
		}
		if e = s.observeRestorationPlan(ctx, op, true); e != nil {
			if e == errRestorationUnapprovedMerge {
				return s.markRestorationUnapproved(ctx, op, token)
			}
			return s.retryRestoration(ctx, op, token, "Waiting for exact external restoration merges")
		}
		if e = s.collectRestorationResults(ctx, &op); e != nil {
			if e == errRestorationUnapprovedMerge {
				return s.markRestorationUnapproved(ctx, op, token)
			}
			return s.retryRestoration(ctx, op, token, "Complete all-original desired state unavailable")
		}
		return s.saveRestoration(ctx, op, token, "GITOPS_APPLIED")
	}
	return s.retryRestoration(ctx, op, token, "Unknown recovery phase; hold retained")
}
func (s *Service) latchRestorationSend(ctx context.Context, op domain.ExternalOperation, token string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		cur := operationByID(st, op.ID)
		if cur == nil || cur.LeaseOwner != token || !cur.LeaseUntil.After(time.Now().UTC()) || ctx.Err() != nil {
			return invalid("recovery send lease lost")
		}
		if e := s.restorationAllowed(st, op, true); e != nil {
			return e
		}
		if e := validateRestorationPlan(op); e != nil {
			return e
		}
		if !restorationFits(op) {
			return invalid("recovery send proof exceeds bound")
		}
		cur.DevRestorationPlan = op.DevRestorationPlan
		return nil
	})
}
func (s *Service) observeRestorationPlan(ctx context.Context, op domain.ExternalOperation, merged bool) error {
	p := op.DevRestorationPlan
	if p == nil {
		return invalid("complete recovery plan required")
	}
	provider, ok := s.provider.(delivery.GitOpsBatchPRProvider)
	if !ok {
		return invalid("grouped provider required")
	}
	for _, g := range p.Groups {
		if g.Batch == nil {
			continue
		}
		if g.PullRequest == nil || g.IntentHash != workerconfig.Fingerprint(g.Batch) || !g.SendStarted {
			return invalid("complete recovery proposal required")
		}
		pr, e := provider.ObserveGitOpsBatchPR(delivery.WithCachedAuthentication(ctx), g.Connection, *g.Batch, *g.PullRequest)
		if e != nil || activationPRHash(pr) != activationPRHash(*g.PullRequest) {
			return invalid("recovery proposal head changed or unknown")
		}
		if merged && !pr.Merged {
			return invalid("external recovery merge pending")
		}
		if merged {
			st, e := s.store.Read(ctx)
			if e != nil {
				return e
			}
			confirmation := restorationConfirmation(&st, op)
			if confirmation == nil {
				return invalid("current separate attributed confirmation required")
			}
			if pr.MergedAt == nil || pr.MergedAt.After(time.Now().UTC()) || !pr.MergedAt.After(confirmation.DevRestorationConfirmation.ConfirmedAt) {
				return errRestorationUnapprovedMerge
			}
		}

		if !merged && (pr.Merged || pr.State != "open") {
			if pr.Merged {
				return errRestorationUnapprovedMerge
			}
			return invalid("unapproved early external merge/closed proposal")
		}
	}
	if !merged {
		for _, m := range p.Components {
			connection := matchingRestorationConnection(*p, m.GroupID)
			req := m.Selector
			req.ExpectedHeadSHA = m.Current.HeadSHA
			actual, e := s.provider.ReadGitOps(ctx, connection, req)
			if e != nil || runtimeGitOpsObservation(actual) != m.Current {
				return invalid("recovery plan target changed before confirmation")
			}
		}
	}
	return nil
}
func (s *Service) collectRestorationResults(ctx context.Context, op *domain.ExternalOperation) error {
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return invalid("revision verifier required")
	}
	for i, g := range op.DevRestorationPlan.Groups {
		if g.Batch != nil {
			provider, ok := s.provider.(delivery.GitOpsBatchPRProvider)
			if !ok || g.PullRequest == nil {
				return invalid("exact merged recovery group required")
			}
			pr, e := provider.ObserveGitOpsBatchPR(delivery.WithCachedAuthentication(ctx), g.Connection, *g.Batch, *g.PullRequest)
			if e != nil || !pr.Merged || activationPRHash(pr) != activationPRHash(*g.PullRequest) {
				return invalid("merged recovery group identity unavailable")
			}
			st, e := s.store.Read(ctx)
			if e != nil {
				return e
			}
			confirmation := restorationConfirmation(&st, *op)
			if confirmation == nil {
				return invalid("separate recovery confirmation required")
			}
			if pr.MergedAt == nil || pr.MergedAt.After(time.Now().UTC()) || !pr.MergedAt.After(confirmation.DevRestorationConfirmation.ConfirmedAt) {
				return errRestorationUnapprovedMerge
			}
			at := *pr.MergedAt
			g.MergedAt = &at
		}
		ancestors := []string{}
		for _, m := range op.DevRestorationPlan.Components {
			if m.GroupID == g.ID {
				ancestors = append(ancestors, m.Original.HeadSHA)
			}
		}
		if g.PullRequest != nil {
			ancestors = append(ancestors, g.PullRequest.HeadSHA)
		}
		slices.Sort(ancestors)
		ancestors = slices.Compact(ancestors)
		req := delivery.GitOpsRevisionRequest{Repository: g.Repository, Ref: g.Ref, AncestorSHAs: ancestors}
		revision, e := verifier.VerifyGitOpsRevision(ctx, g.Connection, req)
		if e != nil || !validRevisionProof(req, revision) {
			return invalid("original recovery ancestry unknown")
		}
		result := []domain.DevRuntimeGitOpsObservation{}
		for _, m := range op.DevRestorationPlan.Components {
			if m.GroupID != g.ID {
				continue
			}
			selector := m.Selector
			selector.ImageRepository = m.Original.ImageRepository
			selector.ExpectedHeadSHA = revision.HeadSHA
			v, e := s.provider.ReadGitOps(ctx, g.Connection, selector)
			if e != nil || v.HeadSHA != revision.HeadSHA || !fullSHA.MatchString(v.BlobSHA) || v.ImageRepository != m.Original.ImageRepository || v.Digest != m.Original.Digest {
				return invalid("complete original recovery image not applied")
			}
			changedFile := false
			if g.Batch != nil {
				for _, change := range g.Batch.Changes {
					if change.Selector.Path == m.Selector.Path {
						changedFile = true
					}
				}
			}
			if changedFile {
				if g.PullRequest == nil {
					return invalid("changed recovery member proposal missing")
				}
				proposal := selector
				proposal.Ref = g.PullRequest.HeadSHA
				proposal.ExpectedHeadSHA = g.PullRequest.HeadSHA
				known, e := s.readRestorationCommit(ctx, g.Connection, proposal, g.PullRequest.HeadSHA)
				if e != nil || known.HeadSHA != g.PullRequest.HeadSHA || known.BlobSHA != v.BlobSHA || known.ImageRepository != v.ImageRepository || known.Digest != v.Digest || workerconfig.Fingerprint(known.Content) != workerconfig.Fingerprint(v.Content) {
					return invalid("selected restored file drifted from exact admitted proposal blob")
				}
			}
			if !changedFile && (v.BlobSHA != m.Current.BlobSHA || workerconfig.Fingerprint(v.Content) != m.Current.ContentHash) {
				return invalid("untouched original file drift")
			}
			result = append(result, runtimeGitOpsObservation(v))
		}
		req.HeadSHA = revision.HeadSHA
		stable, e := verifier.VerifyGitOpsRevision(ctx, g.Connection, req)
		if e != nil || !validRevisionProof(req, stable) || stable.HeadSHA != revision.HeadSHA {
			return invalid("recovery group head changed")
		}
		g.Result = result
		op.DevRestorationPlan.Groups[i] = g
	}
	return nil
}

func (s *Service) validateRestorationAvailability(st *domain.State, op domain.ExternalOperation, components []domain.DevBaselineComponent, results []domain.DevBaselineComponentResult, at time.Time) error {
	if len(components) == 0 || len(results) != len(components) || at.IsZero() {
		return invalid("complete restoration availability required")
	}
	for i, c := range components {
		r := results[i]
		if r.ApplicationID != c.ApplicationID || r.ImageRepository != c.ImageRepository || r.Digest != c.Digest || r.ArtifactID != c.ArtifactID || r.ArtifactHash != c.ArtifactHash || c.ObservedGitOps == nil || r.GitOps != *c.ObservedGitOps {
			return invalid("restoration availability member changed")
		}
		if c.ArtifactID != "" {
			a := r.Availability
			if r.Registry != nil || a == nil || !a.Available || a.Digest != c.Digest || a.ObservedAt.Before(op.CreatedAt) || a.ObservedAt.After(at) || at.Sub(a.ObservedAt) > 5*time.Minute {
				return invalid("original image availability changed")
			}
			if _, e := s.baselineArtifact(st, op.ProductID, c); e != nil {
				return e
			}
		} else {
			receipt, e := s.baselineRegistryReceipt(st, op, c, at)
			if e != nil || r.Availability != nil || !reflect.DeepEqual(receipt, r.Registry) {
				return invalid("original causal IMAGE_CONTENT receipt changed")
			}
		}
	}
	return nil
}

func (s *Service) readRestorationCommit(ctx context.Context, connection delivery.Connection, request delivery.GitOpsRequest, commit string) (delivery.GitOpsSnapshot, error) {
	reader, ok := s.provider.(delivery.GitOpsCommitReader)
	if !ok {
		return delivery.GitOpsSnapshot{}, invalid("cached-only immutable GitOps commit reader required")
	}
	return reader.ReadGitOpsAtCommit(delivery.WithCachedAuthentication(ctx), connection, request, commit)
}
