package application

import (
	"testing"

	"releasecontrol/internal/domain"
)

func TestRecordTimestampsStrictlyIncrease(t *testing.T) {
	prev := meta(domain.Command{}).CreatedAt
	for i := 0; i < 10000; i++ {
		next := meta(domain.Command{}).CreatedAt
		if !next.After(prev) {
			t.Fatalf("record %d stamped %v, not after %v", i, next, prev)
		}
		prev = next
	}
}
