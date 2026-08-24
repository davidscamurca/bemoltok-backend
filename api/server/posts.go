package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── UGC (user-generated posts) ────────────────────────────────────────────────
//
// A logged-in user posts EITHER 3–5 photos OR a single video (up to 2 min, from
// the device gallery). A product_id is mandatory: the app uses it to resolve the
// "go to store" link via VTEX (the backend does not resolve the URL — it only
// requires the id to be present and carries it on the post).
//
// Media never flows through Cloud Run. The flow is:
//   1. POST /posts            → validate, create the post doc (awaiting_upload),
//                               return one V4 signed PUT URL per file.
//   2. (app) PUT each file    → straight to the UGC bucket via the signed URL.
//   3. POST /posts/{id}/complete → backend verifies the objects exist, enforces
//                               type/size caps, flips the post to published.
//
// Reads (GET /posts) return short-lived signed GET URLs for display.
//
// TODO(moderation): posts publish immediately (status=published). When moderation
// is added, /complete should set status=pending_review and a reviewer/automated
// step promotes to approved before the post is eligible for any feed.

const (
	postsCollection = "posts"

	minPhotos = 3
	maxPhotos = 5

	maxPhotoBytes = 10 << 20  // 10 MiB per photo
	maxVideoBytes = 100 << 20 // 100 MiB for the video

	// maxVideoSeconds is the gallery video limit. It is enforced by the app
	// (the backend cannot cheaply probe duration); kept here for documentation.
	maxVideoSeconds = 120

	uploadURLTTL = 15 * time.Minute
	viewURLTTL   = 60 * time.Minute

	postStatusAwaiting  = "awaiting_upload"
	postStatusPublished = "published"

	postTypePhotos = "photos"
	postTypeVideo  = "video"
)

// allowedPhotoTypes / allowedVideoTypes map an accepted MIME type to the file
// extension used for the object name. Gallery exports on iOS/Android fall into
// these types (HEIC is transcoded to JPEG by the picker on the app side).
var (
	allowedPhotoTypes = map[string]string{
		"image/jpeg": "jpg",
		"image/png":  "png",
	}
	allowedVideoTypes = map[string]string{
		"video/mp4":       "mp4",
		"video/quicktime": "mov",
	}
)

// ugcService holds everything the /posts endpoints need. It is nil when the
// UGC bucket or the URL signer could not be initialized, which disables /posts.
type ugcService struct {
	fs         *firestore.Client
	bucket     *storage.BucketHandle
	bucketName string
	signerSA   string // SA email used as GoogleAccessID for V4 signing
	iam        *iamcredentials.Service
}

// newUGCService wires the UGC bucket and the IAM-based URL signer. The signer
// uses iamcredentials.SignBlob (no private key on disk), so the runtime SA needs
// roles/iam.serviceAccountTokenCreator on itself.
func newUGCService(ctx context.Context, fs *firestore.Client, bucketName, signerSA string) (*ugcService, error) {
	sc, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage.NewClient: %w", err)
	}
	iamSvc, err := iamcredentials.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("iamcredentials.NewService: %w", err)
	}
	return &ugcService{
		fs:         fs,
		bucket:     sc.Bucket(bucketName),
		bucketName: bucketName,
		signerSA:   signerSA,
		iam:        iamSvc,
	}, nil
}

// signBytes signs with the runtime service account via the IAM Credentials API.
func (u *ugcService) signBytes(b []byte) ([]byte, error) {
	resp, err := u.iam.Projects.ServiceAccounts.SignBlob(
		"projects/-/serviceAccounts/"+u.signerSA,
		&iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(b)},
	).Do()
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(resp.SignedBlob)
}

func (u *ugcService) signedPutURL(object, contentType string) (string, error) {
	return u.bucket.SignedURL(object, &storage.SignedURLOptions{
		Scheme:         storage.SigningSchemeV4,
		Method:         http.MethodPut,
		Expires:        time.Now().Add(uploadURLTTL),
		ContentType:    contentType,
		GoogleAccessID: u.signerSA,
		SignBytes:      u.signBytes,
	})
}

func (u *ugcService) signedGetURL(object string) (string, error) {
	return u.bucket.SignedURL(object, &storage.SignedURLOptions{
		Scheme:         storage.SigningSchemeV4,
		Method:         http.MethodGet,
		Expires:        time.Now().Add(viewURLTTL),
		GoogleAccessID: u.signerSA,
		SignBytes:      u.signBytes,
	})
}

// ── Firestore model ───────────────────────────────────────────────────────────

