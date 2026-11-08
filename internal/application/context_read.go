package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
)

type contextReader interface {
	ReadContext(context.Context, string, string) (domain.State, error)
}

func (s *Service) contextState(ctx context.Context, product, feature string) (domain.State, error) {
	if r, ok := s.store.(contextReader); ok {
		st, err := r.ReadContext(ctx, product, feature)
		if err != nil {
			return st, err
		}
		return scopedState(ctx, st)
	}
	return s.State(ctx)
}

// ResumeSummary is the bounded MCP entrypoint. Full operator/HTTP Resume remains
// separate, so history is never silently removed from the full context contract.
func (s *Service) ResumeSummary(ctx context.Context, featureID string) (any, error) {
	if err := authorize(ctx, "read", ""); err != nil {
		return nil, err
	}
	st, err := s.contextState(ctx, "", featureID)
	if err != nil {
		return nil, err
	}
	f := feature(&st, featureID)
	if f == nil {
		return nil, missing("feature", featureID)
	}
	if err = authorize(ctx, "read", f.ProductID); err != nil {
		return nil, err
	}
	integrations := []map[string]any{}
	memories := []map[string]any{}
	for _, i := range st.Integrations {
		if i.FeatureID != featureID {
			continue
		}
		integrations = append(integrations, map[string]any{"id": i.ID, "title": contextExcerpt(i.Title, 256), "status": i.Status, "objective": contextExcerpt(i.Objective, 600)})
	}
	integrationsTruncated := len(integrations) > 100
	if integrationsTruncated {
		integrations = integrations[:100]
	}
	for _, m := range st.Memories {
		if m.FeatureID != featureID {
			continue
		}
		memories = append(memories, map[string]any{"id": m.ID, "integration_id": m.IntegrationID, "kind": m.Kind, "title": contextExcerpt(m.Title, 256), "body": contextExcerpt(m.Body, 800), "current": contextExcerpt(m.Current, 400), "updated_at": m.UpdatedAt, "status": m.Status, "reason": contextExcerpt(m.Reason, 400), "completed": contextItems(m.Completed), "remaining": contextItems(m.Remaining), "next": contextItems(m.Next), "warnings": contextItems(m.Warnings)})
	}
	memoriesTruncated := len(memories) > 20
	if memoriesTruncated {
		memories = memories[len(memories)-20:]
	}
	out := map[string]any{"projection": "bounded_feature_summary", "feature": map[string]any{"id": f.ID, "product_id": f.ProductID, "title": contextExcerpt(f.Title, 256), "status": f.Status, "goal": contextExcerpt(f.Goal, 1200)}, "integrations": integrations, "memories": memories, "integrations_truncated": integrationsTruncated, "memories_truncated": memoriesTruncated, "text_excerpted": true, "guidance": "Bounded navigation and latest memory excerpts only; not full history or acceptance evidence. Search graph/text for relevant context; HTTP /api/features/{id}/context retains the full operator context."}
	// UTF-8 can consume four bytes per character. Enforce a hard response budget
	// after excerpts and report every omitted group explicitly.
	for {
		b, e := json.Marshal(out)
		if e != nil {
			return nil, e
		}
		if len(b) <= 32<<10 {
			break
		}
		if len(memories) > 0 {
			memories = memories[1:]
			out["memories"] = memories
			out["memories_truncated"] = true
			continue
		}
		if len(integrations) > 0 {
			integrations = integrations[:len(integrations)-1]
			out["integrations"] = integrations
			out["integrations_truncated"] = true
			continue
		}
		return nil, invalid("feature summary exceeds byte budget")
	}
	return out, nil
}
func contextExcerpt(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit])
	}
	return s
}

func contextItems(items []string) []string {
	out := []string{}
	for i, item := range items {
		if i == 5 {
			break
		}
		out = append(out, contextExcerpt(item, 256))
	}
	return out
}

