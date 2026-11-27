package application

func NewStandalone(store Store) *Service {
	service := New(store)
	service.standalone = true
	return service
}

func (s *Service) requireWorkers(action string) error {
	if s.standalone {
		return invalid("%s requires a configured worker; mcp --data does not run workers. Use the server MCP endpoint with the required worker configuration", action)
	}
	return nil
}

func workerCommand(action string) bool {
	switch action {
	case "refresh_integration_git", "refresh_repository_git", "refresh_environment_runtime",
		"deploy_integration", "deploy_existing_artifact", "create_integration_branch", "protect_integration_revision",
		"update_environment_feature", "assemble_environment", "reconcile_composition", "prepare_release_candidate", "promote_release_candidate",
		"queue_intelligence_task", "queue_worker_job", "queue_worker_publication", "reconcile_worker_publication", "refresh_registry_inventory",
		"reserve_candidate_dev", "capture_candidate_dev_baseline", "deploy_candidate_dev", "verify_candidate_dev_runtime",
		"request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration":
		return true
	default:
		return false
	}
}
