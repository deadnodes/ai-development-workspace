package application

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"releasecontrol/internal/domain"
)

var packageChecksum = regexp.MustCompile(`^(sha256:[0-9a-f]{64}|sha512:[0-9a-f]{128})$`)

func publicationURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && strings.TrimSpace(value) == value
}
func publicationTarget(st *domain.State, id string) *domain.PublicationTarget {
	for i := range st.PublicationTargets {
		if st.PublicationTargets[i].ID == id {
			return &st.PublicationTargets[i]
		}
	}
	return nil
}
func publicationLibrary(st *domain.State, productID, applicationID string) (*domain.Application, error) {
	if !productExists(st, productID) {
		return nil, missing("product", productID)
	}
	app := applicationByID(st, applicationID)
	if app == nil || app.ProductID != productID || domain.ComponentKind(*app) != "LIBRARY" {
		return nil, invalid("publication requires a product-scoped LIBRARY component")
	}
	if err := repositories(st, []string{app.RepositoryID}, productID); err != nil {
		return nil, err
	}
	return app, nil
}
func applyPublication(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "configure_publication":
		var input struct {
			ApplicationID string `json:"application_id"`
			Format        string `json:"format"`
			RegistryURL   string `json:"registry_url"`
			PackageName   string `json:"package_name"`
		}
		if err := decode(c.Data, &input); err != nil {
			return nil, m, err
		}
		if _, err := publicationLibrary(st, c.ProductID, input.ApplicationID); err != nil {
			return nil, m, err
		}
		if !slices.Contains(domain.PublicationFormats(), input.Format) {
			return nil, m, invalid("unsupported publication format")
		}
		if !publicationURL(input.RegistryURL) {
			return nil, m, invalid("registry_url must be HTTPS without credentials, query or fragment")
		}
		if strings.TrimSpace(input.PackageName) == "" || input.PackageName != strings.TrimSpace(input.PackageName) || strings.ContainsAny(input.PackageName, "\r\n\t") {
			return nil, m, invalid("package_name required without surrounding whitespace")
		}
		target := domain.PublicationTarget{Meta: m, ApplicationID: input.ApplicationID, Format: input.Format, RegistryURL: input.RegistryURL, PackageName: input.PackageName}
		st.PublicationTargets = append(st.PublicationTargets, target)
		return target, m, nil
	case "record_package_artifact":
		var input struct {
			ApplicationID       string `json:"application_id"`
			PublicationTargetID string `json:"publication_target_id"`
			SourceCommit        string `json:"source_commit"`
			Version             string `json:"version"`
			Checksum            string `json:"checksum"`
			URI                 string `json:"uri"`
			BuildURL            string `json:"build_url"`
		}
		if err := decode(c.Data, &input); err != nil {
			return nil, m, err
		}
		app, err := publicationLibrary(st, c.ProductID, input.ApplicationID)
		if err != nil {
			return nil, m, err
		}
		target := publicationTarget(st, input.PublicationTargetID)
		if target == nil || target.ProductID != c.ProductID || target.ApplicationID != app.ID {
			return nil, m, invalid("publication target must belong to component and product")
		}
		if !fullSHA.MatchString(input.SourceCommit) || !packageChecksum.MatchString(input.Checksum) {
			return nil, m, invalid("full lowercase source SHA and canonical sha256/sha512 checksum required")
		}
		if strings.TrimSpace(input.Version) == "" || strings.TrimSpace(input.Version) != input.Version || strings.ContainsAny(input.Version, "\r\n\t") {
			return nil, m, invalid("nonempty version without surrounding whitespace required")
		}
		if !publicationURL(input.URI) || (input.BuildURL != "" && !publicationURL(input.BuildURL)) {
			return nil, m, invalid("artifact uri and optional build_url must be HTTPS without credentials, query or fragment")
		}
		if c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in == nil || in.ProductID != c.ProductID {
				return nil, m, invalid("integration must belong to product")
			}
			if len(in.Repositories) > 0 && !slices.Contains(in.Repositories, app.RepositoryID) {
				return nil, m, invalid("library repository outside integration scope")
			}
			f := feature(st, in.FeatureID)
			if f == nil || len(f.Repositories) > 0 && !slices.Contains(f.Repositories, app.RepositoryID) {
				return nil, m, invalid("library repository outside feature scope")
			}
			m.FeatureID = in.FeatureID
		}
		for _, record := range st.PackageArtifacts {
			previous := publicationTarget(st, record.PublicationTargetID)
			if record.ProductID == c.ProductID && record.ApplicationID == app.ID && record.Version == input.Version && previous != nil && previous.Format == target.Format && strings.TrimSuffix(previous.RegistryURL, "/") == strings.TrimSuffix(target.RegistryURL, "/") && previous.PackageName == target.PackageName && (record.Checksum != input.Checksum || record.SourceCommit != input.SourceCommit) {
				return nil, m, invalid("package version already has different immutable provenance")
			}
		}
		record := domain.PackageArtifact{Meta: m, IntegrationID: c.IntegrationID, ApplicationID: app.ID, RepositoryID: app.RepositoryID, SourceCommit: input.SourceCommit, PublicationTargetID: target.ID, Version: input.Version, Checksum: input.Checksum, URI: input.URI, BuildURL: input.BuildURL}
		st.PackageArtifacts = append(st.PackageArtifacts, record)
		return record, m, nil
	}
	return nil, m, invalid("unknown publication command")
}
