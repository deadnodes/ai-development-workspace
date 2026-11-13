package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"sort"
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
	branchError error
	runCalls    int
	listError   string
	filters     []forgejo.Filter
}

func (f *fakeForgejo) BranchHead(context.Context, buildconfig.Connection, string, string) (string, error) {
	f.branchCalls++
	return f.head, f.branchError
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
	f.runCalls++
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return forgejo.Run{}, errors.New("missing")
}
func (f *fakeForgejo) ListRuns(_ context.Context, _ buildconfig.Connection, _ string, workflow string, filter forgejo.Filter) (forgejo.Page, error) {
	out := forgejo.Page{}
	f.filters = append(f.filters, filter)
	if f.listError != "" && (f.listError == "all" || f.listError == filter.Status) {
		return out, errors.New("unavailable")
	}
	f.runCalls++
	for _, r := range f.runs {
		if (filter.SHA == "" || filter.SHA == r.SHA) && (filter.Status == "" || filter.Status == r.Status) && r.Workflow == workflow && (filter.Event == "" || filter.Event == r.EffectiveEvent()) {
			out.Runs = append(out.Runs, r)
		}
	}
	sort.SliceStable(out.Runs, func(i, j int) bool { return out.Runs[i].Created.After(out.Runs[j].Created) })
	out.Total = len(out.Runs)
	start := (filter.Page - 1) * filter.Limit
	if start > len(out.Runs) {
		start = len(out.Runs)
	}
	end := start + filter.Limit
	if end > len(out.Runs) {
		end = len(out.Runs)
	}
	out.Runs = out.Runs[start:end]
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
	if _, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", RequestKey: "two", Branch: "other"}); e == nil || !strings.Contains(e.Error(), "REQUEST_KEY_CONFLICT") {
		t.Fatal(e)
	}
	if _, e := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: b.SourceSHA}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.Builds) != 1 {
		t.Fatal("SHA alias created duplicate")
	}
}
func TestBuildBranchRequestReplay(t *testing.T) {
	for _, scenario := range []string{"branch advanced", "provider unavailable", "branch deleted"} {
		for _, branch := range []string{"", "main"} {
			t.Run(scenario+"/branch="+branch, func(t *testing.T) {
				s, m, f, _ := buildFixture(t)
				original := queue(t, s, "original")
				before, err := json.Marshal(m.state)
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "branch advanced":
					f.head = strings.Repeat("b", 40)
				case "provider unavailable":
					f.branchError = errors.New("provider unavailable")
				case "branch deleted":
					f.head = ""
					f.branchError = errors.New("branch not found")
				}
				got, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", RequestKey: "original", Branch: branch})
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("replay = %+v, %v; want %+v", got, err, original)
				}
				after, err := json.Marshal(m.state)
				if err != nil {
					t.Fatal(err)
				}
				if f.branchCalls != 1 || string(before) != string(after) {
					t.Fatal("replay observed branch or changed durable state")
				}
			})
		}
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
	f.runs = []forgejo.Run{{ID: 99, SHA: strings.Repeat("b", 40), Branch: "other", Workflow: "build.yml", Event: "workflow_dispatch", Status: "running"}}
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
	f.runs = []forgejo.Run{{ID: 99, SHA: strings.Repeat("c", 40), Branch: "other", Workflow: "build.yml", Event: "workflow_dispatch", Status: "running"}}
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

func TestBuildUncertainDispatchTerminalReconciliation(t *testing.T) {
	for _, status := range []string{"failure", "cancelled", "skipped", "blocked"} {
		t.Run(status, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			f.uncertain = true
			b := queue(t, s, "first")
			tick(t, s)
			f.head = strings.Repeat("b", 40)
			second := queue(t, s, "second")
			// A terminal run with the same source but a different request is not ours.
			f.runs[0].Status = status
			other := f.runs[0]
			other.ID = 99
			other.EventPayload = dispatchPayload(b.SourceSHA, "different-request")
			f.runs = append([]forgejo.Run{other}, f.runs...)
			f.uncertain = false
			tick(t, s)
			got := getBuild(t, s, b)
			want := "FAILED"
			if status == "cancelled" {
				want = "CANCELLED"
			}
			if got.Status != want || got.ProviderRunID != 1 || got.RunStatus != status || got.FinishedAt == nil {
				t.Fatalf("terminal evidence lost: %+v", got)
			}
			if f.dispatches != 2 || getBuild(t, s, second).Status != "DISPATCHED" {
				t.Fatal("terminal reconciliation did not free capacity")
			}
		})
	}
}

