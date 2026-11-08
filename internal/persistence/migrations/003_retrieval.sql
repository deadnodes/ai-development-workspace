-- Disposable retrieval index over selected authored text. Source records/audit remain authoritative.
CREATE OR REPLACE FUNCTION rcp_retrieval_title(doc jsonb) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT coalesce(doc->>'title', doc->>'name', doc->>'kind', '')
$$;
CREATE OR REPLACE FUNCTION rcp_retrieval_text(doc jsonb) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT concat_ws(E'\n', doc->>'title',doc->>'name',doc->>'description',doc->>'problem',doc->>'goal',doc->>'requirements',doc->>'constraints',doc->>'context',doc->>'objective',doc->>'rationale',doc->>'acceptance_criteria',doc->>'body',doc->>'reason',doc->>'current',doc->>'completed',doc->>'remaining',doc->>'next',doc->>'warnings',doc->>'resolution')
$$;
CREATE TABLE IF NOT EXISTS retrieval_documents (
 id text PRIMARY KEY,
 kind text NOT NULL,
 product_id text NOT NULL,
 feature_id text NOT NULL DEFAULT '',
 title text NOT NULL,
 body text NOT NULL,
 recorded_status text NOT NULL DEFAULT '',
 search_vector tsvector NOT NULL
);
CREATE INDEX IF NOT EXISTS retrieval_documents_search ON retrieval_documents USING gin(search_vector);
CREATE INDEX IF NOT EXISTS retrieval_documents_product ON retrieval_documents(product_id,feature_id);
CREATE TABLE IF NOT EXISTS retrieval_edges (
 source_id text NOT NULL REFERENCES retrieval_documents(id) ON DELETE CASCADE,
 target_id text NOT NULL,
 relation text NOT NULL,
 PRIMARY KEY(source_id,target_id,relation)
);
CREATE INDEX IF NOT EXISTS retrieval_edges_target ON retrieval_edges(target_id);
CREATE OR REPLACE FUNCTION rcp_index_retrieval_record() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
 p text; t text; b text; ref text;
BEGIN
 IF TG_OP='DELETE' THEN
  IF OLD.kind NOT IN ('products','features','integrations','memories','repositories') THEN RETURN OLD; END IF;
  DELETE FROM retrieval_documents WHERE id=OLD.id;
  RETURN OLD;
 END IF;
 IF NEW.kind NOT IN ('products','features','integrations','memories','repositories') THEN
  IF TG_OP='UPDATE' AND OLD.kind IN ('products','features','integrations','memories','repositories') THEN
   DELETE FROM retrieval_documents WHERE id=OLD.id;
  END IF;
  RETURN NEW;
 END IF;
 p:=CASE WHEN NEW.kind='products' THEN NEW.id ELSE coalesce(NEW.product_id,'') END;
 t:=rcp_retrieval_title(NEW.document); b:=rcp_retrieval_text(NEW.document);
 INSERT INTO retrieval_documents(id,kind,product_id,feature_id,title,body,recorded_status,search_vector)
 VALUES(NEW.id,NEW.kind,p,coalesce(NEW.feature_id,''),t,b,coalesce(NEW.document->>'status',''),setweight(to_tsvector('simple',left(t,512)),'A')||setweight(to_tsvector('simple',left(b,32768)),'B'))
 ON CONFLICT(id) DO UPDATE SET kind=EXCLUDED.kind,product_id=EXCLUDED.product_id,feature_id=EXCLUDED.feature_id,title=EXCLUDED.title,body=EXCLUDED.body,recorded_status=EXCLUDED.recorded_status,search_vector=EXCLUDED.search_vector;
 DELETE FROM retrieval_edges WHERE source_id=NEW.id;
 IF NEW.kind<>'products' AND p<>'' AND p<>NEW.id THEN
  INSERT INTO retrieval_edges VALUES(NEW.id,p,'belongs_to_product') ON CONFLICT DO NOTHING;
 END IF;
 IF coalesce(NEW.feature_id,'')<>'' AND NEW.feature_id<>NEW.id THEN
  INSERT INTO retrieval_edges VALUES(NEW.id,NEW.feature_id,'belongs_to_feature') ON CONFLICT DO NOTHING;
 END IF;
 ref:=NEW.document->>'integration_id';
 IF coalesce(ref,'')<>'' AND ref<>NEW.id THEN
  INSERT INTO retrieval_edges VALUES(NEW.id,ref,'belongs_to_integration') ON CONFLICT DO NOTHING;
 END IF;
 IF jsonb_typeof(NEW.document->'dependencies')='array' THEN
  INSERT INTO retrieval_edges SELECT NEW.id,value,'depends_on' FROM jsonb_array_elements_text(NEW.document->'dependencies') WHERE value<>NEW.id AND value<>'' ON CONFLICT DO NOTHING;
 END IF;
 IF jsonb_typeof(NEW.document->'repositories')='array' THEN
  INSERT INTO retrieval_edges SELECT NEW.id,value,'uses_repository' FROM jsonb_array_elements_text(NEW.document->'repositories') WHERE value<>NEW.id AND value<>'' ON CONFLICT DO NOTHING;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER records_retrieval AFTER INSERT OR UPDATE OR DELETE ON records FOR EACH ROW EXECUTE FUNCTION rcp_index_retrieval_record();
-- Backfill only authored/searchable rows. Never touch a source document or copy an event.
INSERT INTO retrieval_documents(id,kind,product_id,feature_id,title,body,recorded_status,search_vector)
SELECT id,kind,CASE WHEN kind='products' THEN id ELSE coalesce(product_id,'') END,coalesce(feature_id,''),rcp_retrieval_title(document),rcp_retrieval_text(document),coalesce(document->>'status',''),setweight(to_tsvector('simple',left(rcp_retrieval_title(document),512)),'A')||setweight(to_tsvector('simple',left(rcp_retrieval_text(document),32768)),'B')
FROM records WHERE kind IN ('products','features','integrations','memories','repositories');
INSERT INTO retrieval_edges
SELECT id,product_id,'belongs_to_product' FROM retrieval_documents WHERE kind<>'products' AND product_id<>'' AND product_id<>id
UNION ALL SELECT id,feature_id,'belongs_to_feature' FROM retrieval_documents WHERE feature_id<>'' AND feature_id<>id
ON CONFLICT DO NOTHING;
INSERT INTO retrieval_edges
SELECT id,document->>'integration_id','belongs_to_integration' FROM records WHERE kind IN ('features','integrations','memories','repositories') AND coalesce(document->>'integration_id','')<>'' AND document->>'integration_id'<>id
ON CONFLICT DO NOTHING;
INSERT INTO retrieval_edges
SELECT r.id,j.value,fields.relation FROM records r
CROSS JOIN (VALUES('dependencies','depends_on'),('repositories','uses_repository')) fields(field,relation)
CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(r.document->fields.field)='array' THEN r.document->fields.field ELSE '[]'::jsonb END) j(value)
WHERE r.kind IN ('features','integrations','memories','repositories') AND j.value<>r.id AND j.value<>'' ON CONFLICT DO NOTHING;
