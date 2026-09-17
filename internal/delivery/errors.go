package delivery

import "errors"

// ErrNotFound means that a provider resource no longer exists. Callers can
// decide whether that is expected lifecycle state (for example, a merged
// feature branch deleted after its pull request) or an actionable failure.
var ErrNotFound = errors.New("provider resource not found")
