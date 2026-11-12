package transactions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

type fakeCustomers struct {
	byBrand   map[domain.BrandID]domain.Customer
	err       error
	gotBrands []domain.BrandID
}

func (f *fakeCustomers) FindByEmail(_ context.Context, _ string, brands []domain.BrandID) ([]domain.Customer, error) {
	f.gotBrands = brands
	var out []domain.Customer
	for _, b := range brands {
		if c, ok := f.byBrand[b]; ok {
			out = append(out, c)
		}
	}
	return out, f.err
}

// fakeTransactions serves ids n..1 (newest first), one per minute.
type fakeTransactions struct {
	n     int
	err   error
	query PageQuery
}

func (f *fakeTransactions) ListPage(_ context.Context, q PageQuery) ([]domain.Transaction, error) {
	f.query = q
	if f.err != nil {
		return nil, f.err
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out []domain.Transaction
	for id := f.n; id >= 1 && len(out) < q.Limit; id-- {
		created := base.Add(time.Duration(id) * time.Minute)
		if q.After != nil && (created.Unix() > q.After.CreatedUnix ||
			created.Unix() == q.After.CreatedUnix && uint64(id) >= q.After.ID) {
			continue
		}
		out = append(out, domain.Transaction{ID: uint64(id), StatusID: 60, CreatedAt: created, ChangedAt: created})
	}
	return out, nil
}

var (
	jane1  = domain.Customer{ID: 1001, Brand: 1, Currency: "EUR"}
	jane2  = domain.Customer{ID: 1002, Brand: 2, Currency: "GBP"}
	reader = domain.Principal{Subject: "acme", Brands: []domain.BrandID{1, 2}}
)

func TestFindResolvesCustomer(t *testing.T) {
	repoErr := errors.New("connection refused")
	tests := []struct {
		name       string
		customers  map[domain.BrandID]domain.Customer
		lookupErr  error
		principal  domain.Principal
		brand      domain.BrandID
		wantBrands []domain.BrandID // brands passed to the lookup
		wantID     domain.CustomerID
		wantErr    error
		wantAmbig  []domain.BrandID
	}{
		{name: "single match", customers: map[domain.BrandID]domain.Customer{1: jane1}, principal: reader,
			wantBrands: []domain.BrandID{1, 2}, wantID: 1001},
		{name: "not found", principal: reader, wantBrands: []domain.BrandID{1, 2}, wantErr: domain.ErrCustomerNotFound},
		{name: "other brand is invisible", customers: map[domain.BrandID]domain.Customer{2: jane2},
			principal: domain.Principal{Brands: []domain.BrandID{1}}, wantBrands: []domain.BrandID{1}, wantErr: domain.ErrCustomerNotFound},
		{name: "ambiguous across brands", customers: map[domain.BrandID]domain.Customer{1: jane1, 2: jane2}, principal: reader,
			wantBrands: []domain.BrandID{1, 2}, wantAmbig: []domain.BrandID{1, 2}},
		{name: "brand narrows the lookup", customers: map[domain.BrandID]domain.Customer{1: jane1, 2: jane2}, principal: reader,
			brand: 2, wantBrands: []domain.BrandID{2}, wantID: 1002},
		{name: "brand outside the token", principal: domain.Principal{Brands: []domain.BrandID{1}}, brand: 2,
			wantErr: domain.ErrBrandForbidden},
		{name: "lookup failure", lookupErr: repoErr, principal: reader, wantBrands: []domain.BrandID{1, 2}, wantErr: repoErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			customers := &fakeCustomers{byBrand: tt.customers, err: tt.lookupErr}
			svc := NewService(customers, &fakeTransactions{n: 3})

			res, err := svc.Find(context.Background(), tt.principal, Query{Email: "jane@example.com", Limit: 10, Brand: tt.brand})
			assert.Equal(t, tt.wantBrands, customers.gotBrands)
			switch {
			case tt.wantAmbig != nil:
				ambiguous, ok := errors.AsType[*domain.AmbiguousCustomerError](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, tt.wantAmbig, ambiguous.Brands)
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantID, res.Customer.ID)
			}
		})
	}
}

