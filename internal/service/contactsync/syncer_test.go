package contactsync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

func TestSyncCreatesContactsAndSettlesTheQueue(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.addCustomer(2, 1, "b@example.com")
	f.store.addCustomer(3, 2, "c@example.com")
	f.store.queue(1, 2, 1, 3) // customer 1 twice: coalesced into one push

	r := f.syncer().RunOnce(context.Background())

	assert.True(t, r.Leader)
	assert.Equal(t, 4, r.Fetched)
	assert.Equal(t, 3, r.Customers)
	assert.Equal(t, map[string]int{OutcomeCreated: 3}, r.Outcomes)
	assert.Empty(t, f.store.outstanding(), "every entry settled")
	assert.Equal(t, 2, f.acme.count(1))
	assert.Equal(t, 1, f.acme.count(2))
	assert.Len(t, f.store.contacts, 3, "contact ids stored")
	assert.Equal(t, int32(1), f.lock.released.Load(), "leadership released")
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.queue(1)
	s := f.syncer()
	ctx := context.Background()

	s.RunOnce(ctx)
	contactID := f.store.contacts[1]

	r := s.RunOnce(ctx)
	assert.Zero(t, r.Fetched, "nothing outstanding, nothing pushed")

	f.store.queue(1) // the customer changed again
	r = s.RunOnce(ctx)
	assert.Equal(t, map[string]int{OutcomeUpdated: 1}, r.Outcomes)
	assert.Equal(t, 1, f.acme.count(1), "no duplicate contact")
	assert.Equal(t, contactID, f.store.contacts[1])
}

func TestSyncRecoversWhenTheContactExistsAlready(t *testing.T) {
	// Acme created the contact but we never stored its id (crash, timeout
	// or a failed database write): creating again answers 409.
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.queue(1)
	existing := f.acme.seed(1, domain.Contact{ExternalID: "1", Email: "a@example.com", Name: "outdated"})

	r := f.syncer().RunOnce(context.Background())

	assert.Equal(t, map[string]int{OutcomeRecovered: 1}, r.Outcomes)
	assert.Equal(t, existing, f.store.contacts[1])
	assert.Equal(t, 1, f.acme.count(1), "no duplicate contact")
	assert.Equal(t, "First 1", f.acme.contacts[1][existing].Name, "existing contact updated")
	assert.Equal(t, []string{"create:1", "lookup:a@example.com", "update:1"}, f.acme.calls)
}

func TestSyncRecreatesContactUnknownToAcme(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.contacts[1] = "deleted-in-acme"
	f.store.queue(1)

	r := f.syncer().RunOnce(context.Background())

	assert.Equal(t, map[string]int{OutcomeCreated: 1}, r.Outcomes)
	assert.NotEqual(t, "deleted-in-acme", f.store.contacts[1], "new id stored")
}

func TestSyncAdoptsContactFoundByEmail(t *testing.T) {
	f := newFixture()
	f.cfg.LookupBeforeCreate = true
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.queue(1)
	manual := f.acme.seed(1, domain.Contact{Email: "a@example.com", Name: "created by an agent"})

	r := f.syncer().RunOnce(context.Background())

	assert.Equal(t, map[string]int{OutcomeAdopted: 1}, r.Outcomes)
	assert.Equal(t, manual, f.store.contacts[1])
	assert.Equal(t, 1, f.acme.count(1))
}

func TestCustomerFailuresBackOffWithoutBlockingOthers(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fixture)
		outcome string
	}{
		{name: "rejected data", outcome: OutcomeRejected,
			setup: func(f *fixture) {
				f.store.addCustomer(1, 1, "a@example.com")
				f.acme.failNext["1"] = fmt.Errorf("%w: phone", ErrContactRejected)
			}},
		{name: "conflict without email", outcome: OutcomeConflict,
			setup: func(f *fixture) {
				f.store.addCustomer(1, 1, "")
				f.acme.seed(1, domain.Contact{ExternalID: "1"})
			}},
		{name: "conflict not found by email", outcome: OutcomeConflict,
			setup: func(f *fixture) {
				f.store.addCustomer(1, 1, "new@example.com")
				f.acme.seed(1, domain.Contact{ExternalID: "1", Email: "old@example.com"})
			}},
		{name: "customer deleted", outcome: OutcomeMissing,
			setup: func(f *fixture) {
				f.store.customers[1] = domain.SyncCustomer{ID: 1, Brand: 1}
				f.store.beforeLoad = func(id domain.CustomerID) {
					if id == 1 {
						delete(f.store.customers, 1)
					}
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture()
			tt.setup(f)
			f.store.addCustomer(2, 1, "b@example.com")
			f.store.queue(1, 2)
			s := f.syncer()

			r := s.RunOnce(context.Background())
			assert.Equal(t, 1, r.Outcomes[tt.outcome])
			assert.Equal(t, 1, r.Outcomes[OutcomeCreated], "other customers are synced")
			assert.Equal(t, []uint64{1}, f.store.outstanding(), "the failed entry is never dropped")
			assert.Equal(t, 1, r.BackingOff)

			s.RunOnce(context.Background())
			assert.Equal(t, []domain.CustomerID{1}, f.store.excluded, "backing off: not fetched again")
			assert.Equal(t, []uint64{1}, f.store.outstanding())
		})
	}
}

