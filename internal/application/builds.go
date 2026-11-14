package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"sort"
	"sync"
	"time"
)

// ForgejoBuildProvider keeps provider execution replaceable without duplicating policy.
type ForgejoBuildProvider interface {
	BranchHead(context.Context, buildconfig.Connection, string, string) (string, error)
	CommitExists(context.Context, buildconfig.Connection, string, string) error
	Dispatch(context.Context, buildconfig.Connection, string, string, string, string, string) (forgejo.Run, error)
	GetRun(context.Context, buildconfig.Connection, string, int64) (forgejo.Run, error)
	ListRuns(context.Context, buildconfig.Connection, string, string, forgejo.Filter) (forgejo.Page, error)
}
type BuildRegistry interface {
	Resolve(context.Context, buildconfig.Registry, string, string) (buildregistry.Artifact, error)
}
type buildRuntime struct {
	hash     string
	config   *buildconfig.Config
	provider ForgejoBuildProvider
	registry BuildRegistry
	mu       sync.Mutex
}

// ConfigureBuilds is startup operator authority, never a client command.
func (s *Service) ConfigureBuilds(c *buildconfig.Config, p ForgejoBuildProvider, r BuildRegistry) error {
	raw, e := json.Marshal(c)
	if e != nil {
		return e
	}
	var copy buildconfig.Config
	if json.Unmarshal(raw, &copy) != nil {
		return invalid("invalid build authority")
	}
	if e = copy.Validate(); e != nil {
		return e
	}
	if p == nil || r == nil {
		return invalid("build adapters required")
	}
	s.builds = &buildRuntime{hash: buildFingerprint(&copy), config: &copy, provider: p, registry: r}
	return nil
}
func buildActive(b domain.Build) bool {
	return !b.RestoreQuarantined && (b.Status == "DISPATCHED" || b.Status == "RUNNING" || b.Status == "RECONCILIATION_REQUIRED")
}
func buildStatus(s string) bool { return domain.ValidStatus("build", s) }

