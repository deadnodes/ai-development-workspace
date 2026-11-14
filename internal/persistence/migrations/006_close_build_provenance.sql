-- Build-close request-key replay must survive a cold write projection.
-- CREATE INDEX takes a SHARE lock: reads continue, writes wait briefly.
CREATE INDEX IF NOT EXISTS events_close_build_provenance ON events(sequence)
 WHERE document->>'action'='close_build';