type PostMedia struct {
	Object      string `firestore:"object"       json:"object"`
	ContentType string `firestore:"content_type" json:"content_type"`
	Size        int64  `firestore:"size"         json:"size,omitempty"`
}

type PostDoc struct {
	ID          string      `firestore:"id"                     json:"id"`
	UID         string      `firestore:"uid"                    json:"-"`
	ProductID   string      `firestore:"product_id"             json:"product_id"`
	Type        string      `firestore:"type"                   json:"type"`
	Media       []PostMedia `firestore:"media"                  json:"media"`
	Status      string      `firestore:"status"                 json:"status"`
	CreatedAt   time.Time   `firestore:"created_at"             json:"created_at"`
	PublishedAt *time.Time  `firestore:"published_at,omitempty" json:"published_at,omitempty"`
}

// ── Request / response types ──────────────────────────────────────────────────

type postItemInput struct {
	ContentType string `json:"content_type"`
}

type createPostRequest struct {
	ProductID string          `json:"product_id"`
	Type      string          `json:"type"`
	Items     []postItemInput `json:"items"`
}

type uploadTarget struct {
	Object      string            `json:"object"`
	ContentType string            `json:"content_type"`
	Method      string            `json:"method"`
	UploadURL   string            `json:"upload_url"`
	Headers     map[string]string `json:"headers"`
	MaxBytes    int64             `json:"max_bytes"`
}

type createPostResponse struct {
	PostID         string         `json:"post_id"`
	Status         string         `json:"status"`
	UploadExpires  string         `json:"upload_expires_at"`
	MaxVideoSecond int            `json:"max_video_seconds,omitempty"`
	Uploads        []uploadTarget `json:"uploads"`
}

type postView struct {
	PostDoc
	MediaURLs []string `json:"media_urls"`
}

// ── Routing ───────────────────────────────────────────────────────────────────

// handlePosts serves POST /posts (create/init) and GET /posts (list own posts).
func (srv *Server) handlePosts(w http.ResponseWriter, r *http.Request) {
	if srv.ugc == nil {
		writeJSON(w, 503, map[string]string{"error": "posts not configured"})
		return
	}
	uid, ok := uidFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		srv.createPost(w, r, uid)
	case http.MethodGet:
		srv.listPosts(w, r, uid)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handlePostsSub serves POST /posts/{id}/complete.
func (srv *Server) handlePostsSub(w http.ResponseWriter, r *http.Request) {
	if srv.ugc == nil {
		writeJSON(w, 503, map[string]string{"error": "posts not configured"})
		return
	}
	uid, ok := uidFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/posts/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "complete" {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		srv.completePost(w, r, uid, parts[0])
		return
	}
	writeJSON(w, 404, map[string]string{"error": "not found"})
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// createPost validates the request, persists an awaiting_upload post and returns
// one signed PUT URL per file. The client must PUT each file with the exact
// Content-Type echoed back (it is part of the V4 signature).
func (srv *Server) createPost(w http.ResponseWriter, r *http.Request, uid string) {
	var req createPostRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	productID := strings.TrimSpace(req.ProductID)
	if productID == "" {
		writeJSON(w, 400, map[string]string{"error": "product_id is required"})
		return
	}

	var typeMap map[string]string
	var maxBytes int64
	switch req.Type {
	case postTypePhotos:
		if n := len(req.Items); n < minPhotos || n > maxPhotos {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("photos require between %d and %d items", minPhotos, maxPhotos)})
			return
		}
		typeMap, maxBytes = allowedPhotoTypes, maxPhotoBytes
	case postTypeVideo:
		if len(req.Items) != 1 {
			writeJSON(w, 400, map[string]string{"error": "video requires exactly 1 item"})
			return
		}
		typeMap, maxBytes = allowedVideoTypes, maxVideoBytes
	default:
		writeJSON(w, 400, map[string]string{"error": "type must be 'photos' or 'video'"})
		return
	}

	postID := uuid.NewString()
	now := time.Now().UTC()
	media := make([]PostMedia, 0, len(req.Items))
	uploads := make([]uploadTarget, 0, len(req.Items))

	for i, item := range req.Items {
		ct := strings.ToLower(strings.TrimSpace(item.ContentType))
		ext, ok := typeMap[ct]
		if !ok {
			writeJSON(w, 415, map[string]string{"error": "unsupported content_type: " + item.ContentType})
			return
		}
		object := fmt.Sprintf("posts/%s/%s/%d.%s", uid, postID, i, ext)
		url, err := srv.ugc.signedPutURL(object, ct)
		if err != nil {
			log.Printf("posts: sign put url failed: %v", err)
			writeJSON(w, 500, map[string]string{"error": "could not create upload url"})
			return
		}
		media = append(media, PostMedia{Object: object, ContentType: ct})
		uploads = append(uploads, uploadTarget{
			Object:      object,
			ContentType: ct,
			Method:      http.MethodPut,
			UploadURL:   url,
			Headers:     map[string]string{"Content-Type": ct},
			MaxBytes:    maxBytes,
		})
	}

	doc := PostDoc{
		ID:        postID,
		UID:       uid,
		ProductID: productID,
		Type:      req.Type,
		Media:     media,
		Status:    postStatusAwaiting,
		CreatedAt: now,
	}
	if _, err := srv.ugc.fs.Collection(postsCollection).Doc(postID).Set(r.Context(), doc); err != nil {
		log.Printf("posts: create doc failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not create post"})
		return
	}

	resp := createPostResponse{
		PostID:        postID,
		Status:        postStatusAwaiting,
		UploadExpires: now.Add(uploadURLTTL).Format(time.RFC3339),
		Uploads:       uploads,
	}
	if req.Type == postTypeVideo {
		resp.MaxVideoSecond = maxVideoSeconds
	}
	writeJSON(w, 201, resp)
}

