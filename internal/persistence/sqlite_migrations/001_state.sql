CREATE TABLE IF NOT EXISTS control_revision(id integer PRIMARY KEY CHECK(id=1), revision bigint NOT NULL);
INSERT INTO control_revision(id,revision) VALUES(1,0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS records(
 id text PRIMARY KEY NOT NULL,
 kind text NOT NULL,
 product_id text,
 feature_id text,
 document text NOT NULL CHECK(json_valid(document)),
 position integer NOT NULL DEFAULT 0,
 created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS records_kind ON records(kind);
CREATE INDEX IF NOT EXISTS records_product ON records(product_id);
CREATE INDEX IF NOT EXISTS records_feature ON records(feature_id);
CREATE TABLE IF NOT EXISTS events(
 sequence integer PRIMARY KEY AUTOINCREMENT,
 id text NOT NULL UNIQUE,
 product_id text,
 feature_id text,
 document text NOT NULL CHECK(json_valid(document))
);
-- Cover bounded UI collection reads without sorting or scanning unrelated history.
CREATE INDEX IF NOT EXISTS records_ui_kind_product_position ON records(kind,product_id,position DESC,id);
CREATE INDEX IF NOT EXISTS records_ui_delivery_ops ON records(product_id,position DESC) WHERE kind='operations' AND coalesce(document->>'kind','') NOT LIKE 'REFRESH%';
CREATE INDEX IF NOT EXISTS records_ui_operation_steps ON records(product_id,(document->>'operation_id'),position DESC) WHERE kind='operation_steps';
CREATE INDEX IF NOT EXISTS records_ui_git_latest ON records(product_id,(coalesce(document->>'integration_id','')),(coalesce(document->>'repository_id','')),(coalesce(document->>'branch','')),position DESC) WHERE kind='git_observations';
CREATE INDEX IF NOT EXISTS records_ui_runtime_latest ON records(product_id,(coalesce(document->>'environment_id','')),(coalesce(document->>'application_id','')),position DESC) WHERE kind='runtime_snapshots';
CREATE INDEX IF NOT EXISTS records_ui_feature_history ON records(kind,product_id,feature_id,position DESC);
CREATE INDEX IF NOT EXISTS events_ui_feature_history ON events(product_id,feature_id,sequence DESC);
CREATE INDEX IF NOT EXISTS records_ui_refresh_latest ON records(product_id,(document->>'kind'),(coalesce(document->>'integration_id','')),(coalesce(document->>'environment_id','')),position DESC) WHERE kind='operations' AND document->>'kind' IN ('REFRESH_GIT','REFRESH_RUNTIME');
-- The write projection retains only events consumed by provenance policy.
-- History and exports remain in append sequence and outside the writer lock.
CREATE INDEX events_write_provenance ON events(sequence) WHERE
 document->>'action' IN ('close_deployment','close_build','restore_backup','delete_product','assemble_environment','reconcile_composition','update_environment_feature','worker_report','pin_artifact','release_artifact_pin')
 OR document->'data'->>'action'='release_artifact_pin';
CREATE INDEX events_product_feature_sequence ON events(product_id,feature_id,sequence);
CREATE TABLE runtime_observation_freshness (
    operation_id text NOT NULL,
    target text NOT NULL,
    observation_id text NOT NULL,
    observed_at datetime NOT NULL,
    healthy boolean NOT NULL,
    PRIMARY KEY(operation_id, target)
);
CREATE TABLE runtime_reports (
 product_id text NOT NULL, environment_id text NOT NULL, cluster text NOT NULL,
 namespace text NOT NULL, kind text NOT NULL, name text NOT NULL,
 document text NOT NULL, observed_at datetime NOT NULL, changed_at datetime NOT NULL, absent_since datetime NOT NULL,
 PRIMARY KEY(product_id, environment_id, cluster, namespace, kind, name)
);
CREATE TABLE runtime_report_clusters (
 product_id text NOT NULL, environment_id text NOT NULL, cluster text NOT NULL,
 reported_at datetime NOT NULL, workloads int NOT NULL,
 accepted_at datetime NOT NULL,
 journal_budget text NOT NULL DEFAULT '{}',
 PRIMARY KEY(product_id, environment_id, cluster)
);
CREATE INDEX runtime_report_clusters_accepted ON runtime_report_clusters(product_id,environment_id,accepted_at DESC);
CREATE INDEX records_worker_attempts ON records(product_id,(document->>'job_id'),id) WHERE kind='worker_attempts';
