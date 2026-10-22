package domain

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON preserves the exact integer selection token through the command's
// generic data envelope. Float64 would silently round generations above 2^53.
func (c *Command) UnmarshalJSON(raw []byte) error {
	type command Command
	var value command
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if token, ok := envelope.Data["expected_selection"]; ok {
		var exact any
		dec = json.NewDecoder(bytes.NewReader(token))
		dec.UseNumber()
		if err := dec.Decode(&exact); err != nil {
			return err
		}
		value.Data["expected_selection"] = exact
	}
	*c = Command(value)
	return nil
}
