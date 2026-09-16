package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	raw, err := json.Marshal(st)
	if err != nil {
		return ProductConfiguration{}, err
	}
	var all map[string][]map[string]any
	if err = json.Unmarshal(raw, &all); err != nil {
		return ProductConfiguration{}, err
	}
	out := map[string][]map[string]any{}
	// These are configuration records only. Never export operations, reports or audit
	// wholesale: those can contain unrelated product and provider data.
	for _, kind := range []string{"products", "applications", "repositories", "repository_bindings", "component_builds", "environment_bindings", "environments", "connection_grants", "system_relationships"} {
		out[kind] = []map[string]any{}
		for _, row := range all[kind] {
			if (kind == "products" && row["id"] == productID) || (kind != "products" && row["product_id"] == productID) {
				if kind == "environments" {
					delete(row, "reconciled")
					delete(row, "runtime")
				}
				out[kind] = append(out[kind], row)
			}
		}
	}
	// Configuration changes are append-only in storage. Mirror only effective values.
	for kind, keys := range map[string][]string{"component_builds": {"application_id"}, "environment_bindings": {"environment_id", "application_id"}, "repository_bindings": {"repository_id"}} {
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
		for _, row := range all[selection.kind] {
			if !selection.ids[row["id"]] {
				continue
			}
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