func TestBuildImageReferenceRegistryHost(t *testing.T) {
	for _, origin := range []string{"http://registry.example", "https://registry.example:5443"} {
		t.Run(origin, func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			c := buildConfig()
			c.Registries[0].Origin = origin
			if err := s.ConfigureBuilds(c, f, r); err != nil {
				t.Fatal(err)
			}
			b := queue(t, s, "image")
			r.err = nil
			r.artifact = buildregistry.Artifact{Revision: b.SourceSHA, Digest: "sha256:" + strings.Repeat("d", 64)}
			tick(t, s)
			host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
			want := host + "/o/r:main-aaaaaaaa@" + r.artifact.Digest
			if got := getBuild(t, s, b); got.Status != "SUCCEEDED" || got.Images[0].ImageRef != want {
				t.Fatalf("invalid image reference: %+v", got)
			}
		})
	}
}

func TestRestoreBuildQuarantine(t *testing.T) {
	for _, status := range []string{"QUEUED", "DISPATCHED", "RUNNING", "RECONCILIATION_REQUIRED"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			source, m, f, r := buildFixture(t)
			b := queue(t, source, "original")
			m.state.Builds[0].Status = status
			m.state.Builds[0].ProviderRunID = 17
			before := m.state.Builds[0]
			backup, err := source.CreateBackup(ctx, "agent")
			if err != nil {
				t.Fatal(err)
			}
			dest := New(&memoryStore{domain.EmptyState()})
			if err = dest.ConfigureBuilds(buildConfig(), f, r); err != nil {
				t.Fatal(err)
			}
			result, err := dest.RestoreBackup(ctx, "agent", backup.ArchiveBase64, backup.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			state, err := dest.store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			audit := state.Events[len(state.Events)-1].Data.Data["quarantined_builds"]
			raw, err := json.Marshal(audit)
			if err != nil {
				t.Fatal(err)
			}
			var originals []domain.Build
			if err = json.Unmarshal(raw, &originals); err != nil || len(originals) != 1 || originals[0].Status != before.Status || originals[0].RestoreQuarantined {
				t.Fatalf("original build audit lost: %s", raw)
			}
			ids := result.(map[string]any)["quarantined_build_ids"].([]string)
			if len(ids) != 1 || ids[0] != b.ID {
				t.Fatal(result)
			}
			for i := 0; i < 2; i++ {
				tick(t, dest)
			}
			got := getBuild(t, dest, b)
			if !got.RestoreQuarantined || got.Status != "RECONCILIATION_REQUIRED" || got.FailureCode != "BUILD_RESTORED" || got.ProviderRunID != 17 || f.dispatches != 0 || f.runCalls != 0 {
				t.Fatalf("restored build continued: %+v", got)
			}
			if m.state.Builds[0].Status != before.Status {
				t.Fatal("source changed")
			}
			if same := queue(t, dest, "original"); same.ID != b.ID {
				t.Fatal("receipt lost")
			}
			if _, err := dest.QueueBuild(ctx, domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"}); err == nil {
				t.Fatal("retry without new key")
			}
			fresh := queue(t, dest, "fresh")
			if fresh.ID == b.ID {
				t.Fatal("fresh request reused quarantine")
			}
			tick(t, dest)
			if f.dispatches != 1 {
				t.Fatal("quarantine blocked fresh request")
			}
			// Quarantine survives a subsequent backup/restore.
			backup, err = dest.CreateBackup(ctx, "agent")
			if err != nil {
				t.Fatal(err)
			}
			again := New(&memoryStore{domain.EmptyState()})
			if _, err = again.RestoreBackup(ctx, "agent", backup.ArchiveBase64, backup.SHA256); err != nil {
				t.Fatal(err)
			}
			if !getBuild(t, again, b).RestoreQuarantined {
				t.Fatal("quarantine lost")
			}
		})
	}
}

