package domain

func RestoreEvidenceCollections() [4]string {
	return [4]string{"interrupted_operations", "quarantined_builds", "quarantined_deployments", "quarantined_signatures"}
}

func (e Event) HasUnscopedProductEvidence() bool {
	if e.ProductID != "" {
		return false
	}
	switch e.Action {
	case "create_product", "restore_backup_evidence":
		return true
	case "restore_backup":
		for _, collection := range RestoreEvidenceCollections() {
			if _, exists := e.Data.Data[collection]; exists {
				return true
			}
		}
	}
	return false
}
