-- Build-close request-key replay must survive a cold write projection.
DROP INDEX events_write_provenance;
CREATE INDEX events_write_provenance ON events(sequence) WHERE
 document->>'action' IN ('close_build','restore_backup','assemble_environment','reconcile_composition','update_environment_feature','worker_report','pin_artifact','release_artifact_pin')
 OR document->'data'->>'action'='release_artifact_pin';
