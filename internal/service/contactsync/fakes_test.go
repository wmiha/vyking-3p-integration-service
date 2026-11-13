package contactsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// store fakes the database: queue, customers and partner_contacts.
type store struct {
	mu        sync.Mutex
	entries   []*storedEntry
	customers map[domain.CustomerID]domain.SyncCustomer
	contacts  map[domain.CustomerID]string // partner_contacts
	recordErr error
	excluded  []domain.CustomerID // last exclusion list received
	// afterLoad runs after LoadForSync, e.g. to queue a change mid-sync.
	afterLoad func(domain.CustomerID)
	// beforeLoad runs before LoadForSync reads the customer.
	beforeLoad func(domain.CustomerID)
}

type storedEntry struct {
	domain.QueueEntry
	processed bool
}

func newStore() *store {
	return &store{customers: map[domain.CustomerID]domain.SyncCustomer{}, contacts: map[domain.CustomerID]string{}}
}

func (s *store) addCustomer(id domain.CustomerID, brand domain.BrandID, email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customers[id] = domain.SyncCustomer{ID: id, Brand: brand, BrandName: fmt.Sprint("brand-", brand),
		Email: email, FirstName: "First", LastName: fmt.Sprint(id), Currency: "EUR",
		RegisteredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Active: true}
}

func (s *store) queue(customers ...domain.CustomerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range customers {
		s.entries = append(s.entries, &storedEntry{QueueEntry: domain.QueueEntry{
			ID: uint64(len(s.entries) + 1), Customer: c, QueuedAt: time.Now()}})
	}
}

func (s *store) Outstanding(_ context.Context, perBrand int, exclude []domain.CustomerID) ([]domain.QueueEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.excluded = exclude
	taken := map[domain.BrandID]int{}
	var out []domain.QueueEntry
	for _, e := range s.entries {
		if e.processed || slices.Contains(exclude, e.Customer) {
			continue
		}
		var brand domain.BrandID
		if c, ok := s.customers[e.Customer]; ok {
			brand = c.Brand
		}
		if taken[brand] >= perBrand {
			continue
		}
		taken[brand]++
		q := e.QueueEntry
		q.Brand = brand
		out = append(out, q)
	}
	return out, nil
}

func (s *store) LoadForSync(_ context.Context, id domain.CustomerID) (domain.SyncCustomer, error) {
	if s.beforeLoad != nil {
		s.beforeLoad(id)
	}
	s.mu.Lock()
	c, ok := s.customers[id]
	c.ContactID = s.contacts[id]
	hook := s.afterLoad
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if !ok {
		return domain.SyncCustomer{}, domain.ErrCustomerNotFound
	}
	return c, nil
}

func (s *store) RecordSynced(_ context.Context, id domain.CustomerID, contactID string, entries []uint64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	s.contacts[id] = contactID
	var marked int64
	for _, e := range s.entries {
		if slices.Contains(entries, e.ID) && !e.processed {
			e.processed = true
			marked++
		}
	}
	return marked, nil
}

func (s *store) outstanding() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uint64
	for _, e := range s.entries {
		if !e.processed {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// acme fakes Acme Support accounts, one per brand.
type acme struct {
	mu        sync.Mutex
	nextID    int
	contacts  map[domain.BrandID]map[string]domain.Contact // brand -> id -> contact
	failNext  map[string]error                             // external id -> error for its next write
	failAll   error                                        // every call fails
	calls     []string
	delay     time.Duration
	inflight  map[domain.BrandID]int
	maxFlight map[domain.BrandID]int
}

func newAcme() *acme {
	return &acme{contacts: map[domain.BrandID]map[string]domain.Contact{}, failNext: map[string]error{},
		inflight: map[domain.BrandID]int{}, maxFlight: map[domain.BrandID]int{}}
}

func (a *acme) enter(brand domain.BrandID, call string) func() {
	a.mu.Lock()
	a.calls = append(a.calls, call)
	a.inflight[brand]++
	a.maxFlight[brand] = max(a.maxFlight[brand], a.inflight[brand])
	a.mu.Unlock()
	time.Sleep(a.delay)
	return func() {
		a.mu.Lock()
		a.inflight[brand]--
		a.mu.Unlock()
	}
}

func (a *acme) account(brand domain.BrandID) map[string]domain.Contact {
	if a.contacts[brand] == nil {
		a.contacts[brand] = map[string]domain.Contact{}
	}
	return a.contacts[brand]
}

// seed puts a contact into Acme directly (e.g. created before a crash).
func (a *acme) seed(brand domain.BrandID, c domain.Contact) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := fmt.Sprintf("ct-%d", a.nextID)
	a.account(brand)[id] = c
	return id
}

func (a *acme) injected(externalID string) error {
	if a.failAll != nil {
		return a.failAll
	}
	if err, ok := a.failNext[externalID]; ok {
		delete(a.failNext, externalID)
		return err
	}
	return nil
}

func (a *acme) Create(_ context.Context, brand domain.BrandID, c domain.Contact) (string, error) {
	defer a.enter(brand, "create:"+c.ExternalID)()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.injected(c.ExternalID); err != nil {
		return "", err
	}
	for _, existing := range a.account(brand) {
		if existing.ExternalID == c.ExternalID {
			return "", ErrContactConflict
		}
	}
	a.nextID++
	id := fmt.Sprintf("ct-%d", a.nextID)
	a.account(brand)[id] = c
	return id, nil
}

func (a *acme) Update(_ context.Context, brand domain.BrandID, id string, c domain.Contact) error {
	defer a.enter(brand, "update:"+c.ExternalID)()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.injected(c.ExternalID); err != nil {
		return err
	}
	if _, ok := a.account(brand)[id]; !ok {
		return ErrContactNotFound
	}
	a.account(brand)[id] = c
	return nil
}

func (a *acme) FindByEmail(_ context.Context, brand domain.BrandID, email string) (string, error) {
	defer a.enter(brand, "lookup:"+email)()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failAll != nil {
		return "", a.failAll
	}
	for id, c := range a.account(brand) {
		if c.Email == email {
			return id, nil
		}
	}
	return "", ErrContactNotFound
}

func (a *acme) count(brand domain.BrandID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.contacts[brand])
}

// lock fakes the MySQL named lock.
type lock struct {
	busy     bool
	err      error
	lost     atomic.Bool
	released atomic.Int32
}

func (l *lock) TryLock(context.Context, string) (Lease, bool, error) {
	if l.err != nil || l.busy {
		return nil, false, l.err
	}
	return lease{l}, true, nil
}

type lease struct{ l *lock }

func (le lease) Held(context.Context) error {
	if le.l.lost.Load() {
		return errors.New("lock connection lost")
	}
	return nil
}

func (le lease) Release(context.Context) error {
	le.l.released.Add(1)
	return nil
}

type fixture struct {
	store *store
	acme  *acme
	lock  *lock
	cfg   Config
}

func newFixture() *fixture {
	return &fixture{
		store: newStore(), acme: newAcme(), lock: &lock{},
		cfg: Config{
			Interval: time.Minute, BatchPerBrand: 100, WorkersPerBrand: 4, LockName: "sync",
			JobTimeout: time.Minute, BackoffInitial: time.Minute, BackoffMax: time.Hour,
			Brands: []domain.BrandID{1, 2},
		},
	}
}

func (f *fixture) syncer() *Syncer {
	return New(f.cfg, Deps{Queue: f.store, Customers: f.store, Recorder: f.store, Lock: f.lock,
		Writer: f.acme, Finder: f.acme, Logger: slog.New(slog.DiscardHandler)})
}

func (a *acme) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}
