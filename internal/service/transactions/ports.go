package transactions

import (
	"context"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// CustomerFinder looks customers up by email within the given brands.
type CustomerFinder interface {
	FindByEmail(ctx context.Context, email string, brands []domain.BrandID) ([]domain.Customer, error)
}

// TransactionLister returns one page of a customer's transactions, newest
// first.
type TransactionLister interface {
	ListPage(ctx context.Context, q PageQuery) ([]domain.Transaction, error)
}

// ResultCache caches query results. Implementations may be in-process or
// shared (e.g. Redis); a failing cache must behave like an empty one.
type ResultCache interface {
	Get(ctx context.Context, key string) (Result, bool)
	Set(ctx context.Context, key string, r Result)
}

// PageQuery selects a page of transactions using keyset pagination.
type PageQuery struct {
	Customer domain.CustomerID
	// StatusIDs restricts the internal statuses; empty means all.
	StatusIDs []uint32
	// After continues after this position (exclusive); nil starts at the
	// newest transaction.
	After *Position
	Limit int
}

// Position is a point in the (created_at DESC, id DESC) ordering.
type Position struct {
	CreatedUnix int64
	ID          uint64
}
