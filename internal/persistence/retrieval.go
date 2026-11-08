package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
)

const retrievalGuidance = "Search cards only; recorded status is not acceptance evidence. Use graph_neighbors for typed links and get_context_excerpt with this product_id and source ID for bounded text. FTS covers the first32768 text characters; exact substring fallback searches all authored text. Audit, provider payloads and credentials are not indexed."
const retrievalColumns = `id,kind,product_id,feature_id,left(title,128),left(recorded_status,64),char_length(body),(char_length(title)>128)`

func scanRetrievalCard(row interface{ Scan(...any) error }) (domain.ContextCard, error) {
	var c domain.ContextCard
	err := row.Scan(&c.ID, &c.Kind, &c.ProductID, &c.FeatureID, &c.Title, &c.Status, &c.TextChars, &c.Truncated)
	c.IndexedChars = min(c.TextChars, 32768)
	return c, err
}
func validSearchRequest(q domain.ContextSearchRequest) bool {
	return q.ProductID != "" && q.Limit >= 1 && q.Limit <= 20 && strings.TrimSpace(q.Query) != "" && utf8.RuneCountInString(q.Query) <= 256 && utf8.ValidString(q.Query)
}
func validExcerptRequest(q domain.ContextExcerptRequest) bool {
	return q.ProductID != "" && q.ID != "" && q.Offset >= 0 && q.Offset <= 10000000 && q.MaxChars >= 1 && q.MaxChars <= 8000
}
func validGraphRequest(q domain.GraphNeighborsRequest) bool {
	return q.ProductID != "" && q.ID != "" && q.Depth >= 1 && q.Depth <= 2 && q.Limit >= 1 && q.Limit <= 30
}
func (s *Store) SearchContext(ctx context.Context, q domain.ContextSearchRequest) (domain.ContextSearchResult, error) {
	out := domain.ContextSearchResult{Items: []domain.ContextCard{}, Guidance: retrievalGuidance}
	if !validSearchRequest(q) {
		return out, errors.New("invalid bounded search request")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+retrievalColumns+`,substring(body from greatest(1,strpos(lower(body),lower($3))-96) for 384),
 ts_rank_cd(search_vector,plainto_tsquery('simple',$3))::float8 + CASE WHEN strpos(lower(title),lower($3))>0 THEN 2 ELSE 0 END
 FROM retrieval_documents WHERE product_id=$1 AND ($2='' OR feature_id=$2 OR id=$2)
 AND (search_vector @@ plainto_tsquery('simple',$3) OR strpos(lower(body),lower($3))>0)
 ORDER BY 10 DESC,id COLLATE "C" LIMIT $4`, q.ProductID, q.FeatureID, q.Query, q.Limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c domain.ContextCard
		if err = rows.Scan(&c.ID, &c.Kind, &c.ProductID, &c.FeatureID, &c.Title, &c.Status, &c.TextChars, &c.Truncated, &c.Snippet, &c.Score); err != nil {
			return out, err
		}
		c.IndexedChars = min(c.TextChars, 32768)
		c.Truncated = c.Truncated || c.TextChars > utf8.RuneCountInString(c.Snippet)
		out.Items = append(out.Items, c)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > q.Limit {
		out.HasMore = true
		out.Items = out.Items[:q.Limit]
	}
	return out, nil
}
func (s *Store) ContextExcerpt(ctx context.Context, q domain.ContextExcerptRequest) (domain.ContextExcerpt, error) {
	var out domain.ContextExcerpt
	if !validExcerptRequest(q) {
		return out, errors.New("invalid bounded excerpt request")
	}
	err := s.pool.QueryRow(ctx, `SELECT `+retrievalColumns+`,substring(body from $3+1 for $4) FROM retrieval_documents WHERE product_id=$1 AND id=$2`, q.ProductID, q.ID, q.Offset, q.MaxChars).Scan(&out.ID, &out.Kind, &out.ProductID, &out.FeatureID, &out.Title, &out.Status, &out.TextChars, &out.Truncated, &out.Text)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ContextExcerpt{}, nil
	}
	if err != nil {
		return out, err
	}
	finishExcerpt(&out, q)
	return out, nil
}
func finishExcerpt(out *domain.ContextExcerpt, q domain.ContextExcerptRequest) {
	out.IndexedChars = min(out.TextChars, 32768)
	out.Offset = q.Offset
	next := q.Offset + utf8.RuneCountInString(out.Text)
	if next < out.TextChars {
		out.NextOffset = &next
	}
	out.Truncated = out.Truncated || q.Offset > 0 || next < out.TextChars
}
func (s *Store) GraphNeighbors(ctx context.Context, q domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error) {
	out := domain.GraphNeighborsResult{Nodes: []domain.ContextCard{}, Edges: []domain.ContextEdge{}, Guidance: retrievalGuidance}
	if !validGraphRequest(q) {
		return out, errors.New("invalid bounded graph request")
	}
	// Each hop reads at most the remaining node budget plus one. Only edges with
	// both endpoints owned by the requested product are eligible.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	out.Seed, err = scanRetrievalCard(tx.QueryRow(ctx, `SELECT `+retrievalColumns+` FROM retrieval_documents WHERE product_id=$1 AND id=$2`, q.ProductID, q.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.GraphNeighborsResult{}, nil
	}
	if err != nil {
		return out, err
	}
	visited := map[string]bool{q.ID: true}
	frontier := []string{q.ID}
	seenEdges := map[domain.ContextEdge]bool{}
	for depth := 0; depth < q.Depth && len(frontier) > 0; depth++ {
		remaining := q.Limit - len(out.Nodes)
		rows, e := tx.Query(ctx, `WITH candidates AS (
   SELECT e.source_id,e.target_id,e.relation,CASE WHEN e.source_id=ANY($2::text[]) THEN e.target_id ELSE e.source_id END AS neighbor
   FROM retrieval_edges e JOIN retrieval_documents a ON a.id=e.source_id AND a.product_id=$1
   JOIN retrieval_documents b ON b.id=e.target_id AND b.product_id=$1
   WHERE (e.source_id=ANY($2::text[]) OR e.target_id=ANY($2::text[]))
  ), chosen AS (SELECT neighbor FROM candidates WHERE NOT(neighbor=ANY($3::text[])) GROUP BY neighbor ORDER BY neighbor COLLATE "C" LIMIT $4)
  SELECT d.id,d.kind,d.product_id,d.feature_id,left(d.title,128),d.recorded_status,char_length(d.body),char_length(d.title)>128,
  c.source_id,c.target_id,c.relation
  FROM chosen n JOIN retrieval_documents d ON d.id=n.neighbor
  JOIN LATERAL (SELECT source_id,target_id,relation FROM candidates c WHERE c.neighbor=n.neighbor ORDER BY source_id,target_id,relation LIMIT 1) c ON true
  ORDER BY d.id COLLATE "C"`, q.ProductID, frontier, mapKeys(visited), remaining+1)
		if e != nil {
			return out, e
		}
		next := []string{}
		for rows.Next() {
			var c domain.ContextCard
			var edge domain.ContextEdge
			if e = rows.Scan(&c.ID, &c.Kind, &c.ProductID, &c.FeatureID, &c.Title, &c.Status, &c.TextChars, &c.Truncated, &edge.Source, &edge.Target, &edge.Relation); e != nil {
				rows.Close()
				return out, e
			}
			if len(out.Nodes) >= q.Limit {
				out.Truncated = true
				continue
			}
			c.IndexedChars = min(c.TextChars, 32768)
			visited[c.ID] = true
			next = append(next, c.ID)
			out.Nodes = append(out.Nodes, c)
			if !seenEdges[edge] {
				seenEdges[edge] = true
				out.Edges = append(out.Edges, edge)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		frontier = next
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bbolt has one monolithic state document; fallback output remains bounded, but
// unlike PostgreSQL this format cannot promise a bounded storage read.
type localRetrievalDocument struct {
	card  domain.ContextCard
	body  string
	edges []domain.ContextEdge
}

func localRetrievalDocuments(st domain.State) (map[string]localRetrievalDocument, error) {
	groups := map[string]any{"products": st.Products, "features": st.Features, "integrations": st.Integrations, "memories": st.Memories, "repositories": st.Repositories}
	out := map[string]localRetrievalDocument{}
	for kind, v := range groups {
		raw, e := json.Marshal(v)
		if e != nil {
			return nil, e
		}
		var rows []map[string]any
		if e = json.Unmarshal(raw, &rows); e != nil {
			return nil, e
		}
		for _, r := range rows {
			str := func(k string) string {
				v := r[k]
				if v == nil {
					return ""
				}
				if s, ok := v.(string); ok {
					return s
				}
				b, _ := json.Marshal(v)
				return string(b)
			}
			id := str("id")
			product := str("product_id")
			if kind == "products" {
				product = id
			}
			title := str("title")
			if title == "" {
				title = str("name")
			}
			if title == "" {
				title = str("kind")
			}
			text := []string{}
			for _, key := range []string{"title", "name", "description", "problem", "goal", "requirements", "constraints", "context", "objective", "rationale", "acceptance_criteria", "body", "reason", "current", "completed", "remaining", "next", "warnings", "resolution"} {
				if value := str(key); value != "" {
					text = append(text, value)
				}
			}
			body := strings.Join(text, "\n")
			c := domain.ContextCard{ID: id, Kind: kind, ProductID: product, FeatureID: str("feature_id"), Title: runeSlice(title, 0, 128), Status: runeSlice(str("status"), 0, 64), TextChars: utf8.RuneCountInString(body), IndexedChars: min(utf8.RuneCountInString(body), 32768), Truncated: utf8.RuneCountInString(title) > 128}
			edges := []domain.ContextEdge{}
			add := func(target, relation string) {
				if target != "" && target != id {
					edges = append(edges, domain.ContextEdge{Source: id, Target: target, Relation: relation})
				}
			}
			if kind != "products" {
				add(product, "belongs_to_product")
			}
			add(c.FeatureID, "belongs_to_feature")
			add(str("integration_id"), "belongs_to_integration")
			for key, relation := range map[string]string{"dependencies": "depends_on", "repositories": "uses_repository"} {
				if values, ok := r[key].([]any); ok {
					for _, value := range values {
						if target, ok := value.(string); ok {
							add(target, relation)
						}
					}
				}
			}
			out[id] = localRetrievalDocument{c, body, edges}
		}
	}
	return out, nil
}
func runeSlice(s string, offset, count int) string {
	r := []rune(s)
	if offset >= len(r) {
		return ""
	}
	end := offset + count
	if end > len(r) {
		end = len(r)
	}
	return string(r[offset:end])
}
func (s *LocalStore) SearchContext(ctx context.Context, q domain.ContextSearchRequest) (domain.ContextSearchResult, error) {
	out := domain.ContextSearchResult{Items: []domain.ContextCard{}, Guidance: retrievalGuidance}
	if !validSearchRequest(q) {
		return out, errors.New("invalid bounded search request")
	}
	st, e := s.Read(ctx)
	if e != nil {
		return out, e
	}
	docs, e := localRetrievalDocuments(st)
	if e != nil {
		return out, e
	}
	needle := strings.ToLower(q.Query)
	terms := strings.Fields(needle)
	for _, d := range docs {
		if d.card.ProductID != q.ProductID || (q.FeatureID != "" && d.card.FeatureID != q.FeatureID && d.card.ID != q.FeatureID) {
			continue
		}
		body := strings.ToLower(d.body)
		match := strings.Contains(body, needle)
		all := len(terms) > 0
		for _, term := range terms {
			all = all && strings.Contains(body, term)
		}
		if !match && !all {
			continue
		}
		c := d.card
		c.Score = 1
		if strings.Contains(strings.ToLower(c.Title), needle) {
			c.Score += 2
		}
		start := strings.Index(body, needle)
		if start < 0 {
			start = 0
		} else {
			start = utf8.RuneCountInString(body[:start]) - 96
			if start < 0 {
				start = 0
			}
		}
		c.Snippet = runeSlice(d.body, start, 384)
		c.IndexedChars = min(c.TextChars, 32768)
		c.Truncated = c.Truncated || c.TextChars > utf8.RuneCountInString(c.Snippet)
		out.Items = append(out.Items, c)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Score != out.Items[j].Score {
			return out.Items[i].Score > out.Items[j].Score
		}
		return out.Items[i].ID < out.Items[j].ID
	})
	if len(out.Items) > q.Limit {
		out.HasMore = true
		out.Items = out.Items[:q.Limit]
	}
	return out, nil
}
func (s *LocalStore) ContextExcerpt(ctx context.Context, q domain.ContextExcerptRequest) (domain.ContextExcerpt, error) {
	var out domain.ContextExcerpt
	if !validExcerptRequest(q) {
		return out, errors.New("invalid bounded excerpt request")
	}
	st, e := s.Read(ctx)
	if e != nil {
		return out, e
	}
	docs, e := localRetrievalDocuments(st)
	if e != nil {
		return out, e
	}
	d, ok := docs[q.ID]
	if !ok || d.card.ProductID != q.ProductID {
		return out, nil
	}
	out.ContextCard = d.card
	out.Text = runeSlice(d.body, q.Offset, q.MaxChars)
	finishExcerpt(&out, q)
	return out, nil
}
func (s *LocalStore) GraphNeighbors(ctx context.Context, q domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error) {
	out := domain.GraphNeighborsResult{Nodes: []domain.ContextCard{}, Edges: []domain.ContextEdge{}, Guidance: retrievalGuidance}
	if !validGraphRequest(q) {
		return out, fmt.Errorf("invalid bounded graph request")
	}
	st, e := s.Read(ctx)
	if e != nil {
		return out, e
	}
	docs, e := localRetrievalDocuments(st)
	if e != nil {
		return out, e
	}
	seed, ok := docs[q.ID]
	if !ok || seed.card.ProductID != q.ProductID {
		return domain.GraphNeighborsResult{}, nil
	}
	out.Seed = seed.card
	visited := map[string]bool{q.ID: true}
	frontier := map[string]bool{q.ID: true}
	for depth := 0; depth < q.Depth && len(frontier) > 0; depth++ {
		candidates := map[string]domain.ContextEdge{}
		for _, d := range docs {
			if d.card.ProductID != q.ProductID {
				continue
			}
			for _, edge := range d.edges {
				target, ok := docs[edge.Target]
				if !ok || target.card.ProductID != q.ProductID {
					continue
				}
				neighbor := ""
				if frontier[edge.Source] {
					neighbor = edge.Target
				} else if frontier[edge.Target] {
					neighbor = edge.Source
				}
				if neighbor != "" && !visited[neighbor] {
					if old, exists := candidates[neighbor]; !exists || edgeKey(edge) < edgeKey(old) {
						candidates[neighbor] = edge
					}
				}
			}
		}
		ids := make([]string, 0, len(candidates))
		for id := range candidates {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		next := map[string]bool{}
		for _, id := range ids {
			if len(out.Nodes) >= q.Limit {
				out.Truncated = true
				break
			}
			out.Nodes = append(out.Nodes, docs[id].card)
			out.Edges = append(out.Edges, candidates[id])
			visited[id] = true
			next[id] = true
		}
		frontier = next
	}
	return out, nil
}
func edgeKey(e domain.ContextEdge) string { return e.Source + "\x00" + e.Target + "\x00" + e.Relation }
