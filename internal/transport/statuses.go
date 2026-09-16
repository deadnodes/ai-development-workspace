package transport

import "releasecontrol/internal/domain"

// StatusSchema publishes the domain lifecycle without transport-specific rules.
func StatusSchema() map[string]any {
	return map[string]any{"catalog": domain.StatusCatalog(), "transitions": domain.StatusTransitions(), "repository_roles": domain.RepositoryRoles(), "component_kinds": domain.ComponentKinds(), "publication_formats": domain.PublicationFormats()}
}