func TestBuildFilteredHistoryAndListingErrors(t *testing.T) {
	for _, mode := range []string{"admit", "reuse", "recover", "all", "waiting", "running"} {
		t.Run(mode, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			b := queue(t, s, "")
			if mode == "recover" {
				f.uncertain = true
				tick(t, s)
			}
			for i := 0; i < 1100; i++ {
				f.runs = append(f.runs, forgejo.Run{ID: int64(i + 100), Workflow: "build.yml", Event: "workflow_dispatch", Status: "success", SHA: strings.Repeat("c", 40), Created: time.Now().Add(-48 * time.Hour)})
			}
			if mode == "reuse" {
				f.runs = append(f.runs, forgejo.Run{ID: 2000, Workflow: "build.yml", Event: "workflow_dispatch", Status: "running", Created: time.Now().UTC(), SHA: b.SourceSHA, EventPayload: dispatchPayload(b.SourceSHA, "external")})
			}
			if mode == "all" || mode == "waiting" || mode == "running" {
				f.listError = mode
			}
			tick(t, s)
			got := getBuild(t, s, b)
			if f.listError != "" {
				if got.Status != "QUEUED" || !got.ProviderUnavailable || got.LastError == "" || f.dispatches != 0 {
					t.Fatal(got)
				}
				f.listError = ""
				tick(t, s)
				got = getBuild(t, s, b)
			}
			if got.ProviderUnavailable || got.LastError != "" {
				t.Fatal(got)
			}
			if mode == "recover" {
				if got.ProviderRunID != 1 || f.dispatches != 1 {
					t.Fatal(got)
				}
			} else if got.Status != "DISPATCHED" {
				t.Fatal(got)
			}
			if mode == "reuse" && (got.ProviderRunID != 2000 || f.dispatches != 0) {
				t.Fatal(got)
			}
			for _, filter := range f.filters {
				if filter.Event != "workflow_dispatch" || filter.Limit != 50 || filter.SHA != "" {
					t.Fatal(filter)
				}
			}
		})
	}
}

func TestBuildUncertainListingErrorIsVisible(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	f.uncertain = true
	tick(t, s)
	f.listError = "all"
	tick(t, s)
	got := getBuild(t, s, b)
	if got.Status != "RECONCILIATION_REQUIRED" || !got.ProviderUnavailable || got.LastError == "" || f.dispatches != 1 {
		t.Fatal(got)
	}
	f.listError = ""
	tick(t, s)
	got = getBuild(t, s, b)
	if got.Status != "DISPATCHED" || got.ProviderUnavailable || got.LastError != "" || f.dispatches != 1 {
		t.Fatal(got)
	}
}

func TestBuildSourceHistoryPagination(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	for i := 0; i < 51; i++ {
		f.runs = append(f.runs, forgejo.Run{ID: int64(i + 100), Workflow: "build.yml", Event: "workflow_dispatch", Status: "failure", SHA: b.SourceSHA})
	}
	f.runs = append(f.runs, forgejo.Run{ID: 2000, Workflow: "build.yml", Event: "workflow_dispatch", Status: "success", SHA: b.SourceSHA, EventPayload: dispatchPayload(b.SourceSHA, "external")})
	tick(t, s)
	if got := getBuild(t, s, b); got.ProviderRunID != 2000 || got.Reused != "run" || f.dispatches != 0 {
		t.Fatal(got)
	}
	if len(f.filters) != 4 || f.filters[1].Page != 2 || f.filters[1].SHA != "" {
		t.Fatal(f.filters)
	}
}

