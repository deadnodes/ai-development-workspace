package runtimereport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixture(at time.Time) Report {
	return Report{ProductID: "p", EnvironmentID: "prod", Cluster: "cluster", ReportedAt: at, Workloads: []Workload{{Namespace: "app", Kind: "Deployment", Name: "api", Generation: 1, Replicas: 1, ReadyReplicas: 1, Containers: []Container{{Name: "api", Image: "repo:v1", ImageDigest: "sha256:" + strings.Repeat("a", 64), ReadyPods: 1, TotalPods: 1}}}}}
}
func TestValidationBounds(t *testing.T) {
	now := time.Now()
	for _, change := range []func(*Report){func(r *Report) { r.Cluster = "INVALID" }, func(r *Report) { r.Cluster = strings.Repeat("a", 65) }, func(r *Report) { r.ReportedAt = now.Add(6 * time.Minute) }, func(r *Report) { r.Workloads = make([]Workload, 1001) }, func(r *Report) { r.Workloads[0].Containers = make([]Container, 21) }, func(r *Report) { r.Workloads[0].Name = strings.Repeat("a", 254) }, func(r *Report) { r.Workloads[0].Kind = "Pod" }, func(r *Report) { r.Workloads[0].Containers[0].ImageDigest = "sha256:bad" }, func(r *Report) { r.Workloads[0].ReadyReplicas = -1 }, func(r *Report) { r.Workloads = append(r.Workloads, r.Workloads[0]) }} {
		r := fixture(now)
		change(&r)
		if Validate(r, now) == nil {
			t.Fatal("accepted invalid report", r)
		}
	}
	if err := Validate(fixture(now), now); err != nil {
		t.Fatal(err)
	}
}
func TestMemorySemantics(t *testing.T) {
	m := &MemoryStore{}
	ctx := context.Background()
	at := time.Now().UTC()
	r := fixture(at)
	c, e := m.SaveRuntimeReport(ctx, r, at)
	if e != nil || len(c.Changes) != 1 {
		t.Fatal(c, e)
	}
	if _, e = m.SaveRuntimeReport(ctx, r, at.Add(time.Second)); !errors.Is(e, ErrRateLimit) {
		t.Fatal(e)
	}
	r.ReportedAt = at.Add(time.Minute)
	c, e = m.SaveRuntimeReport(ctx, r, r.ReportedAt)
	if e != nil || len(c.Changes) != 0 {
		t.Fatal(c, e)
	}
	v, _ := m.ReadRuntimeReports(ctx, "p", "prod", "")
	if !v[0].Records[0].ChangedAt.Equal(at) || !v[0].Records[0].ObservedAt.Equal(r.ReportedAt) {
		t.Fatal(v)
	}
	r.ReportedAt = at.Add(2 * time.Minute)
	r.Workloads = nil
	c, e = m.SaveRuntimeReport(ctx, r, r.ReportedAt)
	if e != nil || len(c.Changes) != 1 || !c.Changes[0].New.Absent {
		t.Fatal(c, e)
	}
	r.ReportedAt = at.Add(3 * time.Minute)
	c, e = m.SaveRuntimeReport(ctx, r, r.ReportedAt)
	if e != nil || len(c.Changes) != 0 {
		t.Fatal(c, e)
	}
	v, _ = m.ReadRuntimeReports(ctx, "p", "prod", "")
	if !v[0].Records[0].ChangedAt.Equal(at.Add(2 * time.Minute)) {
		t.Fatal(v)
	}
	if !Fresh(v[0], at.Add(9*time.Minute)).Stale {
		t.Fatal("not stale")
	}
}

