// Package transactions implements the partner use case "show a customer's
// recent transactions", scoped to the brands the partner may access.
package transactions

import (
	"context"
	"fmt"
	"slices"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// Query is a partner request.
type Query struct {
	Email    string
	Statuses []domain.Status // empty means all
	Limit    int
	// Brand narrows the lookup to one brand (0 = every brand the partner may
	// access); needed when an email exists in several of them.
	Brand  domain.BrandID
	Cursor string
}

// Result is one page of a customer's transactions, newest first.
type Result struct {
	Customer     domain.Customer
	Transactions []domain.Transaction
	// NextCursor continues after the last transaction; empty on the last page.
	NextCursor string
}

// Service answers partner transaction queries.
type Service struct {
	customers    CustomerFinder
	transactions TransactionLister
}

func NewService(customers CustomerFinder, transactions TransactionLister) *Service {
	return &Service{customers: customers, transactions: transactions}
}

// Find resolves the customer by email within the partner's brands and returns
// one page of their transactions.
func (s *Service) Find(ctx context.Context, p domain.Principal, q Query) (Result, error) {
	brands := p.Brands
	if q.Brand != 0 {
		if !p.CanAccessBrand(q.Brand) {
			return Result{}, domain.ErrBrandForbidden
		}
		brands = []domain.BrandID{q.Brand}
	}

	customer, err := s.resolveCustomer(ctx, q.Email, brands)
	if err != nil {
		return Result{}, err
	}

	statusIDs := domain.InternalStatusIDs(q.Statuses)
	filter := filterKey(customer.ID, statusIDs)
	var after *Position
	if q.Cursor != "" {
		if after, err = decodeCursor(q.Cursor, filter); err != nil {
			return Result{}, err
		}
	}

	// One extra row tells whether another page exists without a COUNT query.
	txs, err := s.transactions.ListPage(ctx, PageQuery{
		Customer:  customer.ID,
		StatusIDs: statusIDs,
		After:     after,
		Limit:     q.Limit + 1,
	})
	if err != nil {
		return Result{}, fmt.Errorf("list transactions: %w", err)
	}

	res := Result{Customer: customer, Transactions: txs}
	if len(txs) > q.Limit {
		res.Transactions = txs[:q.Limit]
		last := res.Transactions[q.Limit-1]
		res.NextCursor = encodeCursor(Position{CreatedUnix: last.CreatedAt.Unix(), ID: last.ID}, filter)
	}
	return res, nil
}

func (s *Service) resolveCustomer(ctx context.Context, email string, brands []domain.BrandID) (domain.Customer, error) {
	customers, err := s.customers.FindByEmail(ctx, email, brands)
	if err != nil {
		return domain.Customer{}, fmt.Errorf("find customer: %w", err)
	}
	switch len(customers) {
	case 0:
		// Also the answer for customers of brands the partner may not
		// access, so their existence is not disclosed.
		return domain.Customer{}, domain.ErrCustomerNotFound
	case 1:
		return customers[0], nil
	default:
		ambiguous := &domain.AmbiguousCustomerError{}
		for _, c := range customers {
			ambiguous.Brands = append(ambiguous.Brands, c.Brand)
		}
		slices.Sort(ambiguous.Brands)
		return domain.Customer{}, ambiguous
	}
}
