package application

import (
	"context"
	"errors"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"strings"
	"testing"
	"time"
)

type fakeForgejo struct {
	head        string
	runs        []forgejo.Run
	dispatches  int
	uncertain   bool
	branchCalls int
}

func (f *fakeForgejo) BranchHead(context.Context, buildconfig.Connection, string, string) (string, error) {
	f.branchCalls++
	return f.head, nil
}
func (f *fakeForgejo) CommitExists(_ context.Context, _ buildconfig.Connection, _, sha string) error {
	if sha == strings.Repeat("f", 40) {
		return errors.New("unknown commit")
	}
	return nil
}
func (f *fakeForgejo) Dispatch(_ context.Context, _ buildconfig.Connection, _ string, _ string, ref, sha, requestID string) (forgejo.Run, error) {
	f.dispatches++
	r := forgejo.Run{ID: int64(f.dispatches), Status: "waiting", SHA: f.head, Branch: ref, Workflow: "build.yml", EventPayload: dispatchPayload(sha, requestID), Event: "workflow_dispatch", Created: time.Now().UTC(), URL: "https://git/run"}
	f.runs = append(f.runs, r)
	if f.uncertain {
		return forgejo.Run{}, &forgejo.Uncertain{}
	}
	return r, nil
}
func (f *fakeForgejo) GetRun(_ context.Context, _ buildconfig.Connection, _ string, id int64) (forgejo.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return forgejo.Run{}, errors.New("missing")
}
func (f *fakeForgejo) ListRuns(_ context.Context, _ buildconfig.Connection, _, _ string, filter forgejo.Filter) (forgejo.Page, error) {
	out := forgejo.Page{}
	for _, r := range f.runs {
		if (filter.SHA == "" || filter.SHA == r.SHA) && (filter.Status == "" || filter.Status == r.Status) {
			out.Runs = append(out.Runs, r)
		}
	}
	out.Total = len(out.Runs)
	return out, nil
}

type fakeBuildRegistry struct {
	artifact buildregistry.Artifact
	err      error
}

