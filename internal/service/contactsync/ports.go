package contactsync

import (
	"context"
	"errors"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// QueueReader reads outstanding sync_queue entries: the oldest perBrand
// entries of every brand, skipping customers listed in exclude. Entries of
// customers that do not exist are returned with Brand 0.
type QueueReader interface {
	Outstanding(ctx context.Context, perBrand int, exclude []domain.CustomerID) ([]domain.QueueEntry, error)
}

// CustomerLoader loads the current state of a customer to push, including
// the stored contact id. Missing customers yield domain.ErrCustomerNotFound.
type CustomerLoader interface {
	LoadForSync(ctx context.Context, id domain.CustomerID) (domain.SyncCustomer, error)
}

// SyncRecorder records a confirmed sync in one transaction: it stores the
// contact id in partner_contacts and marks exactly the given queue entries
// processed (entries queued meanwhile stay outstanding).
type SyncRecorder interface {
	RecordSynced(ctx context.Context, id domain.CustomerID, contactID string, entries []uint64) (marked int64, err error)
}

// LeaderLock elects the single replica allowed to sync, so the per-brand
// Acme rate limits are not shared by several replicas unknowingly.
type LeaderLock interface {
	TryLock(ctx context.Context, name string) (lease Lease, acquired bool, err error)
}

// Lease is held leadership.
type Lease interface {
	// Held returns an error when leadership was lost (e.g. the database
	// connection holding the lock dropped).
	Held(ctx context.Context) error
	Release(ctx context.Context) error
}

// ContactWriter creates and updates contacts in Acme Support using the
// credentials of the given brand.
type ContactWriter interface {
	Create(ctx context.Context, brand domain.BrandID, c domain.Contact) (contactID string, err error)
	Update(ctx context.Context, brand domain.BrandID, contactID string, c domain.Contact) error
}

// ContactFinder looks a contact up by email in a brand's Acme Support account.
type ContactFinder interface {
	FindByEmail(ctx context.Context, brand domain.BrandID, email string) (contactID string, err error)
}

// Outcomes of Acme Support calls the sync reacts to. Adapters wrap them with
// details; the sync only inspects them with errors.Is.
var (
	// ErrContactConflict: a contact with the same external_id exists (409).
	ErrContactConflict = errors.New("contact already exists")
	// ErrContactNotFound: the contact id or email is unknown (404).
	ErrContactNotFound = errors.New("contact not found")
	// ErrContactRejected: the payload was rejected (422 or another 4xx);
	// retrying the same data cannot succeed.
	ErrContactRejected = errors.New("contact rejected")
	// ErrPartnerUnauthorized: the brand's API key was refused (401/403) or
	// is not configured. Every call for the brand will fail the same way.
	ErrPartnerUnauthorized = errors.New("partner credentials refused")
	// ErrPartnerUnavailable: timeouts, 5xx, network errors or an open
	// circuit; worth retrying later.
	ErrPartnerUnavailable = errors.New("partner unavailable")
)
