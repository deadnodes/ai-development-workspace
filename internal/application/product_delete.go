package application

import (
	"encoding/json"
	"releasecontrol/internal/domain"
)

func deleteProduct(st *domain.State, c domain.Command) error {
	var input struct {
		Name string `json:"name"`
	}
	if err := decode(c.Data, &input); err != nil {
		return err
	}
	var found bool
	for _, p := range st.Products {
		if p.ID == c.ProductID {
			found = true
			if input.Name != p.Name {
				return invalid("type the exact product name to confirm deletion")
			}
		}
	}
	if !found {
		return missing("product", c.ProductID)
	}
	for _, op := range st.Operations {
		if op.ProductID == c.ProductID && !terminalOperation(op.Status) {
			return invalid("product has unfinished operations; finish or cancel them before deletion")
		}
	}
	// State collections share product_id ownership. Instance registries and audit
	// events deliberately survive deletion; no external provider is invoked.
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var groups map[string][]json.RawMessage
	if err = json.Unmarshal(b, &groups); err != nil {
		return err
	}
	for kind, rows := range groups {
		if kind == "events" {
			continue
		}
		kept := make([]json.RawMessage, 0, len(rows))
		for _, row := range rows {
			var m domain.Meta
			if err = json.Unmarshal(row, &m); err != nil {
				return err
			}
			if m.ProductID == c.ProductID || (kind == "products" && m.ID == c.ProductID) {
				continue
			}
			kept = append(kept, row)
		}
		groups[kind] = kept
	}
	b, err = json.Marshal(groups)
	if err != nil {
		return err
	}
	// Decode into fresh storage: omitted fields must not survive from records
	// that previously occupied the same slice positions.
	var next domain.State
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	*st = next
	return nil
}
