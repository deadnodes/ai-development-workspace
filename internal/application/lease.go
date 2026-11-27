package application

import "time"

func extendLease(now time.Time, ttl time.Duration, previous *time.Time) time.Time {
	next := now.Add(ttl)
	if previous != nil && !next.After(*previous) {
		return previous.Add(time.Nanosecond)
	}
	return next
}