func buildEvent(st *domain.State, b domain.Build, action string) {
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: b.Actor, At: b.UpdatedAt, EntityID: b.ID, ProductID: b.ProductID, Data: domain.Command{Action: action, ProductID: b.ProductID, ID: b.ID, Actor: b.Actor, Data: map[string]any{"status": b.Status}}})
}
func (s *Service) QueueBuild(ctx context.Context, in domain.QueueBuildRequest) (domain.Build, error) {
	var out domain.Build
	if in.ProductID == "" {
		return out, invalid("product_id required")
	}
	if e := authorize(ctx, "write", in.ProductID); e != nil {
		return out, e
	}
	a := s.builds
	if a == nil {
		return out, invalid("builds not configured")
	}
	p, ok := a.config.Profile(in.ProfileID)
	if !ok || p.ProductID != in.ProductID {
		return out, invalid("profile not configured for Product")
	}
	if in.Branch != "" && in.SHA != "" {
		return out, invalid("branch and sha are mutually exclusive")
	}
	if len(in.RequestKey) > 128 {
		return out, invalid("invalid request_key")
	}
	if in.SHA != "" {
		if !buildconfig.SHA.MatchString(in.SHA) {
			return out, invalid("invalid sha")
		}
	} else {
		if in.Branch == "" {
			in.Branch = p.DefaultBranch
		}
		if !buildconfig.Branch(in.Branch) {
			return out, invalid("invalid branch")
		}
	}
	// Receipts bind normalized selectors; a branch's resolved SHA is historical
	// evidence, not a selector to resolve again on replay.
	replay := func(st *domain.State) (bool, error) {
		for _, receipt := range st.BuildReceipts {
			if in.RequestKey != "" && receipt.ProductID == in.ProductID && receipt.RequestKey == in.RequestKey {
				if receipt.Workflow != p.Workflow || receipt.ProfileID != p.ID || receipt.Branch != in.Branch || in.SHA != "" && receipt.SourceSHA != in.SHA {
					return true, invalid("REQUEST_KEY_CONFLICT")
				}
				for _, b := range st.Builds {
					if b.ID == receipt.BuildID {
						out = b
						return true, nil
					}
				}
				return true, invalid("build receipt missing handle")
			}
		}
		return false, nil
	}
	if in.RequestKey != "" {
		var found bool
		err := s.view(ctx, func(st *domain.State) error {
			var err error
			found, err = replay(st)
			if found {
				out = copyBuild(out)
			}
			return err
		})
		if err != nil || found {
			return out, err
		}
	}
	head := in.SHA
	var e error
	if head != "" {
		if e = a.provider.CommitExists(ctx, a.config.Connection(p.ConnectionID), p.Repository, head); e != nil {
			return out, invalid("commit observation failed")
		}
	} else {
		head, e = a.provider.BranchHead(ctx, a.config.Connection(p.ConnectionID), p.Repository, in.Branch)
		if e != nil {
			return out, errors.New("build branch observation failed")
		}
		if !buildconfig.SHA.MatchString(head) {
			return out, invalid("invalid provider HEAD")
		}
	}
	e = s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, in.ProductID) {
			return missing("product", in.ProductID)
		}
		// Recheck atomically in case another request bound the key during I/O.
		if found, err := replay(st); found {
			return err
		}
		bind := func(b domain.Build) {
			if in.RequestKey != "" {
				st.BuildReceipts = append(st.BuildReceipts, domain.BuildReceipt{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: attributedActor(ctx, "agent"), CreatedAt: time.Now().UTC()}, RequestKey: in.RequestKey, BuildID: b.ID, Workflow: p.Workflow, ProfileID: p.ID, Branch: in.Branch, SourceSHA: head})
			}
		}

		var previous *domain.Build
		for i := range st.Builds {
			b := &st.Builds[i]
			if b.ProfileID == p.ID && b.ProductID == in.ProductID && b.SourceSHA == head && b.Workflow == p.Workflow && b.AuthorityHash == a.hash {
				if !b.RestoreQuarantined && b.Status != "FAILED" && b.Status != "CANCELLED" {
					out = *b
					bind(out)
					return nil
				}
				previous = b
			}
		}
		if previous != nil && in.RequestKey == "" {
			return invalid("new request_key required to retry failed/cancelled or restored build")
		}
		now := time.Now().UTC()
		actor := attributedActor(ctx, "agent")
		out = domain.Build{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: actor, CreatedAt: now, UpdatedAt: now}, AuthorityHash: a.hash, ConnectionID: p.ConnectionID, ConnectionAPIURL: a.config.Connection(p.ConnectionID).APIURL, ProfileID: p.ID, PoolID: p.PoolID, Repository: p.Repository, Workflow: p.Workflow, Branch: in.Branch, SourceSHA: head, RequestKey: in.RequestKey, Status: "QUEUED", QueuedAt: now}
		bind(out)
		st.Builds = append(st.Builds, out)
		buildEvent(st, out, "queue_build")
		return nil
	})
	return out, e
}
func (s *Service) GetBuild(ctx context.Context, in domain.BuildLookup) (domain.Build, error) {
	if in.ProductID == "" || in.BuildID == "" {
		return domain.Build{}, invalid("product_id and build_id required")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return domain.Build{}, e
	}
	var out domain.Build
	err := s.view(ctx, func(st *domain.State) error {
		for _, b := range st.Builds {
			if b.ID == in.BuildID && b.ProductID == in.ProductID {
				out = copyBuild(b)
				return nil
			}
		}
		return missing("build", in.BuildID)
	})
	return out, err
}
func (s *Service) CancelBuild(ctx context.Context, in domain.BuildLookup) (domain.Build, error) {
	var out domain.Build
	if in.ProductID == "" || in.BuildID == "" {
		return out, invalid("product_id and build_id required")
	}
	if e := authorize(ctx, "write", in.ProductID); e != nil {
		return out, e
	}
	e := s.store.Update(ctx, func(st *domain.State) error {
		for i := range st.Builds {
			b := &st.Builds[i]
			if b.ID != in.BuildID || b.ProductID != in.ProductID {
				continue
			}
			if b.Status != "QUEUED" {
				return invalid("only QUEUED builds may be cancelled")
			}
			now := time.Now().UTC()
			b.Status = "CANCELLED"
			b.UpdatedAt = now
			b.FinishedAt = &now
			out = *b
			eventBuild := out
			eventBuild.Actor = attributedActor(ctx, "agent")
			buildEvent(st, eventBuild, "cancel_build")
			return nil
		}
		return missing("build", in.BuildID)
	})
	return out, e
}

