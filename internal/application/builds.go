package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"sort"
	"strings"
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
	return b.Status == "DISPATCHED" || b.Status == "RUNNING" || b.Status == "RECONCILIATION_REQUIRED"
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
	head := in.SHA
	var e error
	if head != "" {
		if !buildconfig.SHA.MatchString(head) {
			return out, invalid("invalid sha")
		}
		if e = a.provider.CommitExists(ctx, a.config.Connection(p.ConnectionID), p.Repository, head); e != nil {
			return out, invalid("commit observation failed")
		}
	} else {
		if in.Branch == "" {
			in.Branch = p.DefaultBranch
		}
		if !buildconfig.Branch(in.Branch) {
			return out, invalid("invalid branch")
		}
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
		for _, receipt := range st.BuildReceipts {
			if in.RequestKey != "" && receipt.ProductID == in.ProductID && receipt.RequestKey == in.RequestKey {
				if receipt.Workflow != p.Workflow || receipt.ProfileID != p.ID || receipt.SourceSHA != head || receipt.Branch != in.Branch {
					return invalid("REQUEST_KEY_CONFLICT")
				}
				for _, b := range st.Builds {
					if b.ID == receipt.BuildID {
						out = b
						return nil
					}
				}
				return invalid("build receipt missing handle")
			}
		}
		bind := func(b domain.Build) {
			if in.RequestKey != "" {
				st.BuildReceipts = append(st.BuildReceipts, domain.BuildReceipt{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: attributedActor(ctx, "agent"), CreatedAt: time.Now().UTC()}, RequestKey: in.RequestKey, BuildID: b.ID, Workflow: p.Workflow, ProfileID: p.ID, Branch: in.Branch, SourceSHA: head})
			}
		}

		var previous *domain.Build
		for i := range st.Builds {
			b := &st.Builds[i]
			if b.ProfileID == p.ID && b.ProductID == in.ProductID && b.SourceSHA == head && b.Workflow == p.Workflow {
				if b.Status != "FAILED" && b.Status != "CANCELLED" {
					out = *b
					bind(out)
					return nil
				}
				previous = b
			}
		}
		if previous != nil && in.RequestKey == "" {
			return invalid("new request_key required to retry failed/cancelled build")
		}
		now := time.Now().UTC()
		actor := attributedActor(ctx, "agent")
		out = domain.Build{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: actor, CreatedAt: now, UpdatedAt: now}, AuthorityHash: a.hash, ProfileID: p.ID, PoolID: p.PoolID, Repository: p.Repository, Workflow: p.Workflow, Branch: in.Branch, SourceSHA: head, RequestKey: in.RequestKey, Status: "QUEUED", QueuedAt: now}
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
	st, e := s.store.Read(ctx)
	if e != nil {
		return domain.Build{}, e
	}
	for _, b := range st.Builds {
		if b.ID == in.BuildID && b.ProductID == in.ProductID {
			return b, nil
		}
	}
	return domain.Build{}, missing("build", in.BuildID)
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
	st, e := s.store.Read(ctx)
	if e != nil {
		return out, e
	}
	sort.Slice(st.Builds, func(i, j int) bool { return st.Builds[i].ID < st.Builds[j].ID })
	for _, b := range st.Builds {
		if b.ProductID != in.ProductID || in.ProfileID != "" && b.ProfileID != in.ProfileID || in.Status != "" && b.Status != in.Status || b.ID <= after {
			continue
		}
		if len(out.Items) == in.Limit {
			raw, _ := json.Marshal(buildCursor{in.ProductID, in.ProfileID, in.Status, out.Items[len(out.Items)-1].ID})
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		out.Items = append(out.Items, b)
	}
	return out, nil
}
func buildFingerprint(p any) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// advanceBuild uses CAS so cancellation/concurrent pollers cannot overwrite newer evidence.
func (s *Service) advanceBuild(ctx context.Context, old, next domain.Build, admit bool) error {
	if reflect.DeepEqual(old, next) {
		return nil
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		for i, b := range st.Builds {
			if b.ID != old.ID {
				continue
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
					return invalid("build pool full")
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
func runTitle(b domain.Build) string { return "build " + b.SourceSHA + " " + b.ID }
func matching(r forgejo.Run, b domain.Build) bool {
	prefix := "build " + b.SourceSHA + " "
	requestID := strings.TrimPrefix(r.Title, prefix)
	return r.ID > 0 && r.Workflow == b.Workflow && r.Event == "workflow_dispatch" && strings.HasPrefix(r.Title, prefix) && buildconfig.RequestID.MatchString(requestID)
}
func usable(r forgejo.Run) bool {
	return r.Status == "waiting" || r.Status == "running" || r.Status == "success"
}
func (a *buildRuntime) runs(ctx context.Context, p buildconfig.Profile, status string) ([]forgejo.Run, error) {
	out := []forgejo.Run{}
	for page := 1; page <= 10; page++ {
		v, e := a.provider.ListRuns(ctx, a.config.Connection(p.ConnectionID), p.Repository, p.Workflow, forgejo.Filter{Status: status, Page: page, Limit: 100})
		if e != nil {
			return nil, e
		}
		out = append(out, v.Runs...)
		if len(v.Runs) < 100 && len(out) >= v.Total || len(out) >= v.Total && v.Total > 0 {
			return out, nil
		}
	}
	return nil, errors.New("build run listing exceeds bound")
}

var errRevision = errors.New("artifact revision mismatch")

func (a *buildRuntime) artifact(ctx context.Context, p buildconfig.Profile, sha string) (buildregistry.Artifact, string, error) {
	for _, template := range p.TagCandidates {
		tag := buildconfig.ExpandTag(template, sha)
		v, e := a.registry.Resolve(ctx, a.config.Registry(p.RegistryID), p.ImageRepository, tag)
		if errors.Is(e, buildregistry.ErrMissing) {
			continue
		}
		if e != nil {
			return v, tag, e
		}
		if v.Revision != sha {
			return v, tag, errRevision
		}
		return v, tag, nil
	}
	return buildregistry.Artifact{}, "", buildregistry.ErrMissing
}
func artifactBuild(b domain.Build, p buildconfig.Profile, c buildconfig.Registry, v buildregistry.Artifact, tag string) domain.Build {
	now := time.Now().UTC()
	b.FinishedAt = &now
	b.Digest = v.Digest
	b.ImageRef = strings.TrimRight(c.Origin, "/") + "/" + p.ImageRepository + ":" + tag + "@" + v.Digest
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
		v, tag, e := s.builds.artifact(ctx, p, b.SourceSHA)
		if errors.Is(e, buildregistry.ErrMissing) {
			next = failedBuild(next, "ARTIFACT_MISSING")
		} else if errors.Is(e, errRevision) {
			next = failedBuild(next, "ARTIFACT_REVISION_MISMATCH")
		} else if e != nil {
			return nil
		} else {
			next = artifactBuild(next, p, s.builds.config.Registry(p.RegistryID), v, tag)
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
	st, e := s.store.Read(ctx)
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
		p, ok := a.config.Profile(b.ProfileID)
		if !ok || p.ProductID != b.ProductID || p.Repository != b.Repository || p.Workflow != b.Workflow || p.PoolID != b.PoolID || b.AuthorityHash != a.hash {
			if b.Status == "QUEUED" {
				if e = s.advanceBuild(ctx, b, failedBuild(b, "BUILD_AUTHORITY_CHANGED"), false); e != nil {
					return e
				}
			} else if buildActive(b) && b.FailureCode != "BUILD_AUTHORITY_CHANGED" {
				next := b
				next.Status = "RECONCILIATION_REQUIRED"
				next.FailureCode = "BUILD_AUTHORITY_CHANGED"
				if e = s.advanceBuild(ctx, b, next, false); e != nil {
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
			runs, e := a.runs(ctx, p, "")
			if e != nil {
				continue
			}
			for _, r := range runs {
				if matching(r, b) && r.Title == runTitle(b) {
					n := b
					n.ProviderRunID = r.ID
					n.RunURL = r.URL
					n.RunStatus = r.Status
					n.Status = "DISPATCHED"
					n.FailureCode = ""
					if e = s.advanceBuild(ctx, b, n, false); e != nil {
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
		v, tag, re := a.artifact(ctx, p, b.SourceSHA)
		if re == nil {
			n := artifactBuild(b, p, a.config.Registry(p.RegistryID), v, tag)
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
			continue
		}
		runs, e := a.runs(ctx, p, "")
		if e != nil {
			continue
		}
		fresh, err := s.store.Read(ctx)
		if err != nil {
			return err
		}
		var match *forgejo.Run
		busy := false
		for i := range runs {
			r := &runs[i]
			if matching(*r, b) && usable(*r) && (match == nil || r.ID > match.ID) {
				match = r
			}
			if r.Workflow == p.Workflow && (r.Status == "waiting" || r.Status == "running") {
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
		// Read the admitted CAS timestamp before contacting provider.
		current, e := s.GetBuild(ctx, domain.BuildLookup{ProductID: b.ProductID, BuildID: b.ID})
		if e != nil {
			return e
		}
		r, e := a.provider.Dispatch(ctx, a.config.Connection(p.ConnectionID), p.Repository, p.Workflow, p.DispatchRef, b.SourceSHA, b.ID)
		next := current
		if e != nil {
			var uncertain *forgejo.Uncertain
			if errors.As(e, &uncertain) {
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
		if e = s.advanceBuild(ctx, current, next, false); e != nil {
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
