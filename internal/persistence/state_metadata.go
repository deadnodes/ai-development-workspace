package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/domain"
	"slices"
	"sort"
	"unicode/utf8"
)

// SQL bounds text before it crosses the connection; only metadata columns are
// read. Neither the events table nor the full record document is transferred.
const stateCardColumns = `left(id,129), left(coalesce(product_id,''),129), left(coalesce(feature_id,''),129), CASE WHEN kind='products' THEN left(coalesce(document->>'name',''),257) ELSE '' END, CASE WHEN kind IN ('features','integrations') THEN left(coalesce(document->>'title',''),257) ELSE '' END, (CASE WHEN kind='products' THEN octet_length(coalesce(document->>'name',''))>256 ELSE octet_length(coalesce(document->>'title',''))>256 END), CASE WHEN kind IN ('features','integrations') THEN left(coalesce(document->>'status',''),65) ELSE '' END`

func scanStateCard(row interface{ Scan(...any) error }) (domain.StateCard, error) {
	var c domain.StateCard
	e := row.Scan(&c.ID, &c.ProductID, &c.FeatureID, &c.Name, &c.Title, &c.Truncated, &c.RecordedStatus)
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
	get := func(kind, id string) (*domain.StateCard, error) {
		c, e := scanStateCard(tx.QueryRow(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind=$1 AND id=$2 AND (NOT $3::boolean OR (CASE WHEN kind='products' THEN id ELSE product_id END)=ANY($4::text[]))`, kind, id, q.RestrictProducts, q.AllowedProductIDs))
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
	var rows pgx.Rows
	switch q.Collection {
	case "products":
		rows, e = tx.Query(ctx, `SELECT `+stateCardColumns+` FROM records WHERE kind='products' AND id COLLATE "C">$1 COLLATE "C" AND (NOT $3::boolean OR id=ANY($4::text[])) ORDER BY id COLLATE "C" LIMIT $2`, q.AfterID, q.Limit, q.RestrictProducts, q.AllowedProductIDs)
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

func metadataAllowsProduct(q domain.StateMetadataRequest, product string) bool {
	return !q.RestrictProducts || slices.Contains(q.AllowedProductIDs, product)
}

// Local storage is a single JSON value. Navigation therefore still scans O(blob)
// bytes inside a read transaction, but never decodes audit/history objects or
// builds State. Only bounded metadata and Limit+parents cards are retained.
func (s *LocalStore) ReadStateMetadata(ctx context.Context, q domain.StateMetadataRequest) (domain.StateMetadata, error) {
	var out domain.StateMetadata
	if !validMetadataRequest(q) {
		return out, errors.New("invalid bounded metadata request")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return out, errors.New("local database is closed")
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(localBucket)
		if b == nil {
			return errors.New("local state bucket missing")
		}
		data := b.Get(localStateKey)
		if len(data) == 0 || len(data) > maxLocalStateBytes {
			return errors.New("local state missing or exceeds 256 MiB limit")
		}
		// Parents are resolved before collection filtering, in their own narrow scan.
		if q.FeatureID != "" {
			if e := walkStateCollection(ctx, data, "features", func(c domain.StateCard) {
				if c.ID == q.FeatureID && metadataAllowsProduct(q, c.ProductID) {
					v := c
					out.Feature = &v
				}
			}); e != nil {
				return e
			}
			if out.Feature == nil {
				return nil
			}
			if q.ProductID != "" && q.ProductID != out.Feature.ProductID {
				return nil
			}
		}
		productID := q.ProductID
		if out.Feature != nil {
			productID = out.Feature.ProductID
		}
		if productID != "" {
			if e := walkStateCollection(ctx, data, "products", func(c domain.StateCard) {
				if c.ID == productID && metadataAllowsProduct(q, c.ID) {
					v := c
					out.Product = &v
				}
			}); e != nil {
				return e
			}
			if out.Product == nil {
				return nil
			}
		}
		return walkStateCollection(ctx, data, q.Collection, func(c domain.StateCard) {
			owner := c.ProductID
			if q.Collection == "products" {
				owner = c.ID
			}
			if !metadataAllowsProduct(q, owner) {
				return
			}
			if c.ID <= q.AfterID {
				return
			}
			if q.Collection == "features" && c.ProductID != q.ProductID {
				return
			}
			if q.Collection == "integrations" && (c.FeatureID != q.FeatureID || c.ProductID != productID) {
				return
			}
			i := sort.Search(len(out.Items), func(i int) bool { return out.Items[i].ID >= c.ID })
			if i >= q.Limit {
				return
			}
			out.Items = append(out.Items, domain.StateCard{})
			copy(out.Items[i+1:], out.Items[i:])
			out.Items[i] = c
			if len(out.Items) > q.Limit {
				out.Items = out.Items[:q.Limit]
			}
		})
	})
	return out, e
}

func white(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipJSON skips without materializing strings or unknown nested objects.
func skipJSON(b []byte, i int) (int, error) {
	i = white(b, i)
	if i >= len(b) {
		return i, errors.New("invalid navigation storage JSON")
	}
	if b[i] == '"' {
		for j := i + 1; j < len(b); j++ {
			if b[j] == '\\' {
				j++
				continue
			}
			if b[j] == '"' {
				return j + 1, nil
			}
		}
		return i, errors.New("invalid navigation storage string")
	}
	if b[i] == '{' || b[i] == '[' {
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				n, e := skipJSON(b, j)
				if e != nil {
					return i, e
				}
				j = n - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return i, errors.New("invalid navigation storage object")
	}
	j := i
	for j < len(b) && !bytes.ContainsRune([]byte(",]} \t\n\r"), rune(b[j])) {
		j++
	}
	if j == i {
		return i, errors.New("invalid navigation storage value")
	}
	return j, nil
}

// Bounded JSON string decoding: escaped text may take six source bytes per
// rune. A long raw field is cut only at complete escape/UTF-8 boundaries.
func metadataString(raw []byte) (string, bool, error) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", false, errors.New("invalid navigation metadata string")
	}
	if len(raw) <= 1600 {
		var s string
		e := json.Unmarshal(raw, &s)
		return s, false, e
	}
	end := 1
	for end < len(raw)-1 && end < 1536 {
		if raw[end] == '\\' {
			end += 2
			if end <= len(raw) && raw[end-1] == 'u' {
				end += 4
			}
		} else {
			_, n := utf8.DecodeRune(raw[end:])
			end += n
		}
	}
	if end >= len(raw) {
		return "", false, errors.New("invalid navigation metadata escape")
	}
	prefix := make([]byte, end+1)
	copy(prefix, raw[:end])
	prefix[end] = '"'
	var s string
	e := json.Unmarshal(prefix, &s)
	return s, true, e
}
func objectFields(raw []byte, visit func(string, []byte) error) error {
	i := white(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return errors.New("invalid navigation storage object")
	}
	i++
	for {
		i = white(raw, i)
		if i >= len(raw) {
			return errors.New("invalid navigation storage object")
		}
		if raw[i] == '}' {
			return nil
		}
		end, e := skipJSON(raw, i)
		if e != nil {
			return e
		}
		key, _, e := metadataString(raw[i:end])
		if e != nil {
			return e
		}
		i = white(raw, end)
		if i >= len(raw) || raw[i] != ':' {
			return errors.New("invalid navigation storage object")
		}
		i = white(raw, i+1)
		end, e = skipJSON(raw, i)
		if e != nil {
			return e
		}
		if e = visit(key, raw[i:end]); e != nil {
			return e
		}
		i = white(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i++
			continue
		}
		if i < len(raw) && raw[i] == '}' {
			return nil
		}
		return errors.New("invalid navigation storage object")
	}
}
func walkStateCollection(ctx context.Context, data []byte, kind string, visit func(domain.StateCard)) error {
	return objectFields(data, func(key string, raw []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if key != kind {
			return nil
		}
		i := white(raw, 0)
		if i >= len(raw) || raw[i] != '[' {
			return errors.New("invalid navigation collection")
		}
		i++
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			i = white(raw, i)
			if i >= len(raw) {
				return errors.New("invalid navigation collection")
			}
			if raw[i] == ']' {
				return nil
			}
			end, e := skipJSON(raw, i)
			if e != nil {
				return e
			}
			var c domain.StateCard
			e = objectFields(raw[i:end], func(k string, v []byte) error {
				var target *string
				switch k {
				case "id":
					target = &c.ID
				case "product_id":
					target = &c.ProductID
				case "feature_id":
					target = &c.FeatureID
				case "name":
					if kind != "products" {
						return nil
					}
					target = &c.Name
				case "title":
					if kind == "products" {
						return nil
					}
					target = &c.Title
				case "status":
					if kind == "products" {
						return nil
					}
					target = &c.RecordedStatus
				default:
					return nil
				}
				s, cut, e := metadataString(v)
				if e != nil {
					return fmt.Errorf("invalid navigation metadata")
				}
				*target = s
				if (k == "name" || k == "title") && cut {
					c.Truncated = true
				}
				return nil
			})
			if e != nil {
				return e
			}
			visit(c)
			i = white(raw, end)
			if i < len(raw) && raw[i] == ',' {
				i++
				continue
			}
			if i < len(raw) && raw[i] == ']' {
				return nil
			}
			return errors.New("invalid navigation collection")
		}
	})
}
