package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
)

// BasicAuth protects operator-only endpoints with HTTP Basic authentication.
//
// Credentials are compared as SHA-256 digests with subtle.ConstantTimeCompare
// and both comparisons always run, so neither the content nor the length of
// the expected values leaks through response timing.
func BasicAuth(realm, username, password string) Middleware {
	wantUser := sha256.Sum256([]byte(username))
	wantPass := sha256.Sum256([]byte(password))
	challenge := `Basic realm="` + realm + `", charset="UTF-8"`

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			gotUser := sha256.Sum256([]byte(user))
			gotPass := sha256.Sum256([]byte(pass))
			userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
			passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])

			if !ok || userOK&passOK != 1 {
				w.Header().Set("WWW-Authenticate", challenge)
				apierror.Write(w, r, apierror.New(http.StatusUnauthorized, apierror.CodeUnauthorized,
					"valid credentials are required"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
