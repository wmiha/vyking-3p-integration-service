// Package domain holds the business types and rules of the integration. It
// has no dependencies on transport, storage or third-party APIs.
package domain

import "slices"

// BrandID identifies a brand (BRD_iID).
type BrandID uint32

// ScopeTransactionsRead allows reading customers' transactions.
const ScopeTransactionsRead = "transactions:read"

// Principal is an authenticated API caller (a partner such as Acme Support)
// and what it may access.
type Principal struct {
	Subject string
	Brands  []BrandID
	Scopes  []string
}

// CanAccessBrand reports whether the principal may see data of brand id.
func (p Principal) CanAccessBrand(id BrandID) bool {
	return slices.Contains(p.Brands, id)
}

// HasScope reports whether the principal was granted scope.
func (p Principal) HasScope(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}
