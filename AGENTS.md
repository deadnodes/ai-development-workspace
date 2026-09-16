# Release Control Plane

Standalone Go modular monolith. Read docs/ARCHITECTURE.md and docs/CONTRACT.md first. Domain/application behavior is shared by HTTP, embedded UI and MCP; never fork business logic in a transport.

During bootstrap consult docs/IMPLEMENTATION_STATE.md. After migration use the tracked project feature and resume MCP operation as the source of development context. The file will contain only the locator and recovery instructions.

Run gofmt, go vet, node --check web/static/app.js, go test -race ./... and go build ./cmd/server. Use TEST_DATABASE_URL to run real PostgreSQL integration tests (isolated test schemas). Do not claim DB tests passed if skipped. UI changes require browser validation. Preserve append-only result/event/handoff provenance and scope integrity. No automatic Git/Flux/Kubernetes production operations.
