CREATE TABLE runtime_observation_freshness (
    operation_id text PRIMARY KEY,
    observation_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    healthy boolean NOT NULL
);
