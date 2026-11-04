package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligence"
	"releasecontrol/internal/intelligencepatch"
	"releasecontrol/internal/workerconfig"
)

// IntelligenceProvider is a capability-limited port. Production uses the
// operator-enrolled adapter; tests substitute only model completion.
type IntelligenceProvider interface {
	Revision() string
	Allows(string) bool
	Model() string
	MaxRequests() int
	Timeout() time.Duration
	Paths(string) []string
	Fingerprint(any) string
	Seal(string, any) (string, error)
	Open(string, string, any) error
	ValidateInput(any) error
	Complete(context.Context, string, any) (json.RawMessage, intelligence.Usage, error)
}

// ConfigureIntelligence is an operator startup seam, never an API command.
func (s *Service) ConfigureIntelligence(a IntelligenceProvider) { s.intelligence = a }

type intelligenceFile struct {
	Path   string `json:"path"`
	Base   string `json:"base"`
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}
type intelligenceRequest struct {
	TaskKind             string             `json:"task_kind"`
	RequestKey           string             `json:"request_key"`
	CandidateOperationID string             `json:"candidate_operation_id,omitempty"`
	PreviousReleaseIDs   map[string]string  `json:"previous_release_ids,omitempty"`
	PreviousReleaseID    string             `json:"previous_release_id,omitempty"`
	ConflictID           string             `json:"conflict_id,omitempty"`
	HeadSHA              string             `json:"head_sha,omitempty"`
	Excerpts             []string           `json:"excerpts,omitempty"`
	Files                []intelligenceFile `json:"files,omitempty"`
}
type intelligenceInput struct {
	Request intelligenceRequest `json:"request"`
	Context json.RawMessage     `json:"context"`
}
type intelligenceChange struct {
	IntegrationIDs []string `json:"integration_ids"`
	Text           string   `json:"text"`
	EvidenceRefs   []string `json:"evidence_refs"`
}
type intelligenceResult struct {
	Decision string                      `json:"decision"`
	Summary  string                      `json:"summary"`
	Changes  []intelligenceChange        `json:"changes"`
	Warnings []string                    `json:"warnings"`
	Patch    *intelligencepatch.Proposal `json:"patch"`
}

type intelligenceIntent struct {
	IntegrationID string   `json:"integration_id"`
	FeatureID     string   `json:"feature_id"`
	Title         string   `json:"title"`
	Objective     string   `json:"objective"`
	Acceptance    []string `json:"acceptance"`
	EvidenceRefs  []string `json:"evidence_refs"`
}
type notesContext struct {
	EnvironmentID        string               `json:"environment_id"`
	PreviousReleaseID    string               `json:"previous_release_id"`
	PreviousReleaseIDs   map[string]string    `json:"previous_release_ids"`
	CandidateOperationID string               `json:"candidate_operation_id"`
	PreviousSources      map[string]string    `json:"previous_sources"`
	Sources              map[string]string    `json:"sources"`
	PreviousArtifacts    map[string]string    `json:"previous_artifacts"`
	Artifacts            map[string]string    `json:"artifacts"`
	CandidateFingerprint string               `json:"candidate_fingerprint"`
	Changes              []intelligenceIntent `json:"changes"`
}

