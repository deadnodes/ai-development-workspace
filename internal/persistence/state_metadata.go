package persistence

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
)

// SQL bounds text before it crosses the connection; only metadata columns are
// read. Neither the events table nor the full record document is transferred.
const stateCardColumns = `left(id,129), left(coalesce(product_id,''),129), left(coalesce(feature_id,''),129), CASE WHEN kind='products' THEN left(coalesce(document->>'name',''),257) ELSE '' END, CASE WHEN kind IN ('features','integrations') THEN left(coalesce(document->>'title',''),257) ELSE '' END, (CASE WHEN kind='products' THEN octet_length(coalesce(document->>'name',''))>256 ELSE octet_length(coalesce(document->>'title',''))>256 END), CASE WHEN kind IN ('features','integrations') THEN left(coalesce(document->>'status',''),65) ELSE '' END, CASE WHEN kind='integrations' THEN left(coalesce(document->>'assignee',''),129) ELSE '' END,
CASE WHEN kind='products' THEN coalesce(nullif(document->>'visibility',''),'shared') ELSE '' END,
CASE WHEN kind='products' THEN left(coalesce(document->>'owner',''),129) ELSE '' END,
CASE WHEN kind='products' THEN ARRAY(SELECT left(member,129) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'members')='array' THEN document->'members' ELSE '[]'::jsonb END) WITH ORDINALITY AS m(member,ordinal) ORDER BY ordinal LIMIT 129) ELSE ARRAY[]::text[] END`

func scanStateCard(row interface{ Scan(...any) error }) (domain.StateCard, error) {
	var c domain.StateCard
	e := row.Scan(&c.ID, &c.ProductID, &c.FeatureID, &c.Name, &c.Title, &c.Truncated, &c.RecordedStatus, &c.Assignee, &c.Visibility, &c.Owner, &c.Members)
	if len(c.Members) == 0 {
		c.Members = nil
	}
	return c, e
}
func (s *Store) ReadStateMetadata(ctx context.Context, q domain.StateMetadataRequest) (domain.StateMetadata, error) {
	var out domain.StateMetadata
	if !validMetadataRequest(q) {
		return out, errors.New("invalid bounded metadata request")
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return out, e
	}
	defer tx.Rollback(ctx)
	hidden, e := hiddenProducts(ctx, tx)
	if e != nil {
		return out, e
	}
	get := func(kind, id string) (*domain.StateCard, error) {
		c, e := scanStateCard(tx.QueryRow(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind=$1 AND id=$2 AND (NOT $3::boolean OR (CASE WHEN kind='products' THEN id ELSE product_id END)=ANY($4::text[])) AND (CASE WHEN kind='products' THEN id ELSE product_id END) <> ALL($5::text[])`, kind, id, q.RestrictProducts, q.AllowedProductIDs, hidden))
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		return &c, nil
	}
	if q.FeatureID != "" {
		out.Feature, e = get("features", q.FeatureID)
		if e != nil {
			return out, e
		}
		if out.Feature == nil {
			return out, nil
		}
		if q.ProductID != "" && q.ProductID != out.Feature.ProductID {
			return out, nil
		}
		out.Product, e = get("products", out.Feature.ProductID)
	} else if q.ProductID != "" {
		out.Product, e = get("products", q.ProductID)
	}
	if e != nil {
		return out, e
	}
	if q.Collection != "products" && out.Product == nil {
		return out, nil
	}
	if q.AfterID != "" {
		anchor, err := get(q.Collection, q.AfterID)
		if err != nil {
			return out, err
		}
		if anchor == nil || q.Collection != "products" && (anchor.ProductID != out.Product.ID || q.Collection == "integrations" && anchor.FeatureID != q.FeatureID) {
			return out, domain.ErrReadScopeNotFound
		}
	}
	var rows pgx.Rows
	switch q.Collection {
	case "products":
		rows, e = tx.Query(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind='products' AND id COLLATE "C">$1 COLLATE "C" AND (NOT $3::boolean OR id=ANY($4::text[])) AND id <> ALL($5::text[]) ORDER BY id COLLATE "C" LIMIT $2`, q.AfterID, q.Limit, q.RestrictProducts, q.AllowedProductIDs, hidden)
	case "features":
		rows, e = tx.Query(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind='features' AND product_id=$1 AND id COLLATE "C">$2 COLLATE "C" ORDER BY id COLLATE "C" LIMIT $3`, q.ProductID, q.AfterID, q.Limit)
	case "integrations":
		rows, e = tx.Query(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind='integrations' AND product_id=$1 AND feature_id=$2 AND id COLLATE "C">$3 COLLATE "C" ORDER BY id COLLATE "C" LIMIT $4`, out.Product.ID, q.FeatureID, q.AfterID, q.Limit)
	}
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		c, e := scanStateCard(rows)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, c)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	rows.Close()
	return out, tx.Commit(ctx)
}
func validMetadataRequest(q domain.StateMetadataRequest) bool {
	return q.Limit >= 1 && q.Limit <= 101 && (q.Collection == "products" || q.Collection == "features" || q.Collection == "integrations")
}