func (f *fakeBuildRegistry) Resolve(context.Context, buildconfig.Registry, string, string) (buildregistry.Artifact, error) {
	return f.artifact, f.err
}
func buildConfig() *buildconfig.Config {
	return &buildconfig.Config{Connections: []buildconfig.Connection{{ID: "forgejo", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "token"}}, Registries: []buildconfig.Registry{{ID: "zot", Origin: "http://registry.example", InsecureHTTP: true}}, Pools: []buildconfig.Pool{{ID: "pool", MaxInFlight: 1}}, Profiles: []buildconfig.Profile{{ID: "app", ProductID: "p", ConnectionID: "forgejo", Repository: "o/r", Workflow: "build.yml", DefaultBranch: "main", PoolID: "pool", RegistryID: "zot", Images: []buildconfig.Image{{ID: "app", Repository: "o/r"}}, DispatchRef: "main", TagCandidates: []string{"main-{sha8}", "br-{sha8}"}}}}
}
func buildFixture(t *testing.T) (*Service, *memoryStore, *fakeForgejo, *fakeBuildRegistry) {
	t.Helper()
	s, m := fixture(t)
	f := &fakeForgejo{head: strings.Repeat("a", 40)}
	r := &fakeBuildRegistry{err: buildregistry.ErrMissing}
	if e := s.ConfigureBuilds(buildConfig(), f, r); e != nil {
		t.Fatal(e)
	}
	return s, m, f, r
}
func queue(t *testing.T, s *Service, key string) domain.Build {
	t.Helper()
	b, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", RequestKey: key})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func tick(t *testing.T, s *Service) {
	t.Helper()
	if e := s.DispatchBuildsOnce(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func getBuild(t *testing.T, s *Service, b domain.Build) domain.Build {
	t.Helper()
	v, e := s.GetBuild(context.Background(), domain.BuildLookup{ProductID: b.ProductID, BuildID: b.ID})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestBuildIdempotencyAndKeys(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	b := queue(t, s, "one")
	events := len(m.state.Events)
	again := queue(t, s, "two")
	if b.ID != again.ID || len(m.state.Builds) != 1 || len(m.state.Events) != events {
		t.Fatal("duplicate build/audit")
	}
	f.head = strings.Repeat("b", 40)
	if _, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", RequestKey: "two"}); e == nil || !strings.Contains(e.Error(), "REQUEST_KEY_CONFLICT") {
		t.Fatal(e)
	}
	if _, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: b.SourceSHA}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.Builds) != 1 {
		t.Fatal("SHA alias created duplicate")
	}
}
func TestBuildPoolLimitAndDigest(t *testing.T) {
	s, _, f, r := buildFixture(t)
	b := queue(t, s, "one")
	f.head = strings.Repeat("b", 40)
	second := queue(t, s, "two")
	f.head = b.SourceSHA
	tick(t, s)
	if f.dispatches != 1 || getBuild(t, s, b).Status != "DISPATCHED" || getBuild(t, s, second).Status != "QUEUED" {
		t.Fatal("pool limit")
	}
	if _, e := s.CancelBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: b.ID}); e == nil {
		t.Fatal("cancelled dispatched")
	}
	f.runs[0].Status = "running"
	tick(t, s)
	if getBuild(t, s, b).Status != "RUNNING" {
		t.Fatal("running mapping")
	}
	f.runs[0].Status = "success"
	r.err = nil
	r.artifact = buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("d", 64), Revision: b.SourceSHA}
	tick(t, s)
	done := getBuild(t, s, b)
	if done.Status != "SUCCEEDED" || done.Images[0].Digest != r.artifact.Digest || !strings.HasSuffix(done.Images[0].ImageRef, "@"+done.Images[0].Digest) || done.RunURL == "" {
		t.Fatal(done)
	}
}
func TestBuildRegistryReuseAndRunAttachment(t *testing.T) {
	for _, target := range []string{"dev", "image"} {
		t.Run(target, func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			branch := "main"
			if target == "image" {
				branch = "topic"
			}
			b, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", Branch: branch})
			if e != nil {
				t.Fatal(e)
			}
			r.err = nil
			r.artifact = buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("d", 64), Revision: b.SourceSHA}
			if target == "dev" {
				f.runs = []forgejo.Run{{ID: 99, SHA: b.SourceSHA, Branch: "main", Status: "success", Event: "push"}}
			}
			tick(t, s)
			if v := getBuild(t, s, b); v.Status != "SUCCEEDED" || v.Reused != "registry" || f.dispatches != 0 {
				t.Fatal(v)
			}
		})
	}
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	f.runs = []forgejo.Run{{ID: 99, SHA: b.SourceSHA, Branch: "main", Workflow: "build.yml", EventPayload: dispatchPayload(b.SourceSHA, "external"), Status: "waiting", Event: "workflow_dispatch"}}
	tick(t, s)
	v := getBuild(t, s, b)
	if v.Reused != "run" || v.ProviderRunID != 99 || f.dispatches != 0 {
		t.Fatal(v)
	}
}
func TestBuildImageAloneProvesBuild(t *testing.T) {
	s, _, f, r := buildFixture(t)
	b := queue(t, s, "")
	r.err = nil
	r.artifact = buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("d", 64), Revision: b.SourceSHA}
	tick(t, s)
	if getBuild(t, s, b).Status != "SUCCEEDED" || f.dispatches != 0 {
		t.Fatal("image not reused")
	}
}
func TestBuildUncertainDispatchAndRestart(t *testing.T) {
	s, m, f, r := buildFixture(t)
	f.uncertain = true
	b := queue(t, s, "")
	tick(t, s)
	if getBuild(t, s, b).Status != "RECONCILIATION_REQUIRED" {
		t.Fatal("uncertainty lost")
	}
	f.runs = nil
	tick(t, s)
	if f.dispatches != 1 {
		t.Fatal("uncertain redispatched")
	}
	f.runs = []forgejo.Run{{ID: 1, SHA: b.SourceSHA, Branch: "main", Workflow: "build.yml", EventPayload: dispatchPayload(b.SourceSHA, b.ID), Event: "workflow_dispatch", Created: time.Now().UTC(), Status: "running"}}
	resumed := New(m)
	if e := resumed.ConfigureBuilds(buildConfig(), f, r); e != nil {
		t.Fatal(e)
	}
	tick(t, resumed)
	tick(t, resumed)
	if v := getBuild(t, resumed, b); v.Status != "RUNNING" || v.ProviderRunID != 1 || f.dispatches != 1 {
		t.Fatal(v)
	}
}
func TestBuildTerminalFailuresAndRetries(t *testing.T) {
	for _, status := range []string{"failure", "cancelled", "success", "mismatch"} {
		t.Run(status, func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			b := queue(t, s, "old")
			tick(t, s)
			f.runs[0].Status = status
			if status == "mismatch" {
				f.runs[0].Status = "success"
				r.err = nil
				r.artifact = buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("c", 64), Revision: strings.Repeat("b", 40)}
			}
			tick(t, s)
			v := getBuild(t, s, b)
			want := "FAILED"
			if status == "cancelled" {
				want = "CANCELLED"
			}
			if v.Status != want {
				t.Fatal(v)
			}
			if status == "success" && v.FailureCode != "ARTIFACT_MISSING" || status == "mismatch" && v.FailureCode != "ARTIFACT_REVISION_MISMATCH" {
				t.Fatal(v)
			}
			if _, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"}); e == nil {
				t.Fatal("implicit retry")
			}
			if same := queue(t, s, "old"); same.ID != b.ID {
				t.Fatal("same key retried")
			}
			if next := queue(t, s, "new"); next.ID == b.ID {
				t.Fatal("explicit retry missing")
			}
		})
	}
}
func TestBuildCancelAndBranchRules(t *testing.T) {
	s, _, _, _ := buildFixture(t)
	b := queue(t, s, "")
	v, e := s.CancelBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if e != nil || v.Status != "CANCELLED" {
		t.Fatal(v, e)
	}
	if _, e = s.CancelBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: b.ID}); e == nil {
		t.Fatal("cancel rules")
	}
	for _, in := range []domain.QueueBuildRequest{{ProductID: "p", ProfileID: "app", Branch: "bad branch"}, {ProductID: "p", ProfileID: "app", Branch: "main", SHA: strings.Repeat("a", 40)}, {ProductID: "else", ProfileID: "app"}} {
		if _, e = s.QueueBuild(context.Background(), in); e == nil {
			t.Fatal("unsafe branch/scope accepted")
		}
	}
}
func TestBuildBusyWorkflowAndHeadMovement(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	f.runs = []forgejo.Run{{ID: 99, SHA: strings.Repeat("b", 40), Branch: "other", Workflow: "build.yml", Status: "running"}}
	tick(t, s)
	if f.dispatches != 0 || getBuild(t, s, b).Status != "QUEUED" {
		t.Fatal("busy workflow ignored")
	}
	f.runs = nil
	f.head = strings.Repeat("b", 40)
	tick(t, s)
	if v := getBuild(t, s, b); v.Status != "DISPATCHED" || f.dispatches != 1 {
		t.Fatal(v)
	}
}
func TestBuildTimeoutAndProductAuthorization(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	b := queue(t, s, "")
	tick(t, s)
	old := time.Now().Add(-3 * time.Hour)
	m.state.Builds[0].DispatchedAt = &old
	tick(t, s)
	if v := getBuild(t, s, b); v.Status != "RECONCILIATION_REQUIRED" || v.FailureCode != "BUILD_TIMEOUT" {
		t.Fatal(v)
	}
	tick(t, s)
	if getBuild(t, s, b).Status != "RECONCILIATION_REQUIRED" {
		t.Fatal("timeout resumed active")
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/other", Products: []string{"else"}, Permissions: []string{"read", "write"}})
	if _, e := s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID}); e == nil {
		t.Fatal("cross product read")
	}
	if _, e := s.QueueBuild(ctx, domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"}); e == nil {
		t.Fatal("cross product write")
	}
	if _, e := s.ListBuildProfiles(ctx, "p"); e == nil {
		t.Fatal("cross product profiles")
	}
}
func TestBuildPagination(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	for _, sha := range []string{"a", "b", "c"} {
		f.head = strings.Repeat(sha, 40)
		queue(t, s, "")
	}
	first, e := s.ListBuilds(context.Background(), domain.ListBuildsRequest{ProductID: "p", Limit: 2})
	if e != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatal(first, e)
	}
	second, e := s.ListBuilds(context.Background(), domain.ListBuildsRequest{ProductID: "p", Limit: 2, Cursor: first.NextCursor})
	if e != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal(second, e)
	}
	if _, e = s.ListBuilds(context.Background(), domain.ListBuildsRequest{ProductID: "else", Cursor: first.NextCursor}); e == nil {
		t.Fatal("cursor scope ignored")
	}
}

