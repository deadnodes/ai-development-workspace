package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Conditional snapshots work with either store and preserve the API's no-store
// policy. Clients explicitly retain a validator, not a shared HTTP cache entry.
func stateSnapshot(service Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := service.State(r.Context())
		if err != nil {
			respond(w, nil, err)
			return
		}
		body, err := json.Marshal(state)
		if err != nil {
			respond(w, nil, err)
			return
		}
		hash := sha256.Sum256(body)
		tag := `"` + hex.EncodeToString(hash[:]) + `"`
		w.Header().Set("ETag", tag)
		w.Header().Set("Vary", "Authorization")
		for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
