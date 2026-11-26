package domain

import "errors"

// ErrReadScopeNotFound also represents a scope the caller cannot see.
var ErrReadScopeNotFound = errors.New("read scope not found")