func (s *Service) executeIntelligence(ctx context.Context, c domain.Command) (any, error) {
	a := s.intelligence
	if c.Action != "cancel_intelligence_task" && (a == nil || !a.Allows(c.ProductID)) {
		return nil, invalid("intelligence is not enrolled for this Product")
	}
	if c.Actor == "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return nil, invalid("explicit actor and bounded task scope required")
	}
	var out any
	err := s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, c.ProductID) {
			return missing("product", c.ProductID)
		}
		if c.Action == "cancel_intelligence_task" {
			var empty struct{}
			if err := decode(c.Data, &empty); err != nil {
				return err
			}
			op := operationByID(st, c.ID)
			if op == nil || op.ProductID != c.ProductID || op.Intelligence == nil {
				return missing("intelligence task", c.ID)
			}
			if c.FeatureID != "" || c.IntegrationID != "" {
				return invalid("cancel uses task ID and Product only")
			}
			if terminalOperation(op.Status) {
				out = *op
				return nil
			}
			now := time.Now().UTC()
			op.Status = "CANCELLED"
			op.Phase = "CANCELLED"
			op.FinishedAt = &now
			op.LeaseOwner = ""
			op.LeaseUntil = time.Time{}
			op.UpdatedAt = now
			intelligenceAudit(st, *op, c.Actor, "intelligence_cancelled")
			out = *op
			return nil
		}
		if c.ID != "" {
			return invalid("task IDs are server generated")
		}
		var request intelligenceRequest
		if e := decode(c.Data, &request); e != nil {
			return e
		}
		if !workerRequestKey.MatchString(request.RequestKey) || !slices.Contains([]string{"annotation", "summary", "log_summary", "release_notes", "conflict_patch"}, request.TaskKind) {
			return invalid("typed task_kind and request_key required")
		}
		hash := a.Fingerprint(struct {
			Actor, Product, Feature, Integration string
			Request                              intelligenceRequest
		}{c.Actor, c.ProductID, c.FeatureID, c.IntegrationID, request})
		for _, op := range st.Operations {
			if op.Kind == domain.IntelligenceOperationKind && op.ProductID == c.ProductID && op.RequestKey == request.RequestKey {
				if op.RequestHash != hash {
					return invalid("request_key already names different inputs")
				}
				out = op
				return nil
			}
		}
		if c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in == nil || in.ProductID != c.ProductID || c.FeatureID != "" && in.FeatureID != c.FeatureID {
				return invalid("integration outside task scope")
			}
			c.FeatureID = in.FeatureID
		}
		if c.FeatureID != "" {
			f := feature(st, c.FeatureID)
			if f == nil || f.ProductID != c.ProductID {
				return invalid("feature outside task scope")
			}
		}
		context, e := s.intelligenceContext(st, c.ProductID, c.FeatureID, c.IntegrationID, request)
		if e != nil {
			return e
		}
		input := intelligenceInput{Request: request, Context: context}
		if e = a.ValidateInput(input); e != nil {
			return invalid("task context exceeds bounds or needs privacy redaction")
		}
		now := time.Now().UTC()
		m := meta(c)
		m.ID = id()
		m.FeatureID = c.FeatureID

		op := domain.ExternalOperation{Meta: m, Kind: domain.IntelligenceOperationKind, IntegrationID: c.IntegrationID, RequestedBy: c.Actor, Status: "PENDING", Phase: "QUEUED", RequestKey: request.RequestKey, RequestHash: hash, NextAttemptAt: now, Intelligence: &domain.IntelligenceTask{Kind: request.TaskKind, ConfigRevision: a.Revision(), PromptVersion: intelligence.PromptVersion, Model: a.Model(), InputFingerprint: a.Fingerprint(input), InputCiphertext: ""}}
		sealed, e := a.Seal(intelligenceAAD(op, "input"), input)
		if e != nil {
			return errors.New("cannot encrypt task input")
		}
		op.Intelligence.InputCiphertext = sealed
		st.Operations = append(st.Operations, op)
		intelligenceAudit(st, op, c.Actor, "intelligence_queued")
		out = op
		return nil
	})
	return out, err
}
func intelligenceAudit(st *domain.State, op domain.ExternalOperation, actor, action string) {
	st.Events = append(st.Events, domain.Event{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, EntityID: op.ID, Actor: actor, At: time.Now().UTC(), Action: action, Data: domain.Command{Action: action, ProductID: op.ProductID, ID: op.ID, Actor: actor, Data: map[string]any{"status": op.Status, "input_fingerprint": op.Intelligence.InputFingerprint}}})
}

