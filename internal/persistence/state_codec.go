package persistence

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"releasecontrol/internal/domain"
)

// The schema is derived from State's existing persisted JSON fields, never a
// second handwritten collection registry. Unsupported future shapes fail closed.
func codecStateSchema() (map[string]int, error) {
	return codecCollectionSchema(reflect.TypeOf(domain.State{}))
}

func codecCollectionSchema(typ reflect.Type) (map[string]int, error) {
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("unsupported State shape")
	}
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	if typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler) || reflect.PointerTo(typ).Implements(unmarshaler) || typ.Implements(textMarshaler) || reflect.PointerTo(typ).Implements(textMarshaler) || typ.Implements(textUnmarshaler) || reflect.PointerTo(typ).Implements(textUnmarshaler) {
		return nil, fmt.Errorf("custom State JSON codec unsupported")
	}
	fields := make(map[string]int, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := f.Tag.Get("json")
		if f.PkgPath != "" || f.Anonymous || f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.Struct || !codecCollectionKey(key) {
			return nil, fmt.Errorf("unsupported State collection %s", f.Name)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate State collection %s", key)
		}
		fields[key] = i
	}
	return fields, nil
}

// Compiled State collection keys use [a-z][a-z0-9_]*. Admit that
// deliberately restricted subset of encoding/json names, rejecting invalid tags
// and unsupported future naming/options rather than inventing a field alias.
func codecCollectionKey(key string) bool {
	if len(key) == 0 || key[0] < 'a' || key[0] > 'z' {
		return false
	}
	for i := 1; i < len(key); i++ {
		b := key[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_') {
			return false
		}
	}
	return true
}

func codecGroupedFallback(groups map[string][]json.RawMessage) (domain.State, error) {
	st := domain.EmptyState()
	raw, e := json.Marshal(groups)
	if e != nil {
		return st, e
	}
	e = json.Unmarshal(raw, &st)
	return st, e
}

func decodeStoredDocuments(groups map[string][]json.RawMessage) (domain.State, error) {
	st := domain.EmptyState()
	schema, e := codecStateSchema()
	if e != nil {
		return st, e
	}
	// Preserve encoding/json's exact legacy handling of unknown/case-variant keys
	// and malformed RawMessages, including error types and returned partial State.
	for kind := range groups {
		if _, ok := schema[kind]; !ok {
			return codecGroupedFallback(groups)
		}

	}
	value := reflect.ValueOf(&st).Elem()
	for kind, docs := range groups {
		field := value.Field(schema[kind])
		if docs == nil {
			field.SetZero()
			continue
		}
		field.Set(reflect.MakeSlice(field.Type(), len(docs), len(docs)))
		for i, raw := range docs {
			if e = json.Unmarshal(raw, field.Index(i).Addr().Interface()); e != nil {
				return codecGroupedFallback(groups)
			}
		}
	}
	return st, nil
}

func codecDocuments(st domain.State) (map[string][]json.RawMessage, error) {
	schema, e := codecStateSchema()
	if e != nil {
		return nil, e
	}
	out := make(map[string][]json.RawMessage, len(schema)-1)
	value := reflect.ValueOf(&st).Elem()
	// Declaration order preserves the first encoding error of json.Marshal(State).
	typ := value.Type()
	for i := 0; i < value.NumField(); i++ {
		key := typ.Field(i).Tag.Get("json")
		field := value.Field(i)
		var rows []json.RawMessage
		if key != "events" && !field.IsNil() {
			rows = make([]json.RawMessage, field.Len())
		}
		for n := 0; n < field.Len(); n++ {
			// Slice elements are addressable in the baseline State encoder as well.
			raw, err := json.Marshal(field.Index(n).Addr().Interface())
			if err != nil {
				return nil, err
			}
			if key != "events" {
				rows[n] = raw
			}
		}
		if key != "events" {
			out[key] = rows
		}
	}
	return out, nil
}
