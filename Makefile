.PHONY: run build test check db
DATABASE_URL ?= postgres://releasecontrol:releasecontrol@localhost:55432/releasecontrol?sslmode=disable
export DATABASE_URL

db:
	docker compose up -d --wait db
run:
	go run ./cmd/server
build:
	go build -trimpath -o bin/release-control ./cmd/server
test:
	go test -race ./...
check:
	test -z "$$(gofmt -l cmd internal web)"
	go vet ./...
	node --check web/static/app.js
	node web/check.mjs
	go test -race ./...
	go build -trimpath -o bin/release-control ./cmd/server
