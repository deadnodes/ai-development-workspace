package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"releasecontrol/internal/domain"
)

// ProductConfiguration is a one-way mirror projection, never an import format.
// Development history and provider observations remain in the authoritative DB.
type ProductConfiguration struct {
	SchemaVersion int                         `json:"schema_version"`
	Authority     string                      `json:"authority"`
	ProductID     string                      `json:"product_id"`
	Revision      string                      `json:"revision"`
	Configuration map[string][]map[string]any `json:"configuration"`
}

func productConfiguration(st domain.State, productID string) (ProductConfiguration, error) {
	if !productExists(&st, productID) {
		return ProductConfiguration{}, missing("product", productID)
	}
	source := reflect.ValueOf(st)
	collections := map[string]int{}
	for i := 0; i < source.NumField(); i++ {
		collections[source.Type().Field(i).Tag.Get("json")] = i
	}
	project := func(kind string, include func(reflect.Value) bool) ([]map[string]any, error) {
		selected := []map[string]any{}
		rows := source.Field(collections[kind])
		for i := 0; i < rows.Len(); i++ {
			row := rows.Index(i)
			if !include(row) {
				continue
			}
			raw, err := json.Marshal(row.Interface())
			if err != nil {
				return nil, err
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				return nil, err
			}
			selected = append(selected, document)
		}
		return selected, nil
	}
	out := map[string][]map[string]any{}
	// These are configuration records only. Never export operations, reports or audit
	// wholesale: those can contain unrelated product and provider data.
	for _, kind := range []string{"products", "applications", "repositories", "repository_bindings", "component_builds", "publication_targets", "environment_bindings", "environments", "connection_grants", "system_relationships"} {
		rows, err := project(kind, func(row reflect.Value) bool {
			owner, _ := stateRowProduct(row, kind == "products")
			return owner == productID
		})
		if err != nil {
			return ProductConfiguration{}, err
		}
		if kind == "environments" {
			for _, row := range rows {
				delete(row, "reconciled")
				delete(row, "runtime")
			}
		}
		out[kind] = rows
	}
	// Configuration changes are append-only in storage. Mirror only effective values.
	for kind, keys := range map[string][]string{"publication_targets": {"application_id"}, "component_builds": {"application_id"}, "environment_bindings": {"environment_id", "application_id"}, "repository_bindings": {"repository_id"}} {
		seen := map[string]bool{}
		rows := out[kind]
		out[kind] = []map[string]any{}
		for i := len(rows) - 1; i >= 0; i-- {
			keyParts := []any{}
			for _, key := range keys {
				keyParts = append(keyParts, rows[i][key])
			}
			keyBytes, _ := json.Marshal(keyParts)
			key := string(keyBytes)
			if !seen[key] {
				out[kind] = append(out[kind], rows[i])
				seen[key] = true
			}
		}
	}
	refs := func(kind, field string) map[any]bool {
		set := map[any]bool{}
		for _, row := range out[kind] {
			if id, ok := row[field].(string); ok && id != "" {
				set[id] = true
			}
		}
		return set
	}
	for _, selection := range []struct {
		kind string
		ids  map[any]bool
	}{
		{"github_connections", refs("connection_grants", "connection_id")},
		{"registered_repositories", refs("repositories", "registered_repository_id")},
		{"external_systems", refs("system_relationships", "external_system_id")},
		{"compositions", refs("environments", "desired_composition_id")},
	} {
		out[selection.kind] = []map[string]any{}
		rows, err := project(selection.kind, func(row reflect.Value) bool {
			return selection.ids[row.FieldByIndex(stateIDPaths[collections[selection.kind]]).String()]
		})
		if err != nil {
			return ProductConfiguration{}, err
		}
		for _, row := range rows {
			if selection.kind == "github_connections" {
				row = map[string]any{"id": row["id"], "name": row["name"], "config": row["config"]}
			}
			if selection.kind == "registered_repositories" {
				row = map[string]any{"id": row["id"], "provider_api_url": row["provider_api_url"], "provider_repository_id": row["provider_repository_id"], "full_name": row["full_name"]}
			}
			out[selection.kind] = append(out[selection.kind], row)
		}
	}
	for _, rows := range out {
		sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	}
	bytes, err := json.Marshal(out)
	if err != nil {
		return ProductConfiguration{}, err
	}
	digest := sha256.Sum256(bytes)
	return ProductConfiguration{SchemaVersion: 1, Authority: "control-plane-database", ProductID: productID, Revision: "sha256:" + hex.EncodeToString(digest[:]), Configuration: out}, nil
}