func TestBuildAuthorityDriftFailsClosed(t *testing.T) {
	s, _, f, r := buildFixture(t)
	b := queue(t, s, "")
	c := buildConfig()
	c.TimeoutSeconds = 3600
	if e := s.ConfigureBuilds(c, f, r); e != nil {
		t.Fatal(e)
	}
	tick(t, s)
	if v := getBuild(t, s, b); v.Status != "FAILED" || v.FailureCode != "BUILD_AUTHORITY_CHANGED" || f.dispatches != 0 {
		t.Fatal(v)
	}
}

func TestBuildFIFOBlockedHead(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	first := queue(t, s, "first")
	f.head = strings.Repeat("b", 40)
	second := queue(t, s, "second")
	f.runs = []forgejo.Run{{ID: 99, SHA: strings.Repeat("c", 40), Branch: "other", Workflow: "build.yml", Status: "running"}}
	tick(t, s)
	if f.dispatches != 0 || getBuild(t, s, first).Status != "QUEUED" || getBuild(t, s, second).Status != "QUEUED" {
		t.Fatal("blocked FIFO head bypassed")
	}
}

// Empty provider listings isolate RCP's durable admission limit from external
// workflow occupancy; neither runner ownership APIs nor listing latency is a lock.
type emptyBuildListing struct{ *fakeForgejo }

