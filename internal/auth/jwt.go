// Package auth verifies partner bearer tokens.
//
// The task does not specify the partner token format. We assume tokens are
// JWTs we issue to each partner (see NOTES.md for alternatives), signed with
// HS256 and carrying the brands and scopes the partner is entitled to:
//
//	header:  {"alg": "HS256", "typ": "JWT", "kid": "2026-11"}
//	payload: {"iss": "vyking-integration", "aud": "vyking-partner-api",
//	          "sub": "acme-support", "brands": [1, 2], "scope": "transactions:read",
//	          "iat": ..., "nbf": ..., "exp": ..., "jti": "..."}
//
// The kid header selects the verification key, which allows rotating keys
// without downtime: add the new key, re-issue tokens, remove the old key.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// Verification failures, mapped to API error codes by the HTTP layer.
var (
	ErrMalformed = errors.New("token is malformed")
	ErrExpired   = errors.New("token is expired")
	ErrInvalid   = errors.New("token is invalid")
)

// SigningMethod is the only accepted algorithm. Pinning it prevents
// algorithm confusion attacks ("alg": "none", RS/HS swaps).
var SigningMethod = jwt.SigningMethodHS256

// Key is a named HMAC verification key.
type Key struct {
	ID     string
	Secret []byte
}

// Config configures the Verifier.
type Config struct {
	Issuer   string
	Audience string
	// Leeway tolerates clock skew between issuer and verifier.
	Leeway time.Duration
	Keys   []Key
}

// Claims are the JWT claims of a partner token.
type Claims struct {
	jwt.RegisteredClaims
	Brands []domain.BrandID `json:"brands"`
	// Scope is a space-separated list, as in OAuth 2.0 (RFC 8693).
	Scope string `json:"scope"`
}

// Verifier validates partner tokens and turns them into principals.
type Verifier struct {
	keys   map[string][]byte
	parser *jwt.Parser
}

func NewVerifier(cfg Config) (*Verifier, error) {
	if len(cfg.Keys) == 0 {
		return nil, errors.New("auth: at least one verification key is required")
	}
	keys := make(map[string][]byte, len(cfg.Keys))
	for _, k := range cfg.Keys {
		keys[k.ID] = k.Secret
	}
	return &Verifier{
		keys: keys,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{SigningMethod.Alg()}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithLeeway(cfg.Leeway),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
		),
	}, nil
}

// Verify checks signature, algorithm, issuer, audience and validity window
// of raw and returns the principal it describes.
func (v *Verifier) Verify(_ context.Context, raw string) (domain.Principal, error) {
	var claims Claims
	_, err := v.parser.ParseWithClaims(raw, &claims, v.key)
	switch {
	case err == nil:
	case errors.Is(err, jwt.ErrTokenMalformed):
		return domain.Principal{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	case errors.Is(err, jwt.ErrTokenExpired):
		return domain.Principal{}, fmt.Errorf("%w: %v", ErrExpired, err)
	default:
		return domain.Principal{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	if claims.Subject == "" {
		return domain.Principal{}, fmt.Errorf("%w: missing subject", ErrInvalid)
	}
	if len(claims.Brands) == 0 {
		return domain.Principal{}, fmt.Errorf("%w: token grants no brands", ErrInvalid)
	}
	return domain.Principal{
		Subject: claims.Subject,
		Brands:  claims.Brands,
		Scopes:  strings.Fields(claims.Scope),
	}, nil
}

func (v *Verifier) key(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	secret, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return secret, nil
}

// Sign creates a token for claims with the given key. It is used by the
// token generator and tests; production tokens are issued out of band.
func Sign(claims Claims, key Key) (string, error) {
	t := jwt.NewWithClaims(SigningMethod, claims)
	t.Header["kid"] = key.ID
	return t.SignedString(key.Secret)
}
