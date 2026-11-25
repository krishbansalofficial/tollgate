package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type ctxKey struct{}

// FromContext returns the Key set by Middleware.
func FromContext(ctx context.Context) (Key, bool) {
	k, ok := ctx.Value(ctxKey{}).(Key)
	return k, ok
}

// ErrorWriter lets the server keep one error format (the OpenAI envelope)
// without this package depending on it.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, typ, msg string)

// Middleware requires a valid `Authorization: Bearer tg_...` header.
func Middleware(a *Authenticator, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				writeErr(w, r, http.StatusUnauthorized, "invalid_request_error", "missing bearer API key")
				return
			}
			k, err := a.Authenticate(r.Context(), strings.TrimSpace(raw))
			switch {
			case errors.Is(err, ErrInvalidKey):
				writeErr(w, r, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
				return
			case err != nil:
				writeErr(w, r, http.StatusServiceUnavailable, "api_error", "authentication backend unavailable")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
		})
	}
}
