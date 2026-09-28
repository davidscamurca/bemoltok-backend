package main

import (
	"log"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PublicUserProfile is the public projection of a user's identity and activity
// returned by GET /users/{uid}. Private details like full email or linked
// BemolClientID are intentionally omitted.
type PublicUserProfile struct {
	UID                string `json:"uid"`
	DisplayLabel       string `json:"display_label"`
	AvatarURL          string `json:"avatar_url,omitempty"`
	PostsCount         int    `json:"posts_count"`
	LikesReceivedCount int    `json:"likes_received_count"`
}

// handleUsersSub routes sub-paths under /users/:
//   GET /users/{uid}
//   GET /users/{uid}/posts
func (srv *Server) handleUsersSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/users/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	targetUID := parts[0]

	if len(parts) == 1 {
		srv.getPublicProfile(w, r, targetUID)
		return
	}

	if len(parts) == 2 && parts[1] == "posts" {
		srv.listUserPosts(w, r, targetUID)
		return
	}

	writeJSON(w, 404, map[string]string{"error": "not found"})
}

// getPublicProfile returns the public profile stats for targetUID.
func (srv *Server) getPublicProfile(w http.ResponseWriter, r *http.Request, targetUID string) {
	ctx := r.Context()

	var displayLabel string
	var userFound bool

	// Try loading users/{targetUID} doc.
	if srv.fb != nil && srv.fb.Firestore != nil {
		userDoc, err := srv.fb.Firestore.Collection(usersCollection).Doc(targetUID).Get(ctx)
		if err == nil {
			userFound = true
			var u UserProfile
			if err := userDoc.DataTo(&u); err == nil && u.Email != "" {
				displayLabel = authorDisplayLabel(u.Email)
			}
		} else if status.Code(err) != codes.NotFound {
			log.Printf("public_profile: load user %s failed: %v", targetUID, err)
		}
	}

	// Tally published posts and likes received.
	postsCount := 0
	likesReceivedCount := 0
	fallbackLabel := ""

	if srv.ugc != nil && srv.ugc.fs != nil {
		iter := srv.ugc.fs.Collection(postsCollection).Where("uid", "==", targetUID).Documents(ctx)
		docs, err := iter.GetAll()
		if err == nil {
			for _, d := range docs {
				var p PostDoc
				if err := d.DataTo(&p); err != nil {
					continue
				}
				if p.Status != postStatusPublished {
					continue
				}
				postsCount++
				likesReceivedCount += p.LikeCount
				if fallbackLabel == "" && p.AuthorLabel != "" {
					fallbackLabel = p.AuthorLabel
				}
			}
		} else {
			log.Printf("public_profile: load posts for %s failed: %v", targetUID, err)
		}
	}

	if displayLabel == "" {
		if fallbackLabel != "" {
			displayLabel = fallbackLabel
		} else if userFound {
			displayLabel = "Utilizador"
		} else if postsCount > 0 {
			displayLabel = "Utilizador"
		} else {
			writeJSON(w, 404, map[string]string{"error": "user not found"})
			return
		}
	}

	profile := PublicUserProfile{
		UID:                targetUID,
		DisplayLabel:       displayLabel,
		PostsCount:         postsCount,
		LikesReceivedCount: likesReceivedCount,
	}
	writeJSON(w, 200, profile)
}

// listUserPosts returns the published posts created by targetUID with short-lived
// signed GET URLs for display.
func (srv *Server) listUserPosts(w http.ResponseWriter, r *http.Request, targetUID string) {
	if srv.ugc == nil {
		writeJSON(w, 503, map[string]string{"error": "posts not configured"})
		return
	}
	ctx := r.Context()
	iter := srv.ugc.fs.Collection(postsCollection).Where("uid", "==", targetUID).Documents(ctx)
	docs, err := iter.GetAll()
	if err != nil {
		log.Printf("public_profile: list posts failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not list posts"})
		return
	}

	posts := make([]PostDoc, 0, len(docs))
	for _, d := range docs {
		var p PostDoc
		if err := d.DataTo(&p); err != nil {
			continue
		}
		if p.Status != postStatusPublished {
			continue
		}
		posts = append(posts, p)
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].CreatedAt.After(posts[j].CreatedAt) })

	callerUID, _ := uidFromContext(ctx)

	views := make([]postView, 0, len(posts))
	for _, p := range posts {
		urls := make([]string, 0, len(p.Media))
		for _, m := range p.Media {
			if url, err := srv.ugc.signedGetURL(m.Object); err == nil {
				urls = append(urls, url)
			}
		}
		likedByMe := false
		if callerUID != "" {
			_, err := srv.ugc.fs.Collection(postsCollection).Doc(p.ID).Collection(postLikesSubcollection).Doc(callerUID).Get(ctx)
			likedByMe = err == nil
		}
		views = append(views, postView{
			PostDoc:   p,
			MediaURLs: urls,
			LikedByMe: likedByMe,
		})
	}

	writeJSON(w, 200, map[string]interface{}{"posts": views})
}