// CloseBuild abandons an active handle without changing any provider evidence.
func (s *Service) CloseBuild(ctx context.Context, in domain.CloseBuildRequest) (domain.Build, error) {
	var out domain.Build
	if e := authorize(ctx, "admin", ""); e != nil {
		return out, e
	}
	if in.ProductID == "" || in.BuildID == "" {
		return out, invalid("product_id and build_id required")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(in.Reason); n < 8 || n > 500 {
		return out, invalid("reason must contain 8..500 characters")
	}
	if len(in.RequestKey) > 128 {
		return out, invalid("invalid request_key")
	}
	old, err := s.GetBuild(ctx, domain.BuildLookup{ProductID: in.ProductID, BuildID: in.BuildID})
	if err != nil {
		return out, err
	}
	err = s.store.Update(ctx, func(st *domain.State) error {
		for i, b := range st.Builds {
			if b.ID != in.BuildID || b.ProductID != in.ProductID {
				continue
			}
			if in.RequestKey != "" {
				for _, event := range st.Events {
					if event.Action == "close_build" && event.ProductID == in.ProductID && event.EntityID == in.BuildID {
						command := event.Data
						if command.Data["request_key"] == in.RequestKey {
							if command.Data["reason"] != in.Reason {
								return invalid("REQUEST_KEY_CONFLICT")
							}
							out = copyBuild(b)
							return nil
						}
					}
				}
			}
			if b.Status != old.Status || !b.UpdatedAt.Equal(old.UpdatedAt) {
				return invalid("build changed")
			}
			switch b.Status {
			case "QUEUED":
				return invalid("use cancel_build for QUEUED builds")
			case "DISPATCHED", "RUNNING", "RECONCILIATION_REQUIRED":
			default:
				return invalid("only active builds may be closed")
			}
			now := time.Now().UTC()
			next := b
			next.Status, next.FailureCode, next.FailureMessage = "FAILED", "OPERATOR_CLOSED", in.Reason
			next.UpdatedAt, next.FinishedAt = now, &now
			st.Builds[i] = next
			audit := next
			audit.Actor = attributedActor(ctx, "agent")
			buildEvent(st, audit, "close_build")
			event := &st.Events[len(st.Events)-1]
			command := event.Data
			command.Data["reason"] = in.Reason
			command.Data["request_key"] = in.RequestKey
			command.Data["previous_status"] = b.Status
			command.Data["previous_failure_code"] = b.FailureCode
			command.Data["provider_run_id"] = b.ProviderRunID
			event.Data = command
			out = copyBuild(next)
			return nil
		}
		return missing("build", in.BuildID)
	})
	return out, err
}
func (s *Service) ListBuildProfiles(ctx context.Context, productID string) ([]buildconfig.Profile, error) {
	if productID == "" {
		return nil, invalid("product_id required")
	}
	if e := authorize(ctx, "read", productID); e != nil {
		return nil, e
	}
	out := []buildconfig.Profile{}
	if s.builds != nil {
		for _, p := range s.builds.config.Profiles {
			if p.ProductID == productID {
				p.TagCandidates = slices.Clone(p.TagCandidates)
				p.Images = slices.Clone(p.Images)
				out = append(out, p)
			}
		}
	}
	return out, nil
}

type buildCursor struct{ Product, Profile, Status, After string }

func (s *Service) ListBuilds(ctx context.Context, in domain.ListBuildsRequest) (domain.BuildPage, error) {
	out := domain.BuildPage{Items: []domain.Build{}}
	if in.ProductID == "" || in.Limit < 0 || in.Limit > 100 || in.Status != "" && !buildStatus(in.Status) {
		return out, invalid("invalid build list scope or bounds")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return out, e
	}
	if in.Limit == 0 {
		in.Limit = 25
	}
	after := ""
	if in.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(in.Cursor)
		var c buildCursor
		if len(in.Cursor) > 2048 || e != nil || json.Unmarshal(raw, &c) != nil || c.Product != in.ProductID || c.Profile != in.ProfileID || c.Status != in.Status {
			return out, invalid("invalid build cursor")
		}
		after = c.After
	}
	err := s.view(ctx, func(st *domain.State) error {
		builds := slices.Clone(st.Builds)
		sort.Slice(builds, func(i, j int) bool { return builds[i].ID < builds[j].ID })
		for _, b := range builds {
			if b.ProductID != in.ProductID || in.ProfileID != "" && b.ProfileID != in.ProfileID || in.Status != "" && b.Status != in.Status || b.ID <= after {
				continue
			}
			if len(out.Items) == in.Limit {
				raw, _ := json.Marshal(buildCursor{in.ProductID, in.ProfileID, in.Status, out.Items[len(out.Items)-1].ID})
				out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
				break
			}
			out.Items = append(out.Items, copyBuild(b))
		}
		return nil
	})
	return out, err
}
func buildFingerprint(p any) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

