package runtimeobserver

import (
	"context"
	"os"
	"testing"
)

// Explicitly opt-in. Only GET requests; config contains secret references, never bytes.
func TestLiveReadOnly(t *testing.T) {
	path := os.Getenv("RCP_TEST_OBSERVERS_FILE")
	if path == "" {
		t.Skip("set RCP_TEST_OBSERVERS_FILE for live read-only observation")
	}
	observers, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	for _, o := range observers {
		result := o.Observe(context.Background(), os.Getenv("RCP_TEST_GITOPS_COMMIT"), os.Getenv("RCP_TEST_ARTIFACT_DIGEST"))
		t.Log(result.Details)
	}
}
