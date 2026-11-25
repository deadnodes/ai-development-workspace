package agentguide

import (
	"encoding/json"
	"strings"

	"releasecontrol/internal/domain"
)

type ToolEntry struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Purpose   string   `json:"purpose"`
	When      string   `json:"when_to_use"`
	Inputs    []Field  `json:"inputs"`
	Output    string   `json:"output"`
	Errors    []string `json:"errors"`
	Example   string   `json:"example"`
	Related   []string `json:"related"`
	Authority string   `json:"authority"`
}
type Field struct {
	Name, Type string
	Required   bool
	Meaning    string
}

// ToolGuide returns bounded reference data. The action list is sourced directly
// from the authoritative domain action catalog.
func ToolGuide(name string) any {
	entries := make([]ToolEntry, 0, len(domain.Actions))
	for _, action := range domain.Actions {
		entries = append(entries, ToolEntry{Name: action, Kind: "execute_action", Purpose: "Submit this named engineering command through the shared application layer; consult /api/schema for its exact action-specific fields.", When: "Use through execute after reading current scoped context; do not use when the requested operation is outside this action's documented contract.", Inputs: []Field{{Name: "action", Type: "string", Required: true, Meaning: "Must equal this action name"}, {Name: "actor", Type: "string", Required: true, Meaning: "Attribution, not authentication"}, {Name: "data", Type: "object", Required: true, Meaning: "Fields defined by this action in /api/schema"}}, Output: "Command result with persisted record or operation handle, depending on action.", Errors: []string{"VALIDATION: correct fields from /api/schema", "NOT_FOUND: refresh scoped context", "STALE_FENCE: reacquire execution fence"}, Example: `{"action":"` + action + `","actor":"agent/example","data":{}}`, Authority: "The action and its documentation do not authorize external mutations; follow the action's own approval, scope and policy checks."})
	}
	if name != "" {
		for _, e := range entries {
			if e.Name == name {
				return e
			}
		}
		return map[string]string{"error": "unknown guide name"}
	}
	type item struct{ Name, Kind, Purpose string }
	index := make([]item, 0, len(entries))
	for _, e := range entries {
		index = append(index, item{e.Name, e.Kind, e.Purpose})
	}
	b, _ := json.Marshal(index)
	if len(b) > 32*1024 {
		return map[string]string{"error": "guide index exceeds output bound"}
	}
	return map[string]any{"area": "execute actions", "entries": index, "note": strings.TrimSpace("Inputs are defined authoritatively by GET /api/schema; examples are minimal envelopes.")}
}
