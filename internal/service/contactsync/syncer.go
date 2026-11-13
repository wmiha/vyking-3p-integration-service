// Package contactsync mirrors changed customers into Acme Support contacts.
//
// sync_queue acts as an outbox written by the producers of customer changes;
// this package is its relay. Delivery is at-least-once with idempotent
// effects:
//
//   - creation is keyed by external_id (our customer id): Acme Support
//     rejects a second contact with 409, which we resolve by looking the
//     contact up and updating it;
//   - updates send the full current snapshot, so repeating one is harmless;
//   - a queue entry is marked processed only after Acme Support confirmed the
//     contact and its id was stored, in one database transaction.
//
// Any other outcome leaves the entry outstanding for a later attempt.
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
	"uuid"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/logging"
)

// Config tunes the sync; values come from the sync section of the config.
type Config struct {
	// Interval between cycles when there is no backlog.
	Interval time.Duration
	// BatchPerBrand caps the queue entries fetched per brand and cycle; it
	// keeps one large brand from starving the others.
	BatchPerBrand int
	// WorkersPerBrand syncs that many customers of a brand concurrently;
	// Acme answers can take up to 30s, so a single worker could not use the
	// brand's rate budget.
	WorkersPerBrand int
	// LockName is the MySQL named lock electing the syncing replica.
	LockName string
	// JobTimeout bounds syncing one customer, including Acme retries. Jobs
	// started before shutdown get this long to finish.
	JobTimeout time.Duration
	// BackoffInitial and BackoffMax shape retries of customers that failed
	// for customer-specific reasons.
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	// LookupBeforeCreate looks contacts up by email before creating them,
	// adopting contacts created manually in Acme Support (one extra call).
	LookupBeforeCreate bool
	// Brands are the brands with Acme Support credentials.
	Brands []domain.BrandID
}

// Syncer runs the scheduled contact sync.
type Syncer struct {
	cfg       Config
	queue     QueueReader
	customers CustomerLoader
	recorder  SyncRecorder
	lock      LeaderLock
	writer    ContactWriter
	finder    ContactFinder
	logger    *slog.Logger

	backoff *backoff
	trigger chan struct{}
	lastRun atomic.Pointer[RunSummary]
}

// Deps are the Syncer's collaborators.
type Deps struct {
	Queue     QueueReader
	Customers CustomerLoader
	Recorder  SyncRecorder
	Lock      LeaderLock
	Writer    ContactWriter
	Finder    ContactFinder
	Logger    *slog.Logger
}

func New(cfg Config, d Deps) *Syncer {
	return &Syncer{
		cfg: cfg, queue: d.Queue, customers: d.Customers, recorder: d.Recorder, lock: d.Lock,
		writer: d.Writer, finder: d.Finder, logger: d.Logger,
		backoff: newBackoff(cfg.BackoffInitial, cfg.BackoffMax),
		trigger: make(chan struct{}, 1),
	}
}

// Outcomes of syncing one customer, used in logs, summaries and metrics.
const (
	OutcomeCreated   = "created"   // new contact
	OutcomeUpdated   = "updated"   // known contact updated
	OutcomeRecovered = "recovered" // 409 resolved through lookup (e.g. after a crash)
	OutcomeAdopted   = "adopted"   // existing contact found by email before creating

	OutcomeRejected   = "rejected"            // Acme refused the data (422)
	OutcomeConflict   = "conflict_unresolved" // 409 but no contact found by email
	OutcomeMissing    = "customer_missing"    // queue entry for a deleted customer
	OutcomePartner    = "partner_unavailable" // Acme down, timeouts, open circuit
	OutcomeAuth       = "partner_unauthorized"
	OutcomeInfra      = "database_error"
	OutcomeNotStarted = "not_started" // brand stopped or shutdown before the job ran
)

