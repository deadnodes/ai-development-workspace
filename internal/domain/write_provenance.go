package domain

// IsWriteProvenanceEvent defines the narrow journal index consumed by write-side
// policy: source-retention ancestry/restore barriers, worker report admission,
// artifact-pin release validation, build-close replay and retention fingerprints.
// History, local Git plan lookup, resume and exports use the complete journal
// outside writer locks.
// Keep persistence's indexed SQL predicate in sync (covered by PostgreSQL tests).
func IsWriteProvenanceEvent(e Event) bool {
	switch e.Action {
	case "close_deployment", "close_build", "restore_backup", "assemble_environment", "reconcile_composition", "update_environment_feature", "worker_report", "pin_artifact", "release_artifact_pin":
		return true
	}
	return e.Data.Action == "release_artifact_pin"
}