var errBuildPoolFull = invalid("build pool full")
var errBuildTerminal = invalid("build is terminal")

// advanceBuild uses CAS so cancellation/concurrent pollers cannot overwrite newer evidence.
func (s *Service) advanceBuild(ctx context.Context, old, next domain.Build, admit bool) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, errBuildPoolFull) && !errors.Is(err, errBuildTerminal) {
			slog.Warn("build transition failed", "build_id", old.ID, "from_status", old.Status, "to_status", next.Status, "error", err.Error())
		}
	}()
	if reflect.DeepEqual(old, next) {
		return nil
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		for i, b := range st.Builds {
			if b.ID != old.ID {
				continue
			}
			// Terminal evidence is immutable, even when a caller re-read it as its CAS base.
			if b.Status == "SUCCEEDED" || b.Status == "FAILED" || b.Status == "CANCELLED" {
				return errBuildTerminal
			}
			if b.Status != old.Status || !b.UpdatedAt.Equal(old.UpdatedAt) {
				return invalid("build changed")
			}
			if admit {
				n := 0
				for _, v := range st.Builds {
					if v.PoolID == old.PoolID && buildActive(v) {
						n++
					}
				}
				if n >= s.builds.config.Capacity(old.PoolID) {
					return errBuildPoolFull
				}
			}
			next.UpdatedAt = time.Now().UTC()
			st.Builds[i] = next
			buildEvent(st, next, "observe_build")
			return nil
		}
		return missing("build", old.ID)
	})
}
func (s *Service) buildProviderUnavailable(ctx context.Context, b domain.Build, cause error) error {
	n := b
	n.ProviderUnavailable = true
	n.LastError = "Forgejo run listing unavailable"
	if errors.Is(cause, errBuildRunBound) {
		n.LastError = errBuildRunBound.Error()
	}
	return s.advanceBuild(ctx, b, n, false)
}
func matching(r forgejo.Run, b domain.Build) bool {
	sha, _ := r.DispatchInputs()
	return r.ID > 0 && r.Workflow == b.Workflow && r.EffectiveEvent() == "workflow_dispatch" && sha == b.SourceSHA
}
func usable(r forgejo.Run) bool {
	return r.Status == "waiting" || r.Status == "running" || r.Status == "success"
}

var errBuildRunBound = errors.New("build run listing page cap reached before time window ended")

// Forgejo lists runs newest first. Active gates deliberately have no time cutoff.
func (a *buildRuntime) runs(ctx context.Context, p buildconfig.Profile, filter forgejo.Filter, since time.Time) ([]forgejo.Run, error) {
	out := []forgejo.Run{}
	for page := 1; page <= 10; page++ {
		filter.Page, filter.Limit, filter.Event = page, 50, "workflow_dispatch"
		v, e := a.provider.ListRuns(ctx, a.config.Connection(p.ConnectionID), p.Repository, p.Workflow, filter)
		if e != nil {
			return nil, e
		}
		for _, run := range v.Runs {
			if !since.IsZero() && !run.Created.IsZero() && run.Created.Before(since) {
				return out, nil
			}
			out = append(out, run)
		}
		if len(v.Runs) < 50 || v.Total > 0 && len(out) >= v.Total {
			return out, nil
		}
	}
	return nil, errBuildRunBound
}

var errRevision = errors.New("artifact revision mismatch")