func TestPartnerAndDatabaseFailuresStopTheBrand(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fixture)
		outcome string
	}{
		{name: "acme unavailable", outcome: OutcomePartner,
			setup: func(f *fixture) { f.acme.failAll = fmt.Errorf("%w: 503", ErrPartnerUnavailable) }},
		{name: "api key refused", outcome: OutcomeAuth,
			setup: func(f *fixture) { f.acme.failAll = fmt.Errorf("%w: 401", ErrPartnerUnauthorized) }},
		{name: "database write fails", outcome: OutcomeInfra,
			setup: func(f *fixture) { f.store.recordErr = errors.New("connection refused") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture()
			f.cfg.WorkersPerBrand = 1 // deterministic: one job at a time
			for id := range domain.CustomerID(5) {
				f.store.addCustomer(id+1, 1, fmt.Sprintf("c%d@example.com", id+1))
				f.store.queue(id + 1)
			}
			tt.setup(f)

			r := f.syncer().RunOnce(context.Background())

			assert.Equal(t, map[string]int{tt.outcome: 1, OutcomeNotStarted: 4}, r.Outcomes,
				"the brand stops after the first failure")
			assert.Len(t, f.store.outstanding(), 5, "every entry kept")
			assert.Zero(t, r.BackingOff, "customers are not to blame")
		})
	}
}

func TestBrandsAreIsolated(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.addCustomer(2, 2, "b@example.com")
	f.store.queue(1, 2)
	f.acme.failNext["1"] = fmt.Errorf("%w: 503", ErrPartnerUnavailable)

	r := f.syncer().RunOnce(context.Background())
	assert.Equal(t, map[string]int{OutcomePartner: 1, OutcomeCreated: 1}, r.Outcomes)
	assert.Equal(t, []uint64{1}, f.store.outstanding())
}

func TestEntriesQueuedDuringSyncStayOutstanding(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.queue(1)
	f.store.afterLoad = func(domain.CustomerID) {
		f.store.afterLoad = nil
		f.store.queue(1) // the customer changes while being synced
	}
	s := f.syncer()

	s.RunOnce(context.Background())
	assert.Equal(t, []uint64{2}, f.store.outstanding(), "only fetched entries are marked processed")

	r := s.RunOnce(context.Background())
	assert.Equal(t, map[string]int{OutcomeUpdated: 1}, r.Outcomes, "the newer change is pushed next cycle")
	assert.Empty(t, f.store.outstanding())
}

func TestOrphansAndUnconfiguredBrandsAreKept(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.addCustomer(3, 3, "poker@example.com") // brand 3 has no credentials
	f.store.queue(1, 3, 99)                        // 99 does not exist

	r := f.syncer().RunOnce(context.Background())
	assert.Equal(t, 1, r.Orphans)
	assert.Equal(t, 1, r.Unconfigured)
	assert.Equal(t, map[string]int{OutcomeCreated: 1}, r.Outcomes)
	assert.Equal(t, []uint64{2, 3}, f.store.outstanding())
}

func TestOnlyTheLeaderSyncs(t *testing.T) {
	f := newFixture()
	f.store.addCustomer(1, 1, "a@example.com")
	f.store.queue(1)
	f.lock.busy = true

	r := f.syncer().RunOnce(context.Background())
	assert.False(t, r.Leader)
	assert.Zero(t, r.Fetched)
	assert.Equal(t, []uint64{1}, f.store.outstanding())

	f.lock.busy, f.lock.err = false, errors.New("database down")
	r = f.syncer().RunOnce(context.Background())
	assert.Contains(t, r.Error, "leader election")
}

