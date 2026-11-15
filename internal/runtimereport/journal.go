package runtimereport

import "time"

// JournalBudget is private snapshot metadata, never part of domain.State.
// At most 500 timestamps are retained for an exact rolling window.
type JournalBudget struct {
	Events            []time.Time `json:"events,omitempty"`
	LastExceeded      time.Time   `json:"last_exceeded,omitempty"`
	SuppressedChanges int64       `json:"suppressed_changes,omitempty"`
	SuppressedReports int64       `json:"suppressed_reports,omitempty"`
}

type JournalResult struct {
	Changes           []Change
	Total             int
	Additional        int
	BudgetExceeded    bool
	SuppressedChanges int64
	SuppressedReports int64
}

// ReserveJournal runs inside the snapshot transaction. Reservations are conservative:
// a subsequent journal write failure does not refund them or retry lost events.
func ReserveJournal(old JournalBudget, changes []Change, now time.Time) (JournalBudget, JournalResult) {
	b := old
	b.Events = nil
	for _, at := range old.Events {
		if at.After(now.Add(-24 * time.Hour)) {
			b.Events = append(b.Events, at)
		}
	}
	out := JournalResult{Total: len(changes)}
	if len(changes) == 0 {
		return b, out
	}
	room := 500 - len(b.Events)
	requested := min(20, len(changes))
	if len(changes) > requested {
		requested++
	}
	details := min(20, len(changes), room)
	// Overflow summaries count toward the same 500-event budget.
	if len(changes) > details && details == room && details > 0 {
		details--
	}
	out.Changes = changes[:details]
	used := details
	if len(changes) > details && room > details {
		out.Additional = len(changes) - details
		used++
	}
	for i := 0; i < used; i++ {
		b.Events = append(b.Events, now)
	}
	if room < requested {
		b.SuppressedChanges += int64(len(changes) - details)
		b.SuppressedReports++
		if b.LastExceeded.IsZero() || !now.Before(b.LastExceeded.Add(time.Hour)) {
			out.BudgetExceeded = true
			out.SuppressedChanges = b.SuppressedChanges
			out.SuppressedReports = b.SuppressedReports
			b.SuppressedChanges = 0
			b.SuppressedReports = 0
			b.LastExceeded = now
		}
	}
	return b, out
}