func (f emptyBuildListing) ListRuns(context.Context, buildconfig.Connection, string, string, forgejo.Filter) (forgejo.Page, error) {
	return forgejo.Page{}, nil
}
func TestBuildPoolAdmissionIndependentOfWorkflowOccupancy(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			c := buildConfig()
			c.Pools[0].MaxInFlight = capacity
			p := c.Profiles[0]
			p.ID = "second"
			p.Repository = "o/second"
			p.Images = []buildconfig.Image{{ID: "app", Repository: "o/second"}}
			c.Profiles = append(c.Profiles, p)
			if e := s.ConfigureBuilds(c, emptyBuildListing{f}, r); e != nil {
				t.Fatal(e)
			}
			queue(t, s, "first")
			b, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "second"})
			if e != nil {
				t.Fatal(e)
			}
			tick(t, s)
			if f.dispatches != capacity {
				t.Fatalf("capacity=%d dispatched=%d", capacity, f.dispatches)
			}
			v := getBuild(t, s, b)
			if capacity == 1 && v.Status != "QUEUED" || capacity == 2 && v.Status != "DISPATCHED" {
				t.Fatal(v)
			}
		})
	}
}

func TestBuildPreventsProductDeletionWhileUnfinished(t *testing.T) {
	s, _, _, _ := buildFixture(t)
	queue(t, s, "")
	if _, e := s.Execute(context.Background(), domain.Command{Action: "delete_product", Actor: "agent", ProductID: "p", Data: map[string]any{"name": "Product"}}); e == nil || !strings.Contains(e.Error(), "unfinished builds") {
		t.Fatal(e)
	}
}
func TestBuildUnchangedObservationDoesNotAppendAudit(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	queue(t, s, "")
	tick(t, s)
	count := len(m.state.Events)
	tick(t, s)
	if len(m.state.Events) != count {
		t.Fatal("unchanged observation appended audit")
	}
}

