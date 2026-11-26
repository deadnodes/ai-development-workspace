package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
)

func TestAdvanceBuildAdmissionRejectsActiveSameBranch(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	queued := queue(t, s, "queued")
	active := queued
	active.ID = "active"
	active.Status = "RUNNING"
	active.Branch = queued.Branch
	active.UpdatedAt = time.Now().UTC()
	m.state.Builds = append(m.state.Builds, active)
	next := queued
	next.Status = "DISPATCHED"
	if err := s.advanceBuild(context.Background(), queued, next, true); !errors.Is(err, errBuildBranchActive) {
		t.Fatalf("expected branch admission sentinel, got %v", err)
	}
}

func TestHeldBuildReadHintAndRegistryReuse(t *testing.T) {
	s, m, _, registry := buildFixture(t)
	active := queue(t, s, "active")
	m.state.Builds[0].Status = "RUNNING"
	queued := active
	queued.ID, queued.RequestKey, queued.Status = "held", "held", "QUEUED"
	queued.CreatedAt, queued.QueuedAt, queued.UpdatedAt = time.Now().UTC(), time.Now().UTC(), time.Now().UTC()
	m.state.Builds = append(m.state.Builds, queued)
	got, err := s.GetBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: queued.ID})
	if err != nil || got.WaitingForBuildID != active.ID {
		t.Fatalf("get hint: %+v %v", got, err)
	}
	listed, err := s.ListBuilds(context.Background(), domain.ListBuildsRequest{ProductID: "p"})
	if err != nil || len(listed.Items) != 2 {
		t.Fatalf("list: %+v %v", listed, err)
	}
	stage := buildPipelineStage(got, false, false)
	if stage.Status != "pending" || stage.Label != "Waiting for the running build of this branch" || stage.Hint != "Waiting for build "+active.ID+"; newer merges replace this build while it waits." {
		t.Fatalf("pipeline stage: %+v", stage)
	}
	pipelines, err := s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "p"})
	if err != nil || len(pipelines.Items) != 2 {
		t.Fatalf("pipelines: %+v %v", pipelines, err)
	}
	for _, item := range pipelines.Items {
		if item.Build.ID == queued.ID && (item.Build.WaitingForBuildID != active.ID || item.Stages[0].Hint != stage.Hint) {
			t.Fatalf("pipeline read hint missing: %+v", item)
		}
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	registry.err = nil
	registry.artifact = buildregistry.Artifact{Revision: queued.SourceSHA, Digest: digest}
	// One configured image is enough for this fixture's registry adapter.
	if err := s.DispatchBuildsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range m.state.Builds {
		if b.ID == queued.ID && (b.Status != "SUCCEEDED" || b.Reused != "registry") {
			t.Fatalf("held build was not completed from registry: %+v", b)
		}
	}
}

func TestBranchQueueSupersedesDivergedHead(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	old := queue(t, s, "old")
	f.head = strings.Repeat("d", 40) // deliberately unrelated history
	f.ancestorFunc = func(context.Context, string, string) (bool, error) { return false, nil }
	newest := queue(t, s, "new")
	got := m.state.Builds[0]
	if got.Status != "CANCELLED" || got.SupersededBy != newest.ID || got.FailureCode != "SUPERSEDED" {
		t.Fatalf("diverged branch intent was not coalesced: %+v (old %s)", got, old.ID)
	}
	for _, event := range m.state.Events {
		if event.Action == "supersede_build" && event.EntityID == old.ID && event.Data.Data["reason"] != "branch_newer_queued" {
			t.Fatalf("missing branch reason: %+v", event.Data.Data)
		}
	}
}

func TestSHAAdmissionRemainsPoolParallel(t *testing.T) {
	s, m, f, r := buildFixture(t)
	cfg := buildConfig()
	cfg.Pools[0].MaxInFlight = 3
	if err := s.ConfigureBuilds(cfg, f, r); err != nil {
		t.Fatal(err)
	}
	first, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: strings.Repeat("a", 40), RequestKey: "sha-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: strings.Repeat("b", 40), RequestKey: "sha-b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []domain.Build{first, second} {
		n := b
		n.Status = "DISPATCHED"
		if err := s.advanceBuild(context.Background(), b, n, true); err != nil {
			t.Fatalf("SHA admission refused: %v", err)
		}
	}
	if m.state.Builds[0].Status != "DISPATCHED" || m.state.Builds[1].Status != "DISPATCHED" {
		t.Fatal(m.state.Builds)
	}
}

func TestDispatchCoalescesBranchQueueWithoutBlockingSharedPool(t *testing.T) {
	s, m, f, r := buildFixture(t)
	cfg := buildConfig()
	cfg.Pools[0].MaxInFlight = 3
	other := cfg.Profiles[0]
	other.ID = "other"
	other.Repository = "o/other"
	other.Images = []buildconfig.Image{{ID: "app", Repository: "o/other"}}
	cfg.Profiles = append(cfg.Profiles, other)
	if err := s.ConfigureBuilds(cfg, f, r); err != nil {
		t.Fatal(err)
	}
	a := queue(t, s, "A")
	m.state.Builds[0].Status = "RUNNING"
	base := time.Now().UTC()
	for i, id := range []string{"Z", "Y", "D"} {
		b := copyBuild(a)
		b.ID, b.RequestKey = id, id
		b.Status, b.FailureCode, b.SupersededBy = "QUEUED", "", ""
		b.SourceSHA = strings.Repeat(string(rune('b'+i)), 40)
		b.QueuedAt, b.CreatedAt, b.UpdatedAt = base, base, base
		m.state.Builds = append(m.state.Builds, b)
	}
	f.head = strings.Repeat("e", 40)
	otherBuild, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "other", RequestKey: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DispatchBuildsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.dispatches != 1 || getBuild(t, s, otherBuild).Status != "DISPATCHED" {
		t.Fatalf("held branch blocked another profile: dispatches=%d other=%+v", f.dispatches, getBuild(t, s, otherBuild))
	}
	for i := 1; i <= 2; i++ {
		b := m.state.Builds[i]
		if b.Status != "CANCELLED" || b.SupersededBy != "D" || b.FailureCode != "SUPERSEDED" {
			t.Fatalf("older queued build not coalesced: %+v", b)
		}
	}
	for _, event := range m.state.Events {
		if (event.EntityID == "Z" || event.EntityID == "Y") && event.Action == "supersede_build" && event.Data.Data["reason"] != "branch_newer_queued" {
			t.Fatalf("missing reason: %+v", event.Data.Data)
		}
	}
	m.state.Builds[0].Status = "FAILED"
	f.runs[0].Status = "failure" // release the other profile's simulated provider slot
	if err := s.DispatchBuildsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if getBuild(t, s, domain.Build{Meta: domain.Meta{ID: "D", ProductID: "p"}}).Status != "DISPATCHED" {
		t.Fatalf("newest build did not dispatch after A ended: %+v", m.state.Builds)
	}
}
