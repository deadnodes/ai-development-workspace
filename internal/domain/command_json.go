package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// UnmarshalJSON preserves the exact integer selection token through the command's
// generic data envelope. Float64 would silently round generations above 2^53.
func (c *Command) UnmarshalJSON(raw []byte) error {
	var scope struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(raw, &scope); err != nil {
		return err
	}
	if scope.Action == "reserve_candidate_dev" || scope.Action == "release_candidate_dev" || scope.Action == "deploy_candidate_dev" || scope.Action == "verify_candidate_dev_runtime" || scope.Action == "capture_candidate_dev_baseline" {
		if err := uniqueReservationJSON(raw); err != nil {
			return err
		}
	}
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

// uniqueReservationJSON rejects duplicate/case-variant keys before generic-map
// decoding can collapse them. It is deliberately scoped to reservation commands.
func uniqueReservationJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	nodes := 0
	var walk func(int) error
	walk = func(depth int) error {
		nodes++
		if depth > 16 || nodes > 50000 {
			return fmt.Errorf("reservation JSON bounds exceeded")
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			seen := map[string]bool{}
			for dec.More() {
				keyToken, e := dec.Token()
				if e != nil {
					return e
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("JSON key required")
				}
				if seen[key] || key != strings.ToLower(key) {
					return fmt.Errorf("duplicate or noncanonical reservation key %s", key)
				}
				seen[key] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		} else if delim == '[' {
			for dec.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		} else {
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing reservation JSON")
	}
	return nil
}
