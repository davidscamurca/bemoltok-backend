package main

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is a private type for context keys to avoid collisions.
type ctxKey string

const (
	uidKey   ctxKey = "uid"
	emailKey ctxKey = "email"
)

func uidFromContext(ctx context.Context) (string, bool) {
	uid, ok := ctx.Value(uidKey).(string)
	return uid, ok && uid != ""
}

func emailFromContext(ctx context.Context) string {
	email, _ := ctx.Value(emailKey).(string)
	return email
}

// emailInDomain reports whether email belongs to domain (case-insensitive).
// An empty domain disables the check (handled by the caller).
func emailInDomain(email, domain string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	return strings.EqualFold(email[at+1:], domain)
}

// authMiddleware verifies a Firebase ID token when present and injects the uid
// (and email) into the request context.
//
// Transition behavior controlled by srv.authRequired:
//   - A present-but-invalid Bearer token is ALWAYS rejected (401).
//   - When authRequired is true, a missing token is rejected (401).
//   - When authRequired is false (current app still in production), a missing
//     token is allowed through; the handler falls back to the legacy identity
//     (X-User-Id header or the path param) via srv.identify / handler logic.
//
// When ALLOWED_EMAIL_DOMAIN is set, tokens whose email is outside that domain
// are rejected (403) — used to restrict access to @bemol.com.br during rollout.
func (srv *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			if srv.fb == nil {
				writeJSON(w, 503, map[string]string{"error": "auth not configured"})
				return
			}
			token := strings.TrimSpace(authHeader[len("Bearer "):])
			decoded, err := srv.fb.Auth.VerifyIDToken(r.Context(), token)
			if err != nil {
				writeJSON(w, 401, map[string]string{"error": "invalid token"})
				return
			}
			email, _ := decoded.Claims["email"].(string)
			if srv.allowedEmailDomain != "" && !emailInDomain(email, srv.allowedEmailDomain) {
				writeJSON(w, 403, map[string]string{"error": "email domain not allowed"})
				return
			}
			ctx := context.WithValue(r.Context(), uidKey, decoded.UID)
			ctx = context.WithValue(ctx, emailKey, email)
			next(w, r.WithContext(ctx))
			return
		}

		if srv.authRequired {
			writeJSON(w, 401, map[string]string{"error": "missing Authorization bearer token"})
			return
		}
		// Legacy mode: no token. Let the handler resolve the legacy identity.
		next(w, r)
	}
}

// identify returns the effective identity for a request: the authenticated
// Firebase uid when a token was verified, or the legacy X-User-Id header while
// authRequired is false. The second return is false when no identity is present.
func (srv *Server) identify(r *http.Request) (string, bool) {
	if uid, ok := uidFromContext(r.Context()); ok {
		return uid, true
	}
	if !srv.authRequired {
		if legacy := r.Header.Get("X-User-Id"); legacy != "" {
			return legacy, true
		}
	}
	return "", false
}
