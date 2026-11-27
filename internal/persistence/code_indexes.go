package persistence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
)

func (s *Store) codeTransaction(ctx context.Context, write bool, fn func(stateTransaction) error) error {
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if write {
		options = pgx.TxOptions{}
	}
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	wrapped := postgresTransaction{tx}
	if err = fn(wrapped); err != nil {
		return err
	}
	return wrapped.Commit(ctx)
}

func (s *SQLiteStore) codeTransaction(ctx context.Context, write bool, fn func(stateTransaction) error) error {
	tx, err := s.begin(ctx, !write)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type codeTransactions interface {
	codeTransaction(context.Context, bool, func(stateTransaction) error) error
}

func codeScope(ctx context.Context, tx stateTransaction, product, repository string) error {
	var visible bool
	var err error
	switch db := tx.(type) {
	case postgresTransaction:
		visible, err = readProductAccess(ctx, db.Tx, product)
	case sqliteTransaction:
		visible, err = sqliteProductAccess(ctx, db, product)
	}
	if err != nil {
		return err
	}
	if !visible {
		return domain.ErrReadScopeNotFound
	}
	if repository != "" {
		err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM records WHERE id=$1 AND product_id=$2 AND kind='repositories')`, repository, product).Scan(&visible)
		if err != nil {
			return err
		}
		if !visible {
			return domain.ErrReadScopeNotFound
		}
	}
	return nil
}

func (s *Store) ReadCodeRepositories(ctx context.Context, product, after string, limit int) ([]domain.CodeRepository, error) {
	return readCodeRepositories(ctx, s, product, after, limit)
}

func (s *SQLiteStore) ReadCodeRepositories(ctx context.Context, product, after string, limit int) ([]domain.CodeRepository, error) {
	return readCodeRepositories(ctx, s, product, after, limit)
}

func readCodeRepositories(ctx context.Context, store codeTransactions, product, after string, limit int) (items []domain.CodeRepository, err error) {
	err = store.codeTransaction(ctx, false, func(tx stateTransaction) error {
		if err := codeScope(ctx, tx, product, ""); err != nil {
			return err
		}
		collation := tx.sql(` COLLATE "C"`, ` COLLATE BINARY`)
		rows, err := tx.Query(ctx, `SELECT substr(r.id,1,129), coalesce(substr(r.document->>'url',1,$4),'')
		FROM records r WHERE r.kind='repositories' AND r.product_id=$1 AND r.id`+collation+`>$2 ORDER BY r.id`+collation+` LIMIT $3`, product, after, limit, domain.CodeRepositoryURLBytes+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item domain.CodeRepository
			var cloneURL string
			if err := rows.Scan(&item.ID, &cloneURL); err != nil {
				return err
			}
			item.Identity = workspace.CodeRepositoryIdentity(cloneURL)
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (s *Store) PutCodeIndex(ctx context.Context, input domain.CodeIndexWrite, status domain.CodeIndexStatus) (domain.CodeIndexWriteResult, error) {
	return putCodeIndex(ctx, s, input, status)
}
func (s *SQLiteStore) PutCodeIndex(ctx context.Context, input domain.CodeIndexWrite, status domain.CodeIndexStatus) (domain.CodeIndexWriteResult, error) {
	return putCodeIndex(ctx, s, input, status)
}

const codeTimeLayout = "2006-01-02T15:04:05.000000000Z"

func putCodeIndex(ctx context.Context, store codeTransactions, input domain.CodeIndexWrite, status domain.CodeIndexStatus) (out domain.CodeIndexWriteResult, err error) {
	err = store.codeTransaction(ctx, true, func(tx stateTransaction) error {
		var revision int64
		if err := tx.QueryRow(ctx, tx.sql(`SELECT revision FROM control_revision WHERE id=1 FOR UPDATE`, `SELECT revision FROM control_revision WHERE id=1`)).Scan(&revision); err != nil {
			return err
		}
		if err := codeScope(ctx, tx, input.ProductID, input.RepositoryID); err != nil {
			return err
		}
		args := []any{input.ProductID, input.RepositoryID, input.Document.Branch}
		rows, err := tx.Query(ctx, `SELECT repository_id,branch,commit_sha,digest,indexed_at,truncation FROM code_indexes WHERE product_id=$1 AND repository_id=$2 AND branch=$3`, args...)
		if err != nil {
			return err
		}
		if rows.Next() {
			prior, err := scanCodeStatus(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if prior.Digest == status.Digest {
				rows.Close()
				out.CodeIndexStatus = prior
				return nil
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		raw, err := json.Marshal(input.Document)
		if err != nil {
			return err
		}
		flags, err := json.Marshal(status.Truncated)
		if err != nil {
			return err
		}
		stamp := status.IndexedAt.UTC().Format(codeTimeLayout)
		if err = tx.Exec(ctx, `INSERT INTO code_indexes (product_id,repository_id,branch,commit_sha,digest,indexed_at,actor,truncation,document)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (product_id,repository_id,branch) DO UPDATE SET
		commit_sha=excluded.commit_sha,digest=excluded.digest,indexed_at=excluded.indexed_at,actor=excluded.actor,truncation=excluded.truncation,document=excluded.document`, append(args, status.Commit, status.Digest, stamp, input.Actor, string(flags), string(raw))...); err != nil {
			return err
		}
		where := `product_id=$1 AND repository_id=$2 AND branch=$3`
		if err = tx.Exec(ctx, `DELETE FROM code_index_cards WHERE `+where, args...); err != nil {
			return err
		}
		batch := tx.batch(ctx)
		ordinal := 0
		input.Document.SearchEntries(func(entry domain.CodeSearchEntry) bool {
			ordinal++
			card := entry.Card
			err = batch.exec(`INSERT INTO code_index_cards (product_id,repository_id,branch,ordinal,path,kind,symbol,line,title,text_body,snippet,words) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, input.ProductID, input.RepositoryID, input.Document.Branch, ordinal, card.Path, card.Kind, card.Symbol, card.Line, entry.Title, entry.Body, entry.Snippet, " "+domain.CodeSearchPhrase(entry.Body)+" ")
			return err == nil
		})
		if err != nil {
			return err
		}
		if err = batch.flush(); err != nil {
			return err
		}
		if err = tx.Exec(ctx, `INSERT INTO code_index_history (product_id,repository_id,branch,commit_sha,digest,indexed_at) VALUES ($1,$2,$3,$4,$5,$6)`, append(args, status.Commit, status.Digest, stamp)...); err != nil {
			return err
		}
		if err = tx.Exec(ctx, `DELETE FROM code_index_history WHERE sequence IN (SELECT sequence FROM code_index_history WHERE `+where+` ORDER BY sequence DESC LIMIT 9223372036854775807 OFFSET $4)`, append(args, domain.CodeHistorySize)...); err != nil {
			return err
		}
		collation := tx.sql(` COLLATE "C"`, ` COLLATE BINARY`)
		if err = tx.Exec(ctx, `DELETE FROM code_indexes WHERE product_id=$1 AND repository_id=$2 AND branch IN (
			SELECT branch FROM code_indexes WHERE product_id=$1 AND repository_id=$2
			ORDER BY indexed_at DESC,branch`+collation+` LIMIT 9223372036854775807 OFFSET $3)`, input.ProductID, input.RepositoryID, domain.CodeBranchLimit); err != nil {
			return err
		}
		out = domain.CodeIndexWriteResult{CodeIndexStatus: status, Changed: true}
		return nil
	})
	return out, err
}