func TestRuntimeScopeParsing(t *testing.T) {
	for _, raw := range []string{"p:prod", "p:prod:", "p:prod:c,", "p:prod:c:extra", "p:prod:BAD"} {
		if _, err := ParseScopes(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	scopes, err := ParseScopes("p:prod:c, p:prod:other")
	if err != nil || !scopes["p:prod:c"] || scopes["p:prod:unknown"] {
		t.Fatal(scopes, err)
	}
}
func TestMemoryRuntimeClusterRotation(t *testing.T) {
	m := &MemoryStore{}
	ctx := context.Background()
	at := time.Now()
	r := fixture(at)
	if _, err := m.SaveRuntimeReport(ctx, r, at); err != nil {
		t.Fatal(err)
	}
	r.Cluster = "other"
	r.ReportedAt = at.Add(time.Second)
	if _, err := m.SaveRuntimeReport(ctx, r, at.Add(19*time.Second)); !errors.Is(err, ErrRateLimit) {
		t.Fatal(err)
	}
	if got, _ := m.ReadRuntimeReports(ctx, "p", "prod", "other"); len(got) != 0 {
		t.Fatal("rejection mutated store")
	}
	if _, err := m.SaveRuntimeReport(ctx, r, at.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
}
func TestMemoryRuntimeTombstoneExpiry(t *testing.T) {
	m := &MemoryStore{}
	ctx := context.Background()
	at := time.Now()
	r := fixture(at)
	save := func(now time.Time) {
		t.Helper()
		r.ReportedAt = now
		if _, err := m.SaveRuntimeReport(ctx, r, now); err != nil {
			t.Fatal(err)
		}
	}
	save(at)
	r.Workloads = nil
	absent := at.Add(time.Minute)
	save(absent)
	save(absent.Add(6 * 24 * time.Hour))
	got, _ := m.ReadRuntimeReports(ctx, "p", "prod", "")
	if len(got[0].Records) != 1 || !got[0].Records[0].AbsentSince.Equal(absent) {
		t.Fatal(got)
	}
	save(absent.Add(7*24*time.Hour + time.Second))
	got, _ = m.ReadRuntimeReports(ctx, "p", "prod", "")
	if len(got[0].Records) != 0 {
		t.Fatal("expired tombstone retained", got)
	}
}
func TestMemoryRuntimeCap(t *testing.T) {
	m := &MemoryStore{}
	ctx := context.Background()
	at := time.Now()
	r := fixture(at)
	for batch := 0; batch < 4; batch++ {
		r.Workloads = nil
		r.ReportedAt = at.Add(time.Duration(batch) * time.Minute)
		for i := 0; i < 1000; i++ {
			r.Workloads = append(r.Workloads, Workload{Namespace: "app", Kind: "Deployment", Name: fmt.Sprintf("w-%d-%04d", batch, i)})
		}
		if _, err := m.SaveRuntimeReport(ctx, r, r.ReportedAt); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := m.ReadRuntimeReports(ctx, "p", "prod", "")
	if len(got[0].Records) != 2000 {
		t.Fatal(len(got[0].Records))
	}
	for _, rec := range got[0].Records {
		if strings.HasPrefix(rec.Document.Name, "w-0-") || strings.HasPrefix(rec.Document.Name, "w-1-") {
			t.Fatal("oldest absence retained")
		}
	}
	// Direct store callers also fail atomically if the active set alone exceeds the cap.
	before := m.snapshots[Scope("p", "prod", "cluster")]
	r.ReportedAt = r.ReportedAt.Add(time.Minute)
	for i := 0; i < 1001; i++ {
		r.Workloads = append(r.Workloads, Workload{Namespace: "app", Kind: "Deployment", Name: fmt.Sprintf("extra-%d", i)})
	}
	if _, err := m.SaveRuntimeReport(ctx, r, r.ReportedAt); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if !same(before, m.snapshots[Scope("p", "prod", "cluster")]) {
		t.Fatal("rejected report changed snapshot")
	}
}

func TestMemoryJournalRollingBudget(t *testing.T) {
	ctx := context.Background()
	at := time.Now().UTC().Add(-48 * time.Hour)
	m := &MemoryStore{}
	r := Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", Workloads: []Workload{{Namespace: "app", Kind: "Deployment", Name: "api"}}}
	for i := 0; i < 500; i++ {
		r.ReportedAt = at.Add(time.Duration(i) * 20 * time.Second)
		r.Workloads[0].ReadyReplicas = i
		got, err := m.SaveRuntimeReport(ctx, r, r.ReportedAt)
		if err != nil || len(got.Changes) != 1 || got.BudgetExceeded {
			t.Fatalf("%d: %+v %v", i, got, err)
		}
	}
	exhausted := r.ReportedAt.Add(20 * time.Second)
	for i := 0; i < 181; i++ {
		r.ReportedAt = exhausted.Add(time.Duration(i) * 20 * time.Second)
		r.Workloads[0].ReadyReplicas = 500 + i
		got, err := m.SaveRuntimeReport(ctx, r, r.ReportedAt)
		if err != nil || len(got.Changes) != 0 || got.Additional != 0 || got.BudgetExceeded != (i == 0 || i == 180) {
			t.Fatalf("suppression %d: %+v %v", i, got, err)
		}
		if got.BudgetExceeded && (got.SuppressedChanges != int64(max(1, i)) || got.SuppressedReports != int64(max(1, i))) {
			t.Fatal(got)
		}
	}
	clusters, err := m.ReadRuntimeReports(ctx, "p", "prod", "c")
	if err != nil || clusters[0].Records[0].Document.ReadyReplicas != 680 {
		t.Fatal(clusters, err)
	}
	// Another exact scope has an independent budget.
	r.Cluster = "other"
	r.ReportedAt = r.ReportedAt.Add(20 * time.Second)
	got, err := m.SaveRuntimeReport(ctx, r, r.ReportedAt)
	if err != nil || len(got.Changes) != 1 {
		t.Fatal(got, err)
	}
	r.Cluster = "c"
	r.ReportedAt = at.Add(24 * time.Hour)
	r.Workloads[0].ReadyReplicas++
	got, err = m.SaveRuntimeReport(ctx, r, r.ReportedAt)
	if err != nil || len(got.Changes) != 1 {
		t.Fatal("rolling boundary", got, err)
	}
}

func TestJournalCapAndPartialBudget(t *testing.T) {
	at := time.Now().UTC()
	changes := make([]Change, 1000)
	b, out := ReserveJournal(JournalBudget{}, changes, at)
	if len(out.Changes) != 20 || out.Additional != 980 || len(b.Events) != 21 {
		t.Fatal(out, len(b.Events))
	}
	full := JournalBudget{Events: make([]time.Time, 479)}
	for i := range full.Events {
		full.Events[i] = at
	}
	_, normal := ReserveJournal(full, changes, at)
	if normal.BudgetExceeded {
		t.Fatal("per-report overflow incorrectly counted as budget suppression")
	}
	b.Events = make([]time.Time, 499)
	for i := range b.Events {
		b.Events[i] = at
	}
	b, out = ReserveJournal(b, changes, at)
	if len(out.Changes) != 0 || out.Additional != 1000 || !out.BudgetExceeded || len(b.Events) != 500 {
		t.Fatal(out, len(b.Events))
	}
	_, out = ReserveJournal(b, nil, at.Add(time.Hour))
	if out.BudgetExceeded {
		t.Fatal("unchanged report warned")
	}
}
