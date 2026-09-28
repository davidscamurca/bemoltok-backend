package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
)

const (
	feedDefaultLimit   = 40
	feedDefaultPostGap = 4 // 1 post per N products
	feedPoolSimilarK   = 5
	feedMaxPostsScan   = 200
)

type feedProductSnapshot struct {
	Name      string   `json:"name"`
	ImageURL  string   `json:"image_url,omitempty"`
	Price     *float64 `json:"price,omitempty"`
	ListPrice *float64 `json:"list_price,omitempty"`
	DetailURL string   `json:"detail_url,omitempty"`
}

type feedItem struct {
	Type string `json:"type"` // "product" | "post"

	ProductID  string   `json:"product_id,omitempty"`
	Rank       int      `json:"rank,omitempty"`
	Score      float64  `json:"score,omitempty"`
	Name       string   `json:"name,omitempty"`
	Price      *float64 `json:"price,omitempty"`
	Category   string   `json:"category,omitempty"`
	IsNew      bool     `json:"is_new,omitempty"`
	Propensity float64  `json:"propensity,omitempty"`

	PostID      string     `json:"post_id,omitempty"`
	AuthorUID   string     `json:"author_uid,omitempty"`
	AuthorLabel string     `json:"author_label,omitempty"`
	Caption     string     `json:"caption,omitempty"`
	PostType    string     `json:"post_type,omitempty"`
	MediaURLs   []string   `json:"media_urls,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Eligibility string     `json:"eligibility,omitempty"`

	ProductSnapshot feedProductSnapshot `json:"product_snapshot,omitempty"`
}

type feedResponse struct {
	ClientID   string     `json:"client_id"`
	Cursor     int        `json:"cursor"`
	NextCursor int        `json:"next_cursor"`
	HasMore    bool       `json:"has_more"`
	Items      []feedItem `json:"items"`
}

type eligiblePost struct {
	doc         PostDoc
	mediaURLs   []string
	eligibility string
	authorLabel string
	snapshot    feedProductSnapshot
}

// GET /feed?cursor=0&limit=40
func (srv *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	uid, ok := uidFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	clientID, err := srv.resolveClientID(r.Context(), uid)
	if err != nil {
		if err == errNoClientLink {
			writeJSON(w, 409, map[string]string{"error": "no ID_CLIENTE linked; call POST /me/client-id first"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	cursor := 0
	if v := strings.TrimSpace(r.URL.Query().Get("cursor")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cursor = n
		}
	}
	limit := feedDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxK {
			limit = n
		}
	}

	productK := cursor + limit + feedMaxPostsScan
	if productK > maxK {
		productK = maxK
	}
	recs, err := srv.Recommend(clientID, productK)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}

	poolSet := make(map[string]struct{}, len(recs))
	for _, rec := range recs {
		poolSet[rec.ProductID] = struct{}{}
	}
	similarSet := srv.similarToPool(poolSet, feedPoolSimilarK)

	eligible, err := srv.loadEligiblePosts(r.Context(), poolSet, similarSet)
	if err != nil {
		log.Printf("feed: load posts: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not build feed"})
		return
	}

	products := make([]feedItem, 0, len(recs))
	for _, rec := range recs {
		snap := srv.productSnapshot(rec.ProductID, rec.Name, rec.Price)
		if !productSnapshotUsable(snap) {
			continue
		}
		products = append(products, feedItem{
			Type:            "product",
			ProductID:       rec.ProductID,
			Rank:            rec.Rank,
			Score:           rec.Score,
			Name:            rec.Name,
			Price:           rec.Price,
			Category:        rec.Category,
			IsNew:           rec.IsNew,
			Propensity:      rec.Propensity,
			ProductSnapshot: snap,
		})
	}

	mixed := interleaveFeed(products, eligible, feedDefaultPostGap)
	total := len(mixed)
	if cursor >= total {
		writeJSON(w, 200, feedResponse{
			ClientID: clientID, Cursor: cursor, NextCursor: cursor,
			HasMore: false, Items: []feedItem{},
		})
		return
	}
	end := cursor + limit
	if end > total {
		end = total
	}
	writeJSON(w, 200, feedResponse{
		ClientID: clientID, Cursor: cursor, NextCursor: end,
		HasMore: end < total, Items: mixed[cursor:end],
	})
}

func (srv *Server) similarToPool(pool map[string]struct{}, k int) map[string]struct{} {
	out := make(map[string]struct{})
	n := 0
	for pid := range pool {
		if n >= 8 {
			break
		}
		sims, err := srv.Similar(pid, k)
		if err != nil {
			continue
		}
		for _, s := range sims {
			if _, inPool := pool[s.ProductID]; inPool {
				continue
			}
			out[s.ProductID] = struct{}{}
		}
		n++
	}
	return out
}

func (srv *Server) productSnapshot(productID, name string, price *float64) feedProductSnapshot {
	srv.idxMu.RLock()
	meta, ok := srv.idx.Meta[productID]
	srv.idxMu.RUnlock()
	displayName := name
	var p *float64
	if ok && meta.Name != "" {
		displayName = meta.Name
	}
	if ok && meta.Price != nil && *meta.Price > 0 {
		p = meta.Price
	}
	if price != nil && *price > 0 {
		p = price
	}
	detail := fmt.Sprintf("https://www.bemol.com.br/%s/p", strings.Trim(productID, "/"))
	snap := feedProductSnapshot{
		Name: displayName, Price: p, DetailURL: detail,
	}
	if srv.vtex != nil {
		if img := srv.vtex.skuImageURL(productID); img != "" {
			snap.ImageURL = img
		}
	}
	return snap
}

func productSnapshotUsable(s feedProductSnapshot) bool {
	return s.Name != "" && s.Price != nil && *s.Price > 0
}

func (srv *Server) loadEligiblePosts(ctx context.Context, pool, similar map[string]struct{}) ([]eligiblePost, error) {
	if srv.ugc == nil {
		return nil, nil
	}
	iter := srv.ugc.fs.Collection(postsCollection).
		Where("status", "==", postStatusPublished).
		Limit(feedMaxPostsScan).
		Documents(ctx)
	docs, err := iter.GetAll()
	if err != nil {
		return nil, err
	}
	authorLabels := srv.loadAuthorLabels(ctx, docs)
	out := make([]eligiblePost, 0)
	for _, d := range docs {
		var p PostDoc
		if err := d.DataTo(&p); err != nil {
			continue
		}
		eligibility := ""
		if _, ok := pool[p.ProductID]; ok {
			eligibility = "pool"
		} else if _, ok := similar[p.ProductID]; ok {
			eligibility = "similar"
		} else {
			continue
		}
		snap := srv.productSnapshot(p.ProductID, "", nil)
		if !productSnapshotUsable(snap) {
			continue
		}
		urls := make([]string, 0, len(p.Media))
		for _, m := range p.Media {
			if url, err := srv.ugc.signedGetURL(m.Object); err == nil {
				urls = append(urls, url)
			}
		}
		if len(urls) == 0 {
			continue
		}
		label := p.AuthorLabel
		if label == "" {
			label = authorLabels[p.UID]
		}
		if label == "" {
			label = "Utilizador"
		}
		out = append(out, eligiblePost{
			doc: p, mediaURLs: urls, eligibility: eligibility,
			authorLabel: label, snapshot: snap,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].eligibility != out[j].eligibility {
			return out[i].eligibility == "pool"
		}
		ti, tj := out[i].doc.PublishedAt, out[j].doc.PublishedAt
		if ti == nil && tj == nil {
			return out[i].doc.CreatedAt.After(out[j].doc.CreatedAt)
		}
		if ti == nil {
			return false
		}
		if tj == nil {
			return true
		}
		return ti.After(*tj)
	})
	return out, nil
}

func (srv *Server) loadAuthorLabels(ctx context.Context, docs []*firestore.DocumentSnapshot) map[string]string {
	out := make(map[string]string)
	if srv.fb == nil {
		return out
	}
	seen := make(map[string]struct{})
	for _, d := range docs {
		var p PostDoc
		if err := d.DataTo(&p); err != nil || p.UID == "" || p.AuthorLabel != "" {
			continue
		}
		seen[p.UID] = struct{}{}
	}
	for uid := range seen {
		snap, err := srv.fb.Firestore.Collection(usersCollection).Doc(uid).Get(ctx)
		if err != nil {
			continue
		}
		var u UserProfile
		if err := snap.DataTo(&u); err != nil {
			continue
		}
		out[uid] = authorDisplayLabel(u.Email)
	}
	return out
}

func interleaveFeed(products []feedItem, posts []eligiblePost, gap int) []feedItem {
	if gap < 1 {
		gap = feedDefaultPostGap
	}
	if len(products) == 0 {
		return nil
	}
	postIdx := 0
	out := make([]feedItem, 0, len(products)+len(posts))
	productsSincePost := 0
	var lastAuthor, lastPostProduct string

	for _, prod := range products {
		out = append(out, prod)
		productsSincePost++
		if postIdx >= len(posts) || productsSincePost < gap {
			continue
		}
		pick := -1
		for i := postIdx; i < len(posts); i++ {
			c := posts[i]
			if lastAuthor != "" && c.doc.UID == lastAuthor {
				continue
			}
			if lastPostProduct != "" && c.doc.ProductID == lastPostProduct {
				continue
			}
			pick = i
			break
		}
		if pick < 0 {
			pick = postIdx
		}
		if pick >= len(posts) {
			continue
		}
		chosen := posts[pick]
		posts[pick], posts[postIdx] = posts[postIdx], posts[pick]
		out = append(out, feedItem{
			Type: "post", PostID: chosen.doc.ID, AuthorUID: chosen.doc.UID,
			AuthorLabel: chosen.authorLabel, ProductID: chosen.doc.ProductID,
			Caption: chosen.doc.Caption, PostType: chosen.doc.Type,
			MediaURLs: chosen.mediaURLs, PublishedAt: chosen.doc.PublishedAt,
			Eligibility: chosen.eligibility, ProductSnapshot: chosen.snapshot,
		})
		lastAuthor = chosen.doc.UID
		lastPostProduct = chosen.doc.ProductID
		postIdx++
		productsSincePost = 0
	}
	return out
}