func TestLostLeadershipStopsDispatching(t *testing.T) {
	f := newFixture()
	f.cfg.WorkersPerBrand = 1
	for id := range domain.CustomerID(3) {
		f.store.addCustomer(id+1, 1, fmt.Sprintf("c%d@example.com", id+1))
		f.store.queue(id + 1)
	}
	f.store.afterLoad = func(domain.CustomerID) { f.lock.lost.Store(true) }

	r := f.syncer().RunOnce(context.Background())
	assert.Equal(t, map[string]int{OutcomeCreated: 1, OutcomeNotStarted: 2}, r.Outcomes,
		"the job in flight completes, no new job starts")
}

func TestBatchIsBoundedPerBrandAndWorkersPerBrand(t *testing.T) {
	f := newFixture()
	f.cfg.BatchPerBrand = 5
	f.cfg.WorkersPerBrand = 3
	f.acme.delay = 10 * time.Millisecond
	for id := range domain.CustomerID(20) {
		f.store.addCustomer(id+1, 1, fmt.Sprintf("c%d@example.com", id+1))
		f.store.queue(id + 1)
	}
	f.store.addCustomer(100, 2, "small@example.com")
	f.store.queue(100)

	r := f.syncer().RunOnce(context.Background())
	assert.Equal(t, 6, r.Fetched, "five of the big brand, the small brand is not starved")
	assert.True(t, r.BatchFull)
	assert.Equal(t, 6, r.Synced())
	assert.LessOrEqual(t, f.acme.maxFlight[1], 3, "never more than workers_per_brand calls in flight")
	assert.Len(t, f.store.outstanding(), 15)
}

func TestRunLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture()
		f.cfg.BatchPerBrand = 2
		for id := range domain.CustomerID(5) {
			f.store.addCustomer(id+1, 1, fmt.Sprintf("c%d@example.com", id+1))
			f.store.queue(id + 1)
		}
		s := f.syncer()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()

		synctest.Wait()
		assert.Empty(t, f.store.outstanding(), "a backlog is drained without waiting for the interval")

		f.store.queue(1)
		synctest.Wait()
		assert.Len(t, f.store.outstanding(), 1, "no backlog: waits for the interval")

		assert.True(t, s.Trigger())
		synctest.Wait()
		assert.Empty(t, f.store.outstanding(), "a trigger starts a cycle at once")

		f.store.queue(2)
		time.Sleep(f.cfg.Interval)
		synctest.Wait()
		assert.Empty(t, f.store.outstanding(), "the interval elapsed")

		last, ok := s.LastRun()
		require.True(t, ok)
		assert.Equal(t, 1, last.Synced())

		cancel()
		<-done
	})
}

func TestRunLoopDoesNotSpinWhenNothingProgresses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture()
		f.cfg.BatchPerBrand = 1
		f.store.addCustomer(1, 1, "a@example.com")
		f.store.queue(1)
		f.acme.failAll = fmt.Errorf("%w: down", ErrPartnerUnavailable)

		s := f.syncer()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()

		synctest.Wait()
		assert.Equal(t, 1, f.acme.callCount(), "a full batch without progress waits for the interval")

		time.Sleep(f.cfg.Interval)
		synctest.Wait()
		assert.Equal(t, 2, f.acme.callCount())

		cancel()
		<-done
	})
}

func TestShutdownLetsInFlightJobsFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture()
		f.cfg.WorkersPerBrand = 1
		f.acme.delay = 10 * time.Second // a slow Acme answer
		for id := range domain.CustomerID(3) {
			f.store.addCustomer(id+1, 1, fmt.Sprintf("c%d@example.com", id+1))
			f.store.queue(id + 1)
		}
		s := f.syncer()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()

		time.Sleep(time.Second) // the first job is waiting for Acme
		cancel()
		<-done

		assert.Equal(t, []uint64{2, 3}, f.store.outstanding(),
			"the in-flight customer was synced and recorded; the others were not started")
		last, _ := s.LastRun()
		assert.Equal(t, map[string]int{OutcomeCreated: 1, OutcomeNotStarted: 2}, last.Outcomes)
	})
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := newBackoff(time.Minute, 10*time.Minute)
	now := time.Now()
	var got []time.Duration
	for range 6 {
		got = append(got, b.fail(7, now).Sub(now))
	}
	assert.Equal(t, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		10 * time.Minute, 10 * time.Minute}, got)

	assert.Equal(t, []domain.CustomerID{7}, b.excluded(now))
	assert.Empty(t, b.excluded(now.Add(11*time.Minute)), "eligible again after the delay")
	b.clear(7)
	assert.Zero(t, b.size(now))
}