func TestBuildWritePermissionAndAttribution(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	reader := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/reader", Products: []string{"p"}, Permissions: []string{"read"}})
	in := domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"}
	if _, e := s.QueueBuild(reader, in); e == nil {
		t.Fatal("reader queued build")
	}
	writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/requester", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	b, e := s.QueueBuild(writer, in)
	if e != nil || b.Actor != "agent/requester" {
		t.Fatal(b, e)
	}
	lookup := domain.BuildLookup{ProductID: "p", BuildID: b.ID}
	if _, e = s.CancelBuild(reader, lookup); e == nil {
		t.Fatal("reader cancelled build")
	}
	canceller := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/canceller", Products: []string{"p"}, Permissions: []string{"write"}})
	done, e := s.CancelBuild(canceller, lookup)
	if e != nil || done.Actor != b.Actor || m.state.Builds[0].Actor != b.Actor || m.state.Events[len(m.state.Events)-1].Actor != "agent/canceller" {
		t.Fatal(done, e)
	}
	if _, e = s.GetBuild(reader, lookup); e != nil {
		t.Fatal(e)
	}
}

func TestBuildRejectsOtherWorkflow(t *testing.T) {
	s, _, f, r := buildFixture(t)
	c := buildConfig()
	c.Profiles[0].Workflow = "other.yml"
	if e := s.ConfigureBuilds(c, f, r); e == nil {
		t.Fatal("wrong workflow accepted")
	}
}

// Tags are individually observable; the workflow chooses main/br based on history.
type candidateRegistry struct {
	calls  []string
	values map[string]buildregistry.Artifact
}