func (s *Service) intelligenceContext(st *domain.State, productID, featureID, integrationID string, in intelligenceRequest) (json.RawMessage, error) {
	var value any
	switch in.TaskKind {
	case "release_notes":
		if featureID != "" || integrationID != "" || len(in.Files) > 0 || len(in.Excerpts) > 0 || in.ConflictID != "" || in.HeadSHA != "" {
			return nil, invalid("release notes use exact release references only")
		}
		ctx, e := s.releaseNotesContext(st, productID, in)
		if e != nil {
			return nil, e
		}
		value = ctx
	case "conflict_patch":
		if in.CandidateOperationID != "" || in.PreviousReleaseID != "" || len(in.PreviousReleaseIDs) > 0 || len(in.Excerpts) > 0 || len(in.Files) < 1 || len(in.Files) > intelligencepatch.MaxFiles {
			return nil, invalid("bounded conflict files required")
		}
		var conflict *domain.CompositionConflict
		for i := range st.CompositionConflicts {
			if st.CompositionConflicts[i].ID == in.ConflictID {
				conflict = &st.CompositionConflicts[i]
			}
		}
		if conflict == nil || conflict.ProductID != productID || conflict.Status != "requires_resolution" && conflict.Status != "claimed" || !slices.Contains(conflict.Source.Plan.HeadSHAs, in.HeadSHA) || !fullSHA.MatchString(conflict.Source.Plan.BaseSHA) {
			return nil, invalid("frozen unresolved conflict and exact participating head required; otherwise use requesting agent")
		}
		if featureID != "" || integrationID != "" {
			matched := false
			for _, v := range conflict.Integrations {
				if (integrationID == "" || v.ID == integrationID) && (featureID == "" || v.FeatureID == featureID) {
					matched = true
				}
			}
			if !matched {
				return nil, invalid("task scope is not a conflict participant")
			}
		}
		paths := s.intelligence.Paths(conflict.RepositoryID)
		if len(paths) == 0 {
			return nil, invalid("small-conflict file paths not operator enrolled; use requesting agent")
		}
		probe := intelligencepatch.Proposal{BaseSHA: conflict.Source.Plan.BaseSHA, HeadSHA: in.HeadSHA}
		files := []map[string]string{}
		bytes := 0
		for _, f := range in.Files {
			bytes += len(f.Base) + len(f.Ours) + len(f.Theirs)
			before := intelligencepatchContentHash(f.Ours)
			probe.Edits = append(probe.Edits, intelligencepatch.Edit{Path: f.Path, BeforeHash: before, Content: f.Ours})
			files = append(files, map[string]string{"path": f.Path, "before_hash": before, "base": f.Base, "ours": f.Ours, "theirs": f.Theirs})
		}
		if bytes > intelligencepatch.MaxBytes || intelligencepatch.Validate(probe, probe.BaseSHA, probe.HeadSHA, paths) != nil {
			return nil, invalid("conflict exceeds automatic patch limits; use requesting agent")
		}
		intents := []intelligenceIntent{}
		for _, v := range conflict.Integrations {
			intents = append(intents, intelligenceIntent{IntegrationID: v.ID, FeatureID: v.FeatureID, Title: v.Title, Objective: v.Objective, Acceptance: v.AcceptanceCriteria})
		}
		value = map[string]any{"conflict_id": conflict.ID, "repository_id": conflict.RepositoryID, "base_sha": probe.BaseSHA, "head_sha": in.HeadSHA, "head_shas": conflict.Source.Plan.HeadSHAs, "files": files, "intents": intents}
	default:
		if in.CandidateOperationID != "" || in.PreviousReleaseID != "" || len(in.PreviousReleaseIDs) > 0 || in.ConflictID != "" || in.HeadSHA != "" || len(in.Files) > 0 || featureID == "" {
			return nil, invalid("annotation/summary requires one Feature and no release/conflict inputs")
		}
		f := feature(st, featureID)
		if f == nil || f.ProductID != productID {
			return nil, invalid("feature outside task scope")
		}
		if len(in.Excerpts) > 10 {
			return nil, invalid("at most 10 explicit excerpts")
		}
		size := 0
		for _, v := range in.Excerpts {
			size += len(v)
		}
		if size > 16<<10 {
			return nil, invalid("excerpts exceed bound")
		}
		if in.TaskKind == "annotation" && len(in.Excerpts) > 0 || in.TaskKind == "log_summary" && len(in.Excerpts) == 0 {
			return nil, invalid("annotation uses intent; log_summary requires explicitly selected redacted excerpts")
		}
		value = map[string]any{"task_kind": in.TaskKind, "feature_id": f.ID, "title": f.Title, "problem": f.Problem, "goal": f.Goal, "requirements": f.Requirements, "excerpts": in.Excerpts}
		if integrationID != "" {
			v := integration(st, integrationID)
			if v == nil || v.FeatureID != featureID {
				return nil, invalid("integration outside feature")
			}
			value.(map[string]any)["integration"] = intelligenceIntent{IntegrationID: v.ID, FeatureID: v.FeatureID, Title: v.Title, Objective: v.Objective, Acceptance: v.AcceptanceCriteria}
		}
	}
	return json.Marshal(value)
}

