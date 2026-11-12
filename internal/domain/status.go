package domain

import "slices"

// Status is Acme Support's transaction status vocabulary.
type Status string

const (
	StatusPending   Status = "pending"   // in progress, no money has moved
	StatusCompleted Status = "completed" // money has moved
	StatusFailed    Status = "failed"    // will not complete
	StatusCancelled Status = "cancelled" // stopped before completion
)

// Statuses lists the vocabulary in a stable order.
var Statuses = []Status{StatusPending, StatusCompleted, StatusFailed, StatusCancelled}

// ParseStatus validates a status from a partner request.
func ParseStatus(s string) (Status, bool) {
	st := Status(s)
	return st, slices.Contains(Statuses, st)
}

// StatusInfo is how an internal transaction status is presented to partners.
type StatusInfo struct {
	Status Status
	// Detail is a stable, partner-safe refinement of Status ("settled_late",
	// "reversed", ...). It is part of the public contract and deliberately
	// decoupled from internal status names.
	Detail string
	// FailureReason is set for failed and cancelled transactions only.
	FailureReason string
	// Settled reports whether the last state change was the settlement, so
	// TRX_dChanged is the settlement time.
	Settled bool
}

// statusMap translates transaction_statuses (STA_iID) to the partner view.
// It is a business decision documented in NOTES.md; in particular:
//   - approved/booked are "pending": accepted but no money has moved yet;
//   - reversed/chargeback are "completed": money did move (and was later
//     returned); Detail tells agents what happened;
//   - risk declines are reported as a generic "declined" so fraud rules are
//     not disclosed to a third party.
var statusMap = map[uint32]StatusInfo{
	10: {Status: StatusPending, Detail: "reserved"},
	20: {Status: StatusPending, Detail: "pending_provider"},
	30: {Status: StatusPending, Detail: "awaiting_approval"},
	40: {Status: StatusPending, Detail: "approved"},
	50: {Status: StatusPending, Detail: "booked"},
	60: {Status: StatusCompleted, Detail: "settled", Settled: true},
	61: {Status: StatusCompleted, Detail: "settled_late", Settled: true},
	70: {Status: StatusFailed, Detail: "declined_by_provider", FailureReason: "declined_by_provider"},
	71: {Status: StatusFailed, Detail: "declined", FailureReason: "declined"},
	72: {Status: StatusFailed, Detail: "limit_exceeded", FailureReason: "limit_exceeded"},
	80: {Status: StatusCancelled, Detail: "cancelled_by_customer", FailureReason: "cancelled_by_customer"},
	81: {Status: StatusCancelled, Detail: "cancelled_by_operator", FailureReason: "cancelled_by_operator"},
	82: {Status: StatusCancelled, Detail: "timed_out", FailureReason: "timed_out"},
	90: {Status: StatusCompleted, Detail: "reversed"},
	91: {Status: StatusCompleted, Detail: "chargeback"},
}

// unknownStatus is used for ids missing from statusMap (e.g. a status added
// to the database later). "pending" never claims that money moved or that a
// transaction reached a final state we cannot verify.
var unknownStatus = StatusInfo{Status: StatusPending, Detail: "unknown"}

// MapStatus returns the partner view of an internal status id. ok is false
// for unknown ids so callers can log and count them.
func MapStatus(id uint32) (info StatusInfo, ok bool) {
	info, ok = statusMap[id]
	if !ok {
		return unknownStatus, false
	}
	return info, true
}

// InternalStatusIDs returns the internal ids that map to any of statuses, in
// ascending order, for filtering in SQL.
func InternalStatusIDs(statuses []Status) []uint32 {
	var ids []uint32
	for id, info := range statusMap {
		if slices.Contains(statuses, info.Status) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
