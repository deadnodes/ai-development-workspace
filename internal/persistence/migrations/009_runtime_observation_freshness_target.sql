ALTER TABLE runtime_observation_freshness ADD COLUMN IF NOT EXISTS target text NOT NULL DEFAULT '';
ALTER TABLE runtime_observation_freshness DROP CONSTRAINT IF EXISTS runtime_observation_freshness_pkey;
ALTER TABLE runtime_observation_freshness ADD CONSTRAINT runtime_observation_freshness_pkey PRIMARY KEY (operation_id, target);