func (s *Service) releaseNotesContext(st *domain.State, product string, in intelligenceRequest) (notesContext, error) {
	v := notesContext{}
	candidate := operationByID(st, in.CandidateOperationID)
	if candidate == nil || candidate.ProductID != product || candidate.Kind != "RELEASE_CANDIDATE" || candidate.CompositionSnapshot == nil || candidate.Status != "SUCCEEDED" {
		return v, invalid("a successfully built exact release candidate is required")
	}
	baselines, previousSources, e := notesBaselines(st, product, candidate.EnvironmentID, in)
	if e != nil {
		return v, e
	}
	if candidate.ReleaseNotesIntentHash == "" || candidate.ReleaseNotesIntentHash != releaseNotesIntentHash(st, *candidate.CompositionSnapshot) {
		return v, invalid("candidate lacks an unchanged frozen intent; prepare a new release candidate")
	}
	previousArtifacts := map[string]string{}
	previousIDs := map[string]string{}
	for app, r := range baselines {
		previousArtifacts[app] = r.ArtifactDigests[app]
		previousIDs[app] = r.ID
	}
	if environmentByID(st, candidate.EnvironmentID) == nil {
		return v, invalid("candidate environment unavailable")
	}
	v = notesContext{EnvironmentID: candidate.EnvironmentID, PreviousReleaseID: in.PreviousReleaseID, PreviousReleaseIDs: previousIDs, CandidateOperationID: candidate.ID, PreviousSources: previousSources, PreviousArtifacts: previousArtifacts, Sources: map[string]string{}, Artifacts: map[string]string{}, CandidateFingerprint: s.intelligence.Fingerprint(candidate), Changes: []intelligenceIntent{}}
	for _, childID := range candidate.ChildIDs {
		child := operationByID(st, childID)
		if child == nil || child.ProductID != product || child.ParentID != candidate.ID || child.Snapshot == nil || child.Artifact == nil || !fullSHA.MatchString(child.Snapshot.Revision.HeadCommit) || !artifactPinDigest.MatchString(child.Artifact.Digest) {
			return v, invalid("candidate lacks complete immutable child source/artifact evidence")
		}
		repo := child.Snapshot.Revision.RepositoryID
		if old := v.Sources[repo]; old != "" && old != child.Snapshot.Revision.HeadCommit {
			return v, invalid("ambiguous candidate repository source")
		}
		v.Sources[repo] = child.Snapshot.Revision.HeadCommit
		v.Artifacts[child.ApplicationID] = child.Artifact.Digest
	}
	if len(v.Sources) == 0 || len(v.Artifacts) == 0 {
		return v, invalid("candidate artifact/source provenance is missing")
	}
	for _, iid := range selectedIntegrations(*candidate.CompositionSnapshot) {
		refs := []string{candidate.ID}
		changed := false
		found := false
		for _, r := range candidate.CompositionSnapshot.RevisionSnapshots {
			if r.IntegrationID != iid {
				continue
			}
			found = true
			if r.ProductID != product || !fullSHA.MatchString(r.HeadCommit) || v.Sources[r.RepositoryID] == "" {
				return v, invalid("candidate integration revision lacks source provenance")
			}
			componentFound := false
			for _, childID := range candidate.ChildIDs {
				child := operationByID(st, childID)
				if child.Snapshot.Revision.RepositoryID != r.RepositoryID {
					continue
				}
				componentFound = true
				baseline, exists := baselines[child.ApplicationID]
				if !exists {
					return v, invalid("candidate component baseline is unknown; explicit first-deployment evidence is required")
				}
				old, e := previousIntegrationHead(st, baseline, iid, r.RepositoryID)
				if e != nil {
					return v, e
				}
				if old != r.HeadCommit {
					changed = true
				}
			}
			if !componentFound {
				return v, invalid("integration has no candidate component")
			}
			refs = append(refs, r.ID)
		}
		if !found || !changed {
			continue
		}
		current := integration(st, iid)
		if current == nil || current.ProductID != product {
			return v, invalid("included integration unavailable")
		}
		v.Changes = append(v.Changes, intelligenceIntent{IntegrationID: current.ID, FeatureID: current.FeatureID, Title: current.Title, Objective: current.Objective, Acceptance: current.AcceptanceCriteria, EvidenceRefs: refs})
	}
	if len(v.Changes) == 0 {
		return v, invalid("no evidenced changed integrations in candidate")
	}
	return v, nil
}