func scanCodeStatus(row stateRow) (status domain.CodeIndexStatus, err error) {
	var stamp string
	var flags []byte
	if err = row.Scan(&status.RepositoryID, &status.Branch, &status.Commit, &status.Digest, &stamp, &flags); err != nil {
		return
	}
	status.IndexedAt, err = time.Parse(codeTimeLayout, stamp)
	if err == nil {
		err = json.Unmarshal(flags, &status.Truncated)
	}
	return
}

func (s *Store) CodeStatuses(ctx context.Context, query domain.CodeStatusRequest, limit int) ([]domain.CodeIndexStatus, error) {
	return codeStatuses(ctx, s, query, limit)
}
func (s *SQLiteStore) CodeStatuses(ctx context.Context, query domain.CodeStatusRequest, limit int) ([]domain.CodeIndexStatus, error) {
	return codeStatuses(ctx, s, query, limit)
}

func codeStatuses(ctx context.Context, store codeTransactions, query domain.CodeStatusRequest, limit int) (items []domain.CodeIndexStatus, err error) {
	err = store.codeTransaction(ctx, false, func(tx stateTransaction) error {
		if err := codeScope(ctx, tx, query.ProductID, query.RepositoryID); err != nil {
			return err
		}
		collation := tx.sql(` COLLATE "C"`, ` COLLATE BINARY`)
		rows, err := tx.Query(ctx, `WITH ranked AS (
		SELECT ci.repository_id,ci.branch,ci.commit_sha,ci.digest,ci.indexed_at,ci.truncation,
		row_number() OVER (PARTITION BY ci.repository_id ORDER BY ci.indexed_at DESC,ci.branch`+collation+`) AS rank
		FROM code_indexes ci
		JOIN records r ON r.id=ci.repository_id AND r.product_id=ci.product_id AND r.kind='repositories'
		WHERE ci.product_id=$1 AND ($2='' OR ci.repository_id=$2) AND ($3='' OR ci.branch=$3))
		SELECT repository_id,branch,commit_sha,digest,indexed_at,truncation FROM ranked WHERE $2<>'' OR rank=1
		ORDER BY repository_id`+collation+`,indexed_at DESC,branch`+collation+` LIMIT $4`, query.ProductID, query.RepositoryID, query.Branch, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			status, err := scanCodeStatus(rows)
			if err != nil {
				return err
			}
			items = append(items, status)
		}
		return rows.Err()
	})
	return
}

