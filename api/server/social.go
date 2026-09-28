package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/iterator"
)

// Social features: likes, bookmarks and comments persisted per user / product
// in Firestore. Interaction events (/events) still feed UCB1; these docs are
// the source of truth for UI state across devices.

const (
	likesSubcollection        = "likes"
	bookmarksSubcollection    = "bookmarks"
	commentsCollection        = "product_comments"
	commentLikesSubcollection = "likes"

	commentStatusPublished     = "published"
	commentStatusHidden        = "hidden"
	commentStatusPendingReview = "pending_review"
	commentStatusRejected      = "rejected"
)

type commentDoc struct {
	ID               string     `json:"id" firestore:"id"`
	UID              string     `json:"uid" firestore:"uid"`
	Email            string     `json:"-" firestore:"email,omitempty"`
	DisplayLabel     string     `json:"author_label" firestore:"display_label"`
	ProductID        string     `json:"product_id" firestore:"product_id"`
	ParentID         string     `json:"parent_id,omitempty" firestore:"parent_id,omitempty"`
	Text             string     `json:"text" firestore:"text"`
	CreatedAt        time.Time  `json:"created_at" firestore:"created_at"`
	LikeCount        int        `json:"like_count" firestore:"like_count"`
	LikedByMe        bool       `json:"liked_by_me"`
	Status           string     `json:"status" firestore:"status"`
	ReportedCount    int        `json:"reported_count,omitempty" firestore:"reported_count,omitempty"`
	ModerationReason string     `json:"moderation_reason,omitempty" firestore:"moderation_reason,omitempty"`
	ModeratedAt      *time.Time `json:"moderated_at,omitempty" firestore:"moderated_at,omitempty"`
	ModeratedBy      string     `json:"moderated_by,omitempty" firestore:"moderated_by,omitempty"`
}

type createCommentRequest struct {
	Text     string `json:"text"`
	ParentID string `json:"parent_id"`

	// Forbidden client-controlled fields (must stay server-side).
	UID              string     `json:"uid"`
	Status           string     `json:"status"`
	ReportedCount    *int       `json:"reported_count"`
	ModerationReason string     `json:"moderation_reason"`
	ModeratedAt      *time.Time `json:"moderated_at"`
	ModeratedBy      string     `json:"moderated_by"`
	LikeCount        *int       `json:"like_count"`
	DisplayLabel     string     `json:"author_label"`
	Email            string     `json:"email"`
}

func (r createCommentRequest) hasForbiddenClientFields() bool {
	return strings.TrimSpace(r.UID) != "" ||
		strings.TrimSpace(r.Status) != "" ||
		r.ReportedCount != nil ||
		strings.TrimSpace(r.ModerationReason) != "" ||
		r.ModeratedAt != nil ||
		strings.TrimSpace(r.ModeratedBy) != "" ||
		r.LikeCount != nil ||
		strings.TrimSpace(r.DisplayLabel) != "" ||
		strings.TrimSpace(r.Email) != ""
}

func decodeCreateCommentRequest(w http.ResponseWriter, r *http.Request) (createCommentRequest, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return createCommentRequest{}, false
	}
	var req createCommentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
		return createCommentRequest{}, false
	}
	if req.hasForbiddenClientFields() {
		writeJSON(w, 400, map[string]string{"error": "client cannot set moderation or identity fields"})
		return createCommentRequest{}, false
	}
	return req, true
}

func (srv *Server) requireUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := srv.identify(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "missing identity"})
		return "", false
	}
	if srv.fb == nil && srv.commentStoreOverride == nil {
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
// PUT|DELETE /products/{productId}/comments/{commentId}/like
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

	if len(parts) >= 4 && parts[3] == "like" {
		commentID := parts[2]
		if commentID == "" {
			writeJSON(w, 400, map[string]string{"error": "missing comment_id"})
			return
		}
		switch r.Method {
		case http.MethodPut:
			if err := srv.commentStore().setCommentLike(r.Context(), productID, commentID, uid, true); err != nil {
				code, msg := mapCommentStoreError(err)
				writeJSON(w, code, map[string]string{"error": msg})
				return
			}
			writeJSON(w, 200, map[string]string{"status": "ok"})
		case http.MethodDelete:
			if err := srv.commentStore().setCommentLike(r.Context(), productID, commentID, uid, false); err != nil {
				code, msg := mapCommentStoreError(err)
				writeJSON(w, code, map[string]string{"error": msg})
				return
			}
			writeJSON(w, 200, map[string]string{"status": "ok"})
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
		return
	}

	if len(parts) != 2 {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		items, err := srv.commentStore().listComments(r.Context(), productID, uid)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"comments": items})
	case http.MethodPost:
		req, ok := decodeCreateCommentRequest(w, r)
		if !ok {
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" || len(text) > 2000 {
			writeJSON(w, 400, map[string]string{"error": "invalid text"})
			return
		}
		email := emailFromContext(r.Context())
		c, err := srv.commentStore().createComment(
			r.Context(),
			uid,
			email,
			productID,
			strings.TrimSpace(req.ParentID),
			text,
		)
		if err != nil {
			code, msg := mapCommentStoreError(err)
			if code >= 500 {
				log.Printf("create comment: %v", err)
				msg = "failed to create comment"
			}
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, 201, c)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
