package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func codecBaselineDocuments(st domain.State) (map[string][]json.RawMessage, error) {
	raw, e := json.Marshal(st)
	if e != nil {
		return nil, e
	}
	var out map[string][]json.RawMessage
	e = json.Unmarshal(raw, &out)
	delete(out, "events")
	return out, e
}
func codecFixture(n int) domain.State {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "<&界>"}}
	st.Operations = make([]domain.ExternalOperation, n)
	st.Events = make([]domain.Event, n*5/4)
	for i := range st.Operations {
		st.Operations[i] = domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprintf("op-%08d", i), ProductID: "p", Actor: "historical"}, Kind: "REFRESH_GIT", Status: "SUCCEEDED", Detail: "Retained immutable historical evidence"}
	}
	for i := range st.Events {
		st.Events[i] = domain.Event{ID: fmt.Sprintf("event-%08d", i), ProductID: "p", Actor: "historical", Action: "record_progress"}
	}
	return st
}
func codecAssertErrors(t *testing.T, a, b error) {
	t.Helper()
	if reflect.TypeOf(a) != reflect.TypeOf(b) || (a != nil && a.Error() != b.Error()) {
		t.Fatalf("error mismatch %T %v / %T %v", a, a, b, b)
	}
}
func TestCodecPrototypeParity(t *testing.T) {
	fixtures := []domain.State{{}, domain.EmptyState(), codecFixture(20)}
	// Every State collection key, nil/non-nil slices, ordering and record shapes.
	mixed := domain.EmptyState()
	v := reflect.ValueOf(&mixed).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		f.Set(reflect.MakeSlice(f.Type(), 2, 2))
	}
	fixtures = append(fixtures, mixed)
	complex := codecFixture(2)
	complex.Operations[0].Snapshot = &domain.DeliverySnapshot{}
	complex.Operations[0].StartedAt = new(time.Time)
	complex.Events[0].Data.Data = map[string]any{"escaped": "<>&\n\u2028", "nested": []any{float64(1), nil, map[string]any{"x": "界"}}}
	fixtures = append(fixtures, complex)
	custom := codecFixture(1)
	custom.WorkerJobs = []domain.WorkerJob{{Inputs: domain.WorkerInputs{Parameters: map[string]any{"custom": &codecCustomPayload{}}}}}
	fixtures = append(fixtures, custom)
	custom = codecFixture(1)
	custom.WorkerJobs = []domain.WorkerJob{{Inputs: domain.WorkerInputs{Parameters: map[string]any{"custom": &codecCustomPayload{Fail: true}}}}}
	fixtures = append(fixtures, custom)
	bad := codecFixture(1)
	bad.Events[0].Data.Data = map[string]any{"bad": math.NaN()}
	fixtures = append(fixtures, bad)
	bad = codecFixture(1)
	bad.Events[0].Data.Data = map[string]any{"bad": func() {}}
	fixtures = append(fixtures, bad)
	bad = codecFixture(1)
	bad.Operations[0].StartedAt = new(time.Time)
	*bad.Operations[0].StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	fixtures = append(fixtures, bad)
	for i, st := range fixtures {
		a, ae := codecBaselineDocuments(st)
		b, be := codecDocuments(st)
		codecAssertErrors(t, ae, be)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("document bytes/nil/keys/order mismatch fixture%d", i)
		}
		if ae == nil { // Deep-copy alias behavior covered below.
			x, xe := codecGroupedFallback(a)
			y, ye := decodeStoredDocuments(a)
			codecAssertErrors(t, xe, ye)
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("group decode mismatch fixture%d", i)
			}
		}
	}
	for _, groups := range []map[string][]json.RawMessage{
		nil, {}, {"unknown": {json.RawMessage(`{"x":1}`)}}, {"operations": nil}, {"operations": {}},
		{"OPERATIONS": {json.RawMessage(`{"id":"upper"}`)}, "operations": {json.RawMessage(`{"id":"lower"}`)}},
		{"events": {json.RawMessage(`{"id":"record-event"}`)}}, {"unknown": {json.RawMessage(`{"bad":`)}},
		{"products": {json.RawMessage(`{"id":5}`)}, "features": {json.RawMessage(`{"id":"after"}`)}},
		{"products": {json.RawMessage(`null`), json.RawMessage(`{"id":"next"}`)}},
	} {
		a, ae := codecGroupedFallback(groups)
		b, be := decodeStoredDocuments(groups)
		codecAssertErrors(t, ae, be)
		if !reflect.DeepEqual(a, b) {
			t.Fatal("edge group mismatch", groups)
		}
	}
	// Before documents must retain bytes after callbacks mutate nested state, and
	// decoded values must own their data rather than aliasing input byte buffers.
	st := codecFixture(1)
	before, e := codecDocuments(st)
	if e != nil {
		t.Fatal(e)
	}
	frozen := append([]byte(nil), before["operations"][0]...)
	st.Operations[0].Detail = "changed"
	after, e := codecDocuments(st)
	if e != nil || !reflect.DeepEqual(frozen, []byte(before["operations"][0])) || reflect.DeepEqual(before, after) {
		t.Fatal("before/after alias")
	}
	decoded, e := decodeStoredDocuments(before)
	if e != nil {
		t.Fatal(e)
	}
	for i := range before["operations"][0] {
		before["operations"][0][i] = 'x'
	}
	if decoded.Operations[0].Detail != "Retained immutable historical evidence" {
		t.Fatal("decoded values alias input")
	}
}
func TestCodecPrototypeUnsupportedSchema(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(5), reflect.TypeOf(struct {
		Scalar string `json:"scalar"`
	}{}), reflect.TypeOf(struct {
		Values []string `json:"values"`
	}{}), reflect.TypeOf(struct {
		Values []domain.Product `json:"values,omitempty"`
	}{}), reflect.StructOf([]reflect.StructField{{Name: "A", Type: reflect.TypeOf([]domain.Product{}), Tag: `json:"same"`}, {Name: "B", Type: reflect.TypeOf([]domain.Product{}), Tag: `json:"same"`}})} {
		if _, e := codecCollectionSchema(typ); e == nil {
			t.Fatal("unsupported future schema accepted", typ)
		}
	}
}

