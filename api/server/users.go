package main

import (
	"context"
	"errors"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const usersCollection = "users"

// errNoClientLink means the user has authenticated but not yet linked a Bemol
// ID_CLIENTE, so recommendations cannot be resolved.
var errNoClientLink = errors.New("no bemolClientId linked")

// UserProfile is the users/{uid} document. JSON tags drive the /me response;
// firestore tags drive persistence.
type UserProfile struct {
	UID              string    `json:"uid"                firestore:"uid"`
	Email            string    `json:"email"              firestore:"email"`
	BemolClientID    string    `json:"bemol_client_id"    firestore:"bemolClientId"`
	ClientIDVerified bool      `json:"client_id_verified" firestore:"clientIdVerified"`
	CreatedAt        time.Time `json:"created_at"         firestore:"createdAt"`
	LastSeenAt       time.Time `json:"last_seen_at"       firestore:"lastSeenAt"`
}

// getOrCreateUser loads users/{uid}, creating it on first sign-in and bumping
// lastSeenAt on subsequent calls.
func (srv *Server) getOrCreateUser(ctx context.Context, uid, email string) (*UserProfile, error) {
	doc := srv.fb.Firestore.Collection(usersCollection).Doc(uid)
	snap, err := doc.Get(ctx)
	now := time.Now().UTC()

	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, err
		}
		profile := &UserProfile{
			UID:        uid,
			Email:      email,
			CreatedAt:  now,
			LastSeenAt: now,
		}
		if _, err := doc.Set(ctx, profile); err != nil {
			return nil, err
		}
		return profile, nil
	}

	var profile UserProfile
	if err := snap.DataTo(&profile); err != nil {
		return nil, err
	}
	profile.UID = uid
	if profile.Email == "" {
		profile.Email = email
	}
	profile.LastSeenAt = now
	// Best-effort touch; ignore failure so /me stays fast and resilient.
	_, _ = doc.Set(ctx, map[string]interface{}{
		"lastSeenAt": now,
		"email":      profile.Email,
	}, firestore.MergeAll)

	srv.cacheClientID(uid, profile.BemolClientID)
	return &profile, nil
}

// setClientID links a Bemol ID_CLIENTE to the user. verified marks whether the
// link came from the authoritative directory (future) vs. self-declared (now).
func (srv *Server) setClientID(ctx context.Context, uid, clientID string, verified bool) (*UserProfile, error) {
	doc := srv.fb.Firestore.Collection(usersCollection).Doc(uid)
	_, err := doc.Set(ctx, map[string]interface{}{
		"bemolClientId":    clientID,
		"clientIdVerified": verified,
	}, firestore.MergeAll)
	if err != nil {
		return nil, err
	}
	srv.cacheClientID(uid, clientID)

	snap, err := doc.Get(ctx)
	if err != nil {
		return nil, err
	}
	var profile UserProfile
	if err := snap.DataTo(&profile); err != nil {
		return nil, err
	}
	profile.UID = uid
	return &profile, nil
}

// resolveClientID maps a Firebase uid to its linked Bemol ID_CLIENTE, using an
// in-memory cache to avoid a Firestore read on every /recommend call.
// Returns errNoClientLink when the user has not linked a client id yet.
func (srv *Server) resolveClientID(ctx context.Context, uid string) (string, error) {
	srv.clientIDMu.RLock()
	cid, ok := srv.clientIDCache[uid]
	srv.clientIDMu.RUnlock()
	if ok {
		if cid == "" {
			return "", errNoClientLink
		}
		return cid, nil
	}

	snap, err := srv.fb.Firestore.Collection(usersCollection).Doc(uid).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", errNoClientLink
		}
		return "", err
	}
	var profile UserProfile
	if err := snap.DataTo(&profile); err != nil {
		return "", err
	}
	srv.cacheClientID(uid, profile.BemolClientID)
	if profile.BemolClientID == "" {
		return "", errNoClientLink
	}
	return profile.BemolClientID, nil
}

func (srv *Server) cacheClientID(uid, clientID string) {
	srv.clientIDMu.Lock()
	if srv.clientIDCache == nil {
		srv.clientIDCache = make(map[string]string)
	}
	srv.clientIDCache[uid] = clientID
	srv.clientIDMu.Unlock()
}
