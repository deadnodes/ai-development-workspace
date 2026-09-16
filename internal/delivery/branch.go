package delivery

import "context"

// RefProvider creates only absent refs; existing refs must match exactly. It never updates or deletes refs.
type RefProvider interface {
	EnsureRef(context.Context, Connection, string, string, string) (string, error)
}