func TestBuildFilteredListingBoundIsVisible(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	for i := 0; i < 501; i++ {
		f.runs = append(f.runs, forgejo.Run{ID: int64(i + 100), Workflow: "build.yml", Event: "workflow_dispatch", Status: "failure", SHA: b.SourceSHA})
	}
	tick(t, s)
	if got := getBuild(t, s, b); got.Status != "QUEUED" || !got.ProviderUnavailable || got.LastError == "" || f.dispatches != 0 {
		t.Fatal(got)
	}
	if len(f.filters) != 10 {
		t.Fatal(f.filters)
	}
}

func TestBuildReconcileDifferentWorkflowCommit(t *testing.T) {
	s, _, f, r := buildFixture(t)
	b, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: strings.Repeat("b", 40)})
	if err != nil {
		t.Fatal(err)
	}
	f.uncertain = true
	tick(t, s)
	f.runs[0].Status = "success"
	r.err = nil
	r.artifact = buildregistry.Artifact{Revision: b.SourceSHA, Digest: "sha256:" + strings.Repeat("d", 64)}
	tick(t, s)
	got := getBuild(t, s, b)
	if got.Status != "SUCCEEDED" || got.ProviderRunID != 1 || f.dispatches != 1 {
		t.Fatal(got)
	}
	for _, filter := range f.filters {
		if filter.SHA != "" {
			t.Fatal(filter)
		}
	}
}

func TestBuildAuthorityChangePreservesReceiptsOnly(t *testing.T) {
	for _, change := range []string{"registry", "repository", "images"} {
		t.Run(change, func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			old := queue(t, s, "historical")
			r.err = nil
			r.artifact = buildregistry.Artifact{Revision: old.SourceSHA, Digest: "sha256:" + strings.Repeat("d", 64)}
			tick(t, s)
			c := buildConfig()
			switch change {
			case "registry":
				c.Registries[0].Origin = "https://new.example"
			case "repository":
				c.Profiles[0].Repository = "o/new"
			case "images":
				c.Profiles[0].Images = append(c.Profiles[0].Images, buildconfig.Image{ID: "extra", Repository: "o/extra"})
			}
			if err := s.ConfigureBuilds(c, f, r); err != nil {
				t.Fatal(err)
			}
			if got := queue(t, s, "historical"); got.ID != old.ID {
				t.Fatal(got)
			}
			fresh := queue(t, s, "")
			if fresh.ID == old.ID || fresh.AuthorityHash == old.AuthorityHash || fresh.Status != "QUEUED" {
				t.Fatal(fresh)
			}
			if got := queue(t, s, "fresh"); got.ID != fresh.ID {
				t.Fatal(got)
			}
		})
	}
}

func TestBuildRunWindowAndCap(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		t.Run(fmt.Sprint(reconcile), func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			b := queue(t, s, "")
			if reconcile {
				f.uncertain = true
				tick(t, s)
			}
			f.runs = nil
			for i := 0; i < 501; i++ {
				f.runs = append(f.runs, forgejo.Run{ID: int64(i + 10), Workflow: "build.yml", Event: "workflow_dispatch", Status: "failure", Created: time.Now()})
			}
			tick(t, s)
			if got := getBuild(t, s, b); got.LastError != errBuildRunBound.Error() {
				t.Fatal(got)
			}
			f.filters = nil
			for i := range f.runs {
				f.runs[i].Created = b.QueuedAt.Add(-48 * time.Hour)
			}
			tick(t, s)
			if got := getBuild(t, s, b); got.LastError != "" {
				t.Fatal(got)
			}
			if len(f.filters) == 0 || f.filters[0].Page != 1 {
				t.Fatal(f.filters)
			}
			if reconcile && len(f.filters) != 1 {
				t.Fatal(f.filters)
			}
		})
	}
}

