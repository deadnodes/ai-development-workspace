package domain

import (
	"fmt"
	"slices"
)

func RepositoryRoles() []string {
	return []string{"APPLICATION", "LIBRARY", "MIXED", "GITOPS"}
}
func ComponentKinds() []string { return []string{"APPLICATION", "LIBRARY"} }
func IsSourceRole(role string) bool {
	return slices.Contains([]string{"APPLICATION", "LIBRARY", "MIXED"}, role)
}

// ComponentKind returns the persisted delivery kind.
func ComponentKind(v Application) string {
	return v.Kind
}
func RepositoryAllows(role, kind string) bool {
	if role == "MIXED" {
		return slices.Contains(ComponentKinds(), kind)
	}
	return (role == "APPLICATION" && kind == "APPLICATION") || (role == "LIBRARY" && kind == "LIBRARY")
}

// ValidateRepositoryKinds also protects backup/store callbacks from introducing
// unsupported purpose labels, including missing persisted classifications.
func ValidateRepositoryKinds(st State) error {
	for _, r := range st.Repositories {
		if !slices.Contains(RepositoryRoles(), r.Role) {
			return fmt.Errorf("invalid repository role %q for %s", r.Role, r.ID)
		}
	}
	for _, r := range st.RepositoryBindings {
		if !slices.Contains(RepositoryRoles(), r.Role) {
			return fmt.Errorf("invalid repository binding role %q for %s", r.Role, r.ID)
		}
	}
	for _, a := range st.Applications {
		if !slices.Contains(ComponentKinds(), ComponentKind(a)) {
			return fmt.Errorf("invalid component kind %q for %s", a.Kind, a.ID)
		}
	}
	for _, p := range st.PublicationTargets {
		if !slices.Contains(PublicationFormats(), p.Format) {
			return fmt.Errorf("invalid publication format %q for %s", p.Format, p.ID)
		}
	}
	return nil
}