func (r *candidateRegistry) Resolve(_ context.Context, _ buildconfig.Registry, _, tag string) (buildregistry.Artifact, error) {
	r.calls = append(r.calls, tag)
	v, ok := r.values[tag]
	if !ok {
		return v, buildregistry.ErrMissing
	}
	return v, nil
}
func TestBuildCandidateTags(t *testing.T) {
	for _, phase := range []string{"reuse", "success", "mismatch"} {
		t.Run(phase, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			r := &candidateRegistry{values: map[string]buildregistry.Artifact{}}
			if e := s.ConfigureBuilds(buildConfig(), f, r); e != nil {
				t.Fatal(e)
			}
			b := queue(t, s, "")
			if phase == "success" {
				tick(t, s)
				f.runs[0].Status = "success"
			}
			revision := b.SourceSHA
			if phase == "mismatch" {
				revision = strings.Repeat("b", 40)
			}
			r.values["br-aaaaaaaa"] = buildregistry.Artifact{Revision: revision, Digest: "sha256:" + strings.Repeat("c", 64)}
			tick(t, s)
			v := getBuild(t, s, b)
			if phase == "mismatch" {
				if v.FailureCode != "ARTIFACT_REVISION_MISMATCH" || f.dispatches != 0 {
					t.Fatal(v)
				}
				return
			}
			if v.Status != "SUCCEEDED" || !strings.Contains(v.Images[0].ImageRef, ":br-aaaaaaaa@sha256:") {
				t.Fatal(v)
			}
			if phase == "reuse" && (v.Reused != "registry" || f.dispatches != 0) {
				t.Fatal(v)
			}
			if len(r.calls) < 2 || r.calls[len(r.calls)-2] != "main-aaaaaaaa" || r.calls[len(r.calls)-1] != "br-aaaaaaaa" {
				t.Fatal(r.calls)
			}
		})
	}
}
func TestBuildSHASourceValidation(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	sha := strings.Repeat("b", 40)
	b, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: sha})
	if e != nil || b.SourceSHA != sha || b.Branch != "" || f.branchCalls != 0 {
		t.Fatal(b, e)
	}
	for _, in := range []domain.QueueBuildRequest{
		{ProductID: "p", ProfileID: "app", SHA: strings.Repeat("f", 40)},
		{ProductID: "p", ProfileID: "app", SHA: sha, Branch: "main"},
		{ProductID: "p", ProfileID: "app", SHA: "bad"},
	} {
		if _, e = s.QueueBuild(context.Background(), in); e == nil {
			t.Fatal("invalid source accepted")
		}
	}
	if len(m.state.Builds) != 1 {
		t.Fatal("rejected request persisted")
	}
	tick(t, s)
	if getBuild(t, s, b).Status != "DISPATCHED" || f.branchCalls != 0 {
		t.Fatal("SHA dispatch depended on HEAD")
	}
	tick(t, s)
	if getBuild(t, s, b).FailureCode != "" {
		t.Fatal("workflow ref SHA confused with built SHA")
	}
}
func TestBuildAttachOnlyDispatchInputs(t *testing.T) {
	for _, mode := range []string{"push", "pull_request", "wrong-workflow", "other-sha", "malformed", "missing", "oversize", "success", "running"} {
		t.Run(mode, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			b := queue(t, s, "")
			run := forgejo.Run{ID: 99, Workflow: "build.yml", EventPayload: dispatchPayload(b.SourceSHA, "external"), Event: "workflow_dispatch", Status: "success", SHA: strings.Repeat("b", 40)}
			switch mode {
			case "push", "pull_request":
				run.Event = mode
			case "wrong-workflow":
				run.Workflow = "checks.yml"
			case "other-sha":
				run.EventPayload = dispatchPayload(strings.Repeat("c", 40), "external")
			case "malformed":
				run.EventPayload = `{`
			case "missing":
				run.EventPayload = `{}`
			case "oversize":
				run.EventPayload += strings.Repeat(" ", 256<<10)
			case "running":
				run.Status = "running"
			}
			f.runs = []forgejo.Run{run}
			tick(t, s)
			v := getBuild(t, s, b)
			if mode == "success" || mode == "running" {
				if v.Reused != "run" || v.ProviderRunID != 99 || f.dispatches != 0 {
					t.Fatal(v)
				}
			} else if v.Reused == "run" || f.dispatches != 1 {
				t.Fatal(v)
			}
		})
	}
}
func TestBuildUncertainRequiresExactRequestInput(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	f.uncertain = true
	b := queue(t, s, "")
	tick(t, s)
	f.runs[0].EventPayload = dispatchPayload(b.SourceSHA, "other")
	tick(t, s)
	if v := getBuild(t, s, b); v.ProviderRunID != 0 || v.Status != "RECONCILIATION_REQUIRED" {
		t.Fatal(v)
	}
	f.runs[0].EventPayload = dispatchPayload(b.SourceSHA, b.ID)
	tick(t, s)
	if v := getBuild(t, s, b); v.ProviderRunID != 1 || f.dispatches != 1 {
		t.Fatal(v)
	}
}
func TestBuildOwnedRunsDoNotBlockPoolCapacity(t *testing.T) {
	s, _, f, r := buildFixture(t)
	c := buildConfig()
	c.Pools[0].MaxInFlight = 2
	if e := s.ConfigureBuilds(c, f, r); e != nil {
		t.Fatal(e)
	}
	first := queue(t, s, "one")
	f.head = strings.Repeat("b", 40)
	second := queue(t, s, "two")
	tick(t, s)
	if f.dispatches != 2 || getBuild(t, s, first).Status != "DISPATCHED" || getBuild(t, s, second).Status != "DISPATCHED" {
		t.Fatal("owned run blocked spare capacity")
	}
	f.head = strings.Repeat("c", 40)
	third := queue(t, s, "three")
	tick(t, s)
	if f.dispatches != 2 || getBuild(t, s, third).Status != "QUEUED" {
		t.Fatal("pool capacity exceeded")
	}
}

