# Contributing

Start with [the agent quickstart](docs/AGENT_QUICKSTART.md) and [AGENTS.md](AGENTS.md).

## Product independence

- Runtime code must not assume an organization, Product ID, repository, team, environment name or private deployment path.
- Discover Products and Features from instance state. Repository identity belongs to the instance Registry; Product attachments and Feature/Integration scopes are explicit.
- Put deployment mappings, provider grants and external dependencies in application configuration. Keep credentials as secret references.
- Use fictional `example`/`acme` values in fixtures and documentation. Optional examples must not run at startup.
- Keep `.local/`, `.secrets/`, environment files, database backups and private deployment configuration out of Git and container build context.
- Preserve one application layer for UI, HTTP and MCP. Run the checks documented in README before submitting code changes.

## Publishing this repository

The tracked working tree contains generic source and examples. Existing Git history may still contain previous organization-specific documentation and author metadata; removing text from HEAD does not remove it from history. Review the intended publication history before pushing. A new clean source export is an option; do not rewrite or discard the local history automatically.

A public repository needs an explicit license to grant reuse rights. No license has been selected yet. The owner must choose and add LICENSE before describing the project as open source. Publication, license selection and Git history rewriting are separate from making the application product-independent.