func (s *Store) ReadCodeIndex(ctx context.Context, product, repo, branch string) (domain.CodeIndex, error) {
	return readCodeIndex(ctx, s, product, repo, branch)
}
func (s *SQLiteStore) ReadCodeIndex(ctx context.Context, product, repo, branch string) (domain.CodeIndex, error) {
	return readCodeIndex(ctx, s, product, repo, branch)
}

func readCodeIndex(ctx context.Context, store codeTransactions, product, repo, branch string) (out domain.CodeIndex, err error) {
	err = store.codeTransaction(ctx, false, func(tx stateTransaction) error {
		if err := codeScope(ctx, tx, product, repo); err != nil {
			return err
		}
		collation := tx.sql(` COLLATE "C"`, ` COLLATE BINARY`)
		rows, err := tx.Query(ctx, `SELECT document FROM code_indexes WHERE product_id=$1 AND repository_id=$2 AND ($3='' OR branch=$3) ORDER BY indexed_at DESC,branch`+collation+` LIMIT 1`, product, repo, branch)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return domain.ErrReadScopeNotFound
		}
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	return
}

func (s *Store) FindCode(ctx context.Context, query domain.CodeSearchRequest) ([]domain.CodeCard, error) {
	return findCode(ctx, s, query)
}
func (s *SQLiteStore) FindCode(ctx context.Context, query domain.CodeSearchRequest) ([]domain.CodeCard, error) {
	return findCode(ctx, s, query)
}

func findCode(ctx context.Context, store codeTransactions, query domain.CodeSearchRequest) (items []domain.CodeCard, err error) {
	return findCodeWithinBudget(ctx, store, query, domain.CodeSearchScanBudget)
}

func findCodeWithinBudget(ctx context.Context, store codeTransactions, query domain.CodeSearchRequest, budget int) (items []domain.CodeCard, err error) {
	ctx, cancel := context.WithTimeout(ctx, domain.CodeSearchTimeout)
	defer cancel()
	err = store.codeTransaction(ctx, false, func(tx stateTransaction) error {
		if _, postgres := tx.(postgresTransaction); postgres {
			if err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, domain.CodeSearchTimeout.String()); err != nil {
				return err
			}
		}
		if err := codeScope(ctx, tx, query.ProductID, query.RepositoryID); err != nil {
			return err
		}
		pos := tx.sql(`strpos`, `instr`)
		collation := tx.sql(` COLLATE "C"`, ` COLLATE BINARY`)
		literal := `(` + pos + `(c.text_body,$4)>0 OR ` + pos + `(c.title,$4)>0)`
		latest := `WITH latest AS (
		 SELECT ci.repository_id,ci.branch,ci.commit_sha, row_number() OVER (PARTITION BY ci.repository_id ORDER BY ci.indexed_at DESC,ci.branch` + collation + `) AS rank
		 FROM code_indexes ci JOIN records r ON r.id=ci.repository_id AND r.product_id=ci.product_id AND r.kind='repositories'
		 WHERE ci.product_id=$1 AND ($2='' OR ci.repository_id=$2)
		)`
		join := ` FROM latest l JOIN code_index_cards c ON c.product_id=$1 AND c.repository_id=l.repository_id AND c.branch=l.branch
		 WHERE l.rank=1 AND ($3='' OR c.kind=$3)`
		score := `CASE WHEN c.title=$4 THEN 3 WHEN ` + pos + `(c.title,$4)>0 THEN 2 ELSE 1 END`
		order := ` DESC,c.repository_id` + collation + `,c.path` + collation + `,c.kind` + collation + `,c.line,c.symbol` + collation + `,c.ordinal LIMIT $5`
		read := func(rows stateRows) error {
			defer rows.Close()
			for rows.Next() {
				var card domain.CodeCard
				var exceeded bool
				if err := rows.Scan(&exceeded, &card.RepositoryID, &card.Branch, &card.Commit, &card.Path, &card.Kind, &card.Symbol, &card.Line, &card.Snippet); err != nil {
					return err
				}
				if exceeded {
					return domain.ErrCodeSearchBudget
				}
				items = append(items, card)
			}
			return rows.Err()
		}
		args := []any{query.ProductID, query.RepositoryID, query.Kind, query.Query, query.Limit + 1}
		if phrase := domain.CodeSearchPhrase(query.Query); phrase != "" {
			match := tx.sql(`c.search_vector @@ phraseto_tsquery('simple',$6)`, `c.rowid IN (SELECT rowid FROM code_index_search WHERE code_index_search MATCH ('"' || $6 || '"'))`)
			rows, err := tx.Query(ctx, latest+` SELECT FALSE,c.repository_id,c.branch,l.commit_sha,c.path,c.kind,c.symbol,c.line,c.snippet`+join+` AND `+match+` ORDER BY `+score+order, append(args, phrase)...)
			if err != nil {
				return err
			}
			if err := read(rows); err != nil || len(items) != 0 {
				return err
			}
		}
		rows, err := tx.Query(ctx, latest+`, candidates AS MATERIALIZED (
		 SELECT c.repository_id,c.branch,l.commit_sha,c.ordinal
		 `+join+` LIMIT ($6 + 1)
		), budget AS (SELECT count(*)>$6 AS exceeded FROM candidates), matches AS (
		 SELECT c.repository_id,c.branch,k.commit_sha,c.path,c.kind,c.symbol,c.line,c.snippet,c.ordinal,`+score+` AS score
		 FROM candidates k JOIN code_index_cards c ON c.product_id=$1 AND c.repository_id=k.repository_id AND c.branch=k.branch AND c.ordinal=k.ordinal
		 WHERE NOT (SELECT exceeded FROM budget) AND `+literal+` ORDER BY score`+order+`
		), results AS (
		 SELECT FALSE AS exceeded,repository_id,branch,commit_sha,path,kind,symbol,line,snippet,score,ordinal FROM matches
		 UNION ALL SELECT exceeded, '', '', '', '', '', '', 0, '', 0, 0 FROM budget WHERE exceeded
		) SELECT exceeded,repository_id,branch,commit_sha,path,kind,symbol,line,snippet FROM results
		 ORDER BY score DESC,repository_id`+collation+`,path`+collation+`,kind`+collation+`,line,symbol`+collation+`,ordinal`, append(args, budget)...)
		if err != nil {
			return err
		}
		return read(rows)
	})
	for index := range items {
		items[index].Snippet = domain.CodeSnippet(items[index].Snippet, query.Query)
	}
	return
}