func dispatchRequestID(r forgejo.Run) string { _, requestID := r.DispatchInputs(); return requestID }
func (a *buildRuntime) artifact(ctx context.Context, p buildconfig.Profile, sha string) ([]domain.BuildImage, error) {
	origin, err := url.Parse(a.config.Registry(p.RegistryID).Origin)
	if err != nil || origin.Host == "" {
		return nil, errors.New("invalid registry origin")
	}
	for _, template := range p.TagCandidates {
		tag := buildconfig.ExpandTag(template, sha)
		images := []domain.BuildImage{}
		missing := false
		for _, image := range p.Images {
			v, e := a.registry.Resolve(ctx, a.config.Registry(p.RegistryID), image.Repository, tag)
			if errors.Is(e, buildregistry.ErrMissing) {
				missing = true
				continue
			}
			if e != nil {
				return nil, e
			}
			if v.Revision != sha {
				return nil, errRevision
			}
			images = append(images, domain.BuildImage{ID: image.ID, ImageRef: origin.Host + "/" + image.Repository + ":" + tag + "@" + v.Digest, Digest: v.Digest})
		}
		if !missing {
			return images, nil
		}
	}
	return nil, buildregistry.ErrMissing
}
func artifactBuild(b domain.Build, images []domain.BuildImage) domain.Build {
	now := time.Now().UTC()
	b.FinishedAt = &now
	b.Images = images
	b.LastError = ""
	b.ProviderUnavailable = false
	b.Status = "SUCCEEDED"
	b.FailureCode = ""
	b.FailureMessage = ""
	return b
}
func failedBuild(b domain.Build, code string) domain.Build {
	now := time.Now().UTC()
	b.Status = "FAILED"
	b.FailureCode = code
	b.FailureMessage = code
	b.FinishedAt = &now
	return b
}
func (s *Service) observeBuild(ctx context.Context, b domain.Build, p buildconfig.Profile, r forgejo.Run) error {
	next := b
	next.FailureCode = ""
	next.FailureMessage = ""
	if !matching(r, b) {
		next.Status = "RECONCILIATION_REQUIRED"
		next.FailureCode = "RUN_SOURCE_MISMATCH"
		return s.advanceBuild(ctx, b, next, false)
	}
	next.ProviderUnavailable = false
	next.LastError = ""
	next.ProviderRunID = r.ID
	next.RunURL = r.URL
	next.RunStatus = r.Status
	if !r.Started.IsZero() {
		next.StartedAt = &r.Started
	}
	switch r.Status {
	case "waiting":
		next.Status = "DISPATCHED"
	case "running":
		next.Status = "RUNNING"
	case "success":
		v, e := s.builds.artifact(ctx, p, b.SourceSHA)
		if errors.Is(e, buildregistry.ErrMissing) {
			next = failedBuild(next, "ARTIFACT_MISSING")
		} else if errors.Is(e, errRevision) {
			next = failedBuild(next, "ARTIFACT_REVISION_MISMATCH")
		} else if e != nil {
			return nil
		} else {
			next = artifactBuild(next, v)
		}

	case "cancelled":
		now := time.Now().UTC()
		next.Status = "CANCELLED"
		next.FinishedAt = &now
	case "failure", "skipped", "blocked":
		next = failedBuild(next, "RUN_FAILED")
	default:
		next.Status = "RECONCILIATION_REQUIRED"
		next.FailureCode = "RUN_STATUS_UNKNOWN"
	}
	return s.advanceBuild(ctx, b, next, false)
}

// Changed authority can observe an existing run, but can never accept its artifacts
// or dispatch again. Unlocatable runs release admission after timeout plus grace.
func (s *Service) reconcileChangedBuildAuthority(ctx context.Context, b domain.Build, p buildconfig.Profile, configured bool) error {
	a := s.builds
	next := b
	next.Status = "RECONCILIATION_REQUIRED"
	next.FailureCode = "BUILD_AUTHORITY_CHANGED"
	next.FailureMessage = "BUILD_AUTHORITY_CHANGED"
	located := false
	// A missing original connection identity cannot safely locate an old run.
	connection := a.config.Connection(p.ConnectionID)
	if configured && p.Repository == b.Repository && b.ConnectionID != "" && p.ConnectionID == b.ConnectionID && connection.ID != "" && b.ConnectionAPIURL != "" && connection.APIURL == b.ConnectionAPIURL && b.ProviderRunID > 0 {
		r, err := a.provider.GetRun(ctx, connection, b.Repository, b.ProviderRunID)
		located = true
		if err != nil {
			next.ProviderUnavailable = true
			next.LastError = "Forgejo run listing unavailable"
		} else {
			next.ProviderUnavailable = false
			next.LastError = ""
			next.RunStatus = r.Status
			switch r.Status {
			case "success", "failure", "cancelled", "skipped", "blocked":
				next = failedBuild(next, "BUILD_AUTHORITY_CHANGED")
				if !r.Stopped.IsZero() {
					next.FinishedAt = &r.Stopped
				}
			}
		}
	}
	since := b.QueuedAt
	if b.DispatchedAt != nil {
		since = *b.DispatchedAt
	}
	if next.Status != "FAILED" && !located && time.Since(since) > time.Duration(a.config.TimeoutSeconds)*time.Second+buildReconciliationGrace {
		next = failedBuild(next, "BUILD_AUTHORITY_CHANGED")
	}
	if b.Status != next.Status || b.FailureCode != next.FailureCode {
		slog.Warn("build authority changed", "build_id", b.ID, "from_status", b.Status, "to_status", next.Status, "run_status", next.RunStatus)
	}
	return s.advanceBuild(ctx, b, next, false)
}

