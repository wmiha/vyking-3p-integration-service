package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// Reference ids from db/migrations/V2__reference_data.sql.
const (
	curEUR, curGBP, curJPY, curBHD, curUSD = 1, 2, 3, 4, 5

	typPayin, typPayout, typAdjustment = 1001, 2001, 3001
)

var statuses = []int{10, 20, 30, 40, 50, 60, 61, 70, 71, 72, 80, 81, 82, 90, 91}

var brands = []row{
	{1, "Vyking Casino"},
	{2, "Vyking Sports"},
	{3, "Vyking Poker"}, // deliberately left without Acme credentials in local config
}

var paymentMethods = []row{
	{1, "PAYIN CARD"},
	{2, "PAYIN BANK TRANSFER"},
	{3, "PAYOUT CARD"},
	{4, "PAYOUT BANK TRANSFER"},
	{5, "E-WALLET"},
}

// row is one INSERT tuple; nil values become SQL NULL.
type row []any

type customer struct {
	id, brand, currency      int
	email, first, last, user any
	phone, country           any
	created                  time.Time
	active                   bool
	// txCount overrides the random number of transactions when > 0.
	txCount int
}

func (c customer) row() row {
	active := 0
	if c.active {
		active = 1
	}
	return row{c.id, c.brand, c.user, c.email, c.first, c.last, c.phone, c.country, c.currency, c.created, active}
}

// fixtures are well-known customers covering the edge cases of the task.
// They are documented in the README so they can be used for manual testing.
func fixtures(now time.Time) []customer {
	reg := now.AddDate(-1, 0, 0)
	return []customer{
		{id: 1001, brand: 1, currency: curEUR, email: "jane.doe@example.com", first: "Jane", last: "Doe", user: "janed", phone: "+4712345678", country: "NO", created: reg, active: true},
		// Same email in another brand: ambiguous for multi-brand partners.
		{id: 1002, brand: 2, currency: curGBP, email: "jane.doe@example.com", first: "Jane", last: "Doe", user: "jane_sports", phone: "+447700900123", country: "GB", created: reg, active: true},
		{id: 1003, brand: 1, currency: curJPY, email: "taro@example.jp", first: "Taro", last: "Yamada", user: "taro", phone: "+81312345678", country: "JP", created: reg, active: true},
		{id: 1004, brand: 1, currency: curBHD, email: "ali@example.bh", first: "Ali", last: "Hassan", user: "ali", phone: "+97317123456", country: "BH", created: reg, active: true},
		// USD has NULL minor units in the reference data.
		{id: 1005, brand: 2, currency: curUSD, email: "sam@example.com", first: "Sam", last: "Smith", user: "sam", phone: "+12025550123", country: "US", created: reg, active: true},
		// No email: cannot be looked up by the partner API.
		{id: 1006, brand: 1, currency: curEUR, first: "No", last: "Email", user: "noemail", country: "DE", created: reg, active: true},
		{id: 1007, brand: 1, currency: curEUR, email: "inactive@example.com", first: "Ina", last: "Active", user: "inactive", country: "SE", created: reg, active: false},
		// Only the mandatory columns.
		{id: 1008, brand: 1, currency: curEUR, email: "minimal@example.com", created: reg, active: true},
		// Enough transactions to page through.
		{id: 1009, brand: 1, currency: curEUR, email: "heavy@example.com", first: "Heavy", last: "User", user: "heavy", country: "MT", created: reg, active: true, txCount: 250},
		// Brand without Acme credentials configured.
		{id: 1010, brand: 3, currency: curEUR, email: "poker@example.com", first: "Pat", last: "Poker", user: "pat", country: "IE", created: reg, active: true},
	}
}

var (
	firstNames = []string{"Ana", "Marko", "Elena", "Luka", "Sara", "Nikola", "Mia", "Stefan", "Ivana", "Petar"}
	lastNames  = []string{"Novak", "Horvat", "Kovac", "Babic", "Maric", "Petrovic", "Jovanovic", "Ilic"}
	countries  = []string{"RS", "HR", "SI", "DE", "AT", "MT", "NO", "SE", "GB", "IE"}
	currencies = []int{curEUR, curEUR, curEUR, curGBP, curJPY, curBHD, curUSD}
)

