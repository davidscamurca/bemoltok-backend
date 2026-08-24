package main

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"firebase.google.com/go/v4/auth"
)

// magiclink.html is the branded email body; {{link}} is replaced with the
// Firebase sign-in link at send time. Embedded at build time.
//
//go:embed magiclink.html
var magicLinkHTML string

// sendLinkService holds everything POST /auth/send-link needs: SendGrid creds,
// the ActionCodeSettings that MUST match the app, and a rate limiter. It is nil
// when SENDGRID_API_KEY is unset, which disables the endpoint (503).
type sendLinkService struct {
	apiKey   string
	from     string
	fromName string
	subject  string
	appKey   string // when set, callers must send a matching X-App-Key header
	acs      *auth.ActionCodeSettings
	limiter  *rateLimiter
	client   *http.Client
}

func newSendLinkService() *sendLinkService {
	apiKey := os.Getenv("SENDGRID_API_KEY")
	if apiKey == "" {
		return nil
	}
	from := envOr("SENDGRID_FROM", "inteligenciadenegocios@bemol.com.br")
	// ActionCodeSettings must stay identical to the app's lib/core/env.dart.
	acs := &auth.ActionCodeSettings{
		URL:                   envOr("AUTH_CONTINUE_URL", "https://bemoltok-dev.firebaseapp.com/finishSignIn"),
		HandleCodeInApp:       true,
		IOSBundleID:           envOr("AUTH_IOS_BUNDLE", "com.davidcamurca.bemoltok"),
		AndroidPackageName:    envOr("AUTH_ANDROID_PACKAGE", "com.example.bemoltok"),
		AndroidInstallApp:     true,
		AndroidMinimumVersion: "1",
	}
	return &sendLinkService{
		apiKey:   apiKey,
		from:     from,
		fromName: envOr("SENDGRID_FROM_NAME", "BemolTok"),
		subject:  envOr("SENDGRID_SUBJECT", "Seu acesso ao BemolTok"),
		appKey:   os.Getenv("SEND_LINK_APP_KEY"),
		acs:      acs,
		limiter: &rateLimiter{
			perEmail:      make(map[string]time.Time),
			perIP:         make(map[string][]time.Time),
			emailInterval: 60 * time.Second,
			ipMax:         10,
			ipWindow:      time.Hour,
		},
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type sendLinkRequest struct {
	Email string `json:"email"`
}

// handleSendLink generates a Firebase email sign-in link and emails it via
// SendGrid from a @bemol.com.br sender. Public (pre-login) endpoint: it does
// its own domain validation and rate limiting. Always answers 204 on the happy
// path so it never reveals whether an email is registered.
func (srv *Server) handleSendLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if srv.sendLink == nil || srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "email sending not configured"})
		return
	}

	// App-key gate: when SEND_LINK_APP_KEY is set, the caller (the app) must
	// present a matching X-App-Key header. When unset, stays open (rollout-safe).
	if !srv.sendLink.authorizeAppKey(r) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}

	var req sendLinkRequest
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

	ip := clientIP(r)
	if !srv.sendLink.limiter.allow(email, ip) {
		writeJSON(w, 429, map[string]string{"error": "too many requests; try again shortly"})
		return
	}

	link, err := srv.fb.Auth.EmailSignInLink(r.Context(), email, srv.sendLink.acs)
	if err != nil {
		log.Printf("send-link: generate failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not generate link"})
		return
	}

	if err := srv.sendLink.send(email, link); err != nil {
		log.Printf("send-link: sendgrid failed: %v", err)
		writeJSON(w, 502, map[string]string{"error": "could not send email"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizeAppKey returns true when the request carries the correct X-App-Key,
// or when no app key is configured (open mode). Constant-time compare avoids
// timing leaks. Shared by /auth/send-link and the OTP endpoints.
func (s *sendLinkService) authorizeAppKey(r *http.Request) bool {
	if s.appKey == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-App-Key")), []byte(s.appKey)) == 1
}

// send emails the magic link using the branded template.
func (s *sendLinkService) send(toEmail, link string) error {
	return s.sendHTML(toEmail, s.subject, strings.ReplaceAll(magicLinkHTML, "{{link}}", link))
}

// sendHTML posts an HTML email to the SendGrid v3 API. Uses the stdlib HTTP
// client to avoid pulling in the SendGrid SDK. Shared by magic link and OTP.
func (s *sendLinkService) sendHTML(toEmail, subject, html string) error {
	body := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{"to": []map[string]string{{"email": toEmail}}},
		},
		"from":    map[string]string{"email": s.from, "name": s.fromName},
		"subject": subject,
		"content": []map[string]string{
			{"type": "text/html", "value": html},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &sendgridError{status: resp.StatusCode}
	}
	return nil
}

type sendgridError struct{ status int }

func (e *sendgridError) Error() string {
	return "sendgrid responded with status " + http.StatusText(e.status)
}

// clientIP extracts the originating IP, honoring X-Forwarded-For set by Cloud Run.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter throttles by email (min interval between sends) and by IP
// (max sends per rolling window). In-memory and per-instance, which is enough
// for the current pilot scale.
type rateLimiter struct {
	mu            sync.Mutex
	perEmail      map[string]time.Time
	perIP         map[string][]time.Time
	emailInterval time.Duration
	ipMax         int
	ipWindow      time.Duration
}

func (rl *rateLimiter) allow(email, ip string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if last, ok := rl.perEmail[email]; ok && now.Sub(last) < rl.emailInterval {
		return false
	}

	// Prune timestamps outside the window for this IP, then enforce the cap.
	cutoff := now.Add(-rl.ipWindow)
	kept := rl.perIP[ip][:0]
	for _, t := range rl.perIP[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.ipMax {
		rl.perIP[ip] = kept
		return false
	}

	rl.perEmail[email] = now
	rl.perIP[ip] = append(kept, now)
	return true
}
