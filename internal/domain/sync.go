package domain

import (
	"strconv"
	"strings"
	"time"
)

// QueueEntry is an outstanding sync_queue row.
type QueueEntry struct {
	ID       uint64
	Customer CustomerID
	// Brand of the customer; 0 when the customer row does not exist.
	Brand    BrandID
	QueuedAt time.Time
}

// SyncCustomer is the current state of a customer to mirror in Acme Support.
// Nullable columns are empty strings.
type SyncCustomer struct {
	ID           CustomerID
	Brand        BrandID
	BrandName    string
	Email        string
	FirstName    string
	LastName     string
	Phone        string
	Country      string
	Currency     string
	RegisteredAt time.Time
	Active       bool
	// ContactID is the Acme Support contact id stored by a previous sync, or
	// empty if the customer was never synced.
	ContactID string
}

// Contact is a customer as represented in Acme Support.
type Contact struct {
	// ExternalID is our customer id. Acme Support rejects a second contact
	// with the same external id (409), which makes creation idempotent.
	ExternalID   string
	Email        string
	Name         string
	Phone        string
	Country      string
	Brand        string
	Currency     string
	RegisteredAt time.Time
	Active       bool
}

// Contact maps the customer to its Acme Support contact.
func (c SyncCustomer) Contact() Contact {
	return Contact{
		ExternalID:   strconv.FormatUint(uint64(c.ID), 10),
		Email:        c.Email,
		Name:         strings.TrimSpace(c.FirstName + " " + c.LastName),
		Phone:        c.Phone,
		Country:      c.Country,
		Brand:        c.BrandName,
		Currency:     c.Currency,
		RegisteredAt: c.RegisteredAt.UTC(),
		Active:       c.Active,
	}
}
