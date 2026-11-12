// Command tokengen issues partner bearer tokens for local testing.
//
// It reads issuer, audience and signing keys from the service configuration
// (APP_ENV / CONFIG_PATH, like the service), so generated tokens always match
// what the running service verifies.
//
//	go run ./scripts/tokengen -matrix                   # named test tokens as TOKEN_*=... lines
//	go run ./scripts/tokengen -brands 1,2 -ttl 24h      # one token
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tokengen:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		matrix  = flag.Bool("matrix", false, "print a named set of valid and invalid tokens")
		subject = flag.String("sub", "acme-support", "partner identifier (sub)")
		brands  = flag.String("brands", "1", "comma-separated brand ids the token grants")
		scope   = flag.String("scope", domain.ScopeTransactionsRead, "space-separated scopes")
		ttl     = flag.Duration("ttl", time.Hour, "token lifetime")
	)
	flag.Parse()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	keys := cfg.Auth.ActiveKeys()
	key := auth.Key{ID: keys[0].ID, Secret: []byte(keys[0].Secret)}
	issuer := tokenIssuer{iss: cfg.Auth.Issuer, aud: cfg.Auth.Audience, key: key, now: time.Now()}

	if !*matrix {
		ids, err := parseBrands(*brands)
		if err != nil {
			return err
		}
		tok, err := issuer.sign(func(c *auth.Claims) {
			c.Subject, c.Brands, c.Scope = *subject, ids, *scope
			c.ExpiresAt = jwt.NewNumericDate(issuer.now.Add(*ttl))
		})
		if err != nil {
			return err
		}
		fmt.Println(tok)
		return nil
	}

	for _, t := range issuer.matrix() {
		tok, err := t.make()
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		fmt.Printf("TOKEN_%s=%s\n", t.name, tok)
	}
	return nil
}

type tokenIssuer struct {
	iss, aud string
	key      auth.Key
	now      time.Time
}

func (ti tokenIssuer) claims() auth.Claims {
	return auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    ti.iss,
			Audience:  jwt.ClaimStrings{ti.aud},
			Subject:   "acme-support",
			IssuedAt:  jwt.NewNumericDate(ti.now),
			NotBefore: jwt.NewNumericDate(ti.now),
			ExpiresAt: jwt.NewNumericDate(ti.now.Add(24 * time.Hour)),
			ID:        strconv.FormatInt(ti.now.UnixNano(), 36),
		},
		Brands: []domain.BrandID{1},
		Scope:  domain.ScopeTransactionsRead,
	}
}

func (ti tokenIssuer) sign(mutate func(*auth.Claims)) (string, error) {
	return ti.signWith(ti.key, mutate)
}

func (ti tokenIssuer) signWith(key auth.Key, mutate func(*auth.Claims)) (string, error) {
	c := ti.claims()
	if mutate != nil {
		mutate(&c)
	}
	return auth.Sign(c, key)
}

type namedToken struct {
	name string
	make func() (string, error)
}

// matrix covers every authentication outcome of the partner API.
func (ti tokenIssuer) matrix() []namedToken {
	brands := func(ids ...domain.BrandID) func(*auth.Claims) {
		return func(c *auth.Claims) { c.Brands = ids }
	}
	return []namedToken{
		{"VALID_BRAND_1", func() (string, error) { return ti.sign(brands(1)) }},
		{"VALID_BRAND_2", func() (string, error) { return ti.sign(brands(2)) }},
		{"VALID_BRANDS_1_2", func() (string, error) { return ti.sign(brands(1, 2)) }},
		{"VALID_ALL_BRANDS", func() (string, error) { return ti.sign(brands(1, 2, 3)) }},
		{"EXPIRED", func() (string, error) {
			return ti.sign(func(c *auth.Claims) {
				c.IssuedAt = jwt.NewNumericDate(ti.now.Add(-2 * time.Hour))
				c.NotBefore = c.IssuedAt
				c.ExpiresAt = jwt.NewNumericDate(ti.now.Add(-time.Hour))
			})
		}},
		{"NOT_YET_VALID", func() (string, error) {
			return ti.sign(func(c *auth.Claims) { c.NotBefore = jwt.NewNumericDate(ti.now.Add(time.Hour)) })
		}},
		{"WRONG_SIGNATURE", func() (string, error) {
			return ti.signWith(auth.Key{ID: ti.key.ID, Secret: []byte("not-the-real-secret-0123456789abcdef")}, nil)
		}},
		{"UNKNOWN_KID", func() (string, error) {
			return ti.signWith(auth.Key{ID: "unknown-kid", Secret: ti.key.Secret}, nil)
		}},
		{"WRONG_AUDIENCE", func() (string, error) {
			return ti.sign(func(c *auth.Claims) { c.Audience = jwt.ClaimStrings{"some-other-api"} })
		}},
		{"WRONG_ISSUER", func() (string, error) {
			return ti.sign(func(c *auth.Claims) { c.Issuer = "someone-else" })
		}},
		{"NO_BRANDS", func() (string, error) { return ti.sign(brands()) }},
		{"NO_SCOPE", func() (string, error) {
			return ti.sign(func(c *auth.Claims) { c.Scope = "" })
		}},
		{"ALG_NONE", func() (string, error) {
			valid, err := ti.sign(nil)
			if err != nil {
				return "", err
			}
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + ti.key.ID + `"}`))
			return header + "." + strings.Split(valid, ".")[1] + ".", nil
		}},
		{"MALFORMED", func() (string, error) { return "this-is-not-a-jwt", nil }},
	}
}

func parseBrands(s string) ([]domain.BrandID, error) {
	var ids []domain.BrandID
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid brand id %q", f)
		}
		ids = append(ids, domain.BrandID(n))
	}
	return ids, nil
}
