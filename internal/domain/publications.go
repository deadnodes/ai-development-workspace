package domain

// PublicationTarget describes an external package destination. It does not
// grant publishing permission or imply this instance operates that registry.
type PublicationTarget struct {
	Meta
	ApplicationID string `json:"application_id"`
	Format        string `json:"format"`
	RegistryURL   string `json:"registry_url"`
	PackageName   string `json:"package_name"`
}

// PackageArtifact is immutable actor-attested provenance. Registry availability
// and successful external publication are not inferred from this record.
type PackageArtifact struct {
	Meta
	IntegrationID       string `json:"integration_id,omitempty"`
	ApplicationID       string `json:"application_id"`
	RepositoryID        string `json:"repository_id"`
	SourceCommit        string `json:"source_commit"`
	PublicationTargetID string `json:"publication_target_id"`
	Version             string `json:"version"`
	Checksum            string `json:"checksum"`
	URI                 string `json:"uri"`
	BuildURL            string `json:"build_url,omitempty"`
}

func PublicationFormats() []string {
	return []string{"npm", "pypi", "nuget", "maven", "oci", "generic"}
}
