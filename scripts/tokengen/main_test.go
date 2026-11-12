package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// TestMatrixMatchesVerifier guarantees every generated token produces the
// outcome its name promises when checked by the service's verifier.
func TestMatrixMatchesVerifier(t *testing.T) {
	key := auth.Key{ID: "local-1", Secret: []byte("local-dev-only-jwt-secret-0123456789")}
	ti := tokenIssuer{iss: "vyking-integration", aud: "vyking-partner-api", key: key, now: time.Now()}
	v, err := auth.NewVerifier(auth.Config{Issuer: ti.iss, Audience: ti.aud, Leeway: 30 * time.Second, Keys: []auth.Key{key}})
	require.NoError(t, err)

	want := map[string]struct {
		brands  []domain.BrandID
		scoped  bool
		wantErr error
	}{
		"VALID_BRAND_1":    {brands: []domain.BrandID{1}, scoped: true},
		"VALID_BRAND_2":    {brands: []domain.BrandID{2}, scoped: true},
		"VALID_BRANDS_1_2": {brands: []domain.BrandID{1, 2}, scoped: true},
		"VALID_ALL_BRANDS": {brands: []domain.BrandID{1, 2, 3}, scoped: true},
		"NO_SCOPE":         {brands: []domain.BrandID{1}, scoped: false},
		"EXPIRED":          {wantErr: auth.ErrExpired},
		"NOT_YET_VALID":    {wantErr: auth.ErrInvalid},
		"WRONG_SIGNATURE":  {wantErr: auth.ErrInvalid},
		"UNKNOWN_KID":      {wantErr: auth.ErrInvalid},
		"WRONG_AUDIENCE":   {wantErr: auth.ErrInvalid},
		"WRONG_ISSUER":     {wantErr: auth.ErrInvalid},
		"NO_BRANDS":        {wantErr: auth.ErrInvalid},
		"ALG_NONE":         {wantErr: auth.ErrInvalid},
		"MALFORMED":        {wantErr: auth.ErrMalformed},
	}

	matrix := ti.matrix()
	require.Len(t, matrix, len(want), "every generated token needs an expectation")
	for _, nt := range matrix {
		t.Run(nt.name, func(t *testing.T) {
			exp, ok := want[nt.name]
			require.True(t, ok, "unexpected token %s", nt.name)

			tok, err := nt.make()
			require.NoError(t, err)
			p, err := v.Verify(context.Background(), tok)
			if exp.wantErr != nil {
				assert.ErrorIs(t, err, exp.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, exp.brands, p.Brands)
			assert.Equal(t, exp.scoped, p.HasScope(domain.ScopeTransactionsRead))
		})
	}
}

func TestParseBrands(t *testing.T) {
	ids, err := parseBrands("1, 2,,3")
	require.NoError(t, err)
	assert.Equal(t, []domain.BrandID{1, 2, 3}, ids)

	_, err = parseBrands("1,x")
	assert.ErrorContains(t, err, `invalid brand id "x"`)
}