func TestBuildReuseConfiguredLookback(t *testing.T) {
	for _, lookback := range []int{0, 3600} {
		t.Run(fmt.Sprint(lookback), func(t *testing.T) {
			s, _, f, r := buildFixture(t)
			c := buildConfig()
			c.RunLookbackSeconds = lookback
			if err := s.ConfigureBuilds(c, f, r); err != nil {
				t.Fatal(err)
			}
			b := queue(t, s, "")
			f.runs = []forgejo.Run{{ID: 99, Workflow: "build.yml", Event: "workflow_dispatch", Status: "success", SHA: strings.Repeat("b", 40), EventPayload: dispatchPayload(b.SourceSHA, "external"), Created: time.Now().Add(-2 * time.Hour)}}
			tick(t, s)
			got := getBuild(t, s, b)
			if lookback == 0 {
				if got.ProviderRunID != 99 || got.Reused != "run" {
					t.Fatal(got)
				}
			} else if got.Reused != "" || f.dispatches != 1 {
				t.Fatal(got)
			}
		})
	}
}

func TestBuildReconcileQueuedTimeSkew(t *testing.T) {
	s, _, f, _ := buildFixture(t)
	b := queue(t, s, "")
	f.uncertain = true
	tick(t, s)
	// A finished run just before queued_at remains eligible despite clock skew.
	f.runs[0].Created = b.QueuedAt.Add(-time.Minute)
	f.runs[0].Status = "failure"
	// A newer, unrelated dispatch must not be mistaken for the lost response.
	other := f.runs[0]
	other.ID = 99
	other.Created = b.QueuedAt
	other.EventPayload = dispatchPayload(b.SourceSHA, "unrelated")
	f.runs = append(f.runs, other)
	tick(t, s)
	if got := getBuild(t, s, b); got.Status != "FAILED" || got.ProviderRunID != 1 || f.dispatches != 1 {
		t.Fatal(got)
	}
}