func TestFindPaginates(t *testing.T) {
	txs := &fakeTransactions{n: 25}
	svc := NewService(&fakeCustomers{byBrand: map[domain.BrandID]domain.Customer{1: jane1}}, txs)

	var (
		seen   []uint64
		cursor string
		pages  int
	)
	for {
		res, err := svc.Find(context.Background(), reader, Query{Email: "jane@example.com", Limit: 10, Cursor: cursor})
		require.NoError(t, err)
		pages++
		for _, tx := range res.Transactions {
			seen = append(seen, tx.ID)
		}
		assert.Equal(t, 11, txs.query.Limit, "one extra row detects the next page")
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	assert.Equal(t, 3, pages)
	require.Len(t, seen, 25)
	for i, id := range seen {
		assert.Equal(t, uint64(25-i), id, "newest first, no gaps, no duplicates")
	}
}

func TestFindStatusFilterAndCursorBinding(t *testing.T) {
	txs := &fakeTransactions{n: 5}
	svc := NewService(&fakeCustomers{byBrand: map[domain.BrandID]domain.Customer{1: jane1}}, txs)
	ctx := context.Background()

	res, err := svc.Find(ctx, reader, Query{Email: "j", Limit: 2, Statuses: []domain.Status{domain.StatusFailed, domain.StatusCancelled}})
	require.NoError(t, err)
	assert.Equal(t, []uint32{70, 71, 72, 80, 81, 82}, txs.query.StatusIDs)
	assert.Nil(t, txs.query.After)
	require.NotEmpty(t, res.NextCursor)

	// Same filter: the cursor positions the next page.
	_, err = svc.Find(ctx, reader, Query{Email: "j", Limit: 2, Cursor: res.NextCursor,
		Statuses: []domain.Status{domain.StatusFailed, domain.StatusCancelled}})
	require.NoError(t, err)
	require.NotNil(t, txs.query.After)
	assert.Equal(t, uint64(4), txs.query.After.ID)

	tests := []struct {
		name  string
		query Query
	}{
		{"different status filter", Query{Email: "j", Limit: 2, Cursor: res.NextCursor, Statuses: []domain.Status{domain.StatusPending}}},
		{"no status filter", Query{Email: "j", Limit: 2, Cursor: res.NextCursor}},
		{"garbage", Query{Email: "j", Limit: 2, Cursor: "not-a-cursor!"}},
		{"valid base64, not a cursor", Query{Email: "j", Limit: 2, Cursor: "e30"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Find(ctx, reader, tt.query)
			assert.ErrorIs(t, err, domain.ErrInvalidCursor)
		})
	}
}

func TestFindCursorIsBoundToCustomer(t *testing.T) {
	svc := NewService(&fakeCustomers{byBrand: map[domain.BrandID]domain.Customer{1: jane1, 2: jane2}}, &fakeTransactions{n: 5})
	ctx := context.Background()

	res, err := svc.Find(ctx, reader, Query{Email: "j", Limit: 2, Brand: 1})
	require.NoError(t, err)

	_, err = svc.Find(ctx, reader, Query{Email: "j", Limit: 2, Brand: 2, Cursor: res.NextCursor})
	assert.ErrorIs(t, err, domain.ErrInvalidCursor)
}

func TestFindPropagatesListErrors(t *testing.T) {
	listErr := errors.New("timeout")
	svc := NewService(&fakeCustomers{byBrand: map[domain.BrandID]domain.Customer{1: jane1}}, &fakeTransactions{err: listErr})
	_, err := svc.Find(context.Background(), reader, Query{Email: "j", Limit: 2})
	assert.ErrorIs(t, err, listErr)
}
