CREATE TABLE code_indexes (
 product_id text NOT NULL REFERENCES records(id) ON DELETE CASCADE,
 repository_id text NOT NULL REFERENCES records(id) ON DELETE CASCADE,
 branch text NOT NULL, commit_sha text NOT NULL, digest text NOT NULL,
 indexed_at text NOT NULL, actor text NOT NULL, truncation jsonb NOT NULL,
 document jsonb NOT NULL,
 PRIMARY KEY(product_id, repository_id, branch)
);
CREATE INDEX code_indexes_latest ON code_indexes(product_id, repository_id, indexed_at DESC, branch);
CREATE TABLE code_index_history (
 sequence bigserial PRIMARY KEY, product_id text NOT NULL, repository_id text NOT NULL, branch text NOT NULL,
 commit_sha text NOT NULL, digest text NOT NULL, indexed_at text NOT NULL,
 FOREIGN KEY(product_id, repository_id, branch) REFERENCES code_indexes ON DELETE CASCADE
);
CREATE INDEX code_index_history_scope ON code_index_history(product_id, repository_id, branch, sequence DESC);
CREATE TABLE code_index_cards (
 product_id text NOT NULL, repository_id text NOT NULL, branch text NOT NULL,
 ordinal integer NOT NULL, path text NOT NULL, kind text NOT NULL, symbol text NOT NULL, line integer NOT NULL,
 title text NOT NULL, text_body text NOT NULL, snippet text NOT NULL, words text NOT NULL,
 search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', words)) STORED,
 PRIMARY KEY(product_id, repository_id, branch, ordinal),
 FOREIGN KEY(product_id, repository_id, branch) REFERENCES code_indexes ON DELETE CASCADE
);
CREATE INDEX code_index_cards_search ON code_index_cards USING GIN(search_vector);
