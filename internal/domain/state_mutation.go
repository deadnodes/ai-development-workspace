package domain

import "errors"

var ErrJournalNotEmpty = errors.New("instance journal is not empty")

var ErrIdentityExists = errors.New("entity identity already exists")

// StateMutationScope is a trusted application-to-store contract, never client
// input. Collections outside the scope are read-only. AppendCollections also
// forbid changes to existing elements, including their nested values.
// Unknown callbacks use the fully isolated Store.Update contract instead.
type StateMutationScope struct {
	// RequireEmptyJournal checks all persisted events under the writer lock.
	RequireEmptyJournal bool
	AllCollections      bool
	RequireAbsentIDs    []string
	// Record reuse is admitted by application replay policy; orphan journal IDs
	// still cannot become new record identities.
	AllowExistingRecords bool
	EventReferences      []string
	MutableCollections   []string
	AppendCollections    []string
}
