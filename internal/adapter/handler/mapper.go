package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

// transactionsResponse is the public contract of GET /v1/transactions.
// Acme Support maps field names on its side; statuses use its vocabulary.
type transactionsResponse struct {
	RequestID    string           `json:"request_id"`
	Customer     customerDTO      `json:"customer"`
	Transactions []transactionDTO `json:"transactions"`
	Page         pageDTO          `json:"page"`
}

type customerDTO struct {
	// ID is also the external_id of the customer's Acme Support contact.
	ID       string  `json:"id"`
	BrandID  uint32  `json:"brand_id"`
	Brand    string  `json:"brand"`
	Currency *string `json:"currency"`
	Active   bool    `json:"active"`
}

type transactionDTO struct {
	ID   string `json:"id"` // string: 64-bit ids exceed JavaScript's safe integers
	Type string `json:"type"`
	// Amount is a JSON number in the customer's currency (not minor units),
	// rendered exactly; null when unknown.
	Amount        *json.Number  `json:"amount"`
	Currency      *string       `json:"currency"`
	Status        domain.Status `json:"status"`
	StatusDetail  string        `json:"status_detail"`
	FailureReason *string       `json:"failure_reason"`
	PaymentMethod *string       `json:"payment_method"`
	CreatedAt     string        `json:"created_at"`
	BookedAt      *string       `json:"booked_at"`
	SettledAt     *string       `json:"settled_at"`
	UpdatedAt     string        `json:"updated_at"`
}

type pageDTO struct {
	Limit      int     `json:"limit"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

func toTransactionsResponse(ctx context.Context, logger *slog.Logger, res transactions.Result, limit int, requestID string) transactionsResponse {
	c := res.Customer
	currency := optional(c.Currency)
	units, unitsKnown := domain.MinorUnits(c.Currency, c.MinorUnits)
	if !unitsKnown {
		logger.WarnContext(ctx, "currency minor units unknown, amounts omitted",
			"customer_id", c.ID, "currency", c.Currency)
	}

	txs := make([]transactionDTO, 0, len(res.Transactions))
	for _, t := range res.Transactions {
		info, known := domain.MapStatus(t.StatusID)
		if !known {
			logger.WarnContext(ctx, "unknown transaction status reported as pending",
				"transaction_id", t.ID, "status_id", t.StatusID)
		}

		dto := transactionDTO{
			ID:            strconv.FormatUint(t.ID, 10),
			Type:          typeName(t.Type),
			Currency:      currency,
			Status:        info.Status,
			StatusDetail:  info.Detail,
			FailureReason: optional(info.FailureReason),
			PaymentMethod: t.PaymentMethod,
			CreatedAt:     rfc3339(t.CreatedAt),
			BookedAt:      optionalTime(t.BookedAt),
			UpdatedAt:     rfc3339(t.ChangedAt),
		}
		if t.AmountMinor != nil && unitsKnown {
			amount := json.Number(domain.FormatMinor(*t.AmountMinor, units))
			dto.Amount = &amount
		}
		if info.Settled {
			// The last state change of a settled transaction is its settlement;
			// the schema has no dedicated settlement timestamp.
			dto.SettledAt = optionalTime(&t.ChangedAt)
		}
		txs = append(txs, dto)
	}

	return transactionsResponse{
		RequestID: requestID,
		Customer: customerDTO{
			ID:       strconv.FormatUint(uint64(c.ID), 10),
			BrandID:  uint32(c.Brand),
			Brand:    c.BrandName,
			Currency: currency,
			Active:   c.Active,
		},
		Transactions: txs,
		Page: pageDTO{
			Limit:      limit,
			HasMore:    res.NextCursor != "",
			NextCursor: optional(res.NextCursor),
		},
	}
}

// typeName renders TYP_cName as a lower-case machine-readable value.
func typeName(name string) string {
	if name == "" {
		return "unknown"
	}
	return strings.ToLower(name)
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := rfc3339(*t)
	return &s
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
