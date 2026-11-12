package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/logging"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

// TransactionFinder answers partner transaction queries.
type TransactionFinder interface {
	Find(ctx context.Context, p domain.Principal, q transactions.Query) (transactions.Result, error)
}

// PageLimits bound the page size of the partner API.
type PageLimits struct {
	Default int
	Max     int
}

const (
	maxEmailLen  = 190 // CUS_cEmail VARCHAR(190)
	maxCursorLen = 512
)

// listTransactions serves GET /v1/transactions:
//
//	?email=jane@example.com     required
//	&status=failed,cancelled    optional, comma-separated or repeated
//	&limit=20                   optional page size
//	&brand_id=1                 optional, disambiguates emails present in several brands
//	&cursor=...                 optional, from page.next_cursor of the previous page
func listTransactions(finder TransactionFinder, limits PageLimits, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		principal, ok := principalFrom(ctx)
		if !ok {
			apierror.Write(w, r, apierror.Internal(errors.New("transactions route is not authenticated")))
			return
		}

		q, apiErr := parseTransactionsQuery(r.URL.Query(), limits)
		if apiErr != nil {
			apierror.Write(w, r, apiErr)
			return
		}
		ctx = logging.WithAttrs(ctx, slog.String("partner", principal.Subject), slog.String("email", logging.MaskEmail(q.Email)))

		res, err := finder.Find(ctx, principal, q)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return // the client went away; nobody is left to answer
			}
			apiErr := domainError(err)
			if e, ok := errors.AsType[*apierror.Error](apiErr); ok && e.Status < http.StatusInternalServerError {
				logger.InfoContext(ctx, "partner lookup rejected", "code", e.Code, "brand_id", uint32(q.Brand))
			}
			apierror.Write(w, r.WithContext(ctx), apiErr)
			return
		}

		// Audit trail: which partner looked at which customer's data.
		logger.InfoContext(ctx, "partner lookup", "customer_id", uint32(res.Customer.ID),
			"brand_id", uint32(res.Customer.Brand), "returned", len(res.Transactions),
			"statuses", q.Statuses, "has_more", res.NextCursor != "", "paged", q.Cursor != "")
		writeJSON(w, http.StatusOK, toTransactionsResponse(ctx, logger, res, q.Limit, requestid.FromContext(ctx)))
	})
}

func parseTransactionsQuery(v url.Values, limits PageLimits) (transactions.Query, *apierror.Error) {
	q := transactions.Query{Limit: limits.Default}

	q.Email = strings.TrimSpace(v.Get("email"))
	switch {
	case q.Email == "":
		return q, apierror.InvalidParameter("email", "email is required")
	case len(q.Email) > maxEmailLen || !validEmail(q.Email):
		return q, apierror.InvalidParameter("email", "email is not a valid address")
	}

	for _, raw := range v["status"] {
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			status, ok := domain.ParseStatus(strings.ToLower(s))
			if !ok {
				return q, apierror.InvalidParameter("status", "status must be one of pending, completed, failed, cancelled")
			}
			if !slices.Contains(q.Statuses, status) {
				q.Statuses = append(q.Statuses, status)
			}
		}
	}

	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > limits.Max {
			return q, apierror.InvalidParameter("limit", fmt.Sprintf("limit must be an integer between 1 and %d", limits.Max))
		}
		q.Limit = n
	}

	if raw := v.Get("brand_id"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || n == 0 {
			return q, apierror.InvalidParameter("brand_id", "brand_id must be a positive integer")
		}
		q.Brand = domain.BrandID(n)
	}

	if q.Cursor = v.Get("cursor"); len(q.Cursor) > maxCursorLen {
		return q, apierror.New(http.StatusBadRequest, apierror.CodeInvalidCursor, "cursor is invalid")
	}
	return q, nil
}

// validEmail accepts a bare RFC 5322 address ("jane@example.com"), rejecting
// display-name forms ("Jane <jane@example.com>").
func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}

// domainError maps use case errors to API errors. Unknown errors become 500.
func domainError(err error) error {
	if ambiguous, ok := errors.AsType[*domain.AmbiguousCustomerError](err); ok {
		return apierror.New(http.StatusConflict, apierror.CodeCustomerAmbiguous,
			"the email matches customers in several brands; repeat the request with brand_id").
			WithDetails(map[string]any{"brand_ids": ambiguous.Brands})
	}
	switch {
	case errors.Is(err, domain.ErrCustomerNotFound):
		return apierror.New(http.StatusNotFound, apierror.CodeCustomerNotFound,
			"no customer with this email in the brands you may access")
	case errors.Is(err, domain.ErrBrandForbidden):
		return apierror.New(http.StatusForbidden, apierror.CodeBrandForbidden,
			"the token does not grant access to this brand")
	case errors.Is(err, domain.ErrInvalidCursor):
		return apierror.New(http.StatusBadRequest, apierror.CodeInvalidCursor,
			"cursor is invalid or belongs to a different query")
	case errors.Is(err, domain.ErrUnavailable):
		return apierror.Unavailable(err)
	default:
		return err
	}
}
