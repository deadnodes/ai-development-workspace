# Bootstrap scaffold retired

Development state is now tracked inside Release Control Plane.

- Product: `rcp` — Release Control Plane
- Feature: `rcp-bootstrap` — RCP-001 · First usable control plane
- Migration task: `rcp-migrate-state` — INT-02 · Migrate bootstrap development state
- UI: http://127.0.0.1:8090/#rcp-bootstrap
- MCP: `resume` with `{"feature_id":"rcp-bootstrap"}`
- HTTP: `GET /api/features/rcp-bootstrap/context`

Do not add ongoing progress here. Use integrations, decisions, findings and structured handoffs in the application. Architecture and contract documentation remain versioned in Git.

Recovery: `docker compose --profile app up -d --build` starts the app and its persistent PostgreSQL volume. If starting with a new empty database, `node scripts/dogfood.mjs` creates initial tracked context; it is an explicit idempotent bootstrap, not a replacement for a database backup. Back up the named PostgreSQL volume or use `pg_dump` to preserve subsequent development history.
