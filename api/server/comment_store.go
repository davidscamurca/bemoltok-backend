package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errCommentNotFound    = errors.New("comment not found")
	errParentNotFound     = errors.New("parent comment not found")
	errParentWrongProduct = errors.New("parent comment belongs to another product")
)

type commentStore interface {
	listComments(ctx context.Context, productID, viewerUID string) ([]commentDoc, error)
	createComment(ctx context.Context, uid, email, productID, parentID, text string) (*commentDoc, error)
	setCommentLike(ctx context.Context, productID, commentID, uid string, on bool) error
}

type firestoreCommentStore struct {
	fb *FirebaseClients
}

func (srv *Server) commentStore() commentStore {
	if srv.commentStoreOverride != nil {
		return srv.commentStoreOverride
	}
	return firestoreCommentStore{fb: srv.fb}
}

func authorDisplayLabel(email string) string {
	e := strings.TrimSpace(email)
	if e == "" {
		return "Utilizador"
	}
	at := strings.Index(e, "@")
	if at > 0 {
		return e[:at]
	}
	return e
}

func commentVisibleStatus(status string) bool {
	s := strings.TrimSpace(status)
	return s == "" || s == commentStatusPublished
}

func (s firestoreCommentStore) listComments(ctx context.Context, productID, viewerUID string) ([]commentDoc, error) {
	iter := s.fb.Firestore.Collection(commentsCollection).
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
		if !commentVisibleStatus(c.Status) {
			continue
		}
		c.LikedByMe = s.commentLikedByViewer(ctx, d.Ref, viewerUID)
		out = append(out, c)
	}
	sortCommentsByCreatedAt(out)
	return out, nil
}

func (s firestoreCommentStore) commentLikedByViewer(ctx context.Context, ref *firestore.DocumentRef, viewerUID string) bool {
	if viewerUID == "" {
		return false
	}
	_, err := ref.Collection(commentLikesSubcollection).Doc(viewerUID).Get(ctx)
	return err == nil
}

func sortCommentsByCreatedAt(out []commentDoc) {
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.Before(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
}

func (s firestoreCommentStore) getComment(ctx context.Context, commentID string) (*commentDoc, error) {
	snap, err := s.fb.Firestore.Collection(commentsCollection).Doc(commentID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, errCommentNotFound
		}
		return nil, err
	}
	var c commentDoc
	if err := snap.DataTo(&c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		c.ID = snap.Ref.ID
	}
	return &c, nil
}

func (s firestoreCommentStore) createComment(ctx context.Context, uid, email, productID, parentID, text string) (*commentDoc, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID != "" {
		parent, err := s.getComment(ctx, parentID)
		if err != nil {
			if errors.Is(err, errCommentNotFound) {
				return nil, errParentNotFound
			}
			return nil, err
		}
		if parent.ProductID != productID {
			return nil, errParentWrongProduct
		}
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	c := commentDoc{
		ID:           id,
		UID:          uid,
		Email:        email,
		DisplayLabel: authorDisplayLabel(email),
		ProductID:    productID,
		ParentID:     parentID,
		Text:         text,
		CreatedAt:    now,
		LikeCount:    0,
		Status:       commentStatusPublished,
	}
	_, err := s.fb.Firestore.Collection(commentsCollection).Doc(id).Set(ctx, c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s firestoreCommentStore) setCommentLike(ctx context.Context, productID, commentID, uid string, on bool) error {
	commentRef := s.fb.Firestore.Collection(commentsCollection).Doc(commentID)
	likeRef := commentRef.Collection(commentLikesSubcollection).Doc(uid)

	return s.fb.Firestore.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(commentRef)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return errCommentNotFound
			}
			return err
		}
		var doc commentDoc
		if err := snap.DataTo(&doc); err != nil {
			return err
		}
		if doc.ProductID != productID {
			return errCommentNotFound
		}

		_, likeErr := tx.Get(likeRef)
		likeExists := likeErr == nil
		if likeErr != nil && status.Code(likeErr) != codes.NotFound {
			return likeErr
		}

		if on {
			if likeExists {
				return nil
			}
			if err := tx.Set(likeRef, map[string]interface{}{
				"uid":        uid,
				"created_at": time.Now().UTC(),
			}); err != nil {
				return err
			}
			return tx.Update(commentRef, []firestore.Update{
				{Path: "like_count", Value: firestore.Increment(1)},
			})
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
		return tx.Update(commentRef, []firestore.Update{
			{Path: "like_count", Value: next},
		})
	})
}

func mapCommentStoreError(err error) (int, string) {
	switch {
	case errors.Is(err, errParentNotFound):
		return 404, "parent comment not found"
	case errors.Is(err, errParentWrongProduct):
		return 400, "parent comment belongs to another product"
	case errors.Is(err, errCommentNotFound):
		return 404, "comment not found"
	default:
		return 500, fmt.Sprintf("comment error: %v", err)
	}
}
