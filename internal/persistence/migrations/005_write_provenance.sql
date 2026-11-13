-- The write projection retains only events consumed by provenance policy.
-- History and exports remain in append sequence and outside the writer lock.
CREATE INDEX events_write_provenance ON events(sequence) WHERE
 document->>'action' IN ('restore_backup','assemble_environment','reconcile_composition','update_environment_feature','worker_report','pin_artifact','release_artifact_pin')
 OR document->'data'->>'action'='release_artifact_pin';
CREATE INDEX events_product_feature_sequence ON events(product_id,feature_id,sequence);
