# Release Control Plane

Start with [docs/AGENT_QUICKSTART.md](docs/AGENT_QUICKSTART.md) for startup, MCP connection and GitHub setup.

Standalone Go modular monolith. Read docs/ARCHITECTURE.md and docs/CONTRACT.md first. Domain/application behavior is shared by HTTP, embedded UI and MCP; never fork business logic in a transport.

During bootstrap consult docs/IMPLEMENTATION_STATE.md. After migration use the tracked project feature and resume MCP operation as the source of development context. The file will contain only the locator and recovery instructions.

Run gofmt, go vet, node --check web/static/app.js, go test -race ./... and go build ./cmd/server. Use TEST_DATABASE_URL to run real PostgreSQL integration tests (isolated test schemas). Do not claim DB tests passed if skipped. UI changes require browser validation. Preserve append-only result/event/handoff provenance and scope integrity. Production mutations require explicit approved candidate/promotion commands; never infer permission from a successful test.

Read [docs/RESPONSIBILITY_BOUNDARY.md](docs/RESPONSIBILITY_BOUNDARY.md) before adding execution capabilities. The service stores context and orchestrates explicit operations; it must not edit managed application source, resolve code conflicts itself, run arbitrary shell/SQL, or modify application business data. Agents/CI are external executors. Keep the GitOps writer bounded to configured deployment image fields.

Release policy: shared DEV compositions are for joint testing. Production candidates must be built from main after an explicitly approved server-orchestrated branch-only merge/fast-forward of selected work. The service may orchestrate Git refs through its provider; application source editing and conflict resolution remain external. Never treat DEV composition verification as automatic approval of a narrower main build. Read docs/TEST_AND_RELEASE_FLOW.md before implementing scenario or production workflows.