// completePost verifies every planned object was uploaded, enforces the size cap
// and content-type, then publishes the post. Idempotent: a second call on an
// already-published post returns it unchanged.
func (srv *Server) completePost(w http.ResponseWriter, r *http.Request, uid, postID string) {
	ctx := r.Context()
	ref := srv.ugc.fs.Collection(postsCollection).Doc(postID)
	snap, err := ref.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			writeJSON(w, 404, map[string]string{"error": "post not found"})
			return
		}
		log.Printf("posts: read failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not read post"})
		return
	}
	var doc PostDoc
	if err := snap.DataTo(&doc); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read post"})
		return
	}
	if doc.UID != uid {
		// Don't reveal existence of someone else's post.
		writeJSON(w, 404, map[string]string{"error": "post not found"})
		return
	}
	if doc.Status == postStatusPublished {
		writeJSON(w, 200, doc)
		return
	}

	sizeCap := int64(maxPhotoBytes)
	if doc.Type == postTypeVideo {
		sizeCap = maxVideoBytes
	}
	for i := range doc.Media {
		obj := srv.ugc.bucket.Object(doc.Media[i].Object)
		attrs, err := obj.Attrs(ctx)
		if err != nil {
			if err == storage.ErrObjectNotExist {
				writeJSON(w, 400, map[string]string{"error": "upload incomplete: missing " + doc.Media[i].Object})
				return
			}
			log.Printf("posts: attrs failed: %v", err)
			writeJSON(w, 500, map[string]string{"error": "could not verify upload"})
			return
		}
		if attrs.Size > sizeCap {
			// Reject and clean up the oversized object to avoid orphan storage.
			_ = obj.Delete(ctx)
			writeJSON(w, 413, map[string]string{"error": fmt.Sprintf("%s exceeds the %d byte limit", doc.Media[i].Object, sizeCap)})
			return
		}
		doc.Media[i].Size = attrs.Size
	}

	now := time.Now().UTC()
	doc.Status = postStatusPublished
	doc.PublishedAt = &now
	if _, err := ref.Set(ctx, doc); err != nil {
		log.Printf("posts: publish failed: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not publish post"})
		return
	}
	writeJSON(w, 200, doc)
}

// listPosts returns the caller's own posts (newest first) with short-lived
// signed GET URLs for display. Filtering/sorting is done in memory to avoid a
// composite Firestore index at pilot scale.
func (srv *Server) listPosts(w http.ResponseWriter, r *http.Request, uid string) {
	ctx := r.Context()
	iter := srv.ugc.fs.Collection(postsCollection).Where("uid", "==", uid).Documents(ctx)
	docs, err := iter.GetAll()
	if err != nil {
		log.Printf("posts: list failed: %v", err)
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

	views := make([]postView, 0, len(posts))
	for _, p := range posts {
		urls := make([]string, 0, len(p.Media))
		for _, m := range p.Media {
			if url, err := srv.ugc.signedGetURL(m.Object); err == nil {
				urls = append(urls, url)
			}
		}
		views = append(views, postView{PostDoc: p, MediaURLs: urls})
	}
	writeJSON(w, 200, map[string]interface{}{"posts": views})
}
