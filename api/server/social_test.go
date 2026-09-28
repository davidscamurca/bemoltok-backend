package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memCommentStore struct {
	mu       sync.Mutex
	comments map[string]commentDoc
	likes    map[string]map[string]struct{} // commentID -> uid set
}

func newMemCommentStore() *memCommentStore {
	return &memCommentStore{
		comments: make(map[string]commentDoc),
		likes:    make(map[string]map[string]struct{}),
	}
}

func (m *memCommentStore) listComments(_ context.Context, productID, viewerUID string) ([]commentDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]commentDoc, 0)
	for _, c := range m.comments {
		if c.ProductID != productID {
			continue
		}
		if !commentVisibleStatus(c.Status) {
			continue
		}
		c.LikedByMe = m.likedLocked(c.ID, viewerUID)
		out = append(out, c)
	}
	sortCommentsByCreatedAt(out)
	return out, nil
}

func (m *memCommentStore) likedLocked(commentID, viewerUID string) bool {
	if viewerUID == "" {
		return false
	}
	set := m.likes[commentID]
	if set == nil {
		return false
	}
	_, ok := set[viewerUID]
	return ok
}

func (m *memCommentStore) createComment(_ context.Context, uid, email, productID, parentID, text string) (*commentDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	parentID = strings.TrimSpace(parentID)
	if parentID != "" {
		parent, ok := m.comments[parentID]
		if !ok {
			return nil, errParentNotFound
		}
		if parent.ProductID != productID {
			return nil, errParentWrongProduct
		}
	}

	id := "c-" + time.Now().UTC().Format("150405.000000")
	c := commentDoc{
		ID:           id,
		UID:          uid,
		Email:        email,
		DisplayLabel: authorDisplayLabel(email),
		ProductID:    productID,
		ParentID:     parentID,
		Text:         text,
		CreatedAt:    time.Now().UTC(),
		LikeCount:    0,
		Status:       commentStatusPublished,
	}
	m.comments[id] = c
	return &c, nil
}

func (m *memCommentStore) setCommentLike(_ context.Context, productID, commentID, uid string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.comments[commentID]
	if !ok || c.ProductID != productID {
		return errCommentNotFound
	}
	if m.likes[commentID] == nil {
		m.likes[commentID] = make(map[string]struct{})
	}
	if on {
		if _, exists := m.likes[commentID][uid]; exists {
			return nil
		}
		m.likes[commentID][uid] = struct{}{}
		c.LikeCount++
	} else {
		if _, exists := m.likes[commentID][uid]; !exists {
			return nil
		}
		delete(m.likes[commentID], uid)
		c.LikeCount--
		if c.LikeCount < 0 {
			c.LikeCount = 0
		}
	}
	m.comments[commentID] = c
	return nil
}

func testCommentServer(store commentStore) *Server {
	return &Server{
		authRequired:         true,
		commentStoreOverride: store,
	}
}

func withAuth(req *http.Request, uid, email string) *http.Request {
	ctx := context.WithValue(req.Context(), uidKey, uid)
	ctx = context.WithValue(ctx, emailKey, email)
	return req.WithContext(ctx)
}

func TestCreateCommentRootAndAuthorIdentity(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	body := `{"text":"Olá mundo"}`
	req := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(body))
	req = withAuth(req, "uid-a", "ana.silva@bemol.com.br")
	w := httptest.NewRecorder()
	srv.handleProductComments(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var c commentDoc
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.UID != "uid-a" {
		t.Fatalf("uid=%q", c.UID)
	}
	if c.DisplayLabel != "ana.silva" {
		t.Fatalf("author_label=%q", c.DisplayLabel)
	}
	if c.Status != commentStatusPublished {
		t.Fatalf("status=%q", c.Status)
	}
	if c.ParentID != "" {
		t.Fatalf("parent_id=%q", c.ParentID)
	}
}

func TestCreateCommentReply(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	rootReq := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(`{"text":"root"}`))
	rootReq = withAuth(rootReq, "uid-a", "a@bemol.com.br")
	rootW := httptest.NewRecorder()
	srv.handleProductComments(rootW, rootReq)
	var root commentDoc
	_ = json.Unmarshal(rootW.Body.Bytes(), &root)

	replyBody := `{"text":"reply","parent_id":"` + root.ID + `"}`
	replyReq := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(replyBody))
	replyReq = withAuth(replyReq, "uid-b", "b@bemol.com.br")
	replyW := httptest.NewRecorder()
	srv.handleProductComments(replyW, replyReq)
	if replyW.Code != http.StatusCreated {
		t.Fatalf("reply status=%d body=%s", replyW.Code, replyW.Body.String())
	}
	var reply commentDoc
	_ = json.Unmarshal(replyW.Body.Bytes(), &reply)
	if reply.ParentID != root.ID {
		t.Fatalf("parent_id=%q", reply.ParentID)
	}
}