func BenchmarkCodecPrototype(b *testing.B) {
	st := codecFixture(20000)
	groups, e := codecBaselineDocuments(st)
	if e != nil {
		b.Fatal(e)
	}
	b.Run("documents-baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, e := codecBaselineDocuments(st); e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("documents-proposal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, e := codecDocuments(st); e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("decode-baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, e := codecGroupedFallback(groups); e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("decode-proposal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, e := decodeStoredDocuments(groups); e != nil {
				b.Fatal(e)
			}
		}
	})
}

type codecCustomPayload struct{ Fail bool }

func (p *codecCustomPayload) MarshalJSON() ([]byte, error) {
	if p.Fail {
		return nil, errors.New("prototype custom marshal rejected")
	}
	return []byte(`{ "custom": "<界>" }`), nil
}

type codecTextValueState struct {
	Values []domain.Product `json:"values"`
}

func (codecTextValueState) MarshalText() ([]byte, error) { return []byte("opaque"), nil }

type codecTextPointerState codecTextValueState

func (*codecTextPointerState) MarshalText() ([]byte, error) { return []byte("opaque"), nil }

type codecTextValueDecodeState codecTextValueState

func (codecTextValueDecodeState) UnmarshalText([]byte) error { return nil }

type codecTextPointerDecodeState codecTextValueState

func (*codecTextPointerDecodeState) UnmarshalText([]byte) error { return nil }

func TestCodecPrototypeRejectsTextCodecsAndTagNames(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(codecTextValueState{}), reflect.TypeOf(codecTextPointerState{}), reflect.TypeOf(codecTextValueDecodeState{}), reflect.TypeOf(codecTextPointerDecodeState{})} {
		if _, e := codecCollectionSchema(typ); e == nil {
			t.Fatal("State text codec accepted", typ)
		}
	}
	// Invalid encoding/json names must never become invented persistence keys.
	// Valid JSON names outside the compiled State key subset also fail closed.
	for _, key := range []string{"bad\nname", "bad\x00name", "bad\\name", "bad\"name", "bad,name", "Values", "two words", "name-dash", "123name", "界"} {
		typ := reflect.StructOf([]reflect.StructField{{Name: "Values", Type: reflect.TypeOf([]domain.Product{}), Tag: reflect.StructTag(fmt.Sprintf("json:%q", key))}})
		if _, e := codecCollectionSchema(typ); e == nil {
			t.Fatal("unsupported State collection name accepted", key)
		}
	}
	if _, e := codecCollectionSchema(reflect.TypeOf(domain.State{})); e != nil {
		t.Fatal("current State schema rejected", e)
	}
}