// DispatchBuildsOnce performs bounded external I/O outside Store transactions. Intent
// is persisted before dispatch; a crash in that window can only reconcile, never retry.
func (s *Service) DispatchBuildsOnce(ctx context.Context) error {
	a := s.builds
	if a == nil {
		return nil
	}
	if !a.mu.TryLock() {
		return nil
	}
	defer a.mu.Unlock()
	st, e := s.buildSnapshot(ctx)
	if e != nil {
		return e
	}
	sort.SliceStable(st.Builds, func(i, j int) bool {
		if st.Builds[i].QueuedAt.Equal(st.Builds[j].QueuedAt) {
			return st.Builds[i].ID < st.Builds[j].ID
		}
		return st.Builds[i].QueuedAt.Before(st.Builds[j].QueuedAt)
	})
	blockedPools := map[string]bool{}
	for _, b := range st.Builds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.RestoreQuarantined {
			continue
		}
		p, ok := a.config.Profile(b.ProfileID)
		if !ok || p.ProductID != b.ProductID || p.Repository != b.Repository || p.Workflow != b.Workflow || p.PoolID != b.PoolID || b.AuthorityHash != a.hash {
			if b.Status == "QUEUED" {
				if e = s.advanceBuild(ctx, b, failedBuild(b, "BUILD_AUTHORITY_CHANGED"), false); e != nil {
					return e
				}
			} else if buildActive(b) {
				if e = s.reconcileChangedBuildAuthority(ctx, b, p, ok); e != nil {
					return e
				}
			}
			continue
		}
		if !buildconfig.SHA.MatchString(b.SourceSHA) {
			if b.Status == "QUEUED" {
				if e = s.advanceBuild(ctx, b, failedBuild(b, "INVALID_BUILD_SOURCE"), false); e != nil {
					return e
				}
			}
			continue
		}
		if buildActive(b) {
			if b.Status == "RECONCILIATION_REQUIRED" && b.DispatchedAt != nil && time.Since(*b.DispatchedAt) > time.Duration(a.config.TimeoutSeconds)*time.Second+buildReconciliationGrace {
				if e = s.expireReconciliation(ctx, b, p); e != nil {
					return e
				}
				continue
			}
			if b.DispatchedAt != nil && time.Since(*b.DispatchedAt) > time.Duration(a.config.TimeoutSeconds)*time.Second && b.Status != "RECONCILIATION_REQUIRED" {
				n := b
				n.Status = "RECONCILIATION_REQUIRED"
				n.FailureCode = "BUILD_TIMEOUT"
				if e = s.advanceBuild(ctx, b, n, false); e != nil {
					return e
				}
				continue
			}
			if b.ProviderRunID > 0 {
				r, e := a.provider.GetRun(ctx, a.config.Connection(p.ConnectionID), p.Repository, b.ProviderRunID)
				if e == nil {
					if b.FailureCode == "BUILD_TIMEOUT" && (r.Status == "waiting" || r.Status == "running") {
						continue
					}
					if e = s.observeBuild(ctx, b, p, r); e != nil {
						return e
					}
				}
				continue
			}
			runs, e := a.runs(ctx, p, forgejo.Filter{}, b.QueuedAt.Add(-2*time.Minute))
			if e != nil {
				if e = s.buildProviderUnavailable(ctx, b, e); e != nil {
					return e
				}
				continue
			}
			if b.LastError != "" {
				n := b
				n.LastError = ""
				n.ProviderUnavailable = false
				if e = s.advanceBuild(ctx, b, n, false); e != nil {
					return e
				}
				previousStatus := b.Status
				b, e = s.GetBuild(ctx, domain.BuildLookup{ProductID: b.ProductID, BuildID: b.ID})
				if e != nil {
					return e
				}
				if !buildActive(b) || b.Status != previousStatus {
					continue
				}
			}
			for _, r := range runs {
				if matching(r, b) && dispatchRequestID(r) == b.ID {
					if e = s.observeBuild(ctx, b, p, r); e != nil {
						return e
					}
					break
				}
			}
			continue
		}
		if b.Status != "QUEUED" {
			continue
		}

		// A waiting head blocks younger queued work in this pool. Admission or
		// terminal reuse frees the next FIFO position within this tick.
		if blockedPools[b.PoolID] {
			continue
		}
		blockedPools[b.PoolID] = true
		v, re := a.artifact(ctx, p, b.SourceSHA)
		if re == nil {
			n := artifactBuild(b, v)
			n.Reused = "registry"
			if e = s.advanceBuild(ctx, b, n, false); e != nil {
				return e
			}
			delete(blockedPools, b.PoolID)
			continue
		}
		if errors.Is(re, errRevision) {
			if e = s.advanceBuild(ctx, b, failedBuild(b, "ARTIFACT_REVISION_MISMATCH"), false); e != nil {
				return e
			}
			delete(blockedPools, b.PoolID)
			continue
		}
		if !errors.Is(re, buildregistry.ErrMissing) {
			slog.Warn("build registry lookup failed; pool stays blocked", "build_id", b.ID, "profile_id", b.ProfileID, "error", re.Error())
			continue
		}
		runs, e := a.runs(ctx, p, forgejo.Filter{}, time.Now().UTC().Add(-time.Duration(a.config.RunLookbackSeconds)*time.Second))
		sourceRunCount := len(runs)
		if e == nil {
			for _, status := range []string{"waiting", "running"} {
				var active []forgejo.Run
				active, e = a.runs(ctx, p, forgejo.Filter{Status: status}, time.Time{})
				if e != nil {
					break
				}
				runs = append(runs, active...)
			}
		}
		if e != nil {
			if e = s.buildProviderUnavailable(ctx, b, e); e != nil {
				return e
			}
			continue
		}
		if b.LastError != "" {
			n := b
			n.LastError = ""
			n.ProviderUnavailable = false
			if e = s.advanceBuild(ctx, b, n, false); e != nil {
				return e
			}
			previousStatus := b.Status
			b, e = s.GetBuild(ctx, domain.BuildLookup{ProductID: b.ProductID, BuildID: b.ID})
			if e != nil {
				return e
			}
			if b.RestoreQuarantined || b.Status != previousStatus || b.Status != "QUEUED" {
				continue
			}
		}
		fresh, err := s.buildSnapshot(ctx)
		if err != nil {
			return err
		}
		var match *forgejo.Run
		busy := false
		for i := range runs {
			r := &runs[i]
			if i < sourceRunCount && matching(*r, b) && usable(*r) && (match == nil || r.ID > match.ID) {
				match = r
			}
			if r.Workflow == p.Workflow && r.EffectiveEvent() == "workflow_dispatch" && (r.Status == "waiting" || r.Status == "running") {
				owned := false
				for _, owner := range fresh.Builds {
					ownerProfile, configured := a.config.Profile(owner.ProfileID)
					if configured && owner.AuthorityHash == a.hash && ownerProfile.ConnectionID == p.ConnectionID && owner.Repository == b.Repository && owner.PoolID == b.PoolID && owner.Workflow == b.Workflow && owner.ProviderRunID == r.ID && buildActive(owner) {
						owned = true
						break
					}
				}
				if !owned {
					busy = true
				}
			}
		}

		if match != nil {
			n := b
			now := time.Now().UTC()
			n.DispatchedAt = &now
			n.Status = "DISPATCHED"
			n.Reused = "run"
			n.ProviderRunID = match.ID
			n.RunURL = match.URL
			n.RunStatus = match.Status
			if e = s.advanceBuild(ctx, b, n, true); e != nil {
				continue
			}
			delete(blockedPools, b.PoolID)
			continue
		}
		if busy {
			continue
		}

		n := b
		now := time.Now().UTC()
		n.Status = "RECONCILIATION_REQUIRED"
		n.DispatchedAt = &now
		n.FailureCode = "DISPATCH_UNCERTAIN"
		if e = s.advanceBuild(ctx, b, n, true); e != nil {
			continue
		}
		delete(blockedPools, b.PoolID)
		// Verify admission still holds before contacting the provider.
		current, e := s.GetBuild(ctx, domain.BuildLookup{ProductID: b.ProductID, BuildID: b.ID})
		if e != nil {
			return e
		}
		if !buildActive(current) || current.ID != n.ID || current.Status != n.Status || current.FailureCode != n.FailureCode ||
			current.DispatchedAt == nil || !current.DispatchedAt.Equal(*n.DispatchedAt) {
			continue
		}
		// advanceBuild assigns UpdatedAt when persisting the admitted intent.
		// Keep that intent as the CAS base even if an operator closes it during dispatch.
		n.UpdatedAt = current.UpdatedAt
		r, e := a.provider.Dispatch(ctx, a.config.Connection(p.ConnectionID), p.Repository, p.Workflow, p.DispatchRef, b.SourceSHA, b.ID)
		next := n
		if e != nil {
			var uncertain *forgejo.Uncertain
			if errors.As(e, &uncertain) {
				slog.Warn("build dispatch uncertain", "build_id", b.ID, "profile_id", b.ProfileID)
				continue
			}
			next = failedBuild(next, "DISPATCH_REJECTED")
		} else {
			next.Status = "DISPATCHED"
			next.ProviderRunID = r.ID
			next.RunURL = r.URL
			next.RunStatus = r.Status
			next.FailureCode = ""
		}
		if e = s.advanceBuild(ctx, n, next, false); e != nil {
			return e
		}
	}
	return nil
}