// ProjectContextSummary is MCP navigation, not an export of all source documents.
func (s *Service) ProjectContextSummary(ctx context.Context, product string) (any, error) {
	if err := authorize(ctx, "read", product); err != nil {
		return nil, err
	}
	st, err := s.contextState(ctx, product, "")
	if err != nil {
		return nil, err
	}
	if !productExists(&st, product) {
		return nil, missing("product", product)
	}
	k := knowledgeFor(st, product)
	repos := []map[string]any{}
	apps := []map[string]any{}
	envs := []map[string]any{}
	nodes := []map[string]any{}
	for _, r := range st.Repositories {
		if r.ProductID == product && len(repos) < 50 {
			repos = append(repos, map[string]any{"id": r.ID, "name": contextExcerpt(r.Name, 256), "url": contextExcerpt(r.URL, 512), "role": r.Role})
		}
	}
	for _, a := range st.Applications {
		if a.ProductID == product && len(apps) < 50 {
			apps = append(apps, map[string]any{"id": a.ID, "name": contextExcerpt(a.Name, 256), "repository_id": a.RepositoryID, "kind": a.Kind})
		}
	}
	for _, e := range st.Environments {
		if e.ProductID == product && len(envs) < 25 {
			envs = append(envs, map[string]any{"id": e.ID, "name": contextExcerpt(e.Name, 256)})
		}
	}
	for _, n := range k.Nodes {
		if n.Status != "archived" && len(nodes) < 25 {
			nodes = append(nodes, map[string]any{"id": n.ID, "title": contextExcerpt(n.Title, 256), "kind": n.Kind, "area_id": n.AreaID})
		}
	}
	var p domain.Product
	for _, v := range st.Products {
		if v.ID == product {
			p = v
			break
		}
	}
	out := map[string]any{"projection": "bounded_project_summary", "product": map[string]any{"id": p.ID, "name": contextExcerpt(p.Name, 256), "description": contextExcerpt(p.Description, 600)}, "knowledge": map[string]any{"overview": contextExcerpt(k.Overview, 1600), "instructions": contextExcerpt(k.Instructions, 2000), "parameters": contextParameters(k.Parameters)}, "repositories": repos, "components": apps, "environments": envs, "knowledge_nodes": nodes, "text_excerpted": true, "documents_omitted": true, "guidance": "Navigation and excerpts only (32KiB maximum); use graph/text search to locate relevant facts, source documents, and exact evidence. Full operator context remains at HTTP /api/products/{id}/context."}
	for {
		b, e := json.Marshal(out)
		if e != nil {
			return nil, e
		}
		if len(b) <= 32<<10 {
			break
		}
		if len(nodes) > 0 {
			nodes = nodes[:len(nodes)-1]
			out["knowledge_nodes"] = nodes
			continue
		}
		if len(repos) > 0 {
			repos = repos[:len(repos)-1]
			out["repositories"] = repos
			continue
		}
		if len(apps) > 0 {
			apps = apps[:len(apps)-1]
			out["components"] = apps
			continue
		}
		if len(envs) > 0 {
			envs = envs[:len(envs)-1]
			out["environments"] = envs
			continue
		}
		return nil, invalid("project summary exceeds byte budget")
	}
	out["lists_limited"] = true
	return out, nil
}

func contextParameters(parameters map[string]string) map[string]string {
	out := map[string]string{}
	keys := []string{}
	for k := range parameters {
		if strings.HasPrefix(k, "control_plane.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	bytes := 0
	for _, k := range keys {
		key := contextExcerpt(k, 128)
		v := contextExcerpt(parameters[k], 512)
		if len(out) == 20 {
			break
		}
		if bytes+len(key)+len(v) > 4096 {
			continue
		}
		out[key] = v
		bytes += len(key) + len(v)
	}
	return out
}

// SearchKnowledgeSummary keeps the legacy knowledge-node search usable without
// copying whole documents into an agent conversation. HTTP remains explicit/full.
func (s *Service) SearchKnowledgeSummary(ctx context.Context, product, query string, limit int) (any, error) {
	if len(query) > 4096 {
		return nil, invalid("knowledge query exceeds 4096 bytes")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	full, err := s.SearchKnowledge(ctx, product, query, 100)
	if err != nil {
		return nil, err
	}
	result := full.(map[string]any)
	matched := result["nodes"].([]domain.KnowledgeNode)
	edges := result["edges"].([]domain.KnowledgeEdge)
	nodes := []map[string]any{}
	links := []map[string]any{}
	selected := map[string]bool{}
	for _, n := range matched {
		if len(nodes) == limit {
			break
		}
		nodes = append(nodes, map[string]any{"id": n.ID, "kind": n.Kind, "title": contextExcerpt(n.Title, 256), "area_id": n.AreaID, "snippet": contextKnowledgeSnippet(n.Content, query), "status": n.Status, "updated_at": n.UpdatedAt})
		selected[n.ID] = true
	}
	for _, e := range edges {
		if len(links) == 50 {
			break
		}
		if selected[e.From] || selected[e.To] {
			links = append(links, map[string]any{"from": e.From, "to": e.To, "type": e.Type})
		}
	}
	out := map[string]any{"projection": "bounded_knowledge_search", "product_id": product, "query": query, "nodes": nodes, "edges": links, "content_excerpted": true, "nodes_truncated": len(matched) > len(nodes), "guidance": "Knowledge-node excerpts only (32KiB maximum); indexed source text search does not include these curated knowledge nodes. Full curated node text remains in the explicit operator HTTP /api/products/{id}/context endpoint."}
	for {
		b, e := json.Marshal(out)
		if e != nil {
			return nil, e
		}
		if len(b) <= 32<<10 {
			break
		}
		if len(links) > 0 {
			links = links[:len(links)-1]
			out["edges"] = links
			out["edges_truncated"] = true
			continue
		}
		if len(nodes) > 0 {
			nodes = nodes[:len(nodes)-1]
			out["nodes"] = nodes
			out["nodes_truncated"] = true
			continue
		}
		return nil, invalid("knowledge summary exceeds byte budget")
	}
	return out, nil
}
func contextKnowledgeSnippet(content, query string) string {
	runes := []rune(content)
	start := 0
	tokens := strings.Fields(strings.ToLower(query))
	lower := strings.ToLower(content)
	for _, token := range tokens {
		if i := strings.Index(lower, token); i >= 0 {
			start = len([]rune(lower[:i])) - 150
			if start < 0 {
				start = 0
			}
			break
		}
	}
	if start > len(runes) {
		start = 0
	}
	end := start + 800
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}
