package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleUsersSubRouting(t *testing.T) {
	srv := &Server{}

	// Non-GET methods should return 405 Method Not Allowed
	req := httptest.NewRequest(http.MethodPost, "/users/user123", nil)
	w := httptest.NewRecorder()
	srv.handleUsersSub(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	// Empty path under /users/ should return 404
	req = httptest.NewRequest(http.MethodGet, "/users/", nil)
	w = httptest.NewRecorder()
	srv.handleUsersSub(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}

	// Unknown subpath should return 404
	req = httptest.NewRequest(http.MethodGet, "/users/user123/unknown", nil)
	w = httptest.NewRecorder()
	srv.handleUsersSub(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetPublicProfileNotFound(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/users/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.handleUsersSub(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when user has no profile or posts, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json response: %v", err)
	}
	if resp["error"] != "user not found" {
		t.Fatalf("expected 'user not found', got %q", resp["error"])
	}
}
