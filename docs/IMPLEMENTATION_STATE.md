# Bootstrap scaffold retired

Development progress belongs in the configured Control Plane instance, not this file.

1. Start or recover the instance: `docker compose --profile app up -d --build`.
2. Call MCP `get_state` and select the relevant Product and Feature.
3. Call `resume` with that Feature ID; follow its latest structured handoff.

There is no mandatory Product, Feature ID, organization or deployment topology. A fresh database is empty. Optionally run `node scripts/dogfood.mjs` to seed historical bootstrap context for developing Release Control Plane itself; it is not required for using the application and is not a backup or current release status.

Back up PostgreSQL to preserve your instance's history. Keep database dumps, credentials and private product configuration outside the public repository. See [agent quickstart](AGENT_QUICKSTART.md). Do not add ongoing progress here.
