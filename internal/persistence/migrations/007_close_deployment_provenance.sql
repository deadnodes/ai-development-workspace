CREATE INDEX IF NOT EXISTS events_close_deployment_provenance ON events(sequence) WHERE document->>'action'='close_deployment';
