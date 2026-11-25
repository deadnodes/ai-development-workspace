package agentguide

import "testing"

func TestRegistryActionsExist(t *testing.T) {
	if err := ValidateActionCoverage(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDocumentationOnlyShrinks(t *testing.T) {
	if got := len(PendingDocumentation()); got > 209 {
		t.Fatalf("pendingDocumentation grew: got %d; Stage 5A baseline is 209", got)
	}
}
