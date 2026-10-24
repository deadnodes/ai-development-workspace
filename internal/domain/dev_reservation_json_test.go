package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReservationCommandJSONStrictKeysAndExactUint64(t *testing.T) {
	raw := `{"action":"reserve_candidate_dev","actor":"owner","product_id":"p","data":{"candidate_operation_id":"candidate","environment_id":"dev","request_key":"reservation-key-01","expected_selection":{"composition_id":"base","operation_id":"op","generation":18446744073709551615}}}`
	var c Command
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Data["expected_selection"].(map[string]any)["generation"].(json.Number).String() != "18446744073709551615" {
		t.Fatal("uint64 rounded")
	}
	for _, bad := range []string{strings.Replace(raw, `"actor":"owner"`, `"actor":"owner","actor":"second"`, 1), strings.Replace(raw, `"generation":18446744073709551615`, `"generation":1,"generation":2`, 1), strings.Replace(raw, `"generation":18446744073709551615`, `"Generation":1`, 1), strings.Replace(raw, `"candidate_operation_id":"candidate"`, `"candidate_operation_id":"candidate","extra":`+strings.Repeat("[", 20)+"0"+strings.Repeat("]", 20), 1)} {
		if err := json.Unmarshal([]byte(bad), &c); err == nil {
			t.Fatal("ambiguous/unbounded reservation JSON accepted")
		}
	}
}
