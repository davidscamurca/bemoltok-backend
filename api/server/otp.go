package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"firebase.google.com/go/v4/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// otp.html is the branded email body; {{code}} is replaced with the 6-digit
// code at send time. Embedded at build time.
//
//go:embed otp.html
var otpHTML string

const (
	otpCollection  = "auth_codes"
	otpTTL         = 10 * time.Minute
	otpMaxAttempts = 5
	otpSubject     = "Seu código de acesso ao BemolTok"
)

// otpDoc is one pending code in auth_codes/{emailHMAC}. The code itself is never
// stored: only HMAC-SHA256(secret, "otp:"+email+":"+code). Locked-down Firestore
// plus a server-side HMAC means a DB dump never reveals the 6-digit code.
type otpDoc struct {
	CodeHash  string    `firestore:"code_hash"`
	ExpiresAt time.Time `firestore:"expires_at"`
	Attempts  int       `firestore:"attempts"`
	Used      bool      `firestore:"used"`
	CreatedAt time.Time `firestore:"created_at,serverTimestamp"`
}

// otpEnabled reports whether the email-OTP flow can run: it needs Firebase
// (custom token + user lookup), SendGrid (to email the code) and the HMAC secret
// (to key the doc and hash codes).
func (srv *Server) otpEnabled() bool {
	return srv.fb != nil && srv.sendLink != nil && srv.directoryHMACSecret != ""
}

// hmacCode binds the stored hash to the email so a code is only valid for the
// email it was issued to. Uses the same server secret as the email directory.
func hmacCode(secret, email, code string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("otp:" + strings.ToLower(strings.TrimSpace(email)) + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// generateOTP returns a uniformly random 6-digit code (with leading zeros).
func generateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

type requestCodeRequest struct {
	Email string `json:"email"`
}

// handleRequestCode generates a 6-digit code, stores its HMAC (with TTL) in
// Firestore and emails it via SendGrid. Public (pre-login): does its own domain
// validation, X-App-Key gate and rate limiting, and always answers 204 so it
// never reveals whether an email is registered.
func (srv *Server) handleRequestCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !srv.otpEnabled() {
		writeJSON(w, 503, map[string]string{"error": "otp not configured"})
		return
	}
	if !srv.sendLink.authorizeAppKey(r) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}

	var req requestCodeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, 400, map[string]string{"error": "invalid email"})
		return
	}
	if srv.allowedEmailDomain != "" && !emailInDomain(email, srv.allowedEmailDomain) {
		writeJSON(w, 403, map[string]string{"error": "email domain not allowed"})
		return
	}
	if !srv.sendLink.limiter.allow(email, clientIP(r)) {
		writeJSON(w, 429, map[string]string{"error": "too many requests; try again shortly"})
		return
	}

	code, err := generateOTP()
	if err != nil {
		log.Printf("otp: generate failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not generate code"})
		return
	}

	docID := hmacEmail(srv.directoryHMACSecret, email)
	_, err = srv.fb.Firestore.Collection(otpCollection).Doc(docID).Set(r.Context(), otpDoc{
		CodeHash:  hmacCode(srv.directoryHMACSecret, email, code),
		ExpiresAt: time.Now().UTC().Add(otpTTL),
		Attempts:  0,
		Used:      false,
	})
	if err != nil {
		log.Printf("otp: store failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not store code"})
		return
	}

	html := strings.ReplaceAll(otpHTML, "{{code}}", code)
	if err := srv.sendLink.sendHTML(email, otpSubject, html); err != nil {
		log.Printf("otp: sendgrid failed: %v", err)
		writeJSON(w, 502, map[string]string{"error": "could not send email"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type verifyCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type verifyCodeResponse struct {
	CustomToken string `json:"custom_token"`
}

// handleVerifyCode validates a code and, on success, mints a Firebase custom
// token for the email's (get-or-created) user. The app exchanges it via
// signInWithCustomToken to obtain the same ID Token used by every other route.
func (srv *Server) handleVerifyCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !srv.otpEnabled() {
		writeJSON(w, 503, map[string]string{"error": "otp not configured"})
		return
	}
	if !srv.sendLink.authorizeAppKey(r) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}

	var req verifyCodeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)
	if email == "" || !strings.Contains(email, "@") || code == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid email or code"})
		return
	}
	if srv.allowedEmailDomain != "" && !emailInDomain(email, srv.allowedEmailDomain) {
		writeJSON(w, 403, map[string]string{"error": "email domain not allowed"})
		return
	}

	ctx := r.Context()
	docRef := srv.fb.Firestore.Collection(otpCollection).Doc(hmacEmail(srv.directoryHMACSecret, email))
	snap, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeJSON(w, 401, map[string]string{"error": "invalid or expired code"})
			return
		}
		log.Printf("otp: read failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not verify code"})
		return
	}
	var d otpDoc
	if err := snap.DataTo(&d); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not verify code"})
		return
	}
	if d.Used || time.Now().UTC().After(d.ExpiresAt) {
		writeJSON(w, 401, map[string]string{"error": "invalid or expired code"})
		return
	}
	if d.Attempts >= otpMaxAttempts {
		writeJSON(w, 401, map[string]string{"error": "too many attempts; request a new code"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(hmacCode(srv.directoryHMACSecret, email, code)), []byte(d.CodeHash)) != 1 {
		_, _ = docRef.Update(ctx, []firestore.Update{{Path: "attempts", Value: firestore.Increment(1)}})
		writeJSON(w, 401, map[string]string{"error": "invalid or expired code"})
		return
	}

	// Correct code: burn it (single use) before minting the token.
	if _, err := docRef.Update(ctx, []firestore.Update{{Path: "used", Value: true}}); err != nil {
		log.Printf("otp: mark-used failed: %v", err)
	}

	uid, err := srv.getOrCreateAuthUser(ctx, email)
	if err != nil {
		log.Printf("otp: user resolve failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not resolve user"})
		return
	}
	token, err := srv.fb.Auth.CustomToken(ctx, uid)
	if err != nil {
		log.Printf("otp: custom token failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not mint token"})
		return
	}
	writeJSON(w, 200, verifyCodeResponse{CustomToken: token})
}

// getOrCreateAuthUser returns the Firebase uid for an email, creating the user
// (email marked verified) on first sign-in. Keeping the email on the user record
// ensures the resulting ID Token carries the email claim that authMiddleware
// uses for the domain check and that auto-link relies on.
func (srv *Server) getOrCreateAuthUser(ctx context.Context, email string) (string, error) {
	u, err := srv.fb.Auth.GetUserByEmail(ctx, email)
	if err == nil {
		return u.UID, nil
	}
	if !auth.IsUserNotFound(err) {
		return "", err
	}
	created, err := srv.fb.Auth.CreateUser(ctx, (&auth.UserToCreate{}).Email(email).EmailVerified(true))
	if err != nil {
		return "", err
	}
	return created.UID, nil
}
