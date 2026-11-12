package domain

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapStatus(t *testing.T) {
	tests := []struct {
		id      uint32
		want    StatusInfo
		wantOK  bool
		comment string
	}{
		{10, StatusInfo{Status: StatusPending, Detail: "reserved"}, true, ""},
		{20, StatusInfo{Status: StatusPending, Detail: "pending_provider"}, true, ""},
		{30, StatusInfo{Status: StatusPending, Detail: "awaiting_approval"}, true, ""},
		{40, StatusInfo{Status: StatusPending, Detail: "approved"}, true, "accepted, no money moved"},
		{50, StatusInfo{Status: StatusPending, Detail: "booked"}, true, "accepted, no money moved"},
		{60, StatusInfo{Status: StatusCompleted, Detail: "settled", Settled: true}, true, ""},
		{61, StatusInfo{Status: StatusCompleted, Detail: "settled_late", Settled: true}, true, ""},
		{70, StatusInfo{Status: StatusFailed, Detail: "declined_by_provider", FailureReason: "declined_by_provider"}, true, ""},
		{71, StatusInfo{Status: StatusFailed, Detail: "declined", FailureReason: "declined"}, true, "risk rules not disclosed"},
		{72, StatusInfo{Status: StatusFailed, Detail: "limit_exceeded", FailureReason: "limit_exceeded"}, true, ""},
		{80, StatusInfo{Status: StatusCancelled, Detail: "cancelled_by_customer", FailureReason: "cancelled_by_customer"}, true, ""},
		{81, StatusInfo{Status: StatusCancelled, Detail: "cancelled_by_operator", FailureReason: "cancelled_by_operator"}, true, ""},
		{82, StatusInfo{Status: StatusCancelled, Detail: "timed_out", FailureReason: "timed_out"}, true, ""},
		{90, StatusInfo{Status: StatusCompleted, Detail: "reversed"}, true, "money moved, then returned"},
		{91, StatusInfo{Status: StatusCompleted, Detail: "chargeback"}, true, "money moved, then returned"},
		{99, StatusInfo{Status: StatusPending, Detail: "unknown"}, false, "never claim a final state"},
	}
	for _, tt := range tests {
		got, ok := MapStatus(tt.id)
		assert.Equal(t, tt.want, got, "status %d %s", tt.id, tt.comment)
		assert.Equal(t, tt.wantOK, ok, "status %d", tt.id)
	}
}

func TestInternalStatusIDs(t *testing.T) {
	tests := []struct {
		name     string
		statuses []Status
		want     []uint32
	}{
		{"pending", []Status{StatusPending}, []uint32{10, 20, 30, 40, 50}},
		{"completed", []Status{StatusCompleted}, []uint32{60, 61, 90, 91}},
		{"failed", []Status{StatusFailed}, []uint32{70, 71, 72}},
		{"cancelled", []Status{StatusCancelled}, []uint32{80, 81, 82}},
		{"failed or cancelled", []Status{StatusFailed, StatusCancelled}, []uint32{70, 71, 72, 80, 81, 82}},
		{"none", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, InternalStatusIDs(tt.statuses))
		})
	}

	var all []uint32
	for _, s := range Statuses {
		all = append(all, InternalStatusIDs([]Status{s})...)
	}
	assert.Len(t, all, len(statusMap), "every internal status maps to exactly one partner status")
}

func TestParseStatus(t *testing.T) {
	for _, s := range []string{"pending", "completed", "failed", "cancelled"} {
		got, ok := ParseStatus(s)
		assert.True(t, ok)
		assert.Equal(t, Status(s), got)
	}
	for _, s := range []string{"", "PENDING", "settled", "canceled"} {
		_, ok := ParseStatus(s)
		assert.False(t, ok, s)
	}
}

func TestMinorUnits(t *testing.T) {
	u := func(v uint8) *uint8 { return &v }
	tests := []struct {
		name   string
		alpha3 string
		db     *uint8
		want   int
		wantOK bool
	}{
		{"from database", "EUR", u(2), 2, true},
		{"database wins over iso", "JPY", u(1), 1, true},
		{"zero decimals", "JPY", u(0), 0, true},
		{"null falls back to iso", "USD", nil, 2, true},
		{"iso is case-insensitive", "bhd", nil, 3, true},
		{"unknown currency", "XYZ", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MinorUnits(tt.alpha3, tt.db)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatMinor(t *testing.T) {
	tests := []struct {
		minor int64
		units int
		want  string
	}{
		{12345, 2, "123.45"},
		{100, 2, "1.00"},
		{5, 2, "0.05"},
		{-5, 2, "-0.05"},
		{0, 2, "0.00"},
		{1000, 0, "1000"},
		{-1000, 0, "-1000"},
		{1234, 3, "1.234"},
		{1, 3, "0.001"},
		{math.MaxInt64, 2, "92233720368547758.07"},
		{math.MinInt64, 2, "-92233720368547758.08"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatMinor(tt.minor, tt.units)
			assert.Equal(t, tt.want, got)

			// The result must be a valid JSON number literal.
			b, err := json.Marshal(json.Number(got))
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(b))
		})
	}
}
