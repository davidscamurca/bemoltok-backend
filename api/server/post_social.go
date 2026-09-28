package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	postLikesSubcollection = "likes"
	postCommentsCollection = "post_comments"
)

type postCommentDoc struct {
	ID           string     `json:"id" firestore:"id"`
	UID          string     `json:"uid" firestore:"uid"`
	Email        string     `json:"-" firestore:"email,omitempty"`
	DisplayLabel string     `json:"author_label" firestore:"display_label"`
	PostID       string     `json:"post_id" firestore:"post_id"`
	ParentID     string     `json:"parent_id,omitempty" firestore:"parent_id,omitempty"`
	Text         string     `json:"text" firestore:"text"`
	CreatedAt    time.Time  `json:"created_at" firestore:"created_at"`
	LikeCount    int        `json:"like_count" firestore:"like_count"`
	LikedByMe    bool       `json:"liked_by_me"`
	Status       string     `json:"status" firestore:"status"`
	ReportedCount int       `json:"reported_count,omitempty" firestore:"reported_count,omitempty"`
	ModerationReason string `json:"moderation_reason,omitempty" firestore:"moderation_reason,omitempty"`
	ModeratedAt  *time.Time `json:"moderated_at,omitempty" firestore:"moderated_at,omitempty"`
	ModeratedBy  string     `json:"moderated_by,omitempty" firestore:"moderated_by,omitempty"`
}

func (srv *Server) postPublished(ctx context.Context, postID string) (*PostDoc, error) {
	ref := srv.ugc.fs.Collection(postsCollection).Doc(postID)
	snap, err := ref.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, errPostNotFound
		}
		return nil, err
	}
	var doc PostDoc
	if err := snap.DataTo(&doc); err != nil {
		return nil, err
	}
	if doc.Status != postStatusPublished {
		return nil, errPostNotFound
	}
	if doc.ID == "" {
		doc.ID = snap.Ref.ID
	}
	return &doc, nil
}

var errPostNotFound = errors.New("post not found")

