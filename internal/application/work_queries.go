package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"releasecontrol/internal/domain"
)

type questionView struct {
	domain.Meta
	Title         string `json:"title"`
	Body          string `json:"body"`
	Status        string `json:"status"`
	IntegrationID string `json:"integration_id,omitempty"`
	domain.QuestionDetails
	Context            []questionReference `json:"context_refs"`
	AgeSeconds         int64               `json:"age_seconds"`
	BlockedExecutionID string              `json:"blocked_execution_id,omitempty"`
}

func presentQuestion(st *domain.State, memory domain.Memory, now time.Time, ctx context.Context) questionView {
	view := questionView{Meta: memory.Meta, Title: memory.Title, Body: memory.Body, Status: memory.Status, IntegrationID: memory.IntegrationID, QuestionDetails: *memory.Question, Context: []questionReference{}, AgeSeconds: max(0, int64(now.Sub(memory.CreatedAt)/time.Second))}
	for _, ref := range memory.Question.ContextRefs {
		if item, found := resolveQuestionReference(st, memory.ProductID, ref); found {
			view.Context = append(view.Context, item)
		}
	}
	for _, execution := range st.IntegrationExecutions {
		if execution.ProductID == memory.ProductID && execution.State == "blocked" && execution.BlockerRef == memory.ID && executionVisibleTo(ctx, integration(st, execution.IntegrationID)) {
			view.BlockedExecutionID = execution.ID
			break
		}
	}
	return view
}

