package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"releasecontrol/internal/domain"
)

const sqliteStateCardColumns = `substr(id,1,129), substr(coalesce(product_id,''),1,129), substr(coalesce(feature_id,''),1,129), CASE WHEN kind='products' THEN substr(coalesce(document->>'name',''),1,257) ELSE '' END, CASE WHEN kind IN ('features','integrations') THEN substr(coalesce(document->>'title',''),1,257) ELSE '' END, (CASE WHEN kind='products' THEN octet_length(coalesce(document->>'name',''))>256 ELSE octet_length(coalesce(document->>'title',''))>256 END), CASE WHEN kind IN ('features','integrations') THEN substr(coalesce(document->>'status',''),1,65) ELSE '' END, CASE WHEN kind='integrations' THEN substr(coalesce(document->>'assignee',''),1,129) ELSE '' END,
CASE WHEN kind='products' THEN coalesce(nullif(document->>'visibility',''),'shared') ELSE '' END,
CASE WHEN kind='products' THEN substr(coalesce(document->>'owner',''),1,129) ELSE '' END,
CASE WHEN kind='products' THEN (SELECT json_group_array(substr(value,1,129)) FROM (SELECT value FROM json_each(document,'$.members') WHERE type='text' ORDER BY key LIMIT 129)) ELSE '[]' END`

func scanSQLiteStateCard(row interface{ Scan(...any) error }) (domain.StateCard, error) {
	var c domain.StateCard
	var members []byte
	e := row.Scan(&c.ID, &c.ProductID, &c.FeatureID, &c.Name, &c.Title, &c.Truncated, &c.RecordedStatus, &c.Assignee, &c.Visibility, &c.Owner, &members)
	if e == nil && string(members) != "[]" {
		e = json.Unmarshal(members, &c.Members)
	}
	return c, e
}
func (s *SQLiteStore) ReadStateMetadata(ctx context.Context, q domain.StateMetadataRequest) (domain.StateMetadata, error) {
	var out domain.StateMetadata
	if !validMetadataRequest(q) {
		return out, errors.New("invalid bounded metadata request")
	}
	tx, e := s.begin(ctx, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback(ctx)
	hidden, e := sqliteHiddenProducts(ctx, tx)
	if e != nil {
		return out, e
	}
	get := func(kind, id string) (*domain.StateCard, error) {
		c, e := scanSQLiteStateCard(tx.QueryRow(ctx, `SELECT `+sqliteStateCardColumns+` FROM records WHERE kind=$1 AND id=$2 AND (NOT $3 OR (CASE WHEN kind='products' THEN id ELSE product_id END) IN (SELECT value FROM json_each($4))) AND (CASE WHEN kind='products' THEN id ELSE product_id END)  NOT IN (SELECT value FROM json_each($5))`, kind, id, q.RestrictProducts, q.AllowedProductIDs, hidden))
		if errors.Is(e, sql.ErrNoRows) {
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
	var rows stateRows
	switch q.Collection {
	case "products":
		rows, e = tx.Query(ctx, `SELECT `+sqliteStateCardColumns+` FROM records WHERE kind='products' AND id COLLATE BINARY>$1 COLLATE BINARY AND (NOT $3 OR id IN (SELECT value FROM json_each($4))) AND id  NOT IN (SELECT value FROM json_each($5)) ORDER BY id COLLATE BINARY LIMIT $2`, q.AfterID, q.Limit, q.RestrictProducts, q.AllowedProductIDs, hidden)
	case "features":
		rows, e = tx.Query(ctx, `SELECT `+sqliteStateCardColumns+` FROM records WHERE kind='features' AND product_id=$1 AND id COLLATE BINARY>$2 COLLATE BINARY ORDER BY id COLLATE BINARY LIMIT $3`, q.ProductID, q.AfterID, q.Limit)
	case "integrations":
		rows, e = tx.Query(ctx, `SELECT `+sqliteStateCardColumns+` FROM records WHERE kind='integrations' AND product_id=$1 AND feature_id=$2 AND id COLLATE BINARY>$3 COLLATE BINARY ORDER BY id COLLATE BINARY LIMIT $4`, out.Product.ID, q.FeatureID, q.AfterID, q.Limit)
	}
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		c, e := scanSQLiteStateCard(rows)
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