func (s *Service) taskInput(st *domain.State, op domain.ExternalOperation) (intelligenceInput, error) {
	var in intelligenceInput
	a := s.intelligence
	t := op.Intelligence
	if a == nil || t == nil || !a.Allows(op.ProductID) || t.ConfigRevision != a.Revision() || t.Model != a.Model() || t.PromptVersion != intelligence.PromptVersion {
		return in, invalid("intelligence authority changed")
	}
	if e := a.Open(intelligenceAAD(op, "input"), t.InputCiphertext, &in); e != nil {
		return in, invalid("task input unavailable")
	}
	context, e := s.intelligenceContext(st, op.ProductID, op.FeatureID, op.IntegrationID, in.Request)
	if e != nil {
		return in, e
	}
	current := intelligenceInput{Request: in.Request, Context: context}
	if a.Fingerprint(current) != t.InputFingerprint {
		return in, invalid("task context or release candidate changed; regenerate draft")
	}
	return in, nil
}

func (s *Service) tickIntelligence(ctx context.Context, op domain.ExternalOperation, token string) error {
	var input intelligenceInput
	admitted := false
	// Persist intent before any I/O. An expired lease after this point is an
	// uncertain paid call, never an automatic request replay.
	err := s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || current.Status != "RUNNING" {
			return invalid("intelligence lease changed")
		}
		if current.Phase == "DISPATCH_INTENT" {
			return finishIntelligence(st, current, "BLOCKED", "PROVIDER_OUTCOME_UNCERTAIN", nil)
		}
		var e error
		input, e = s.taskInput(st, *current)
		if e != nil {
			return finishIntelligence(st, current, "BLOCKED", "INPUT_STALE_OR_AUTHORITY_CHANGED", nil)
		}
		requests := 0
		for _, v := range st.Operations {
			if v.Kind == domain.IntelligenceOperationKind && v.ProductID == op.ProductID && v.StartedAt != nil && v.StartedAt.After(time.Now().UTC().Add(-24*time.Hour)) {
				requests += v.Attempts
			}
		}
		if requests > s.intelligence.MaxRequests() {
			return finishIntelligence(st, current, "BLOCKED", "PROVIDER_REQUEST_BUDGET_EXHAUSTED", nil)
		}
		admitted = true
		current.Phase = "DISPATCH_INTENT"
		current.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	// A blocked recovery has no decrypted input and makes no external call.
	if !admitted {
		return nil
	}
	raw, usage, callErr := s.intelligence.Complete(ctx, input.Request.TaskKind, json.RawMessage(input.Context))
	var result intelligenceResult
	if callErr == nil {
		if intelligence.Decode(raw, &result) != nil || s.validateIntelligenceResult(input, result) != nil {
			callErr = errors.New("PROVIDER_RESULT_INVALID")
		}
	}
	// Parent cancellation can leave dispatch intent durable. Recovery will block
	// without retry. Explicit task cancellation also fences late results.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || current.Status != "RUNNING" || !current.LeaseUntil.After(time.Now().UTC()) {
			return invalid("late intelligence result rejected")
		}
		if _, e := s.taskInput(st, *current); e != nil {
			return finishIntelligence(st, current, "BLOCKED", "INPUT_STALE_OR_AUTHORITY_CHANGED", nil)
		}
		current.Intelligence.Usage = usage
		if callErr != nil {
			return finishIntelligence(st, current, "FAILED", callErr.Error(), nil)
		}
		sealed, e := s.intelligence.Seal(intelligenceAAD(op, "result"), result)
		if e != nil {
			return e
		}
		current.Intelligence.ResultCiphertext = sealed
		current.Intelligence.ResultFingerprint = s.intelligence.Fingerprint(result)
		return finishIntelligence(st, current, "SUCCEEDED", "REVIEW_REQUIRED", nil)
	})
}

