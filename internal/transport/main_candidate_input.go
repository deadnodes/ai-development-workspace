package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Named native candidate input has one canonical spelling and no ambiguous
// duplicate members. Domain validation still owns identity and authorization.
func uniqueMainCandidateJSON(raw []byte) error {
	if len(raw) > 1<<20 {
		return fmt.Errorf("main candidate input exceeds limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	nodes := 0
	var value func(int) error
	value = func(depth int) error {
		nodes++
		if depth > 16 || nodes > 50000 {
			return fmt.Errorf("main candidate input exceeds structure limits")
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, e := dec.Token()
				if e != nil {
					return e
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate JSON field")
				}
				seen[name] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON structure")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
