-- Preserve semantic append order when restoring a complete state in one transaction.
ALTER TABLE records ADD COLUMN position bigint NOT NULL DEFAULT 0;
WITH ordered AS (
 SELECT id, row_number() OVER (PARTITION BY kind ORDER BY created_at,id) - 1 AS n
 FROM records
)
UPDATE records SET position=ordered.n FROM ordered WHERE records.id=ordered.id;