func finishIntelligence(st *domain.State, op *domain.ExternalOperation, status, detail string, _ any) error {
	now := time.Now().UTC()
	op.Status = status
	op.Phase = detail
	op.Error = ""
	if status != "SUCCEEDED" {
		op.Error = detail
	}
	op.Detail = detail
	op.UpdatedAt = now
	op.FinishedAt = &now
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	intelligenceAudit(st, *op, "system/intelligence", "intelligence_result")
	return nil
}

func (s *Service) validateIntelligenceResult(in intelligenceInput, out intelligenceResult) error {
	if len(out.Summary) > 8192 || strings.TrimSpace(out.Summary) == "" || len(out.Changes) > 50 || len(out.Warnings) > 10 {
		return invalid("invalid result shape")
	}
	if in.Request.TaskKind == "conflict_patch" {
		if out.Decision == "requires_agent" && out.Patch == nil {
			return nil
		}
		var c struct {
			BaseSHA      string              `json:"base_sha"`
			HeadSHA      string              `json:"head_sha"`
			RepositoryID string              `json:"repository_id"`
			Files        []map[string]string `json:"files"`
		}
		// Context deliberately contains other intent fields, so this projection
		// reads only immutable patch admission inputs.
		if json.Unmarshal(in.Context, &c) != nil || out.Decision != "patch_proposed" || out.Patch == nil {
			return invalid("invalid patch response")
		}
		paths := []string{}
		hashes := map[string]string{}
		for _, f := range c.Files {
			paths = append(paths, f["path"])
			hashes[f["path"]] = f["before_hash"]
		}
		if e := intelligencepatch.Validate(*out.Patch, c.BaseSHA, c.HeadSHA, paths); e != nil {
			return invalid("patch exceeds admitted conflict inputs")
		}
		for _, e := range out.Patch.Edits {
			if hashes[e.Path] != e.BeforeHash {
				return invalid("patch before hash mismatch")
			}
		}
		return nil
	}
	if out.Decision != "draft" || out.Patch != nil {
		return invalid("summary must remain a draft")
	}
	if in.Request.TaskKind == "release_notes" {
		var c notesContext
		if json.Unmarshal(in.Context, &c) != nil || len(out.Changes) == 0 {
			return invalid("release changes required")
		}
		allowed := map[string][]string{}
		for _, v := range c.Changes {
			allowed[v.IntegrationID] = v.EvidenceRefs
		}
		covered := map[string]bool{}
		for _, change := range out.Changes {
			if strings.TrimSpace(change.Text) == "" || len(change.Text) > 4096 || len(change.IntegrationIDs) == 0 || len(change.EvidenceRefs) == 0 {
				return invalid("release change evidence required")
			}
			refs := []string{}
			for _, iid := range change.IntegrationIDs {
				if covered[iid] {
					return invalid("duplicate release integration citation")
				}
				covered[iid] = true
				r, ok := allowed[iid]
				if !ok {
					return invalid("unshipped integration in notes")
				}
				refs = append(refs, r...)
			}
			for _, ref := range change.EvidenceRefs {
				if !slices.Contains(refs, ref) {
					return invalid("unknown release evidence reference")
				}
			}
		}

		if len(covered) != len(allowed) {
			return invalid("release notes omit changed integrations")
		}
	}
	return nil
}

