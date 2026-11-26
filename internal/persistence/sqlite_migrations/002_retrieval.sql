CREATE TABLE IF NOT EXISTS retrieval_documents (
 id text PRIMARY KEY,
 kind text NOT NULL,
 product_id text NOT NULL,
 feature_id text NOT NULL DEFAULT '',
 title text NOT NULL,
 body text NOT NULL,
 recorded_status text NOT NULL DEFAULT '',
 FOREIGN KEY(id) REFERENCES records(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS retrieval_documents_product ON retrieval_documents(product_id,feature_id);
CREATE TABLE IF NOT EXISTS retrieval_edges (
 source_id text NOT NULL REFERENCES retrieval_documents(id) ON DELETE CASCADE,
 target_id text NOT NULL,
 relation text NOT NULL,
 PRIMARY KEY(source_id,target_id,relation)
);
CREATE INDEX IF NOT EXISTS retrieval_edges_target ON retrieval_edges(target_id);
CREATE VIRTUAL TABLE retrieval_search USING fts5(title, body, tokenize='unicode61');
CREATE TRIGGER retrieval_document_insert AFTER INSERT ON retrieval_documents BEGIN
 INSERT INTO retrieval_search(rowid,title,body) VALUES(NEW.rowid,substr(NEW.title,1,512),substr(NEW.body,1,32768));
END;
CREATE TRIGGER retrieval_document_delete AFTER DELETE ON retrieval_documents BEGIN
 DELETE FROM retrieval_search WHERE rowid=OLD.rowid;
END;
CREATE VIEW retrieval_source AS
 SELECT id,kind,CASE WHEN kind='products' THEN id ELSE coalesce(product_id,'') END AS product_id,
 coalesce(feature_id,'') AS feature_id,coalesce(document->>'title',document->>'name',document->>'kind','') AS title,
 concat_ws(char(10),document->>'title',document->>'name',document->>'description',document->>'problem',document->>'goal',document->>'requirements',document->>'constraints',document->>'context',document->>'objective',document->>'rationale',document->>'acceptance_criteria',document->>'body',document->>'reason',document->>'current',document->>'completed',document->>'remaining',document->>'next',document->>'warnings',document->>'resolution') AS body,coalesce(document->>'status','') AS recorded_status
 FROM records WHERE kind IN ('products','features','integrations','memories','repositories');
CREATE VIEW retrieval_source_edges AS
 SELECT id AS source_id, product_id AS target_id,'belongs_to_product' AS relation FROM retrieval_documents WHERE kind<>'products'
 UNION ALL SELECT id,feature_id,'belongs_to_feature' FROM retrieval_documents
 UNION ALL SELECT id,document->>'integration_id','belongs_to_integration' FROM records
 UNION ALL SELECT r.id,j.value,'depends_on' FROM records r JOIN json_each(r.document,'$.dependencies') j WHERE json_type(r.document,'$.dependencies')='array' AND j.type='text'
 UNION ALL SELECT r.id,j.value,'uses_repository' FROM records r JOIN json_each(r.document,'$.repositories') j WHERE json_type(r.document,'$.repositories')='array' AND j.type='text';
CREATE TRIGGER records_retrieval_insert AFTER INSERT ON records WHEN NEW.kind IN ('products','features','integrations','memories','repositories') BEGIN
 INSERT INTO retrieval_documents SELECT * FROM retrieval_source WHERE id=NEW.id;
 INSERT OR IGNORE INTO retrieval_edges SELECT source_id,target_id,relation FROM retrieval_source_edges WHERE source_id=NEW.id AND target_id<>'' AND target_id<>source_id;
END;
CREATE TRIGGER records_retrieval_update AFTER UPDATE ON records WHEN NEW.kind IN ('products','features','integrations','memories','repositories') BEGIN
 DELETE FROM retrieval_documents WHERE id=OLD.id;
 INSERT INTO retrieval_documents SELECT * FROM retrieval_source WHERE id=NEW.id;
 INSERT OR IGNORE INTO retrieval_edges SELECT source_id,target_id,relation FROM retrieval_source_edges WHERE source_id=NEW.id AND target_id<>'' AND target_id<>source_id;
END;
