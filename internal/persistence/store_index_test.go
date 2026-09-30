package persistence

import (
	"encoding/json"
	"fmt"
	"testing"
)

func syntheticDocuments(count int) map[string][]json.RawMessage {
	rows := make([]json.RawMessage, count)
	for i := range rows {
		rows[i] = json.RawMessage(fmt.Sprintf(`{"id":"operation-%08d","product_id":"p","status":"SUCCEEDED","detail":"historical operation with retained immutable evidence"}`, i))
	}
	return map[string][]json.RawMessage{"products": {json.RawMessage(`{"id":"p","name":"Product"}`)}, "operations": rows}
}

// Baseline is the exact document equality loop used by Store.Update before the
// migration fix. Keep it here to make the scaling reproduction independently runnable.
func legacyUnchanged(before map[string][]json.RawMessage, kind string, raw json.RawMessage) bool {
	for _, old := range before[kind] {
		if string(old) == string(raw) {
			return true
		}
	}
	return false
}

var documentMatches int

func BenchmarkHistoricalDocumentLookup(b *testing.B) {
	for _, count := range []int{1000, 5000, 20000} {
		before := syntheticDocuments(count)
		b.Run(fmt.Sprintf("legacy/%d", count), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				matches := 0
				for kind, rows := range before {
					for _, row := range rows {
						if legacyUnchanged(before, kind, row) {
							matches++
						}
					}
				}
				documentMatches = matches
			}
		})
		b.Run(fmt.Sprintf("indexed/%d", count), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				index, err := indexDocuments(before)
				if err != nil {
					b.Fatal(err)
				}
				matches := 0
				for kind, ids := range index.byKind {
					for id, rows := range ids {
						for _, raw := range rows {
							if index.unchanged(kind, id, raw) {
								matches++
							}
						}
					}
				}
				documentMatches = matches
			}
		})
	}
}

func TestDocumentIndexPreservesUpdateAndDeletionDecisions(t *testing.T) {
	before := map[string][]json.RawMessage{
		"products":   {json.RawMessage(`{"id":"p"}`), json.RawMessage(`{"id":"q"}`)},
		"operations": {json.RawMessage(`{"id":"one","product_id":"p","detail":"old"}`), json.RawMessage(`{"id":"two","product_id":"p"}`), json.RawMessage(`{"id":"three","product_id":"q"}`)},
		"global":     {json.RawMessage(`{"id":"shared"}`)},
	}
	cases := map[string]map[string][]json.RawMessage{
		"unchanged": before,
		"updated-and-reordered": {
			"products":   before["products"],
			"operations": {before["operations"][1], json.RawMessage(`{"id":"one","product_id":"p","detail":"new"}`), before["operations"][2], json.RawMessage(`{"id":"new","product_id":"p"}`)},
			"global":     before["global"],
		},
		"complete-product-deletion": {
			"products": {before["products"][1]}, "operations": {before["operations"][2]}, "global": before["global"],
		},
		"product-with-retained-owned-record": {
			"products": {before["products"][1]}, "operations": {before["operations"][1], before["operations"][2]},
		},
		"ordinary-removal-not-allowed": {
			"products": before["products"], "operations": {before["operations"][2]},
		},
		"same-id-moved-kind": {
			"products": before["products"], "moved": {before["operations"][0]},
		},
		"same-id-different-json-bytes": {
			"products": before["products"], "operations": {json.RawMessage(`{ "id":"two", "product_id":"p" }`)},
		},
	}
	oldIndex, err := indexDocuments(before)
	if err != nil {
		t.Fatal(err)
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			newIndex, err := indexDocuments(after)
			if err != nil {
				t.Fatal(err)
			}
			for kind, rows := range after {
				for position, raw := range rows {
					var meta struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}
					if got, want := oldIndex.unchanged(kind, meta.ID, raw), legacyUnchanged(before, kind, raw); got != want {
						t.Fatalf("position %d update decision changed: got %t want %t", position, got, want)
					}
				}
			}
			for kind, rows := range before {
				for _, raw := range rows {
					if got, want := oldIndex.productRecordRemoved(newIndex, kind, raw), productRecordRemoved(before, after, kind, raw); got != want {
						t.Fatalf("deletion policy changed for %s: got %t want %t", raw, got, want)
					}
				}
			}
		})
	}
	// A duplicate ID must retain membership semantics rather than last-write-wins.
	duplicate := map[string][]json.RawMessage{"operations": {before["operations"][0], json.RawMessage(`{"id":"one","product_id":"p","detail":"new"}`)}}
	dupIndex, err := indexDocuments(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range duplicate["operations"] {
		if !dupIndex.unchanged("operations", "one", raw) {
			t.Fatal("duplicate ID changed exact membership")
		}
	}
	if _, err = indexDocuments(map[string][]json.RawMessage{"broken": {json.RawMessage(`{"id":`)}}); err == nil {
		t.Fatal("invalid document silently indexed")
	}
}

func BenchmarkDeletedProductLookup(b *testing.B) {
	for _, count := range []int{100, 1000} {
		before := syntheticDocuments(count)
		after := map[string][]json.RawMessage{"products": {json.RawMessage(`{"id":"q"}`)}, "operations": make([]json.RawMessage, count)}
		for i := range after["operations"] {
			after["operations"][i] = json.RawMessage(fmt.Sprintf(`{"id":"retained-%08d","product_id":"q"}`, i))
		}
		b.Run(fmt.Sprintf("legacy/%d", count), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				matches := 0
				for kind, rows := range before {
					for _, raw := range rows {
						if productRecordRemoved(before, after, kind, raw) {
							matches++
						}
					}
				}
				documentMatches = matches
			}
		})
		b.Run(fmt.Sprintf("indexed/%d", count), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				oldIndex, err := indexDocuments(before)
				if err != nil {
					b.Fatal(err)
				}
				newIndex, err := indexDocuments(after)
				if err != nil {
					b.Fatal(err)
				}
				matches := 0
				for kind, rows := range before {
					for _, raw := range rows {
						if oldIndex.productRecordRemoved(newIndex, kind, raw) {
							matches++
						}
					}
				}
				documentMatches = matches
			}
		})
	}
}
