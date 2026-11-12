package domain

import "errors"

var ErrIdentityExists = errors.New("entity identity already exists")

// StateMutationScope is a trusted application-to-store contract, never client
// input. Collections outside the scope are read-only. AppendCollections also
// forbid changes to existing elements, including their nested values.
// Unknown callbacks use the fully isolated Store.Update contract instead.
type StateMutationScope struct {
	AllCollections     bool
	RequireAbsentIDs   []string
	EventReferences    []string
	MutableCollections []string
	AppendCollections  []string
}