func (s *Service) intelligenceQuery(st *domain.State, id string) (any, error) {
	op := operationByID(st, id)
	if op == nil || op.Intelligence == nil {
		return nil, missing("intelligence task", id)
	}
	out := map[string]any{"task": op, "review_required": true, "published": false, "usage_cost": "unknown"}
	input, e := s.taskInput(st, *op)
	if e != nil {
		out["stale"] = true
		out["action"] = "Regenerate against current inputs and enrolled authority"
		return out, nil
	}
	out["stale"] = false
	if op.Intelligence.ResultCiphertext != "" {
		var result intelligenceResult
		if s.intelligence.Open(intelligenceAAD(*op, "result"), op.Intelligence.ResultCiphertext, &result) != nil || s.intelligence.Fingerprint(result) != op.Intelligence.ResultFingerprint {
			return nil, invalid("task result unavailable")
		}
		out["result"] = result
		if input.Request.TaskKind == "release_notes" {
			var c notesContext
			_ = json.Unmarshal(input.Context, &c)
			out["provenance"] = c
		}
	}
	if input.Request.TaskKind == "conflict_patch" {
		out["patch_status"] = "CHECK_REQUIRED"
		if r, ok := out["result"].(intelligenceResult); ok && r.Decision == "requires_agent" {
			out["patch_status"] = "REQUIRES_AGENT"
		}
		checks := []map[string]string{}
		for _, j := range st.WorkerJobs {
			if b := j.Inputs.IntelligencePatch; b != nil && b.TaskID == op.ID && b.ResultFingerprint == op.Intelligence.ResultFingerprint {
				checks = append(checks, map[string]string{"job_id": j.ID, "recorded_status": j.Status, "source_sha": j.Inputs.SourceSHA, "result_fingerprint": b.ResultFingerprint})
				if j.Status == "SUCCEEDED" {
					out["patch_status"] = "PATCH_VERIFIED_REVIEW_REQUIRED"
				}
			}
		}
		out["patch_checks"] = checks
		for _, c := range st.CompositionConflicts {
			if c.ID == input.Request.ConflictID {
				if parent := operationByID(st, c.OperationID); parent != nil {
					out["agent_action"] = operationConflictAction(st, *parent, time.Now().UTC())
				}
			}
		}
	}
	return out, nil
}

func intelligencepatchContentHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func intelligenceAAD(op domain.ExternalOperation, part string) string {
	return op.ProductID + "/" + op.ID + "/" + op.Intelligence.Kind + "/" + op.Intelligence.InputFingerprint + "/" + part
}