func TestCreateCommentInvalidParent(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	req := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(`{"text":"x","parent_id":"missing"}`))
	req = withAuth(req, "uid-a", "a@bemol.com.br")
	w := httptest.NewRecorder()
	srv.handleProductComments(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCreateCommentParentWrongProduct(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	rootReq := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(`{"text":"root"}`))
	rootReq = withAuth(rootReq, "uid-a", "a@bemol.com.br")
	rootW := httptest.NewRecorder()
	srv.handleProductComments(rootW, rootReq)
	var root commentDoc
	_ = json.Unmarshal(rootW.Body.Bytes(), &root)

	req := httptest.NewRequest(http.MethodPost, "/products/p2/comments", bytes.NewBufferString(`{"text":"bad","parent_id":"`+root.ID+`"}`))
	req = withAuth(req, "uid-a", "a@bemol.com.br")
	w := httptest.NewRecorder()
	srv.handleProductComments(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCommentLikeUnlikeAndIdempotency(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	createReq := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(`{"text":"x"}`))
	createReq = withAuth(createReq, "uid-a", "a@bemol.com.br")
	createW := httptest.NewRecorder()
	srv.handleProductComments(createW, createReq)
	var c commentDoc
	_ = json.Unmarshal(createW.Body.Bytes(), &c)

	likeURL := "/products/p1/comments/" + c.ID + "/like"
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPut, likeURL, nil)
		req = withAuth(req, "uid-b", "b@bemol.com.br")
		w := httptest.NewRecorder()
		srv.handleProductComments(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("like attempt %d status=%d", i, w.Code)
		}
	}

	listReq := httptest.NewRequest(http.MethodGet, "/products/p1/comments", nil)
	listReq = withAuth(listReq, "uid-b", "b@bemol.com.br")
	listW := httptest.NewRecorder()
	srv.handleProductComments(listW, listReq)
	var payload struct {
		Comments []commentDoc `json:"comments"`
	}
	_ = json.Unmarshal(listW.Body.Bytes(), &payload)
	if len(payload.Comments) != 1 {
		t.Fatalf("comments=%d", len(payload.Comments))
	}
	if payload.Comments[0].LikeCount != 1 {
		t.Fatalf("like_count=%d", payload.Comments[0].LikeCount)
	}
	if !payload.Comments[0].LikedByMe {
		t.Fatal("expected liked_by_me=true")
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodDelete, likeURL, nil)
		req = withAuth(req, "uid-b", "b@bemol.com.br")
		w := httptest.NewRecorder()
		srv.handleProductComments(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("unlike attempt %d status=%d", i, w.Code)
		}
	}

	listReq2 := httptest.NewRequest(http.MethodGet, "/products/p1/comments", nil)
	listReq2 = withAuth(listReq2, "uid-b", "b@bemol.com.br")
	listW2 := httptest.NewRecorder()
	srv.handleProductComments(listW2, listReq2)
	_ = json.Unmarshal(listW2.Body.Bytes(), &payload)
	if payload.Comments[0].LikeCount != 0 {
		t.Fatalf("like_count after unlike=%d", payload.Comments[0].LikeCount)
	}
	if payload.Comments[0].LikedByMe {
		t.Fatal("expected liked_by_me=false")
	}
}

func TestCommentLikeCountNeverNegative(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	createReq := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(`{"text":"x"}`))
	createReq = withAuth(createReq, "uid-a", "a@bemol.com.br")
	createW := httptest.NewRecorder()
	srv.handleProductComments(createW, createReq)
	var c commentDoc
	_ = json.Unmarshal(createW.Body.Bytes(), &c)

	unlikeURL := "/products/p1/comments/" + c.ID + "/like"
	req := httptest.NewRequest(http.MethodDelete, unlikeURL, nil)
	req = withAuth(req, "uid-b", "b@bemol.com.br")
	w := httptest.NewRecorder()
	srv.handleProductComments(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	store.mu.Lock()
	got := store.comments[c.ID].LikeCount
	store.mu.Unlock()
	if got < 0 {
		t.Fatalf("like_count=%d", got)
	}
}

func TestCreateCommentRejectsModerationFields(t *testing.T) {
	store := newMemCommentStore()
	srv := testCommentServer(store)

	body := `{"text":"x","status":"hidden","moderated_by":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/products/p1/comments", bytes.NewBufferString(body))
	req = withAuth(req, "uid-a", "a@bemol.com.br")
	w := httptest.NewRecorder()
	srv.handleProductComments(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthorDisplayLabel(t *testing.T) {
	if got := authorDisplayLabel("joao@bemol.com.br"); got != "joao" {
		t.Fatalf("got %q", got)
	}
	if got := authorDisplayLabel(""); got != "Utilizador" {
		t.Fatalf("got %q", got)
	}
}
