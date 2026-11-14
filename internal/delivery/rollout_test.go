package delivery

import (
	"strings"
	"testing"
)

func TestSafeRolloutReason(t *testing.T) {
	for _, message := range []string{`"password":"private-value"`, `'token': 'private-value'`, `password=private-value`, `Authorization: Bearer private-value`, `https://user:private-value@example.test`, `{"client_secret":"private-value"}`, `api_key="private-value with spaces"`} {
		got := SafeRolloutReason("UpgradeFailed: " + message)
		if !strings.HasPrefix(got, "UpgradeFailed: ") || strings.Contains(got, "private-value") || !strings.Contains(got, "[REDACTED]") {
			t.Fatal(got)
		}
	}
	got := SafeRolloutReason("UpgradeFailed: " + strings.Repeat("界", 400))
	if len([]rune(got)) != len("UpgradeFailed: ")+300 {
		t.Fatal(len([]rune(got)))
	}
	// Untrusted text before a colon does not receive an unbounded prefix allowance.
	if got := SafeRolloutReason(strings.Repeat("x", 400) + ": diagnostic"); len([]rune(got)) != 300 {
		t.Fatal(got)
	}
}