type options struct {
	customers  int     // random customers in addition to the fixtures
	maxTx      int     // max transactions per random customer
	queueShare float64 // share of random customers with outstanding sync entries
	seed       uint64
	now        time.Time
}

type dataset struct {
	brands, paymentMethods, customers, transactions, syncQueue, partnerContacts []row
}

// generate builds a deterministic dataset: the same options always produce
// the same rows, so test scenarios are reproducible.
func generate(o options) dataset {
	rng := rand.New(rand.NewPCG(o.seed, o.seed^0x5eed))
	ds := dataset{brands: brands, paymentMethods: paymentMethods}

	custs := fixtures(o.now)
	for i := range o.customers {
		id := 2001 + i
		first := firstNames[rng.IntN(len(firstNames))]
		last := lastNames[rng.IntN(len(lastNames))]
		custs = append(custs, customer{
			id:       id,
			brand:    1 + rng.IntN(len(brands)),
			currency: currencies[rng.IntN(len(currencies))],
			email:    fmt.Sprintf("customer%d@example.com", id),
			first:    first, last: last,
			user:    fmt.Sprintf("user%d", id),
			phone:   fmt.Sprintf("+3816%07d", rng.IntN(10_000_000)),
			country: countries[rng.IntN(len(countries))],
			created: o.now.AddDate(0, 0, -rng.IntN(700)-1).Truncate(time.Second),
			active:  rng.IntN(20) != 0,
		})
	}

	trxID := 1
	queueID := 1
	for _, c := range custs {
		ds.customers = append(ds.customers, c.row())

		isFixture := c.id < 2000
		n := c.txCount
		switch {
		case n > 0:
		case isFixture:
			n = len(statuses) + rng.IntN(10)
		default:
			n = rng.IntN(o.maxTx + 1)
		}
		for k := range n {
			status := statuses[rng.IntN(len(statuses))]
			if isFixture && k < len(statuses) {
				status = statuses[k] // every fixture sees every status
			}
			ds.transactions = append(ds.transactions, transaction(rng, trxID, c, status, o.now))
			trxID++
		}

		// Every fixture and a share of random customers have outstanding sync
		// entries; some customers have several (coalescing) and some have an
		// already processed history entry.
		if isFixture || rng.Float64() < o.queueShare {
			queued := o.now.Add(-time.Duration(rng.IntN(3600)+60) * time.Second)
			if rng.IntN(3) == 0 {
				ds.syncQueue = append(ds.syncQueue, row{queueID, c.id, queued.Add(-48 * time.Hour), queued.Add(-47 * time.Hour)})
				queueID++
			}
			for range 1 + rng.IntN(3) {
				ds.syncQueue = append(ds.syncQueue, row{queueID, c.id, queued, nil})
				queueID++
				queued = queued.Add(time.Duration(rng.IntN(120)+1) * time.Second)
			}
		}
	}

	// A contact id Acme Support does not know: exercises the "PATCH returns
	// 404, create again" path of the sync.
	ds.partnerContacts = append(ds.partnerContacts, row{1003, "stale-contact-1003", o.now.Add(-72 * time.Hour)})
	return ds
}

func transaction(rng *rand.Rand, id int, c customer, status int, now time.Time) row {
	created := now.Add(-time.Duration(rng.IntN(90*24*3600)+60) * time.Second).Truncate(time.Second)

	typ := typPayin
	switch r := rng.IntN(10); {
	case r >= 9:
		typ = typAdjustment
	case r >= 6:
		typ = typPayout
	}

	var amount any = int64(rng.IntN(500_000) + 100)
	if rng.IntN(25) == 0 {
		amount = nil // amounts are nullable in the schema
	}
	var method any = 1 + rng.IntN(len(paymentMethods))
	if typ == typAdjustment || rng.IntN(30) == 0 {
		method = nil
	}

	// Booked once accepted for processing (status >= booked), changed at the
	// last state transition.
	var booked any
	changed := created
	if status >= 50 {
		b := created.Add(time.Duration(rng.IntN(600)+5) * time.Second)
		booked = b
		changed = b
	}
	if status != 10 {
		changed = changed.Add(time.Duration(rng.IntN(7200)+1) * time.Second)
	}
	return row{id, c.id, typ, status, amount, method, created, booked, changed}
}
