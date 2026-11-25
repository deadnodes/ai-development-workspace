package agentguide

import "testing"

func TestRegistryActionsExist(t *testing.T) {
	if err := ValidateActionCoverage(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDocumentationOnlyShrinks(t *testing.T) {
	if got := len(PendingDocumentation()); got > 192 {
		t.Fatalf("pendingDocumentation grew: got %d; updated bound is 192", got)
	}
}
