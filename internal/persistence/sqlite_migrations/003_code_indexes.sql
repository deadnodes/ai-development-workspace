CREATE TABLE code_indexes (
 product_id text NOT NULL REFERENCES records(id) ON DELETE CASCADE,
 repository_id text NOT NULL REFERENCES records(id) ON DELETE CASCADE,
 branch text NOT NULL, commit_sha text NOT NULL, digest text NOT NULL,
 indexed_at text NOT NULL, write_seq integer NOT NULL, actor text NOT NULL, truncation text NOT NULL CHECK(json_valid(truncation)),
 document text NOT NULL CHECK(json_valid(document)),
 PRIMARY KEY(product_id, repository_id, branch)
);
CREATE INDEX code_indexes_latest ON code_indexes(product_id, repository_id, write_seq DESC);
CREATE TABLE code_index_history (
 sequence integer PRIMARY KEY AUTOINCREMENT, product_id text NOT NULL, repository_id text NOT NULL, branch text NOT NULL,
 commit_sha text NOT NULL, digest text NOT NULL, indexed_at text NOT NULL,
 FOREIGN KEY(product_id, repository_id, branch) REFERENCES code_indexes ON DELETE CASCADE
);
CREATE INDEX code_index_history_scope ON code_index_history(product_id, repository_id, branch, sequence DESC);
CREATE TABLE code_index_cards (
 product_id text NOT NULL, repository_id text NOT NULL, branch text NOT NULL,
 ordinal integer NOT NULL, path text NOT NULL, kind text NOT NULL, symbol text NOT NULL, line integer NOT NULL,
 title text NOT NULL, text_body text NOT NULL, snippet text NOT NULL, words text NOT NULL,
 PRIMARY KEY(product_id, repository_id, branch, ordinal),
 FOREIGN KEY(product_id, repository_id, branch) REFERENCES code_indexes ON DELETE CASCADE
);
