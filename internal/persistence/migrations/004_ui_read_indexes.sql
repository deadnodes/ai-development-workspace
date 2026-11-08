-- Cover bounded UI collection reads without sorting or scanning unrelated history.
CREATE INDEX IF NOT EXISTS records_ui_kind_product_position ON records(kind,product_id,position DESC,id);
CREATE INDEX IF NOT EXISTS records_ui_delivery_ops ON records(product_id,position DESC) WHERE kind='operations' AND coalesce(document->>'kind','') NOT LIKE 'REFRESH%';
CREATE INDEX IF NOT EXISTS records_ui_operation_steps ON records(product_id,(document->>'operation_id'),position DESC) WHERE kind='operation_steps';
CREATE INDEX IF NOT EXISTS records_ui_git_latest ON records(product_id,(coalesce(document->>'integration_id','')),(coalesce(document->>'repository_id','')),(coalesce(document->>'branch','')),position DESC) WHERE kind='git_observations';
CREATE INDEX IF NOT EXISTS records_ui_runtime_latest ON records(product_id,(coalesce(document->>'environment_id','')),(coalesce(document->>'application_id','')),position DESC) WHERE kind='runtime_snapshots';
CREATE INDEX IF NOT EXISTS records_ui_feature_history ON records(kind,product_id,feature_id,position DESC);
CREATE INDEX IF NOT EXISTS events_ui_feature_history ON events(product_id,feature_id,sequence DESC);
CREATE INDEX IF NOT EXISTS records_ui_refresh_latest ON records(product_id,(document->>'kind'),(coalesce(document->>'integration_id','')),(coalesce(document->>'environment_id','')),position DESC) WHERE kind='operations' AND document->>'kind' IN ('REFRESH_GIT','REFRESH_RUNTIME');
