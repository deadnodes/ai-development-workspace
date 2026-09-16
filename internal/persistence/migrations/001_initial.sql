CREATE TABLE IF NOT EXISTS control_revision(id integer PRIMARY KEY CHECK(id=1), revision bigint NOT NULL);
INSERT INTO control_revision(id,revision) VALUES(1,0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS records(
 id text PRIMARY KEY,
 kind text NOT NULL,
 product_id text,
 feature_id text,
 document jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS records_kind ON records(kind);
CREATE INDEX IF NOT EXISTS records_product ON records(product_id);
CREATE INDEX IF NOT EXISTS records_feature ON records(feature_id);
CREATE TABLE IF NOT EXISTS events(
 sequence bigserial PRIMARY KEY,
 id text NOT NULL UNIQUE,
 product_id text,
 feature_id text,
 document jsonb NOT NULL
);
