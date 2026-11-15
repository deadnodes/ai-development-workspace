package application

import (
	"context"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"testing"
	"time"
)

type reportMemory struct {
	rr.MemoryStore
	state  domain.State
	now    time.Time
	scopes []domain.StateMutationScope
}

func (m *reportMemory) Read(context.Context) (domain.State, error) { return m.state, nil }
func (m *reportMemory) Update(context.Context, func(*domain.State) error) error {
	panic("report must use scoped update")
}
func (m *reportMemory) UpdateScoped(_ context.Context, s domain.StateMutationScope, f func(*domain.State) error) error {
	m.scopes = append(m.scopes, s)
	return f(&m.state)
}
func (m *reportMemory) SaveRuntimeReport(ctx context.Context, r rr.Report, _ time.Time) (rr.JournalResult, error) {
	m.now = m.now.Add(time.Minute)
	return m.MemoryStore.SaveRuntimeReport(ctx, r, m.now)
}
func TestRuntimeReportChangeJournal(t *testing.T) {
	at := time.Now().UTC().Add(-10 * time.Minute)
	m := &reportMemory{now: at}
	s := New(m)
	ctx := context.Background()
	r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", ReportedAt: at, Workloads: []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api", Containers: []rr.Container{{Name: "api", Image: "v1"}}}}}
	for i := 0; i < 3; i++ {
		r.ReportedAt = at.Add(time.Duration(i) * time.Minute)
		if i == 2 {
			r.Workloads[0].Containers[0].Image = "v2"
		}
		if _, err := s.ReportRuntime(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.state.Events) != 2 || len(m.scopes) != 2 {
		t.Fatal(m.state.Events, m.scopes)
	}
	for _, scope := range m.scopes {
		if scope.AllCollections || len(scope.MutableCollections) > 0 || len(scope.AppendCollections) > 0 {
			t.Fatal(scope)
		}
	}
	restricted := auth.WithPrincipal(ctx, auth.Principal{ID: "agent/reader", Permissions: []string{"read"}, Products: []string{"other"}})
	if _, err := s.GetRuntimeReport(restricted, "p", "prod", ""); err != auth.ErrForbidden {
		t.Fatal(err)
	}
}
func TestRuntimeReportEventCap(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	m := &reportMemory{now: at}
	s := New(m)
	r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", ReportedAt: at}
	for i := 0; i < 102; i++ {
		r.Workloads = append(r.Workloads, rr.Workload{Namespace: "app", Kind: "Deployment", Name: fmt.Sprintf("api-%d", i)})
	}
	if _, err := s.ReportRuntime(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Events) != 21 || m.state.Events[20].Data.Data["additional_changes"] != 82 {
		t.Fatal(len(m.state.Events))
	}
}

func TestRuntimeReportPagination(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	m := &reportMemory{now: at}
	s := New(m)
	r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", ReportedAt: at}
	for i := 0; i < 600; i++ {
		r.Workloads = append(r.Workloads, rr.Workload{Namespace: "app", Kind: "Deployment", Name: fmt.Sprintf("w-%04d", i)})
	}
	if _, err := s.ReportRuntime(ctx, r); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetRuntimeReport(ctx, "p", "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	page := v.(map[string]any)
	pagination := page["pagination"].(map[string]any)
	if pagination["returned"] != 500 || pagination["has_more"] != true {
		t.Fatal(pagination)
	}
	cursor := pagination["next_cursor"].(string)
	v, err = s.GetRuntimeReport(ctx, "p", "prod", "", rr.PageRequest{Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	pagination = v.(map[string]any)["pagination"].(map[string]any)
	if pagination["returned"] != 100 || pagination["has_more"] != false {
		t.Fatal(pagination)
	}
	for _, p := range []rr.PageRequest{{Limit: 1001}, {Limit: -1}, {Cursor: "bad"}} {
		if _, err := s.GetRuntimeReport(ctx, "p", "prod", "", p); err == nil {
			t.Fatal("invalid pagination accepted", p)
		}
	}
	if _, err := s.GetRuntimeReport(ctx, "p", "prod", "c", rr.PageRequest{Cursor: cursor}); err == nil {
		t.Fatal("cursor scope substitution")
	}
}

func TestRuntimeReportPaginationAcrossClusterPrefixes(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	m := &reportMemory{now: at}
	s := New(m)
	for _, name := range []string{"c", "c-other", "z"} {
		r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: name, ReportedAt: at}
		if name != "z" {
			r.Workloads = []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api"}}
		}
		if _, err := s.ReportRuntime(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cursor := ""
	for i, want := range []string{"c", "c-other", "z"} {
		v, err := s.GetRuntimeReport(ctx, "p", "prod", "", rr.PageRequest{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		page := v.(map[string]any)
		clusters := page["clusters"].([]rr.Cluster)
		if len(clusters) != 1 || clusters[0].Cluster != want {
			t.Fatal(clusters)
		}
		pagination := page["pagination"].(map[string]any)
		if pagination["has_more"] != (i < 2) {
			t.Fatal(pagination)
		}
		cursor = pagination["next_cursor"].(string)
	}
}

func TestRuntimeReportBudgetJournal(t *testing.T) {
	at := time.Now().UTC().Add(-48 * time.Hour)
	m := &reportMemory{now: at}
	s := New(m)
	r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", Workloads: []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api"}}}
	for i := 0; i < 562; i++ {
		r.ReportedAt = at.Add(time.Duration(i) * time.Minute)
		r.Workloads[0].ReadyReplicas = i
		v, err := s.ReportRuntime(context.Background(), r)
		if err != nil || v.(map[string]any)["changes"] != 1 {
			t.Fatal(i, v, err)
		}
	}
	changed, warnings := 0, 0
	for _, event := range m.state.Events {
		switch event.Action {
		case "runtime_report_changed":
			changed++
		case "runtime_report_budget_exceeded":
			warnings++
			want := int64(1)
			if warnings == 2 {
				want = 60
			}
			if event.Data.Data["suppressed_changes"] != want || event.Data.Data["suppressed_reports"] != want || event.Data.Data["cluster"] != "c" {
				t.Fatal(event)
			}
		default:
			t.Fatal(event)
		}
	}
	if changed != 500 || warnings != 2 {
		t.Fatal(changed, warnings)
	}
	clusters, err := m.ReadRuntimeReports(context.Background(), "p", "prod", "c")
	if err != nil || clusters[0].Records[0].Document.ReadyReplicas != 561 {
		t.Fatal(clusters, err)
	}
}
