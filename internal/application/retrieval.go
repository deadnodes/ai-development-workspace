package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/domain"
	"strings"
	"unicode/utf8"
)

type contextRetrievalReader interface {
	SearchContext(context.Context, domain.ContextSearchRequest) (domain.ContextSearchResult, error)
	ContextExcerpt(context.Context, domain.ContextExcerptRequest) (domain.ContextExcerpt, error)
	GraphNeighbors(context.Context, domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error)
}

const ContextRetrievalMaxBytes = 64 << 10

func retrievalBudget(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > ContextRetrievalMaxBytes {
		return invalid("context result exceeds byte budget; use a smaller limit or text window")
	}
	return nil
}
func (s *Service) retrievalReader(ctx context.Context, product string) (contextRetrievalReader, error) {
	if !stateID(product) {
		return nil, invalid("exact product_id required")
	}
	if e := authorize(ctx, "read", product); e != nil {
		return nil, e
	}
	r, ok := s.store.(contextRetrievalReader)
	if !ok {
		return nil, invalid("context retrieval requires the indexed store reader")
	}
	return r, nil
}
func (s *Service) SearchContext(ctx context.Context, q domain.ContextSearchRequest) (domain.ContextSearchResult, error) {
	var out domain.ContextSearchResult
	r, e := s.retrievalReader(ctx, q.ProductID)
	if e != nil {
		return out, e
	}
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" || !utf8.ValidString(q.Query) || utf8.RuneCountInString(q.Query) > 256 {
		return out, invalid("query must contain 1..256 UTF-8 characters")
	}
	if q.FeatureID != "" && !stateID(q.FeatureID) {
		return out, invalid("invalid feature_id")
	}
	if q.Limit == 0 {
		q.Limit = 10
	}
	if q.Limit < 1 || q.Limit > 20 {
		return out, invalid("search limit must be 1..20")
	}
	out, e = r.SearchContext(ctx, q)
	if e != nil {
		return out, e
	}
	if len(out.Items) > q.Limit {
		return domain.ContextSearchResult{}, invalid("search reader exceeded item budget")
	}
	for _, c := range out.Items {
		if c.ProductID != q.ProductID || (q.FeatureID != "" && c.FeatureID != q.FeatureID && c.ID != q.FeatureID) {
			return domain.ContextSearchResult{}, invalid("search reader returned a different scope")
		}
	}
	if e = retrievalBudget(out); e != nil {
		return domain.ContextSearchResult{}, e
	}
	return out, nil
}
func (s *Service) ContextExcerpt(ctx context.Context, q domain.ContextExcerptRequest) (domain.ContextExcerpt, error) {
	var out domain.ContextExcerpt
	r, e := s.retrievalReader(ctx, q.ProductID)
	if e != nil {
		return out, e
	}
	if !stateID(q.ID) {
		return out, invalid("exact source id required")
	}
	if q.MaxChars == 0 {
		q.MaxChars = 2000
	}
	if q.MaxChars < 1 || q.MaxChars > 8000 || q.Offset < 0 || q.Offset > 10000000 {
		return out, invalid("excerpt offset must be 0..10000000 and max_chars 1..8000")
	}
	out, e = r.ContextExcerpt(ctx, q)
	if e != nil {
		return out, e
	}
	if out.ID == "" {
		return out, missing("context source", q.ID)
	}
	if out.ProductID != q.ProductID || out.ID != q.ID || utf8.RuneCountInString(out.Text) > q.MaxChars {
		return domain.ContextExcerpt{}, invalid("excerpt reader exceeded scope or window")
	}
	if e = retrievalBudget(out); e != nil {
		return domain.ContextExcerpt{}, e
	}
	return out, nil
}
func (s *Service) GraphNeighbors(ctx context.Context, q domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error) {
	var out domain.GraphNeighborsResult
	r, e := s.retrievalReader(ctx, q.ProductID)
	if e != nil {
		return out, e
	}
	if !stateID(q.ID) {
		return out, invalid("exact source id required")
	}
	if q.Depth == 0 {
		q.Depth = 1
	}
	if q.Limit == 0 {
		q.Limit = 15
	}
	if q.Depth < 1 || q.Depth > 2 || q.Limit < 1 || q.Limit > 30 {
		return out, invalid("graph depth must be 1..2 and limit 1..30")
	}
	out, e = r.GraphNeighbors(ctx, q)
	if e != nil {
		return out, e
	}
	if out.Seed.ID == "" {
		return out, missing("graph source", q.ID)
	}
	if out.Seed.ProductID != q.ProductID || out.Seed.ID != q.ID || len(out.Nodes) > q.Limit || len(out.Edges) > q.Limit {
		return domain.GraphNeighborsResult{}, invalid("graph reader exceeded scope or node/edge budget")
	}
	ids := map[string]bool{q.ID: true}
	for _, c := range out.Nodes {
		if c.ProductID != q.ProductID || ids[c.ID] {
			return domain.GraphNeighborsResult{}, invalid("graph reader returned foreign or duplicate node")
		}
		ids[c.ID] = true
	}
	for _, edge := range out.Edges {
		if !ids[edge.Source] || !ids[edge.Target] {
			return domain.GraphNeighborsResult{}, invalid("graph reader returned an unrequested endpoint")
		}
	}
	if e = retrievalBudget(out); e != nil {
		return domain.GraphNeighborsResult{}, e
	}
	return out, nil
}
