package persistence

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/auth"
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
