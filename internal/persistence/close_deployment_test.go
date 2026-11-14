package persistence

import (
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestCloseDeploymentProvenanceMigration(t *testing.T) {
	sql, err := migrations.ReadFile("migrations/007_close_deployment_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "CREATE INDEX IF NOT EXISTS events_close_deployment_provenance ON events(sequence) WHERE document->>'action'='close_deployment'") {
		t.Fatal(string(sql))
	}
	if !domain.IsWriteProvenanceEvent(domain.Event{Action: "close_deployment"}) || !strings.Contains(writeEventPredicate, "'close_deployment'") {
		t.Fatal("missing write provenance")
	}
}