type workQueryInput struct {
	ProductID       string `json:"product_id,omitempty"`
	FeatureID       string `json:"feature_id,omitempty"`
	Status          string `json:"status,omitempty"`
	AddressedTo     string `json:"addressed_to,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	IncludeProposed bool   `json:"include_proposed,omitempty"`
}

func (s *Service) queryWork(ctx context.Context, name, input string) (any, error) {
	if err := authorize(ctx, "read", ""); err != nil {
		return nil, err
	}
	var options workQueryInput
	if name != "get_question" && input != "" {
		if !strings.HasPrefix(input, "{") {
			options.ProductID = input
		} else if err := json.Unmarshal([]byte(input), &options); err != nil {
			return nil, invalid("invalid work query")
		}
	}
	if options.Limit < 0 || options.Limit > 100 {
		return nil, invalid("limit must be between 1 and 100")
	}
	if options.Limit == 0 {
		options.Limit = 25
	}
	var output any
	err := s.view(ctx, func(raw *domain.State) error {
		st, err := scopedState(ctx, *raw)
		if err != nil {
			return err
		}
		visible := func(productID string) bool { return productExists(&st, productID) }
		if options.ProductID != "" && !visible(options.ProductID) {
			return missing("product", options.ProductID)
		}
		if options.FeatureID != "" {
			parent := feature(&st, options.FeatureID)
			if parent == nil || options.ProductID != "" && parent.ProductID != options.ProductID {
				return missing("feature", options.FeatureID)
			}
		}
		user, now := workUser(ctx), nowUTC()
		switch name {
		case "get_question":
			question := questionByID(&st, input)
			if question == nil {
				return missing("question", input)
			}
			output = presentQuestion(&st, *question, now, ctx)
		case "list_questions":
			if options.Status == "" {
				options.Status = "open"
			}
			if !domain.ValidStatus("question", options.Status) {
				return invalid("invalid question status")
			}
			if options.AddressedTo == "" || options.AddressedTo == "me" {
				options.AddressedTo = user
			}
			cursorTime, cursorID, valid := parseExecutionEventCursor(options.Cursor)
			if options.Cursor != "" && !valid {
				return invalid("invalid question cursor")
			}
			rows := []domain.Memory{}
			for _, memory := range st.Memories {
				if memory.Kind == "question" && memory.Question != nil && memory.Status == options.Status && memory.Question.AddressedTo == options.AddressedTo && (options.ProductID == "" || memory.ProductID == options.ProductID) && (options.FeatureID == "" || memory.FeatureID == options.FeatureID) && (memory.CreatedAt.After(cursorTime) || memory.CreatedAt.Equal(cursorTime) && memory.ID > cursorID) {
					if memory.Status == "open" && memory.Question.DecisionFor == "idea_promotion" && !decisionMatches(&memory, feature(&st, memory.FeatureID), nil) {
						continue
					}
					rows = append(rows, memory)
				}
			}
			sort.Slice(rows, func(left, right int) bool {
				if rows[left].CreatedAt.Equal(rows[right].CreatedAt) {
					return rows[left].ID < rows[right].ID
				}
				return rows[left].CreatedAt.Before(rows[right].CreatedAt)
			})
			next := ""
			if len(rows) > options.Limit {
				rows = rows[:options.Limit]
				last := rows[len(rows)-1]
				next = last.CreatedAt.Format(time.RFC3339Nano) + "|" + last.ID
			}
			items := make([]questionView, 0, len(rows))
			for _, row := range rows {
				items = append(items, presentQuestion(&st, row, now, ctx))
			}
			output = map[string]any{"items": items, "next_cursor": next}
		case "get_dependency_graph":
			if options.ProductID == "" {
				return invalid("product_id is required")
			}
			output = dependencyGraph(raw, options, visible)
		case "next_work":
			items, skipped := []workItem{}, []workItem{}
			if user != "" {
				items, skipped = nextWork(raw, user, options.ProductID, visible, options.Limit)
			}
			output = map[string]any{"product_id": options.ProductID, "items": items, "skipped": skipped}
		case "status_overview":
			output = statusOverview(&st, user, options.ProductID, now)
		default:
			return fmt.Errorf("unknown work query: %s", name)
		}
		return nil
	})
	return output, err
}

func dependencyGraph(st *domain.State, options workQueryInput, visible func(string) bool) any {
	selected := map[string]bool{}
	edges := []domain.FeatureDependency{}
	for _, edge := range st.FeatureDependencies {
		from, to := feature(st, edge.FromFeatureID), feature(st, edge.ToFeatureID)
		if from == nil || to == nil || !visible(from.ProductID) || !visible(to.ProductID) || from.ProductID != options.ProductID && to.ProductID != options.ProductID || edge.Status != "accepted" && !(options.IncludeProposed && edge.Status == "proposed") {
			continue
		}
		selected[from.ID], selected[to.ID] = true, true
		edges = append(edges, edge)
	}
	nodes := []map[string]any{}
	for _, parent := range st.Features {
		if parent.ProductID == options.ProductID || selected[parent.ID] {
			waiting, unblocked := featureWaitingOn(st, parent.ID, visible)
			nodes = append(nodes, map[string]any{"id": parent.ID, "product_id": parent.ProductID, "title": parent.Title, "status": parent.Status, "unblocked": unblocked, "waiting_on": waiting})
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

func statusOverview(st *domain.State, user, productID string, now time.Time) any {
	products := []map[string]any{}
	for _, product := range st.Products {
		if productID != "" && product.ID != productID {
			continue
		}
		questions, ideas, ready, completed := []map[string]any{}, []map[string]any{}, []map[string]any{}, []map[string]any{}
		counts := map[string]int{"open_questions": 0, "ideas": 0, "ready_for_execution": 0, "recently_completed": 0, "running": 0, "stalled": 0, "lost": 0, "blocked": 0}
		add := func(key string, list *[]map[string]any, row map[string]any) {
			counts[key]++
			if len(*list) < 10 {
				*list = append(*list, row)
			}
		}
		for _, memory := range st.Memories {
			if memory.ProductID == product.ID && memory.Kind == "question" && memory.Status == "open" && memory.Question != nil && memory.Question.AddressedTo == user {
				add("open_questions", &questions, map[string]any{"id": memory.ID, "title": contextExcerpt(memory.Title, 120), "blocking": memory.Question.Blocking})
			}
		}
		for _, parent := range st.Features {
			if parent.ProductID != product.ID {
				continue
			}
			row := map[string]any{"id": parent.ID, "title": contextExcerpt(parent.Title, 120)}
			switch parent.Status {
			case "idea":
				add("ideas", &ideas, row)
			case "ready_for_execution":
				add("ready_for_execution", &ready, row)
			case "completed":
				completedAt := parent.UpdatedAt
				if parent.CompletedAt != nil {
					completedAt = *parent.CompletedAt
				}
				if !completedAt.Before(now.AddDate(0, 0, -7)) {
					add("recently_completed", &completed, row)
				}
			}
		}
		executions := []map[string]any{}
		for _, execution := range st.IntegrationExecutions {
			if execution.ProductID != product.ID || user == "" {
				continue
			}
			child := integration(st, execution.IntegrationID)
			if child == nil || child.Assignee != user {
				continue
			}
			switch execution.State {
			case "running", "stalled", "lost", "blocked":
				add(execution.State, &executions, map[string]any{"id": execution.ID, "integration_id": child.ID, "title": contextExcerpt(child.Title, 120), "state": execution.State})
			}
		}
		products = append(products, map[string]any{"product_id": product.ID, "name": product.Name, "counts": counts, "open_questions": questions, "ideas": ideas, "ready_for_execution": ready, "executions": executions, "recently_completed": completed})
	}
	return map[string]any{"products": products}
}