// Hash-only capture prevents later mutable intent from being presented as shipped.
func releaseNotesIntentHash(st *domain.State, c domain.Composition) string {
	intents := []intelligenceIntent{}
	for _, iid := range selectedIntegrations(c) {
		v := integration(st, iid)
		if v == nil || v.ProductID != c.ProductID {
			return ""
		}
		intents = append(intents, intelligenceIntent{IntegrationID: v.ID, FeatureID: v.FeatureID, Title: v.Title, Objective: v.Objective, Acceptance: v.AcceptanceCriteria})
	}
	return workerconfig.Fingerprint(intents)
}
func notesBaselines(st *domain.State, product, environment string, in intelligenceRequest) (map[string]domain.Release, map[string]string, error) {
	baselines := map[string]domain.Release{}
	sources := map[string]string{}
	latest := map[string]domain.RuntimeObservation{}
	if in.PreviousReleaseID != "" && len(in.PreviousReleaseIDs) > 0 {
		return nil, nil, invalid("choose a single baseline or per-component release IDs")
	}
	for _, o := range st.RuntimeObservations {
		if o.ProductID != product || o.EnvironmentID != environment {
			continue
		}
		op := operationByID(st, o.OperationID)
		if op == nil || op.ProductID != product || op.ApplicationID == "" {
			return nil, nil, invalid("runtime baseline has unknown component identity")
		}
		old, ok := latest[op.ApplicationID]
		if !ok || o.CreatedAt.After(old.CreatedAt) || o.CreatedAt.Equal(old.CreatedAt) && o.ID > old.ID {
			latest[op.ApplicationID] = o
		}
	}
	if len(latest) == 0 || len(latest) > 20 {
		return nil, nil, invalid("complete current runtime baseline is unknown; explicitly adopt first or unrecorded deployment")
	}
	if len(in.PreviousReleaseIDs) > 0 && len(in.PreviousReleaseIDs) != len(latest) {
		return nil, nil, invalid("per-component baseline must cover all currently observed components")
	}
	for app, o := range latest {
		rid := in.PreviousReleaseID
		if len(in.PreviousReleaseIDs) > 0 {
			rid = in.PreviousReleaseIDs[app]
		}
		var baseline *domain.Release
		for i := range st.Releases {
			r := &st.Releases[i]
			if r.ID == rid && r.ProductID == product && r.EnvironmentID == environment && r.Status == "released" {
				baseline = r
			}
		}
		if baseline == nil {
			return nil, nil, invalid("recorded delivered baseline required for every component")
		}
		r := *baseline
		op := operationByID(st, o.OperationID)
		if !o.Healthy || time.Since(o.CreatedAt) > 15*time.Minute || o.CreatedAt.After(time.Now().Add(time.Minute)) || !artifactPinDigest.MatchString(r.ArtifactDigests[app]) || o.ArtifactDigest != r.ArtifactDigests[app] || r.GitOpsCommits[app] == "" || o.GitOpsCommit != r.GitOpsCommits[app] || op == nil || op.Snapshot == nil || op.Snapshot.Application.ID != app || op.Snapshot.Application.ProductID != product || r.ExecutionOperationID != "" && op.ParentID != r.ExecutionOperationID {
			return nil, nil, invalid("release baseline does not match current recorded environment; reobserve or adopt actual release")
		}
		repo := op.Snapshot.Revision.RepositoryID
		sha := r.SourceCommits[app]
		if sha == "" {
			sha = r.SourceCommits[repo]
		}
		if !fullSHA.MatchString(sha) || op.Snapshot.Revision.HeadCommit != sha {
			return nil, nil, invalid("baseline source provenance missing or mismatched")
		}
		sources[app] = sha
		baselines[app] = r
	}
	return baselines, sources, nil
}
func previousIntegrationHead(st *domain.State, r domain.Release, iid, repo string) (string, error) {
	if !slices.Contains(r.IntegrationIDs, iid) {
		return "", nil
	}
	if r.CandidateOperationID != "" {
		p := operationByID(st, r.CandidateOperationID)
		if p == nil || p.ProductID != r.ProductID || p.EnvironmentID != r.EnvironmentID || p.CompositionSnapshot == nil {
			return "", invalid("previous candidate revision evidence unavailable")
		}
		head := ""
		for _, v := range p.CompositionSnapshot.RevisionSnapshots {
			if v.IntegrationID == iid && v.RepositoryID == repo {
				if head != "" && head != v.HeadCommit {
					return "", invalid("ambiguous previous integration revision")
				}
				head = v.HeadCommit
			}
		}
		if fullSHA.MatchString(head) {
			return head, nil
		}
		return "", invalid("previous release lacks per-integration revision evidence")
	}
	for _, v := range r.Snapshots {
		if v.ID != iid {
			continue
		}
		head := ""
		count := 0
		for _, c := range v.Commits {
			if c.RepositoryID == repo {
				head = c.SHA
				count++
			}
		}
		if count == 1 && fullSHA.MatchString(head) {
			return head, nil
		}
	}
	return "", invalid("previous release lacks an unambiguous integration revision; adopt exact baseline")
}
