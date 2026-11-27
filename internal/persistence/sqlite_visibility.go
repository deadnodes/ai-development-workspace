package persistence

import (
	"context"
	"fmt"
	"releasecontrol/internal/auth"
)

const sqlitePrivateProductDeniedSQL = `%[1]sdocument->>'visibility'='private' AND
	(%[2]s='' OR (coalesce(%[1]sdocument->>'owner','')<>%[2]s AND NOT EXISTS (SELECT 1 FROM json_each(%[1]sdocument,'$.members') WHERE type='text' AND value=%[2]s)))`

func sqliteHiddenProducts(ctx context.Context, tx sqliteTransaction) ([]string, error) {
	ids := []string{}
	user, restricted := auth.PrivateProductViewer(ctx)
	if !restricted {
		return ids, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM records WHERE kind='products' AND `+fmt.Sprintf(sqlitePrivateProductDeniedSQL, "", "$1"), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const sqliteVisibleProductSQL = `EXISTS (SELECT 1 FROM records WHERE kind='products' AND id=$1 AND id  NOT IN (SELECT value FROM json_each($2)))`

func sqliteVisibleProductIn(ctx context.Context, tx sqliteTransaction, id string, hidden []string) (bool, error) {
	var visible bool
	err := tx.QueryRow(ctx, `SELECT `+sqliteVisibleProductSQL, id, hidden).Scan(&visible)
	return visible, err
}

func sqliteProductAccess(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) stateRow
}, id string) (bool, error) {
	user, restricted := auth.PrivateProductViewer(ctx)
	var visible bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE kind='products' AND id=$1
		AND (NOT $2 OR (`+fmt.Sprintf(sqlitePrivateProductDeniedSQL, "", "$3")+`) IS NOT TRUE))`, id, restricted, user).Scan(&visible)
	return visible, err
}

func (s *SQLiteStore) ReadProductAccess(ctx context.Context, id string) (bool, error) {
	tx, err := s.begin(ctx, true)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	return sqliteProductAccess(ctx, tx, id)
}
