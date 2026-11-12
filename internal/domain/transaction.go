package domain

import (
	"errors"
	"fmt"
	"time"
)

// CustomerID identifies a customer (CUS_iID). It is also the external_id of
// the customer's Acme Support contact.
type CustomerID uint32

// Customer is a customer as seen by the partner API.
type Customer struct {
	ID        CustomerID
	Brand     BrandID
	BrandName string
	Currency  string // ISO 4217 alpha-3; empty if the currency row is missing
	// MinorUnits as stored in the currencies table; nil when NULL.
	MinorUnits *uint8
	Active     bool
}

// Transaction is a customer's transaction as stored, before mapping to the
// partner vocabulary.
type Transaction struct {
	ID            uint64
	Type          string // TYP_cName, e.g. PAYIN; empty for unknown type ids
	StatusID      uint32
	AmountMinor   *int64 // minor units of the customer's currency; nullable
	PaymentMethod *string
	CreatedAt     time.Time
	BookedAt      *time.Time
	ChangedAt     time.Time
}

// Errors returned by the use cases and mapped to API errors by the HTTP layer.
var (
	ErrCustomerNotFound = errors.New("customer not found")
	ErrBrandForbidden   = errors.New("brand not accessible")
	ErrInvalidCursor    = errors.New("invalid cursor")
	// ErrUnavailable wraps failures of infrastructure (database, cache, ...)
	// that are expected to be temporary.
	ErrUnavailable = errors.New("dependency unavailable")
)

// AmbiguousCustomerError reports an email matching customers of several
// brands the caller may access (emails are unique per brand only).
type AmbiguousCustomerError struct {
	Brands []BrandID
}

func (e *AmbiguousCustomerError) Error() string {
	return fmt.Sprintf("email matches customers in brands %v", e.Brands)
}
