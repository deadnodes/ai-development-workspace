package persistence

import (
	"context"
	"database/sql"
	"errors"
	"releasecontrol/internal/domain"
	"strings"
	"unicode"
	"unicode/utf8"
)

const sqliteRetrievalColumns = `id,kind,product_id,feature_id,substr(title,1,128),substr(recorded_status,1,64),length(body),(length(title)>128)`

func (s *SQLiteStore) SearchContext(ctx context.Context, q domain.ContextSearchRequest) (domain.ContextSearchResult, error) {
	out := domain.ContextSearchResult{Items: []domain.ContextCard{}, Guidance: retrievalGuidance}
	if !validSearchRequest(q) {
		return out, errors.New("invalid bounded search request")
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	visible, err := sqliteProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return out, domain.ErrReadScopeNotFound
	}
	rows, err := tx.Query(ctx, `WITH matches AS (SELECT rowid FROM retrieval_search WHERE retrieval_search MATCH $5) SELECT `+sqliteRetrievalColumns+`,substr(body,max(1,instr(rcp_lower(body),rcp_lower($3))-96),384),
 CASE WHEN d.rowid IN (SELECT rowid FROM matches) THEN 1 ELSE 0 END + CASE WHEN instr(rcp_lower(title),rcp_lower($3))>0 THEN 2 ELSE 0 END
 FROM retrieval_documents d WHERE product_id=$1 AND ($2='' OR feature_id=$2 OR id=$2)
 AND (d.rowid IN (SELECT rowid FROM matches) OR instr(rcp_lower(body),rcp_lower($3))>0)
 ORDER BY 10 DESC,id COLLATE BINARY LIMIT $4`, q.ProductID, q.FeatureID, q.Query, q.Limit+1, sqliteFTSQuery(q.Query))
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
func (s *SQLiteStore) ContextExcerpt(ctx context.Context, q domain.ContextExcerptRequest) (domain.ContextExcerpt, error) {
	var out domain.ContextExcerpt
	if !validExcerptRequest(q) {
		return out, errors.New("invalid bounded excerpt request")
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	visible, err := sqliteProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return out, nil
	}
	err = tx.QueryRow(ctx, `SELECT `+sqliteRetrievalColumns+`,substr(body,$3+1,$4) FROM retrieval_documents WHERE product_id=$1 AND id=$2`, q.ProductID, q.ID, q.Offset, q.MaxChars).Scan(&out.ID, &out.Kind, &out.ProductID, &out.FeatureID, &out.Title, &out.Status, &out.TextChars, &out.Truncated, &out.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ContextExcerpt{}, nil
	}
	if err != nil {
		return out, err
	}
	finishExcerpt(&out, q)
	return out, nil
}
func (s *SQLiteStore) GraphNeighbors(ctx context.Context, q domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error) {
	out := domain.GraphNeighborsResult{Nodes: []domain.ContextCard{}, Edges: []domain.ContextEdge{}, Guidance: retrievalGuidance}
	if !validGraphRequest(q) {
		return out, errors.New("invalid bounded graph request")
	}
	// Each hop reads at most the remaining node budget plus one. Only edges with
	// both endpoints owned by the requested product are eligible.
	tx, err := s.begin(ctx, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	visible, err := sqliteProductAccess(ctx, tx, q.ProductID)
	if err != nil {
		return out, err
	}
	if !visible {
		return domain.GraphNeighborsResult{}, nil
	}
	out.Seed, err = scanRetrievalCard(tx.QueryRow(ctx, `SELECT `+sqliteRetrievalColumns+` FROM retrieval_documents WHERE product_id=$1 AND id=$2`, q.ProductID, q.ID))
	if errors.Is(err, sql.ErrNoRows) {
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
   SELECT e.source_id,e.target_id,e.relation,CASE WHEN e.source_id IN (SELECT value FROM json_each($2)) THEN e.target_id ELSE e.source_id END AS neighbor
   FROM retrieval_edges e JOIN retrieval_documents a ON a.id=e.source_id AND a.product_id=$1
   JOIN retrieval_documents b ON b.id=e.target_id AND b.product_id=$1
   WHERE (e.source_id IN (SELECT value FROM json_each($2)) OR e.target_id IN (SELECT value FROM json_each($2)))
  ), chosen AS (SELECT neighbor FROM candidates WHERE NOT(neighbor IN (SELECT value FROM json_each($3))) GROUP BY neighbor ORDER BY neighbor COLLATE BINARY LIMIT $4)
  SELECT d.id,d.kind,d.product_id,d.feature_id,substr(d.title,1,128),d.recorded_status,length(d.body),length(d.title)>128,
  c.source_id,c.target_id,c.relation
  FROM chosen n JOIN retrieval_documents d ON d.id=n.neighbor
  JOIN (SELECT *,row_number() OVER(PARTITION BY neighbor ORDER BY source_id,target_id,relation) AS rn FROM candidates) c ON c.neighbor=n.neighbor AND c.rn=1
  ORDER BY d.id COLLATE BINARY`, q.ProductID, frontier, mapKeys(visited), remaining+1)
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

func sqliteFTSQuery(query string) string {
	tokens := strings.FieldsFunc(query, func(char rune) bool {
		return !unicode.IsLetter(char) && !unicode.IsNumber(char) && !unicode.IsMark(char)
	})
	for index, token := range tokens {
		tokens[index] = `"` + token + `"`
	}
	if len(tokens) == 0 {
		return `""`
	}
	return strings.Join(tokens, " AND ")
}
