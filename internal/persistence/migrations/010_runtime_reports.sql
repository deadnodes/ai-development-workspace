CREATE TABLE runtime_reports (
 product_id text NOT NULL, environment_id text NOT NULL, cluster text NOT NULL,
 namespace text NOT NULL, kind text NOT NULL, name text NOT NULL,
 document jsonb NOT NULL, observed_at timestamptz NOT NULL, changed_at timestamptz NOT NULL, absent_since timestamptz NOT NULL,
 PRIMARY KEY(product_id, environment_id, cluster, namespace, kind, name)
);
CREATE TABLE runtime_report_clusters (
 product_id text NOT NULL, environment_id text NOT NULL, cluster text NOT NULL,
 reported_at timestamptz NOT NULL, workloads int NOT NULL,
 accepted_at timestamptz NOT NULL,
 journal_budget jsonb NOT NULL DEFAULT '{}',
 PRIMARY KEY(product_id, environment_id, cluster)
);