// RunSummary describes one sync cycle.
type RunSummary struct {
	RunID      string         `json:"run_id"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Leader     bool           `json:"leader"`
	Fetched    int            `json:"fetched_entries"`
	Customers  int            `json:"customers"`
	Outcomes   map[string]int `json:"outcomes"`
	// Orphans are entries of missing customers; Unconfigured are entries of
	// brands without credentials. Both stay outstanding.
	Orphans      int    `json:"orphan_entries"`
	Unconfigured int    `json:"unconfigured_brand_entries"`
	BackingOff   int    `json:"customers_backing_off"`
	BatchFull    bool   `json:"batch_full"`
	Error        string `json:"error,omitempty"`
}

// Synced counts customers whose contact was confirmed this cycle.
func (r RunSummary) Synced() int {
	return r.Outcomes[OutcomeCreated] + r.Outcomes[OutcomeUpdated] + r.Outcomes[OutcomeRecovered] + r.Outcomes[OutcomeAdopted]
}

// Trigger asks for a cycle as soon as possible; it never blocks. It reports
// false when a trigger is already pending.
func (s *Syncer) Trigger() bool {
	select {
	case s.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

// LastRun returns the summary of the last completed cycle.
func (s *Syncer) LastRun() (RunSummary, bool) {
	r := s.lastRun.Load()
	if r == nil {
		return RunSummary{}, false
	}
	return *r, true
}

// Run executes cycles until ctx is cancelled. When a cycle made progress
// with a full batch there is a backlog and the next cycle starts at once;
// otherwise Run waits for the interval or a trigger. In-flight jobs finish
// before Run returns.
func (s *Syncer) Run(ctx context.Context) {
	s.logger.InfoContext(ctx, "contact sync started", "interval", s.cfg.Interval.String(),
		"brands", s.cfg.Brands, "batch_per_brand", s.cfg.BatchPerBrand, "workers_per_brand", s.cfg.WorkersPerBrand)
	for {
		summary := s.RunOnce(ctx)
		if ctx.Err() != nil {
			s.logger.InfoContext(ctx, "contact sync stopped")
			return
		}
		if summary.BatchFull && summary.Synced() > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			s.logger.InfoContext(ctx, "contact sync stopped")
			return
		case <-s.trigger:
		case <-time.After(s.cfg.Interval):
		}
	}
}

// RunOnce executes one cycle: become leader, fetch the outstanding entries
// and sync every affected customer, brands in parallel. The summary is
// completed (duration, backoff size) by a deferred step, hence the named
// result.
func (s *Syncer) RunOnce(ctx context.Context) (summary RunSummary) {
	summary = RunSummary{RunID: uuid.NewV7().String(), StartedAt: time.Now().UTC(), Outcomes: map[string]int{}}
	ctx = logging.WithAttrs(ctx, slog.String("run_id", summary.RunID))
	defer func() {
		summary.FinishedAt = time.Now().UTC()
		summary.BackingOff = s.backoff.size(time.Now())
		s.lastRun.Store(&summary)
		s.logSummary(ctx, summary)
	}()

	lease, leader, err := s.lock.TryLock(ctx, s.cfg.LockName)
	if err != nil {
		summary.Error = "leader election: " + err.Error()
		return summary
	}
	if !leader {
		return summary
	}
	summary.Leader = true
	defer func() {
		// Release even during shutdown, so another replica can take over.
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			s.logger.WarnContext(ctx, "releasing sync leadership failed", "error", err)
		}
	}()

	entries, err := s.queue.Outstanding(ctx, s.cfg.BatchPerBrand, s.backoff.excluded(time.Now()))
	if err != nil {
		summary.Error = "fetch queue: " + err.Error()
		return summary
	}
	summary.Fetched = len(entries)

	jobs, perBrand := s.plan(ctx, entries, &summary)
	for _, n := range perBrand {
		summary.BatchFull = summary.BatchFull || n >= s.cfg.BatchPerBrand
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for brand, brandJobs := range jobs {
		wg.Go(func() {
			outcomes := s.syncBrand(ctx, lease, brand, brandJobs)
			mu.Lock()
			defer mu.Unlock()
			for o, n := range outcomes {
				summary.Outcomes[o] += n
			}
		})
	}
	wg.Wait()
	return summary
}

// job is one customer to sync and the queue entries it settles.
type job struct {
	customer domain.CustomerID
	entries  []uint64
	oldest   time.Time
}

// plan coalesces entries per customer and groups them by brand, keeping the
// queue order. Orphans and unconfigured brands are reported and skipped.
func (s *Syncer) plan(ctx context.Context, entries []domain.QueueEntry, summary *RunSummary) (map[domain.BrandID][]*job, map[domain.BrandID]int) {
	jobs := map[domain.BrandID][]*job{}
	byCustomer := map[domain.CustomerID]*job{}
	perBrand := map[domain.BrandID]int{}

	for _, e := range entries {
		perBrand[e.Brand]++
		switch {
		case e.Brand == 0:
			summary.Orphans++
			continue
		case !slices.Contains(s.cfg.Brands, e.Brand):
			summary.Unconfigured++
			continue
		}
		j, ok := byCustomer[e.Customer]
		if !ok {
			j = &job{customer: e.Customer, oldest: e.QueuedAt}
			byCustomer[e.Customer] = j
			jobs[e.Brand] = append(jobs[e.Brand], j)
		}
		j.entries = append(j.entries, e.ID)
	}
	summary.Customers = len(byCustomer)

	if summary.Orphans > 0 {
		s.logger.WarnContext(ctx, "queue entries reference missing customers; kept outstanding", "entries", summary.Orphans)
	}
	if summary.Unconfigured > 0 {
		s.logger.WarnContext(ctx, "queue entries of brands without Acme credentials; kept outstanding", "entries", summary.Unconfigured)
	}
	return jobs, perBrand
}

// syncBrand runs the brand's jobs on a bounded worker pool. A partner or
// database failure stops the brand for this cycle: further jobs would fail
// the same way and only burn the rate budget.
func (s *Syncer) syncBrand(ctx context.Context, lease Lease, brand domain.BrandID, jobs []*job) map[string]int {
	ctx = logging.WithAttrs(ctx, slog.Int("brand_id", int(brand)))
	brandCtx, stopBrand := context.WithCancel(ctx)
	defer stopBrand()

	queue := make(chan *job, len(jobs))
	for _, j := range jobs {
		queue <- j
	}
	close(queue)

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		outcomes = map[string]int{}
	)
	for range min(s.cfg.WorkersPerBrand, len(jobs)) {
		wg.Go(func() {
			for j := range queue {
				if brandCtx.Err() != nil {
					mu.Lock()
					outcomes[OutcomeNotStarted]++
					mu.Unlock()
					continue
				}
				if err := lease.Held(brandCtx); err != nil {
					s.logger.ErrorContext(ctx, "sync leadership lost, stopping", "error", err)
					stopBrand()
					mu.Lock()
					outcomes[OutcomeNotStarted]++
					mu.Unlock()
					continue
				}

				outcome := s.syncCustomer(brandCtx, brand, j)
				switch outcome {
				case OutcomePartner, OutcomeAuth, OutcomeInfra:
					stopBrand()
				}
				mu.Lock()
				outcomes[outcome]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return outcomes
}

// syncCustomer pushes one customer's current state and records the result.
// The job runs on a context detached from shutdown so an in-flight Acme call
// and its database write complete instead of being abandoned halfway.
func (s *Syncer) syncCustomer(ctx context.Context, brand domain.BrandID, j *job) string {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.JobTimeout)
	defer cancel()
	ctx = logging.WithAttrs(ctx, slog.Int("customer_id", int(j.customer)), slog.Any("queue_ids", j.entries))

	outcome, contactID, err := s.push(ctx, brand, j)
	attrs := []any{"outcome", outcome, "duration_ms", time.Since(start).Milliseconds(),
		"queue_lag_ms", time.Since(j.oldest).Milliseconds()}
	if contactID != "" {
		attrs = append(attrs, "contact_id", contactID)
	}

	switch outcome {
	case OutcomeCreated, OutcomeUpdated, OutcomeRecovered, OutcomeAdopted:
		s.backoff.clear(j.customer)
		s.logger.InfoContext(ctx, "customer synced", attrs...)
	case OutcomeRejected, OutcomeConflict, OutcomeMissing:
		retry := s.backoff.fail(j.customer, time.Now())
		attrs = append(attrs, "error", err.Error(), "retry_after", retry.Format(time.RFC3339))
		s.logger.WarnContext(ctx, "customer sync failed, entries kept outstanding", attrs...)
	default:
		attrs = append(attrs, "error", err.Error())
		s.logger.ErrorContext(ctx, "customer sync failed, brand paused for this cycle", attrs...)
	}
	return outcome
}

// push performs the idempotent upsert and records it. It returns the
// outcome, the contact id when known and the error of a failed outcome.
func (s *Syncer) push(ctx context.Context, brand domain.BrandID, j *job) (string, string, error) {
	customer, err := s.customers.LoadForSync(ctx, j.customer)
	switch {
	case errors.Is(err, domain.ErrCustomerNotFound):
		return OutcomeMissing, "", err
	case err != nil:
		return OutcomeInfra, "", err
	}

	outcome, contactID, err := s.upsert(ctx, brand, customer)
	if err != nil {
		return failureOutcome(err), contactID, err
	}

	marked, err := s.recorder.RecordSynced(ctx, customer.ID, contactID, j.entries)
	if err != nil {
		// Acme has the contact but we could not store it: the entries stay
		// outstanding and the next attempt resolves it through 409/lookup.
		return OutcomeInfra, contactID, fmt.Errorf("record sync: %w", err)
	}
	if marked < int64(len(j.entries)) {
		s.logger.InfoContext(ctx, "some queue entries were already processed", "marked", marked, "entries", len(j.entries))
	}
	return outcome, contactID, nil
}

// errUnresolvedConflict reports a 409 whose contact cannot be found.
var errUnresolvedConflict = errors.New("contact exists but cannot be found by email")

// upsert creates or updates the customer's contact:
//
//  1. known contact id: update it; if Acme no longer knows it, create anew;
//  2. optionally adopt a contact found by email;
//  3. create; on 409 (we created it before but did not store the id, e.g.
//     after a crash or timeout) look it up by email and update it.
func (s *Syncer) upsert(ctx context.Context, brand domain.BrandID, customer domain.SyncCustomer) (string, string, error) {
	contact := customer.Contact()

	if id := customer.ContactID; id != "" {
		err := s.writer.Update(ctx, brand, id, contact)
		if err == nil {
			return OutcomeUpdated, id, nil
		}
		if !errors.Is(err, ErrContactNotFound) {
			return "", id, err
		}
		s.logger.WarnContext(ctx, "stored contact id unknown to Acme Support, creating the contact again", "contact_id", id)
	}

	if s.cfg.LookupBeforeCreate && contact.Email != "" {
		id, err := s.finder.FindByEmail(ctx, brand, contact.Email)
		switch {
		case err == nil:
			if err := s.writer.Update(ctx, brand, id, contact); err != nil {
				return "", id, err
			}
			return OutcomeAdopted, id, nil
		case !errors.Is(err, ErrContactNotFound):
			return "", "", err
		}
	}

	id, err := s.writer.Create(ctx, brand, contact)
	if err == nil {
		return OutcomeCreated, id, nil
	}
	if !errors.Is(err, ErrContactConflict) {
		return "", "", err
	}

	if contact.Email == "" {
		return "", "", fmt.Errorf("%w: customer has no email", errUnresolvedConflict)
	}
	id, err = s.finder.FindByEmail(ctx, brand, contact.Email)
	if errors.Is(err, ErrContactNotFound) {
		return "", "", fmt.Errorf("%w: %w", errUnresolvedConflict, err)
	}
	if err != nil {
		return "", "", err
	}
	if err := s.writer.Update(ctx, brand, id, contact); err != nil {
		return "", id, err
	}
	return OutcomeRecovered, id, nil
}

func failureOutcome(err error) string {
	switch {
	case errors.Is(err, ErrContactRejected):
		return OutcomeRejected
	case errors.Is(err, errUnresolvedConflict):
		return OutcomeConflict
	case errors.Is(err, ErrPartnerUnauthorized):
		return OutcomeAuth
	case errors.Is(err, ErrContactNotFound):
		// Update after a successful lookup lost the race with a deletion.
		return OutcomeConflict
	default:
		return OutcomePartner
	}
}

func (s *Syncer) logSummary(ctx context.Context, r RunSummary) {
	if !r.Leader && r.Error == "" {
		s.logger.DebugContext(ctx, "sync cycle skipped: another replica is the leader")
		return
	}
	attrs := []any{"leader", r.Leader, "fetched_entries", r.Fetched, "customers", r.Customers,
		"synced", r.Synced(), "outcomes", r.Outcomes, "orphan_entries", r.Orphans,
		"unconfigured_brand_entries", r.Unconfigured, "customers_backing_off", r.BackingOff,
		"batch_full", r.BatchFull, "duration_ms", r.FinishedAt.Sub(r.StartedAt).Milliseconds()}
	switch {
	case r.Error != "":
		s.logger.ErrorContext(ctx, "sync cycle failed", append(attrs, "error", r.Error)...)
	case r.Fetched == 0:
		s.logger.DebugContext(ctx, "sync cycle finished: nothing to do")
	default:
		s.logger.InfoContext(ctx, "sync cycle finished", attrs...)
	}
}
