package persistence

import (
	"context"
	"errors"
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	visible, err := readProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return out, domain.ErrReadScopeNotFound
	}
	rows, err := tx.Query(ctx, `SELECT `+retrievalColumns+`,substring(body from greatest(1,strpos(lower(body),lower($3))-96) for 384),
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	visible, err := readProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return out, nil
	}
	err = tx.QueryRow(ctx, `SELECT `+retrievalColumns+`,substring(body from $3+1 for $4) FROM retrieval_documents WHERE product_id=$1 AND id=$2`, q.ProductID, q.ID, q.Offset, q.MaxChars).Scan(&out.ID, &out.Kind, &out.ProductID, &out.FeatureID, &out.Title, &out.Status, &out.TextChars, &out.Truncated, &out.Text)
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
	visible, err := readProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return domain.GraphNeighborsResult{}, nil
	}
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
