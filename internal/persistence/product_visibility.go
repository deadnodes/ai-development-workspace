package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

const privateProductDeniedSQL = `%[1]sdocument->>'visibility'='private' AND
	(%[2]s='' OR (coalesce(%[1]sdocument->>'owner','')<>%[2]s AND NOT coalesce((%[1]sdocument->'members') ? %[2]s,false)))`

func hiddenProducts(ctx context.Context, tx pgx.Tx) ([]string, error) {
	ids := []string{}
	user, restricted := auth.PrivateProductViewer(ctx)
	if !restricted {
		return ids, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM records WHERE kind='products' AND `+fmt.Sprintf(privateProductDeniedSQL, "", "$1"), user)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(ids, rows, pgx.RowTo[string])
}

const visibleProductSQL = `EXISTS (SELECT 1 FROM records WHERE kind='products' AND id=$1 AND id <> ALL($2::text[]))`

func visibleProductIn(ctx context.Context, tx pgx.Tx, id string, hidden []string) (bool, error) {
	var visible bool
	err := tx.QueryRow(ctx, `SELECT `+visibleProductSQL, id, hidden).Scan(&visible)
	return visible, err
}

func readProductAccess(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (bool, error) {
	user, restricted := auth.PrivateProductViewer(ctx)
	var visible bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE kind='products' AND id=$1
		AND (NOT $2::boolean OR (`+fmt.Sprintf(privateProductDeniedSQL, "", "$3")+`) IS NOT TRUE))`, id, restricted, user).Scan(&visible)
	return visible, err
}

func (s *Store) ReadProductAccess(ctx context.Context, id string) (bool, error) {
	return readProductAccess(ctx, s.pool, id)
}

func (s *LocalStore) ReadProductAccess(ctx context.Context, id string) (bool, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return false, errors.New("local database is closed")
	}
	var visible bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		visible, err = localProductAccess(ctx, tx, id)
		return err
	})
	return visible, err
}

func localProductAccess(ctx context.Context, tx *bolt.Tx, id string) (bool, error) {
	var visible bool
	err := historyScan(ctx, tx.Bucket(localBucket).Get(localStateKey), "products", func(raw []byte) error {
		if scopeText(raw, "id") != id {
			return nil
		}
		var product domain.Product
		if err := json.Unmarshal(raw, &product); err != nil {
			return err
		}
		visible = productVisible(ctx, product)
		return nil
	})
	return visible, err
}

func hiddenLocalProducts(ctx context.Context, data []byte) (map[string]bool, error) {
	_, restricted := auth.PrivateProductViewer(ctx)
	if !restricted {
		return nil, nil
	}
	hidden := map[string]bool{}
	err := historyScan(ctx, data, "products", func(raw []byte) error {
		if historyText(raw, "visibility") != "private" {
			return nil
		}
		var product struct {
			ID      string   `json:"id"`
			Owner   string   `json:"owner"`
			Members []string `json:"members"`
		}
		if err := json.Unmarshal(raw, &product); err != nil {
			return err
		}
		if !auth.ProductVisible(ctx, "private", product.Owner, product.Members) {
			hidden[product.ID] = true
		}
		return nil
	})
	return hidden, err
}

func visibleStateProduct(ctx context.Context, state domain.State, id string) bool {
	for _, product := range state.Products {
		if product.ID == id {
			return productVisible(ctx, product)
		}
	}
	return false
}

func productVisible(ctx context.Context, product domain.Product) bool {
	return auth.ProductVisible(ctx, product.Visibility, product.Owner, product.Members)
}

func scopeText(raw []byte, key string) string {
	var value string
	if field := historyPath(raw, key); len(field) != 0 {
		_ = json.Unmarshal(field, &value)
	}
	return value
}
