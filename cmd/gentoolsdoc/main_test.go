package main

import (
	"os"
	"testing"
)

func TestGeneratedReferenceCurrent(t *testing.T) {
	want, err := os.ReadFile("../../docs/AGENT_TOOLS.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != render() {
		t.Fatal("docs/AGENT_TOOLS.md is stale; regenerate with `go run ./cmd/gentoolsdoc`")
	}
}