func TestBuildPoolOwnershipRejectsDriftedAuthority(t *testing.T) {
	s, m, f, r := buildFixture(t)
	c := buildConfig()
	c.Pools[0].MaxInFlight = 2
	if e := s.ConfigureBuilds(c, f, r); e != nil {
		t.Fatal(e)
	}
	b := queue(t, s, "")
	// An old provider's ID is not proof of ownership in the current provider.
	owner := domain.Build{Meta: domain.Meta{ID: "old-owner", ProductID: "p", UpdatedAt: time.Now().UTC()}, ProfileID: "app", PoolID: "pool", Repository: "o/r", Workflow: "build.yml", AuthorityHash: "previous-authority", SourceSHA: strings.Repeat("b", 40), Status: "RUNNING", ProviderRunID: 99, QueuedAt: b.QueuedAt.Add(time.Second)}
	m.state.Builds = append(m.state.Builds, owner)
	f.runs = []forgejo.Run{{ID: 99, Workflow: "build.yml", EventPayload: dispatchPayload(owner.SourceSHA, owner.ID), Event: "workflow_dispatch", Status: "running"}}
	tick(t, s)
	if getBuild(t, s, b).Status != "QUEUED" || f.dispatches != 0 {
		t.Fatal("stale authority masked foreign run occupancy")
	}
	if v := getBuild(t, s, owner); v.Status != "RECONCILIATION_REQUIRED" || v.FailureCode != "BUILD_AUTHORITY_CHANGED" {
		t.Fatal(v)
	}
}

func dispatchPayload(sha, requestID string) string {
	return `{"ref":"main","inputs":{"sha":"` + sha + `","request_id":"` + requestID + `"},"repository":{"full_name":"o/r"},"sender":{"login":"builder"}}`
}

type multiImageRegistry map[string]buildregistry.Artifact

func (r multiImageRegistry) Resolve(_ context.Context, _ buildconfig.Registry, repository, tag string) (buildregistry.Artifact, error) {
	if v, ok := r[repository+":"+tag]; ok {
		return v, nil
	}
	return buildregistry.Artifact{}, buildregistry.ErrMissing
}
func TestBuildMultipleImages(t *testing.T) {
	for _, mode := range []string{"all", "partial", "split-tags", "wrong-revision", "success", "success-partial"} {
		t.Run(mode, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			c := buildConfig()
			c.Profiles[0].Images = []buildconfig.Image{{ID: "api", Repository: "o/api"}, {ID: "scheduler", Repository: "o/scheduler"}, {ID: "web", Repository: "o/web"}}
			r := multiImageRegistry{}
			sha := f.head
			for i, image := range c.Profiles[0].Images {
				tag := "main-" + sha[:8]
				if mode == "split-tags" && i == 2 {
					tag = "br-" + sha[:8]
				}
				if (mode == "partial" || mode == "success-partial") && i == 2 {
					continue
				}
				revision := sha
				if mode == "wrong-revision" && i == 2 {
					revision = strings.Repeat("b", 40)
				}
				r[image.Repository+":"+tag] = buildregistry.Artifact{Revision: revision, Digest: "sha256:" + strings.Repeat(string(rune('a'+i)), 64)}
			}
			if mode == "success" || mode == "success-partial" {
				// Dispatch before publication; completion must verify the complete set.
				if e := s.ConfigureBuilds(c, f, multiImageRegistry{}); e != nil {
					t.Fatal(e)
				}
			} else if e := s.ConfigureBuilds(c, f, r); e != nil {
				t.Fatal(e)
			}
			b := queue(t, s, "")
			tick(t, s)
			if mode == "success" || mode == "success-partial" {
				f.runs[0].Status = "success"
				if e := s.ConfigureBuilds(c, f, r); e != nil {
					t.Fatal(e)
				}
				tick(t, s)
			}
			v := getBuild(t, s, b)
			switch mode {
			case "all", "success":
				if v.Status != "SUCCEEDED" || len(v.Images) != 3 {
					t.Fatal(v)
				}
				for i, image := range v.Images {
					if image.ID != c.Profiles[0].Images[i].ID || !strings.Contains(image.ImageRef, c.Profiles[0].Images[i].Repository+":main-") || !strings.HasSuffix(image.ImageRef, "@"+image.Digest) {
						t.Fatal(image)
					}
				}
			case "wrong-revision":
				if v.FailureCode != "ARTIFACT_REVISION_MISMATCH" {
					t.Fatal(v)
				}
			case "success-partial":
				if v.FailureCode != "ARTIFACT_MISSING" || len(v.Images) != 0 {
					t.Fatal(v)
				}
			default:
				if f.dispatches != 1 || v.Status != "DISPATCHED" || len(v.Images) != 0 {
					t.Fatal(v)
				}
			}
		})
	}
}
