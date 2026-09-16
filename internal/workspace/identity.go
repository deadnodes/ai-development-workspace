package workspace

import (
	"net/url"
	"strings"
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
	if path == "" || strings.Contains(path, "//") {
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