func (srv *Server) handlePostLike(w http.ResponseWriter, r *http.Request, postID, uid string) {
	ctx := r.Context()
	if _, err := srv.postPublished(ctx, postID); err != nil {
		code, msg := 404, "post not found"
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	postRef := srv.ugc.fs.Collection(postsCollection).Doc(postID)
	likeRef := postRef.Collection(postLikesSubcollection).Doc(uid)
	switch r.Method {
	case http.MethodPut:
		err := srv.ugc.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			snap, err := tx.Get(postRef)
			if err != nil {
				return err
			}
			var doc PostDoc
			if err := snap.DataTo(&doc); err != nil {
				return err
			}
			if _, err := tx.Get(likeRef); err == nil {
				return nil
			} else if status.Code(err) != codes.NotFound {
				return err
			}
			if err := tx.Set(likeRef, map[string]interface{}{"uid": uid, "created_at": time.Now().UTC()}); err != nil {
				return err
			}
			return tx.Update(postRef, []firestore.Update{{Path: "like_count", Value: firestore.Increment(1)}})
		})
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case http.MethodDelete:
		err := srv.ugc.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			snap, err := tx.Get(postRef)
			if err != nil {
				return err
			}
			var doc PostDoc
			if err := snap.DataTo(&doc); err != nil {
				return err
			}
			if _, err := tx.Get(likeRef); err != nil {
				if status.Code(err) == codes.NotFound {
					return nil
				}
				return err
			}
			if err := tx.Delete(likeRef); err != nil {
				return err
			}
			next := doc.LikeCount - 1
			if next < 0 {
				next = 0
			}
			return tx.Update(postRef, []firestore.Update{{Path: "like_count", Value: next}})
		})
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (srv *Server) handlePostComments(w http.ResponseWriter, r *http.Request, postID, uid string) {
	ctx := r.Context()
	if _, err := srv.postPublished(ctx, postID); err != nil {
		writeJSON(w, 404, map[string]string{"error": "post not found"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := srv.listPostComments(ctx, postID, uid)
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
		c, err := srv.createPostComment(ctx, uid, emailFromContext(r.Context()), postID, strings.TrimSpace(req.ParentID), text)
		if err != nil {
			code, msg := mapCommentStoreError(err)
			if strings.Contains(msg, "parent") {
				code = 400
			}
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, 201, c)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (srv *Server) listPostComments(ctx context.Context, postID, viewerUID string) ([]postCommentDoc, error) {
	iter := srv.ugc.fs.Collection(postCommentsCollection).
		Where("post_id", "==", postID).
		Limit(200).
		Documents(ctx)
	defer iter.Stop()
	out := make([]postCommentDoc, 0)
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var c postCommentDoc
		if err := d.DataTo(&c); err != nil {
			continue
		}
		if c.ID == "" {
			c.ID = d.Ref.ID
		}
		if !commentVisibleStatus(c.Status) {
			continue
		}
		if viewerUID != "" {
			_, err := d.Ref.Collection(commentLikesSubcollection).Doc(viewerUID).Get(ctx)
			c.LikedByMe = err == nil
		}
		out = append(out, c)
	}
	sortCommentsByCreatedAtPost(out)
	return out, nil
}

func sortCommentsByCreatedAtPost(out []postCommentDoc) {
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.Before(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
}

func (srv *Server) createPostComment(ctx context.Context, uid, email, postID, parentID, text string) (*postCommentDoc, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID != "" {
		snap, err := srv.ugc.fs.Collection(postCommentsCollection).Doc(parentID).Get(ctx)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, errParentNotFound
			}
			return nil, err
		}
		var parent postCommentDoc
		if err := snap.DataTo(&parent); err != nil {
			return nil, err
		}
		if parent.PostID != postID {
			return nil, errParentWrongProduct
		}
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	c := postCommentDoc{
		ID: id, UID: uid, Email: email, DisplayLabel: authorDisplayLabel(email),
		PostID: postID, ParentID: parentID, Text: text, CreatedAt: now,
		LikeCount: 0, Status: commentStatusPublished,
	}
	_, err := srv.ugc.fs.Collection(postCommentsCollection).Doc(id).Set(ctx, c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (srv *Server) handlePostCommentLike(w http.ResponseWriter, r *http.Request, postID, commentID, uid string) {
	ctx := r.Context()
	if _, err := srv.postPublished(ctx, postID); err != nil {
		writeJSON(w, 404, map[string]string{"error": "post not found"})
		return
	}
	commentRef := srv.ugc.fs.Collection(postCommentsCollection).Doc(commentID)
	likeRef := commentRef.Collection(commentLikesSubcollection).Doc(uid)
	err := srv.ugc.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(commentRef)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return errCommentNotFound
			}
			return err
		}
		var doc postCommentDoc
		if err := snap.DataTo(&doc); err != nil {
			return err
		}
		if doc.PostID != postID {
			return errCommentNotFound
		}
		_, likeErr := tx.Get(likeRef)
		likeExists := likeErr == nil
		if likeErr != nil && status.Code(likeErr) != codes.NotFound {
			return likeErr
		}
		if r.Method == http.MethodPut {
			if likeExists {
				return nil
			}
			if err := tx.Set(likeRef, map[string]interface{}{"uid": uid, "created_at": time.Now().UTC()}); err != nil {
				return err
			}
			return tx.Update(commentRef, []firestore.Update{{Path: "like_count", Value: firestore.Increment(1)}})
		}
		if !likeExists {
			return nil
		}
		if err := tx.Delete(likeRef); err != nil {
			return err
		}
		next := doc.LikeCount - 1
		if next < 0 {
			next = 0
		}
		return tx.Update(commentRef, []firestore.Update{{Path: "like_count", Value: next}})
	})
	if err != nil {
		code, msg := mapCommentStoreError(err)
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