// RunBuilds is owned by the server context and does not spawn executors.
func (s *Service) RunBuilds(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.DispatchDeploymentsOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("deployment observation failed")
		}
		if err := s.DispatchBuildsOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("build queue observation failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// The grace is additional time for delayed provider visibility after the deadline.
const buildReconciliationGrace = 10 * time.Minute

func (s *Service) expireReconciliation(ctx context.Context, b domain.Build, p buildconfig.Profile) error {
	a := s.builds
	keep := func() error {
		next := b
		next.ProviderUnavailable = false
		next.LastError = ""
		return s.advanceBuild(ctx, b, next, false)
	}
	// Preserve terminal evidence and self-heal known runs before considering expiry.
	if b.ProviderRunID > 0 {
		r, err := a.provider.GetRun(ctx, a.config.Connection(p.ConnectionID), p.Repository, b.ProviderRunID)
		if err != nil {
			return s.buildProviderUnavailable(ctx, b, err)
		}
		if matching(r, b) && (usable(r) || r.Status == "failure" || r.Status == "cancelled" || r.Status == "skipped" || r.Status == "blocked") {
			if b.FailureCode == "BUILD_TIMEOUT" && (r.Status == "waiting" || r.Status == "running") {
				return keep()
			}
			return s.observeBuild(ctx, b, p, r)
		}
	} else {
		// A lost dispatch response has no run ID. Reconcile the request across
		// terminal runs too, using the same time window as ordinary observation.
		runs, err := a.runs(ctx, p, forgejo.Filter{}, b.QueuedAt.Add(-2*time.Minute))
		if err != nil {
			return s.buildProviderUnavailable(ctx, b, err)
		}
		for _, r := range runs {
			if matching(r, b) && dispatchRequestID(r) == b.ID {
				return s.observeBuild(ctx, b, p, r)
			}
		}
	}
	for _, status := range []string{"waiting", "running"} {
		runs, err := a.runs(ctx, p, forgejo.Filter{Status: status}, time.Time{})
		if err != nil {
			return s.buildProviderUnavailable(ctx, b, err)
		}
		for _, r := range runs {
			sha, _ := r.DispatchInputs()
			if r.Workflow == b.Workflow && r.EffectiveEvent() == "workflow_dispatch" && (r.Status == "waiting" || r.Status == "running") && (sha == b.SourceSHA || b.ProviderRunID > 0 && r.ID == b.ProviderRunID) {
				return keep()
			}
		}
	}
	next := failedBuild(b, "RECONCILIATION_TIMEOUT")
	next.ProviderUnavailable = false
	next.LastError = ""
	return s.advanceBuild(ctx, b, next, false)
}

// Keep the build contribution bounded independently of other attention sources.
func buildReconciliationAttention(st domain.State, productID string, now time.Time) []map[string]any {
	builds := []domain.Build{}
	for _, b := range st.Builds {
		if b.ProductID == productID && b.Status == "RECONCILIATION_REQUIRED" {
			builds = append(builds, b)
		}
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].ID < builds[j].ID })
	if len(builds) > 100 {
		builds = builds[:100]
	}
	items := []map[string]any{}
	for _, b := range builds {
		since := b.QueuedAt
		if b.DispatchedAt != nil {
			since = *b.DispatchedAt
		}
		age := max(int64(0), int64(now.Sub(since)/time.Second))
		items = append(items, map[string]any{"id": b.ID, "reason": "BUILD_RECONCILIATION_REQUIRED", "profile_id": b.ProfileID, "failure_code": b.FailureCode, "age_seconds": age})
	}
	return items
}
