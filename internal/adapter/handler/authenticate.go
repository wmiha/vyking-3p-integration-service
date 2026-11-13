package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// TokenVerifier authenticates a bearer token.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (domain.Principal, error)
}

const bearerRealm = `Bearer realm="partner-api"`

type principalKey struct{}

// principalFrom returns the authenticated caller stored by Authenticate.
func principalFrom(ctx context.Context) (domain.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(domain.Principal)
	return p, ok
}

// Authenticate requires a valid bearer token granting scope and stores the
// resulting principal in the request context. Failures follow RFC 6750: 401
// with a WWW-Authenticate challenge for missing or invalid tokens, 403 for a
// valid token without the required scope. obs may be nil.
func Authenticate(v TokenVerifier, scope string, logger *slog.Logger, obs PartnerObserver) Middleware {
	obs = partnerObserver(obs)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				obs.AuthFailed(string(apierror.CodeUnauthorized))
				w.Header().Set("WWW-Authenticate", bearerRealm)
				apierror.Write(w, r, apierror.New(http.StatusUnauthorized, apierror.CodeUnauthorized,
					"a bearer token is required"))
				return
			}

			principal, err := v.Verify(r.Context(), token)
			if err != nil {
				apiErr := tokenError(err)
				obs.AuthFailed(string(apiErr.Code))
				// The token itself is never logged; the reason is enough to debug.
				logger.InfoContext(r.Context(), "authentication failed", "code", apiErr.Code, "reason", err.Error())
				w.Header().Set("WWW-Authenticate", bearerRealm+`, error="invalid_token", error_description="`+apiErr.Message+`"`)
				apierror.Write(w, r, apiErr)
				return
			}

			if !principal.HasScope(scope) {
				obs.AuthFailed(string(apierror.CodeInsufficientScope))
				logger.InfoContext(r.Context(), "insufficient scope", "subject", principal.Subject, "required", scope)
				w.Header().Set("WWW-Authenticate", bearerRealm+`, error="insufficient_scope", scope="`+scope+`"`)
				apierror.Write(w, r, apierror.New(http.StatusForbidden, apierror.CodeInsufficientScope,
					"token does not grant the "+scope+" scope"))
				return
			}

			ctx := context.WithValue(r.Context(), principalKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>"; the
// scheme is case-insensitive (RFC 7235).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func tokenError(err error) *apierror.Error {
	switch {
	case errors.Is(err, auth.ErrMalformed):
		return apierror.New(http.StatusUnauthorized, apierror.CodeTokenMalformed, "token is malformed")
	case errors.Is(err, auth.ErrExpired):
		return apierror.New(http.StatusUnauthorized, apierror.CodeTokenExpired, "token is expired")
	default:
		return apierror.New(http.StatusUnauthorized, apierror.CodeTokenInvalid, "token is invalid")
	}
}
