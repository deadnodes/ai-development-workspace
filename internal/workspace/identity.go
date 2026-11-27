package workspace

import (
	"net/url"
	"strings"
	"unicode"

	"releasecontrol/internal/domain"
)

// RemoteIdentity equates SSH/HTTPS clone spellings, not merely directory names.
// Non-default ports remain distinct. Only GitHub paths are case insensitive.
func RemoteIdentity(raw string) string {
	clean, ok := sanitizeRemote(strings.TrimSpace(raw))
	if !ok {
		return ""
	}
	if strings.HasPrefix(clean, "git@") && !strings.Contains(clean, "://") {
		parts := strings.SplitN(strings.TrimPrefix(clean, "git@"), ":", 2)
		clean = "ssh://git@" + parts[0] + "/" + parts[1]
	}
	u, err := url.Parse(clean)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !((u.Scheme == "https" && port == "443") || (u.Scheme == "ssh" && port == "22")) {
		host += ":" + port
	}
	path := strings.TrimSuffix(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if path == "" || strings.Contains(path, "//") || strings.ContainsAny(path, "@?#\\") || strings.ContainsFunc(path, unicode.IsControl) || domain.CodeHasSecret(host+"/"+path) {
		return ""
	}
	for _, p := range strings.Split(path, "/") {
		if p == "." || p == ".." {
			return ""
		}
	}
	if host == "github.com" {
		path = strings.ToLower(path)
	}
	return host + "/" + path
}

func CloneURLHasCredentials(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.ContainsAny(raw, "?#") {
		return true
	}
	if strings.HasPrefix(raw, "ssh://git@") || strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		_, tail, _ := strings.Cut(raw, "@")
		return strings.Contains(tail, "@") || domain.CodeHasSecret(tail)
	}
	if domain.CodeHasSecret(raw) {
		return true
	}
	if _, tail, ok := strings.Cut(raw, "://"); ok {
		authority, _, _ := strings.Cut(tail, "/")
		return strings.Contains(authority, "@")
	}
	return strings.Contains(raw, "@")
}

func CodeRepositoryIdentity(raw string) string {
	if len(raw) > domain.CodeRepositoryURLBytes {
		return ""
	}
	return RemoteIdentity(raw)
}
