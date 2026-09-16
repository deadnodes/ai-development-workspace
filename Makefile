.PHONY: run build test check db

db:
	docker compose up -d --wait db
run:
	go run ./cmd/server
build:
	go build -trimpath -ldflags="-s -w" -o bin/release-control ./cmd/server
test:
	go test -race ./...
check:
	test -z "$$(gofmt -l cmd internal web)"
	go vet ./...
	node --check web/static/app.js
	node --check web/static/external.js
	node --check web/static/flow.js
	node --check web/static/library.js
	node --check web/static/workspace.js
	node --check web/static/live.js
	node --check web/static/graph.js
	node --check scripts/export-configuration.mjs
	node --check scripts/backup.mjs
	node web/check.mjs
	node web/workspace-check.mjs
	node web/live-check.mjs
	node web/routes-check.mjs
	node web/graph-check.mjs
	python3 -m unittest discover -s examples/github-actions -p 'test_*.py'
	go test -race ./...
	go build -trimpath -ldflags="-s -w" -o bin/release-control ./cmd/server