func forgejo15Run(t *testing.T, b domain.Build) forgejo.Run {
	t.Helper()
	raw := fmt.Sprintf(`{"id":1,"workflow_id":"build.yml","status":"running","event":"","trigger_event":"workflow_dispatch","commit_sha":%q,"event_payload":%q}`, strings.Repeat("b", 40), dispatchPayload(b.SourceSHA, b.ID))
	var r forgejo.Run
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildForgejo15MatchingAndSelfHeal(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	b := queue(t, s, "")
	tick(t, s)
	run := forgejo15Run(t, b)
	if !matching(run, b) {
		t.Fatal("trigger_event did not match")
	}
	run.Event = "push"
	if matching(run, b) {
		t.Fatal("trigger overrode event")
	}
	run.Event = ""
	run.TriggerEvent = ""
	if matching(run, b) {
		t.Fatal("missing event matched")
	}
	f.runs = []forgejo.Run{forgejo15Run(t, b)}
	m.state.Builds[0].Status = "RECONCILIATION_REQUIRED"
	m.state.Builds[0].FailureCode = "RUN_SOURCE_MISMATCH"
	tick(t, s)
	got := getBuild(t, s, b)
	if got.Status != "RUNNING" || got.FailureCode != "" || got.ProviderRunID != 1 || f.dispatches != 1 {
		t.Fatal(got)
	}
}

func TestBuildForgejo15ReuseGateAndReconcile(t *testing.T) {
	for _, mode := range []string{"reuse", "gate", "reconcile"} {
		t.Run(mode, func(t *testing.T) {
			s, _, f, _ := buildFixture(t)
			b := queue(t, s, "")
			if mode == "reconcile" {
				f.uncertain = true
				tick(t, s)
			}
			run := forgejo15Run(t, b)
			if mode == "gate" {
				run.EventPayload = dispatchPayload(strings.Repeat("c", 40), "other")
			}
			f.runs = []forgejo.Run{run}
			tick(t, s)
			got := getBuild(t, s, b)
			switch mode {
			case "reuse":
				if got.Reused != "run" || got.ProviderRunID != 1 || f.dispatches != 0 {
					t.Fatal(got)
				}
			case "gate":
				if got.Status != "QUEUED" || f.dispatches != 0 {
					t.Fatal(got)
				}
			case "reconcile":
				if got.Status != "RUNNING" || got.ProviderRunID != 1 || f.dispatches != 1 {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestBuildReconciliationTimeout(t *testing.T) {
	for _, mode := range []string{"release", "before-grace", "no-dispatch-time", "unavailable", "running-query-error", "get-error", "active-sha", "active-waiting", "active-id", "known-inactive", "page-cap", "other-sha", "other-event", "other-workflow"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f, _ := buildFixture(t)
			b := queue(t, s, "first")
			tick(t, s)
			old := time.Now().Add(-2*time.Hour - buildReconciliationGrace - time.Minute)
			m.state.Builds[0].Status = "RECONCILIATION_REQUIRED"
			m.state.Builds[0].FailureCode = "RUN_SOURCE_MISMATCH"
			m.state.Builds[0].DispatchedAt = &old
			m.state.Builds[0].ProviderRunID = 0
			f.runs = nil
			active := forgejo15Run(t, b)
			switch mode {
			case "before-grace":
				recent := time.Now().Add(-2*time.Hour - time.Minute)
				m.state.Builds[0].DispatchedAt = &recent
			case "no-dispatch-time":
				m.state.Builds[0].DispatchedAt = nil
			case "unavailable":
				f.listError = "waiting"
			case "running-query-error":
				f.listError = "running"
			case "get-error":
				m.state.Builds[0].ProviderRunID = 99
			case "active-sha":
				active.EventPayload = dispatchPayload(b.SourceSHA, "other")
				f.runs = []forgejo.Run{active}
			case "active-waiting":
				active.Status = "waiting"
				active.EventPayload = dispatchPayload(b.SourceSHA, "other")
				f.runs = []forgejo.Run{active}
			case "known-inactive":
				m.state.Builds[0].ProviderRunID = 1
				active.Status = "success"
				active.EventPayload = `{}`
				f.runs = []forgejo.Run{active}
			case "active-id":
				m.state.Builds[0].ProviderRunID = 1
				active.EventPayload = `{}`
				f.runs = []forgejo.Run{active}
			case "page-cap":
				for i := 0; i < 501; i++ {
					r := active
					r.ID = int64(i + 10)
					r.EventPayload = `{}`
					f.runs = append(f.runs, r)
				}
			case "other-sha":
				active.EventPayload = dispatchPayload(strings.Repeat("c", 40), "other")
				active.Status = "waiting"
				f.runs = []forgejo.Run{active}
			case "other-event":
				active.Event = "push"
				f.runs = []forgejo.Run{active}
			case "other-workflow":
				active.Workflow = "other.yml"
				f.runs = []forgejo.Run{active}
			}
			tick(t, s)
			got := getBuild(t, s, b)
			release := mode == "release" || mode == "known-inactive" || mode == "other-sha" || mode == "other-event" || mode == "other-workflow"
			if release {
				if got.Status != "FAILED" || got.FailureCode != "RECONCILIATION_TIMEOUT" || got.FinishedAt == nil || buildActive(got) {
					t.Fatal(got)
				}
			} else if got.Status != "RECONCILIATION_REQUIRED" || !buildActive(got) {
				t.Fatal(got)
			}
			if mode == "release" {
				f.head = strings.Repeat("c", 40)
				second := queue(t, s, "second")
				tick(t, s)
				if getBuild(t, s, second).Status != "DISPATCHED" {
					t.Fatal("slot not released")
				}
			}
		})
	}
}

func TestBuildLostDispatchReconciliationAfterGrace(t *testing.T) {
	for _, mode := range []string{"success", "failure", "listing-error", "page-cap"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f, registry := buildFixture(t)
			f.uncertain = true
			b := queue(t, s, "lost-dispatch")
			tick(t, s)
			if got := getBuild(t, s, b); got.ProviderRunID != 0 || got.FailureCode != "DISPATCH_UNCERTAIN" {
				t.Fatal(got)
			}
			old := time.Now().Add(-2*time.Hour - buildReconciliationGrace - time.Minute)
			m.state.Builds[0].DispatchedAt = &old
			f.runs[0].Status = "success"
			registry.err = nil
			registry.artifact = buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("d", 64), Revision: b.SourceSHA}
			switch mode {
			case "failure":
				f.runs[0].Status = "failure"
			case "listing-error":
				f.listError = "all"
			case "page-cap":
				// Terminal unrelated runs fill the listing before our match.
				for i := 0; i < 501; i++ {
					r := f.runs[0]
					r.ID = int64(i + 10)
					r.Created = r.Created.Add(time.Minute)
					r.EventPayload = dispatchPayload(b.SourceSHA, "other")
					f.runs = append(f.runs, r)
				}
			}
			tick(t, s)
			got := getBuild(t, s, b)
			switch mode {
			case "success":
				if got.Status != "SUCCEEDED" || got.FailureCode != "" || len(got.Images) != 1 || got.Images[0].Digest != registry.artifact.Digest {
					t.Fatal(got)
				}
			case "failure":
				if got.Status != "FAILED" || got.FailureCode != "RUN_FAILED" {
					t.Fatal(got)
				}
			default:
				if got.Status != "RECONCILIATION_REQUIRED" || !got.ProviderUnavailable || got.FinishedAt != nil {
					t.Fatal(got)
				}
			}
			if mode == "success" || mode == "failure" {
				if got.ProviderRunID != 1 || got.RunStatus != mode || got.FinishedAt == nil {
					t.Fatalf("terminal evidence lost: %+v", got)
				}
			}
			if f.dispatches != 1 {
				t.Fatal("lost dispatch was retried")
			}
		})
	}
}

func TestBuildReconciliationAttention(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	b := queue(t, s, "")
	old := time.Now().Add(-time.Hour)
	m.state.Builds[0].Status = "RECONCILIATION_REQUIRED"
	m.state.Builds[0].FailureCode = "RUN_SOURCE_MISMATCH"
	m.state.Builds[0].DispatchedAt = &old
	result, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.([]map[string]any) {
		if item["id"] == b.ID {
			found = true
			if item["profile_id"] != "app" || item["failure_code"] != "RUN_SOURCE_MISMATCH" || item["age_seconds"].(int64) < 3600 {
				t.Fatal(item)
			}
		}
	}
	if !found {
		t.Fatal(result)
	}
	for i := 0; i < 110; i++ {
		copy := m.state.Builds[0]
		copy.ID = fmt.Sprintf("extra-%03d", i)
		m.state.Builds = append(m.state.Builds, copy)
	}
	foreign := m.state.Builds[0]
	foreign.ID = "foreign"
	foreign.ProductID = "other"
	m.state.Builds = append(m.state.Builds, foreign)
	items := buildReconciliationAttention(m.state, "p", time.Now())
	if len(items) != 100 {
		t.Fatal(len(items))
	}
	for _, item := range items {
		if item["id"] == "foreign" {
			t.Fatal("scope leaked")
		}
	}
}

func TestBuildChangedAuthorityReconciliation(t *testing.T) {
	for _, status := range []string{"success", "failure", "cancelled", "skipped", "blocked", "waiting", "running", "missing", "no-id", "no-profile", "no-connection", "changed-connection", "queued-time", "no-id-before-grace", "get-error-before-grace", "missing-connection-id", "missing-connection-api-url", "changed-api-url", "unknown"} {
		t.Run(status, func(t *testing.T) {
			s, m, f, registry := buildFixture(t)
			b := queue(t, s, "old")
			if b.ConnectionID != "forgejo" || b.ConnectionAPIURL != "https://git.example" {
				t.Fatal("queue did not freeze provider identity", b)
			}
			tick(t, s)
			m.state.Builds[0].Status = "RUNNING"
			cfg := buildConfig()
			cfg.TimeoutSeconds = 60
			if err := s.ConfigureBuilds(cfg, f, registry); err != nil {
				t.Fatal(err)
			}
			tick(t, s)
			if got := getBuild(t, s, b); got.Status != "RECONCILIATION_REQUIRED" || got.FailureCode != "BUILD_AUTHORITY_CHANGED" {
				t.Fatal(got)
			}
			old := time.Now().Add(-time.Hour - buildReconciliationGrace)
			m.state.Builds[0].DispatchedAt = &old
			f.runs[0].Status = status
			finished := time.Now().UTC().Add(-time.Minute)
			f.runs[0].Stopped = finished
			terminal := true
			switch status {
			case "waiting", "running", "unknown":
				terminal = false
			case "no-id-before-grace", "get-error-before-grace":
				terminal = false
				recent := time.Now().UTC()
				m.state.Builds[0].DispatchedAt = &recent
				if status == "no-id-before-grace" {
					m.state.Builds[0].ProviderRunID = 0
				} else {
					f.runs = nil
				}
			case "missing-connection-id":
				m.state.Builds[0].ConnectionID = ""
			case "missing":
				terminal = false
				f.runs = nil
			case "missing-connection-api-url":
				m.state.Builds[0].ConnectionAPIURL = ""
			case "changed-api-url":
				s.builds.config.Connections[0].APIURL = "https://other.example"
				f.runs[0].Status = "success"
			case "no-id":
				m.state.Builds[0].ProviderRunID = 0
			case "no-profile":
				s.builds.config.Profiles = nil
			case "no-connection":
				s.builds.config.Connections = nil
			case "changed-connection":
				s.builds.config.Profiles[0].ConnectionID = "other"
			case "queued-time":
				m.state.Builds[0].DispatchedAt = nil
				m.state.Builds[0].QueuedAt = old
				m.state.Builds[0].ProviderRunID = 0
			}
			calls := f.runCalls
			// An old-authority success must not even attempt artifact resolution.
			registry.err = nil
			registry.artifact.Revision = b.SourceSHA
			tick(t, s)
			got := getBuild(t, s, b)
			if got.FailureCode != "BUILD_AUTHORITY_CHANGED" || len(got.Images) != 0 || f.dispatches != 1 {
				t.Fatal(got, f.dispatches)
			}
			if status == "missing" || status == "get-error-before-grace" {
				if !got.ProviderUnavailable || got.LastError != "Forgejo run listing unavailable" || got.FinishedAt != nil {
					t.Fatal("provider failure lost reconciliation state", got)
				}
			}
			if status == "changed-api-url" || status == "missing-connection-api-url" || status == "missing-connection-id" {
				if f.runCalls != calls {
					t.Fatal("consumed evidence without matching provider identity")
				}
			}
			if terminal {
				if got.Status != "FAILED" || got.FinishedAt == nil || buildActive(got) {
					t.Fatal(got)
				}
			} else if got.Status != "RECONCILIATION_REQUIRED" || !buildActive(got) {
				t.Fatal(got)
			}
			if status == "success" {
				if !got.FinishedAt.Equal(finished) || got.RunStatus != status || got.ProviderRunID != 1 || got.RunURL != "https://git/run" {
					t.Fatal(got)
				}
				registry.err = buildregistry.ErrMissing
				f.head = strings.Repeat("c", 40)
				next := queue(t, s, "next")
				tick(t, s)
				if getBuild(t, s, next).Status != "DISPATCHED" || f.dispatches != 2 {
					t.Fatal("pool was not released")
				}
			}
			tick(t, s)
			if status != "success" && f.dispatches != 1 {
				t.Fatal("old build redispatched")
			}
			if status == "missing" {
				f.runs = []forgejo.Run{{ID: got.ProviderRunID, Status: "running"}}
				tick(t, s)
				recovered := getBuild(t, s, b)
				if recovered.Status != "RECONCILIATION_REQUIRED" || recovered.ProviderUnavailable || recovered.LastError != "" || recovered.RunStatus != "running" {
					t.Fatal("provider recovery did not clear unavailability", recovered)
				}
			}
		})
	}
}
