package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

var (
	current  = Key{ID: "2026-11", Secret: []byte("current-secret-0123456789-0123456789")}
	previous = Key{ID: "2026-10", Secret: []byte("previous-secret-0123456789-012345678")}
	foreign  = Key{ID: "2026-11", Secret: []byte("attacker-secret-0123456789-01234567")}
)

func testVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(Config{
		Issuer: "vyking-integration", Audience: "vyking-partner-api",
		Leeway: 30 * time.Second, Keys: []Key{current, previous},
	})
	require.NoError(t, err)
	return v
}

func validClaims() Claims {
	now := time.Now()
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "vyking-integration",
			Audience:  jwt.ClaimStrings{"vyking-partner-api"},
			Subject:   "acme-support",
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        "tok-1",
		},
		Brands: []domain.BrandID{1, 2},
		Scope:  "transactions:read other:scope",
	}
}

func sign(t *testing.T, mutate func(*Claims), key Key) string {
	t.Helper()
	c := validClaims()
	if mutate != nil {
		mutate(&c)
	}
	tok, err := Sign(c, key)
	require.NoError(t, err)
	return tok
}

// unsigned builds a token with an arbitrary header, e.g. "alg": "none".
func unsigned(t *testing.T, header string, signature string) string {
	t.Helper()
	tok := sign(t, nil, current)
	payload := strings.Split(tok, ".")[1]
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + payload + "." + signature
}

func TestVerify(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rs256 := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims())
	rs256.Header["kid"] = current.ID
	rsToken, err := rs256.SignedString(rsaKey)
	require.NoError(t, err)

	hs512 := jwt.NewWithClaims(jwt.SigningMethodHS512, validClaims())
	hs512.Header["kid"] = current.ID
	hsToken, err := hs512.SignedString(current.Secret)
	require.NoError(t, err)

	now := time.Now()
	tests := []struct {
		name    string
		token   string
		want    domain.Principal
		wantErr error
	}{
		{
			name:  "valid multi-brand token",
			token: sign(t, nil, current),
			want: domain.Principal{Subject: "acme-support", Brands: []domain.BrandID{1, 2},
				Scopes: []string{"transactions:read", "other:scope"}},
		},
		{
			name:  "previous key still accepted during rotation",
			token: sign(t, func(c *Claims) { c.Brands = []domain.BrandID{3} }, previous),
			want: domain.Principal{Subject: "acme-support", Brands: []domain.BrandID{3},
				Scopes: []string{"transactions:read", "other:scope"}},
		},
		{
			name:  "expired within leeway is accepted",
			token: sign(t, func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-10 * time.Second)) }, current),
			want: domain.Principal{Subject: "acme-support", Brands: []domain.BrandID{1, 2},
				Scopes: []string{"transactions:read", "other:scope"}},
		},
		{name: "expired", token: sign(t, func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour)) }, current), wantErr: ErrExpired},
		{name: "not yet valid", token: sign(t, func(c *Claims) { c.NotBefore = jwt.NewNumericDate(now.Add(time.Hour)) }, current), wantErr: ErrInvalid},
		{name: "issued in the future", token: sign(t, func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour)) }, current), wantErr: ErrInvalid},
		{name: "missing expiry", token: sign(t, func(c *Claims) { c.ExpiresAt = nil }, current), wantErr: ErrInvalid},
		{name: "wrong issuer", token: sign(t, func(c *Claims) { c.Issuer = "someone-else" }, current), wantErr: ErrInvalid},
		{name: "wrong audience", token: sign(t, func(c *Claims) { c.Audience = jwt.ClaimStrings{"other-api"} }, current), wantErr: ErrInvalid},
		{name: "missing subject", token: sign(t, func(c *Claims) { c.Subject = "" }, current), wantErr: ErrInvalid},
		{name: "no brands", token: sign(t, func(c *Claims) { c.Brands = nil }, current), wantErr: ErrInvalid},
		{name: "forged signature", token: sign(t, nil, foreign), wantErr: ErrInvalid},
		{name: "unknown key id", token: sign(t, nil, Key{ID: "nope", Secret: current.Secret}), wantErr: ErrInvalid},
		{name: "missing key id", token: unsigned(t, `{"alg":"HS256","typ":"JWT"}`, "c2ln"), wantErr: ErrInvalid},
		{name: "alg none", token: unsigned(t, `{"alg":"none","typ":"JWT","kid":"2026-11"}`, ""), wantErr: ErrInvalid},
		{name: "rs256 algorithm confusion", token: rsToken, wantErr: ErrInvalid},
		{name: "hs512 not allowed", token: hsToken, wantErr: ErrInvalid},
		{name: "tampered payload", token: func() string {
			parts := strings.Split(sign(t, nil, current), ".")
			parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"acme-support","brands":[1,2,3,4,5]}`))
			return strings.Join(parts, ".")
		}(), wantErr: ErrInvalid},
		{name: "empty", token: "", wantErr: ErrMalformed},
		{name: "garbage", token: "not-a-token", wantErr: ErrMalformed},
		{name: "two segments", token: "abc.def", wantErr: ErrMalformed},
		{name: "bad base64", token: "a!b.c@d.e#f", wantErr: ErrMalformed},
		{name: "brands of wrong type", token: func() string {
			c := jwt.MapClaims{"iss": "vyking-integration", "aud": "vyking-partner-api", "sub": "acme-support",
				"exp": now.Add(time.Hour).Unix(), "brands": "all"}
			tok := jwt.NewWithClaims(SigningMethod, c)
			tok.Header["kid"] = current.ID
			s, err := tok.SignedString(current.Secret)
			require.NoError(t, err)
			return s
		}(), wantErr: ErrMalformed},
	}
	v := testVerifier(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), tt.token)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewVerifierRequiresKeys(t *testing.T) {
	_, err := NewVerifier(Config{Issuer: "i", Audience: "a"})
	assert.ErrorContains(t, err, "at least one verification key")
}
