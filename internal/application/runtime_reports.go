package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"sort"
	"time"
)

func (s *Service) ReportRuntime(ctx context.Context, r rr.Report) (any, error) {
	if p, ok := auth.FromContext(ctx); ok && p.ID != "service/prod-runtime-reporter" {
		if err := authorize(ctx, "write", r.ProductID); err != nil {
			return nil, err
		}
	}
	if err := rr.Validate(r, time.Now()); err != nil {
		return nil, invalid("%s", err)
	}
	store, ok := s.store.(rr.Store)
	if !ok {
		return nil, invalid("runtime report storage unavailable")
	}
	changes, err := store.SaveRuntimeReport(ctx, r, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if len(changes.Changes) > 0 || changes.Additional > 0 || changes.BudgetExceeded {
		err = s.updateWorkerScoped(ctx, domain.StateMutationScope{}, func(st *domain.State) error {
			for _, c := range changes.Changes {
				data := map[string]any{"environment_id": r.EnvironmentID, "cluster": r.Cluster, "namespace": c.New.Namespace, "kind": c.New.Kind, "name": c.New.Name, "old": reportEventSignal(c.Old), "new": reportEventSignal(&c.New)}
				st.Events = append(st.Events, domain.Event{ID: id(), Action: "runtime_report_changed", Actor: attributedActor(ctx, "agent"), ProductID: r.ProductID, At: time.Now().UTC(), Data: domain.Command{Action: "runtime_report_changed", ProductID: r.ProductID, Data: data}})
			}
			if changes.Additional > 0 {
				st.Events = append(st.Events, domain.Event{ID: id(), Action: "runtime_report_changed", Actor: attributedActor(ctx, "agent"), ProductID: r.ProductID, At: time.Now().UTC(), Data: domain.Command{Action: "runtime_report_changed", ProductID: r.ProductID, Data: map[string]any{"environment_id": r.EnvironmentID, "cluster": r.Cluster, "additional_changes": changes.Additional}}})
			}
			if changes.BudgetExceeded {
				st.Events = append(st.Events, domain.Event{ID: id(), Action: "runtime_report_budget_exceeded", Actor: attributedActor(ctx, "agent"), ProductID: r.ProductID, At: time.Now().UTC(), Data: domain.Command{Action: "runtime_report_budget_exceeded", ProductID: r.ProductID, Data: map[string]any{"environment_id": r.EnvironmentID, "cluster": r.Cluster, "suppressed_changes": changes.SuppressedChanges, "suppressed_reports": changes.SuppressedReports}}})
			}
			return nil
		})
	}
	return map[string]any{"accepted": true, "workloads": len(r.Workloads), "changes": changes.Total}, err
}
func (s *Service) GetRuntimeReport(ctx context.Context, p, e, c string, pages ...rr.PageRequest) (any, error) {
	if p == "" || e == "" {
		return nil, invalid("product_id and environment_id required")
	}
	if err := authorize(ctx, "read", p); err != nil {
		return nil, err
	}
	store, ok := s.store.(rr.Store)
	if !ok {
		return nil, invalid("runtime report storage unavailable")
	}
	page := rr.PageRequest{Limit: 500}
	if len(pages) > 0 {
		page = pages[0]
		if page.Limit == 0 {
			page.Limit = 500
		}
	}
	if page.Limit < 1 || page.Limit > 1000 {
		return nil, invalid("limit must be 1..1000")
	}
	type cursor struct{ Product, Environment, Cluster, After string }
	after := ""
	if len(page.Cursor) > 3000 {
		return nil, invalid("invalid runtime report cursor")
	}
	if page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page.Cursor)
		var cur cursor
		if err != nil || len(raw) > 2048 || json.Unmarshal(raw, &cur) != nil || cur.Product != p || cur.Environment != e || cur.Cluster != c || cur.After == "" {
			return nil, invalid("invalid runtime report cursor")
		}
		after = cur.After
	}
	clusters, err := store.ReadRuntimeReports(ctx, p, e, c, rr.ReadWindow{After: after, Limit: page.Limit + 1})
	if err != nil {
		return nil, err
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Cluster < clusters[j].Cluster })
	out := []rr.Cluster{}
	count := 0
	last := ""
	more := false
	for _, cluster := range clusters {
		sort.Slice(cluster.Records, func(i, j int) bool { return rr.Key(cluster.Records[i].Document) < rr.Key(cluster.Records[j].Document) })
		records := cluster.Records
		cluster.Records = []rr.Record{}
		// Empty clusters still consume a page slot and remain discoverable.
		if len(records) == 0 {
			key := cluster.Cluster + "!"
			if key > after {
				if count == page.Limit {
					more = true
					break
				}
				count++
				last = key
				out = append(out, cluster)
			}
			continue
		}
		for _, rec := range records {
			key := cluster.Cluster + "!" + rr.Key(rec.Document)
			if key <= after {
				continue
			}
			if count == page.Limit {
				more = true
				break
			}
			cluster.Records = append(cluster.Records, rec)
			count++
			last = key
		}
		if len(cluster.Records) > 0 {
			out = append(out, cluster)
		}
		if more {
			break
		}
	}
	next := ""
	if more {
		raw, err := json.Marshal(cursor{p, e, c, last})
		if err != nil {
			return nil, fmt.Errorf("cursor: %w", err)
		}
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	clusters = out
	for i := range clusters {
		clusters[i] = rr.Fresh(clusters[i], time.Now())
	}
	return map[string]any{"clusters": clusters, "pagination": map[string]any{"limit": page.Limit, "returned": count, "has_more": more, "next_cursor": next}}, err
}

// Keep journal entries small: the full snapshot belongs only in report storage.
func reportEventSignal(w *rr.Workload) any {
	if w == nil {
		return nil
	}
	containers := make([]map[string]any, 0, len(w.Containers))
	for _, c := range w.Containers {
		containers = append(containers, map[string]any{"name": c.Name, "digest": c.ImageDigest, "ready_pods": c.ReadyPods, "total_pods": c.TotalPods})
	}
	return map[string]any{"absent": w.Absent, "ready_replicas": w.ReadyReplicas, "available_replicas": w.AvailableReplicas, "containers": containers}
}
