package main

import (
	"testing"
	"time"
)

func TestOperationRetentionDelay(t *testing.T) {
	for _, tc := range []struct {
		deleted int
		want    time.Duration
	}{
		{0, time.Hour}, {4999, time.Hour}, {5000, time.Minute},
	} {
		if got := operationRetentionDelay(tc.deleted); got != tc.want {
			t.Errorf("deleted=%d: got %v want %v", tc.deleted, got, tc.want)
		}
	}
}
