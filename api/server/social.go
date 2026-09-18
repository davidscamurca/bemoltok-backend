package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/iterator"
)

// Social features: likes, bookmarks and comments persisted per user / product
// in Firestore. Interaction events (/events) still feed UCB1; these docs are
// the source of truth for UI state across devices.

const (
	likesSubcollection     = "likes"
	bookmarksSubcollection = "bookmarks"
	commentsCollection     = "product_comments"
)

type commentDoc struct {
	ID        string    `json:"id" firestore:"id"`
	UID       string    `json:"uid" firestore:"uid"`
	Email     string    `json:"email,omitempty" firestore:"email,omitempty"`
	ProductID string    `json:"product_id" firestore:"product_id"`
	Text      string    `json:"text" firestore:"text"`
	CreatedAt time.Time `json:"created_at" firestore:"created_at"`
}

func (srv *Server) requireUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := srv.identify(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "missing identity"})
		return "", false
	}
	if srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "store not available"})
		return "", false
	}
	return uid, true
}

// GET /me/likes  |  PUT|DELETE /me/likes/{productId}
func (srv *Server) handleMeLikes(w http.ResponseWriter, r *http.Request) {
	uid, ok := srv.requireUID(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/me/likes")
	path = strings.Trim(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		ids, err := srv.listUserProductIDs(r.Context(), uid, likesSubcollection)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"product_ids": ids})
	case path != "" && r.Method == http.MethodPut:
		if err := srv.setUserProductFlag(r.Context(), uid, likesSubcollection, path, true); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case path != "" && r.Method == http.MethodDelete:
		if err := srv.setUserProductFlag(r.Context(), uid, likesSubcollection, path, false); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// GET /me/bookmarks  |  PUT|DELETE /me/bookmarks/{productId}
func (srv *Server) handleMeBookmarks(w http.ResponseWriter, r *http.Request) {
	uid, ok := srv.requireUID(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/me/bookmarks")
	path = strings.Trim(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		ids, err := srv.listUserProductIDs(r.Context(), uid, bookmarksSubcollection)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"product_ids": ids})
	case path != "" && r.Method == http.MethodPut:
		if err := srv.setUserProductFlag(r.Context(), uid, bookmarksSubcollection, path, true); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case path != "" && r.Method == http.MethodDelete:
		if err := srv.setUserProductFlag(r.Context(), uid, bookmarksSubcollection, path, false); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (srv *Server) listUserProductIDs(ctx context.Context, uid, sub string) ([]string, error) {
	iter := srv.fb.Firestore.Collection(usersCollection).Doc(uid).Collection(sub).Documents(ctx)
	defer iter.Stop()
	out := make([]string, 0)
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, d.Ref.ID)
	}
	return out, nil
}

func (srv *Server) setUserProductFlag(ctx context.Context, uid, sub, productID string, on bool) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil
	}
	ref := srv.fb.Firestore.Collection(usersCollection).Doc(uid).Collection(sub).Doc(productID)
	if !on {
		_, err := ref.Delete(ctx)
		return err
	}
	_, err := ref.Set(ctx, map[string]interface{}{
		"product_id": productID,
		"updated_at": time.Now().UTC(),
	})
	return err
}

// GET|POST /products/{productId}/comments
func (srv *Server) handleProductComments(w http.ResponseWriter, r *http.Request) {
	uid, ok := srv.requireUID(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/products/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "comments" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	productID := parts[0]
	if productID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing product_id"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		items, err := srv.listComments(r.Context(), productID)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"comments": items})
	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" || len(text) > 2000 {
			writeJSON(w, 400, map[string]string{"error": "invalid text"})
			return
		}
		email := ""
		c, err := srv.createComment(r.Context(), uid, email, productID, text)
		if err != nil {
			log.Printf("create comment: %v", err)
			writeJSON(w, 500, map[string]string{"error": "failed to create comment"})
			return
		}
		writeJSON(w, 201, c)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (srv *Server) listComments(ctx context.Context, productID string) ([]commentDoc, error) {
	iter := srv.fb.Firestore.Collection(commentsCollection).
		Where("product_id", "==", productID).
		Limit(200).
		Documents(ctx)
	defer iter.Stop()
	out := make([]commentDoc, 0)
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var c commentDoc
		if err := d.DataTo(&c); err != nil {
			continue
		}
		if c.ID == "" {
			c.ID = d.Ref.ID
		}
		out = append(out, c)
	}
	// Sort in memory to avoid a composite index requirement.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.Before(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (srv *Server) createComment(ctx context.Context, uid, email, productID, text string) (*commentDoc, error) {
	id := uuid.NewString()
	c := commentDoc{
		ID:        id,
		UID:       uid,
		Email:     email,
		ProductID: productID,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	_, err := srv.fb.Firestore.Collection(commentsCollection).Doc(id).Set(ctx, c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
