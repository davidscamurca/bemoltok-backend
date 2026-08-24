package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"strings"
)

// The email→id_cliente directory is loaded as a privacy-preserving map keyed by
// HMAC-SHA256(secret, lower(trim(email))). The raw emails never reach the
// service: the hashing happens offline (tools/hashdir) and only the hashed
// artifact is shipped. The same secret here lets us hash a logged-in user's
// email and look up their Bemol ID_CLIENTE for automatic linking.

// hmacEmail returns the lookup key for an email, applying the same
// normalization (lower + trim) the offline tool uses.
func hmacEmail(secret, email string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(mac.Sum(nil))
}

// loadDirectory reads the hashed artifact (email_hmac;id_cliente) into a map.
// A missing file is not fatal: the service runs without auto-link (users fall
// back to manual ID_CLIENTE entry).
func loadDirectory(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dir := make(map[string]string)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, ';')
		if i <= 0 || i == len(line)-1 {
			continue
		}
		h := strings.TrimSpace(line[:i])
		id := strings.TrimSpace(line[i+1:])
		if h != "" && id != "" {
			dir[h] = id
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return dir, nil
}

// tryAutoLink resolves a user's email against the directory and, when found AND
// the ID_CLIENTE exists in the recommendation index, links it to the user with
// verified=true. Returns the updated profile and true on success; (nil, false)
// when no link was made (no directory, no match, or id not in the index) so the
// caller keeps the manual-entry fallback.
func (srv *Server) tryAutoLink(ctx context.Context, uid, email string) (*UserProfile, bool) {
	if len(srv.directory) == 0 || srv.directoryHMACSecret == "" || email == "" {
		return nil, false
	}
	id, ok := srv.directory[hmacEmail(srv.directoryHMACSecret, email)]
	if !ok {
		return nil, false
	}

	// Mirror the POST /me/client-id guard: never link an id the index can't serve.
	srv.idxMu.RLock()
	_, inIndex := srv.idx.ClientMap[id]
	srv.idxMu.RUnlock()
	if !inIndex {
		log.Printf("auto-link skipped: directory id %q not in recommendation index", id)
		return nil, false
	}

	profile, err := srv.setClientID(ctx, uid, id, true)
	if err != nil {
		log.Printf("auto-link setClientID failed: %v", err)
		return nil, false
	}
	return profile, true
}
