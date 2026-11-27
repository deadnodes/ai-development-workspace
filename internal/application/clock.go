package application

import (
	"sync/atomic"
	"time"
)

var lastRecordTime atomic.Int64

// monotonicNow stamps records strictly after the previous record of this
// process. Coarse clocks (Windows) otherwise give consecutive records equal
// timestamps, and freshness rules such as "evidence after the scope change"
// would accept evidence recorded before the change.
func monotonicNow() time.Time {
	for {
		now := time.Now().UTC().UnixNano()
		last := lastRecordTime.Load()
		if now <= last {
			now = last + 1
		}
		if lastRecordTime.CompareAndSwap(last, now) {
			return time.Unix(0, now).UTC()
		}
	}
}
