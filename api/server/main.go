package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	ucbC             = float32(0.5)
	ucbGamma         = float32(0.2) // weight of UCB boost in final score
	ucbWarmThreshold = int64(50)    // products with >= 50 impressions are "warm"
	statsRefreshRate = 5 * time.Minute
	defaultK         = 10
	maxK             = 200

	maxRequestBody    = 1 << 20 // 1 MiB cap on any request body
	maxEventsPerBatch = 500     // max events accepted in a single POST /events
)

// ── Types ─────────────────────────────────────────────────────────────────────

type Meta struct {
	Dims        int     `json:"dims"`
	NClients    int     `json:"n_clients"`
	NProducts   int     `json:"n_products"`
	HybridAlpha float32 `json:"hybrid_alpha"`
}

// ProductMeta holds per-product metadata returned with recommendations.
// Supports both the legacy format (map[string]string) and the new format
// (map[string]ProductMeta) produced by export.py.
type ProductMeta struct {
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Price    *float64 `json:"price,omitempty"`
}

// ProductStats holds UCB1 accumulators for one product, sourced from the
// product_stats Firestore collection and refreshed every statsRefreshRate.
type ProductStats struct {
	Impressions     int64
	PositiveSignals float64
}

// Index is the in-memory recommendation index loaded from disk artifacts.
// Hot-swappable via POST /admin/reload.
type Index struct {
	ClientMap  map[string]int
	ProductMap map[string]int
	ClientEmb  []float32
	ProductEmb []float32
	ProductPop []float32 // normalized popularity from historical sales
	ClientIDs  []string
	ProductIDs []string
	Meta       map[string]ProductMeta
	Dims       int
	Alpha      float32
}

// Recommendation is one item in the ranked feed returned to the Flutter app.
type Recommendation struct {
	Rank       int      `json:"rank"`
	ProductID  string   `json:"product_id"`
	Name       string   `json:"name"`
	Score      float64  `json:"score"`
	Price      *float64 `json:"price,omitempty"`
	Category   string   `json:"category,omitempty"`
	IsNew      bool     `json:"is_new"`
	Propensity float64  `json:"propensity"`
}

type RecommendResponse struct {
	ClientID        string           `json:"client_id"`
	Recommendations []Recommendation `json:"recommendations"`
	LatencyMs       float64          `json:"latency_ms"`
}

type SimilarResponse struct {
	ProductID string           `json:"product_id"`
	Similar   []Recommendation `json:"similar"`
	LatencyMs float64          `json:"latency_ms"`
}

// HealthResponse is intentionally minimal for a public endpoint: it omits the
// client/product counts and directory size to avoid leaking the base size.
type HealthResponse struct {
	Status             string `json:"status"`
	StatsAge           string `json:"stats_age,omitempty"`
	FirestoreConnected bool   `json:"firestore_connected"`
}

// ── Event ingestion types ─────────────────────────────────────────────────────

type EventInput struct {
	EventID         string   `json:"event_id,omitempty"` // UUID from app; used for idempotency
	ProductID       string   `json:"product_id"`
	Category        string   `json:"category"`
	Kind            string   `json:"kind"`
	Value           float64  `json:"value"`
	At              string   `json:"at"`
	Name            string   `json:"name,omitempty"`
	Price           *float64 `json:"price,omitempty"`
	Rank            *int     `json:"rank,omitempty"`
	Propensity      *float64 `json:"propensity,omitempty"`
	InjectionSource string   `json:"injection_source,omitempty"`
	ExperimentID    string   `json:"experiment_id,omitempty"`
	Variant         string   `json:"variant,omitempty"`
	SessionID       string   `json:"session_id,omitempty"`
	ImpressionID    string   `json:"impression_id,omitempty"`
	WeightsVersion  string   `json:"weights_version,omitempty"`
}

type EventsBatch struct {
	Events []EventInput `json:"events"`
}

// ── Experiment types ──────────────────────────────────────────────────────────

type ExperimentResult struct {
	ExperimentID string                 `json:"experiment_id"`
	Variant      string                 `json:"variant"`
	Params       map[string]interface{} `json:"params"`
}

type ExperimentsResponse struct {
	Experiments []ExperimentResult `json:"experiments"`
}

// ── Server ────────────────────────────────────────────────────────────────────

type Server struct {
	// Hot-swappable index protected by idxMu.
	idxMu sync.RWMutex
	idx   *Index

	// UCB stats refreshed from Firestore every statsRefreshRate.
	// ucbScores is aligned 1:1 with idx.ProductIDs at the time of the last refresh.
	statsMu          sync.RWMutex
	productStats     map[string]ProductStats
	ucbScores        []float32
	totalImpressions int64
	statsUpdatedAt   time.Time

	artifactsDir string

	// Auth / identity (Phase 2).
	fb                 *FirebaseClients
	authRequired       bool   // when false, legacy X-User-Id is accepted as fallback
	allowedEmailDomain string // e.g. "bemol.com.br"; empty disables the check

	// adminToken guards /admin/reload. When empty the endpoint is disabled
	// (returns 503) so it is never publicly callable by accident.
	adminToken string

	// directory maps HMAC-SHA256(secret, email) → bemolClientId, loaded from a
	// hashed artifact. Enables automatic ID_CLIENTE linking at login. Empty when
	// the artifact is absent (users fall back to manual entry).
	directory           map[string]string
	directoryHMACSecret string

	// sendLink powers POST /auth/send-link (magic link via SendGrid). nil when
	// SENDGRID_API_KEY is unset, which disables the endpoint.
	sendLink *sendLinkService

	// ugc powers the /posts endpoints (user photos/video tied to a product).
	// nil when the UGC bucket or URL signer is unavailable, which disables /posts.
	ugc *ugcService

	// vtex proxies catalog SKU/search so AppKey/AppToken stay server-side.
	// nil when VTEX_APP_KEY / VTEX_APP_TOKEN are unset (catalog routes → 503).
	vtex *vtexCatalog

	// commentStoreOverride allows in-memory comment persistence in tests.
	// nil means use Firestore via srv.fb.
	commentStoreOverride commentStore

	// uid → bemolClientId cache to avoid a Firestore read per /recommend.
	clientIDMu    sync.RWMutex
	clientIDCache map[string]string
}

// ── Scored + min-heap ─────────────────────────────────────────────────────────

type scored struct {
	idx   int
	score float32
}

type topK struct {
	items []scored
	cap_  int
}

func newTopK(k int) topK {
	return topK{items: make([]scored, 0, k), cap_: k}
}

func (t *topK) push(s scored) {
	if len(t.items) < t.cap_ {
		t.items = append(t.items, s)
		i := len(t.items) - 1
		for i > 0 {
			p := (i - 1) / 2
			if t.items[p].score <= t.items[i].score {
				break
			}
			t.items[p], t.items[i] = t.items[i], t.items[p]
			i = p
		}
	} else if s.score > t.items[0].score {
		t.items[0] = s
		n := len(t.items)
		i := 0
		for {
			sm := i
			if l := 2*i + 1; l < n && t.items[l].score < t.items[sm].score {
				sm = l
			}
			if r := 2*i + 2; r < n && t.items[r].score < t.items[sm].score {
				sm = r
			}
			if sm == i {
				break
			}
			t.items[i], t.items[sm] = t.items[sm], t.items[i]
			i = sm
		}
	}
}

func (t *topK) sorted() []scored {
	res := make([]scored, len(t.items))
	copy(res, t.items)
	sort.Slice(res, func(a, b int) bool { return res[a].score > res[b].score })
	return res
}

// ── Loading ───────────────────────────────────────────────────────────────────

func loadEmbeddings(path string, n, dims int) ([]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	expected := n * dims * 4
	if len(data) != expected {
		return nil, fmt.Errorf("%s: expected %d bytes, got %d", path, expected, len(data))
	}
	embs := make([]float32, n*dims)
	dst := unsafe.Slice((*byte)(unsafe.Pointer(&embs[0])), len(data))
	copy(dst, data)
	return embs, nil
}

func loadIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		var intIDs []int64
		if err2 := json.Unmarshal(data, &intIDs); err2 != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		ids = make([]string, len(intIDs))
		for i, v := range intIDs {
			ids[i] = strconv.FormatInt(v, 10)
		}
	}
	return ids, nil
}

// loadProductMeta reads product_meta.json. Supports both formats:
//   - Legacy: map[string]string  → only name, no category/price
//   - New:    map[string]ProductMeta → name + category + price
func loadProductMeta(path string) (map[string]ProductMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Peek at the first value to detect format.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	result := make(map[string]ProductMeta, len(raw))
	for pid, v := range raw {
		var pm ProductMeta
		if err := json.Unmarshal(v, &pm); err == nil && (pm.Name != "" || pm.Category != "") {
			// New format: {"name":..., "category":..., "price":...}
			result[pid] = pm
		} else {
			// Legacy format: bare string
			var name string
			if err2 := json.Unmarshal(v, &name); err2 == nil {
				result[pid] = ProductMeta{Name: name}
			}
		}
	}
	return result, nil
}

func loadIndex(dir string) (*Index, error) {
	metaData, err := os.ReadFile(dir + "/meta.json")
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, err
	}

	clientIDs, err := loadIDs(dir + "/client_ids.json")
	if err != nil {
		return nil, err
	}
	productIDs, err := loadIDs(dir + "/product_ids.json")
	if err != nil {
		return nil, err
	}

	clientEmb, err := loadEmbeddings(dir+"/client_embeddings.bin", meta.NClients, meta.Dims)
	if err != nil {
		return nil, err
	}
	productEmb, err := loadEmbeddings(dir+"/product_embeddings.bin", meta.NProducts, meta.Dims)
	if err != nil {
		return nil, err
	}

	clientMap := make(map[string]int, len(clientIDs))
	for i, id := range clientIDs {
		clientMap[id] = i
	}
	productMap := make(map[string]int, len(productIDs))
	for i, id := range productIDs {
		productMap[id] = i
	}

	productMeta := make(map[string]ProductMeta)
	if pm, err := loadProductMeta(dir + "/product_meta.json"); err == nil {
		productMeta = pm
		log.Printf("Product meta loaded: %d products", len(productMeta))
	} else {
		log.Printf("Warning: could not load product_meta.json: %v", err)
	}

	productPop := make([]float32, meta.NProducts)
	popPath := dir + "/product_popularity.bin"
	if popData, err := os.ReadFile(popPath); err == nil {
		dst := unsafe.Slice((*byte)(unsafe.Pointer(&productPop[0])), meta.NProducts*4)
		copy(dst, popData)
		log.Printf("Product popularity loaded: %d products", len(productPop))
	} else {
		log.Printf("No product_popularity.bin found, using zeros")
	}

	alpha := meta.HybridAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = 0.7
	}

	log.Printf("Index loaded: %d clients, %d products, dims=%d, alpha=%.2f",
		meta.NClients, meta.NProducts, meta.Dims, alpha)

	return &Index{
		ClientMap:  clientMap,
		ProductMap: productMap,
		ClientEmb:  clientEmb,
		ProductEmb: productEmb,
		ProductPop: productPop,
		ClientIDs:  clientIDs,
		ProductIDs: productIDs,
		Meta:       productMeta,
		Dims:       meta.Dims,
		Alpha:      alpha,
	}, nil
}

// ── UCB1 ──────────────────────────────────────────────────────────────────────

// buildUCBScores computes a UCB1 exploration bonus for every product, aligned
// with the productIDs slice. Products with >= ucbWarmThreshold impressions get
// 0 (warm); unseen products get 1 (maximum exploration).
func buildUCBScores(productIDs []string, stats map[string]ProductStats, total int64) []float32 {
	scores := make([]float32, len(productIDs))
	logTotal := float32(math.Log(float64(total + 1)))
	for i, pid := range productIDs {
		s, ok := stats[pid]
		if !ok || s.Impressions == 0 {
			scores[i] = 1.0
			continue
		}
		if s.Impressions >= ucbWarmThreshold {
			scores[i] = 0.0
			continue
		}
		mean := float32(s.PositiveSignals / float64(s.Impressions))
		explore := ucbC * float32(math.Sqrt(float64(logTotal)/float64(s.Impressions)))
		scores[i] = mean + explore
	}
	return scores
}

// ── Stats refresh ─────────────────────────────────────────────────────────────

// refreshStats loads the product_stats counters from Firestore (maintained
// inline by handleEvents) and rebuilds the in-memory ucbScores slice.
// Safe to call concurrently.
func (srv *Server) refreshStats() error {
	if srv.fb == nil {
		return nil
	}

	stats, totalImpressions, err := srv.loadProductStats(context.Background())
	if err != nil {
		return fmt.Errorf("loading product_stats: %w", err)
	}

	srv.idxMu.RLock()
	productIDs := srv.idx.ProductIDs
	srv.idxMu.RUnlock()

	ucb := buildUCBScores(productIDs, stats, totalImpressions)

	srv.statsMu.Lock()
	srv.productStats = stats
	srv.ucbScores = ucb
	srv.totalImpressions = totalImpressions
	srv.statsUpdatedAt = time.Now()
	srv.statsMu.Unlock()

	log.Printf("Stats refreshed: %d products tracked, %d total impressions",
		len(stats), totalImpressions)
	return nil
}

func (srv *Server) startStatsRefresh(interval time.Duration) {
	go func() {
		if err := srv.refreshStats(); err != nil {
			log.Printf("Initial stats refresh failed: %v", err)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if err := srv.refreshStats(); err != nil {
				log.Printf("Stats refresh error: %v", err)
			}
		}
	}()
}

// ── Recommendation ────────────────────────────────────────────────────────────

var numWorkers = runtime.NumCPU()

// Recommend returns the top-k products for clientID, ranked by:
//
//	score = alpha·dot(client_emb, product_emb)
//	       + (1-alpha)·product_popularity
//	       + ucbGamma·ucb1_boost
//
// The UCB1 term connects real-time interaction signals to the ranking.
// Propensity is score[i] / sum(top-k scores), used for IPW in offline eval.
func (srv *Server) Recommend(clientID string, k int) ([]Recommendation, error) {
	srv.idxMu.RLock()
	idx := srv.idx
	srv.idxMu.RUnlock()

	ci, ok := idx.ClientMap[clientID]
	if !ok {
		return nil, fmt.Errorf("client not found: %s", clientID)
	}

	srv.statsMu.RLock()
	ucbScores := srv.ucbScores
	statsSnap := srv.productStats
	srv.statsMu.RUnlock()

	dims := idx.Dims
	query := idx.ClientEmb[ci*dims : (ci+1)*dims : (ci+1)*dims]
	nProducts := len(idx.ProductIDs)
	alpha := idx.Alpha
	beta := float32(1.0) - alpha
	prodEmb := idx.ProductEmb
	prodPop := idx.ProductPop

	if k > nProducts {
		k = nProducts
	}

	hasUCB := len(ucbScores) == nProducts

	workers := numWorkers
	chunkSize := (nProducts + workers - 1) / workers
	localHeaps := make([]topK, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			start := w * chunkSize
			end := start + chunkSize
			if end > nProducts {
				end = nProducts
			}
			h := newTopK(k)
			for i := start; i < end; i++ {
				off := i * dims
				row := prodEmb[off : off+dims : off+dims]

				// Unrolled dot product (8-wide, same as original).
				var s0, s1, s2, s3, s4, s5, s6, s7 float32
				j := 0
				for ; j+7 < dims; j += 8 {
					s0 += query[j] * row[j]
					s1 += query[j+1] * row[j+1]
					s2 += query[j+2] * row[j+2]
					s3 += query[j+3] * row[j+3]
					s4 += query[j+4] * row[j+4]
					s5 += query[j+5] * row[j+5]
					s6 += query[j+6] * row[j+6]
					s7 += query[j+7] * row[j+7]
				}
				dot := s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7
				for ; j < dims; j++ {
					dot += query[j] * row[j]
				}

				score := alpha*dot + beta*prodPop[i]
				if hasUCB {
					score += ucbGamma * ucbScores[i]
				}
				h.push(scored{idx: i, score: score})
			}
			localHeaps[w] = h
		}(w)
	}
	wg.Wait()

	merged := newTopK(k)
	for w := 0; w < workers; w++ {
		for _, s := range localHeaps[w].items {
			merged.push(s)
		}
	}

	top := merged.sorted()

	// Propensity: score[i] / sum(top-K scores).
	// Approximates P(item|policy) for the exposed items only.
	// Guaranteed > 0 because min score after softmax is positive.
	var scoreSum float64
	for _, s := range top {
		if float64(s.score) > 0 {
			scoreSum += float64(s.score)
		}
	}

	recs := make([]Recommendation, len(top))
	for i, s := range top {
		pid := idx.ProductIDs[s.idx]
		pm := idx.Meta[pid]

		var propensity float64
		if scoreSum > 0 {
			propensity = float64(s.score) / scoreSum
		}

		ps, hasStat := statsSnap[pid]
		isNew := !hasStat || ps.Impressions < ucbWarmThreshold

		recs[i] = Recommendation{
			Rank:       i + 1,
			ProductID:  pid,
			Name:       pm.Name,
			Score:      float64(s.score),
			Price:      pm.Price,
			Category:   pm.Category,
			IsNew:      isNew,
			Propensity: propensity,
		}
	}
	return recs, nil
}

// Similar returns the top-k products most similar to productID by embedding
// distance. Does not apply UCB or popularity — pure content similarity.
func (srv *Server) Similar(productID string, k int) ([]Recommendation, error) {
	srv.idxMu.RLock()
	idx := srv.idx
	srv.idxMu.RUnlock()

	pi, ok := idx.ProductMap[productID]
	if !ok {
		return nil, fmt.Errorf("product not found: %s", productID)
	}
	dims := idx.Dims
	query := idx.ProductEmb[pi*dims : (pi+1)*dims : (pi+1)*dims]
	prodEmb := idx.ProductEmb
	nProducts := len(idx.ProductIDs)
	kHeap := k + 1

	workers := numWorkers
	chunkSize := (nProducts + workers - 1) / workers
	localHeaps := make([]topK, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			start := w * chunkSize
			end := start + chunkSize
			if end > nProducts {
				end = nProducts
			}
			h := newTopK(kHeap)
			for i := start; i < end; i++ {
				off := i * dims
				row := prodEmb[off : off+dims : off+dims]

				var s0, s1, s2, s3, s4, s5, s6, s7 float32
				j := 0
				for ; j+7 < dims; j += 8 {
					s0 += query[j] * row[j]
					s1 += query[j+1] * row[j+1]
					s2 += query[j+2] * row[j+2]
					s3 += query[j+3] * row[j+3]
					s4 += query[j+4] * row[j+4]
					s5 += query[j+5] * row[j+5]
					s6 += query[j+6] * row[j+6]
					s7 += query[j+7] * row[j+7]
				}
				dot := s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7
				for ; j < dims; j++ {
					dot += query[j] * row[j]
				}
				h.push(scored{idx: i, score: dot})
			}
			localHeaps[w] = h
		}(w)
	}
	wg.Wait()

	merged := newTopK(kHeap)
	for w := 0; w < workers; w++ {
		for _, s := range localHeaps[w].items {
			merged.push(s)
		}
	}

	srv.statsMu.RLock()
	statsSnap := srv.productStats
	srv.statsMu.RUnlock()

	top := merged.sorted()
	recs := make([]Recommendation, 0, k)
	for _, s := range top {
		if idx.ProductIDs[s.idx] == productID {
			continue
		}
		pid := idx.ProductIDs[s.idx]
		pm := idx.Meta[pid]

		ps, hasStat := statsSnap[pid]
		isNew := !hasStat || ps.Impressions < ucbWarmThreshold

		recs = append(recs, Recommendation{
			Rank:      len(recs) + 1,
			ProductID: pid,
			Name:      pm.Name,
			Score:     float64(s.score),
			Price:     pm.Price,
			Category:  pm.Category,
			IsNew:     isNew,
		})
		if len(recs) >= k {
			break
		}
	}
	return recs, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeJSONBody caps the request body at maxRequestBody and decodes it into dst.
// On failure it writes the error response (413 when the body exceeded the cap,
// 400 otherwise) and returns false, so callers just `if !decodeJSONBody(...) { return }`.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + err.Error()})
		}
		return false
	}
	return true
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (srv *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	srv.statsMu.RLock()
	updatedAt := srv.statsUpdatedAt
	srv.statsMu.RUnlock()

	var statsAge string
	if !updatedAt.IsZero() {
		statsAge = time.Since(updatedAt).Round(time.Second).String()
	}

	writeJSON(w, 200, HealthResponse{
		Status:             "ok",
		StatsAge:           statsAge,
		FirestoreConnected: srv.fb != nil,
	})
}

func (srv *Server) handleRecommend(w http.ResponseWriter, r *http.Request) {
	// Two shapes are supported during the auth rollout:
	//   - Legacy: GET /recommend/{client_id}  (client_id in the path)
	//   - New:    GET /recommend               (client_id resolved from the token)
	clientID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/recommend"), "/")
	if clientID == "" {
		uid, ok := uidFromContext(r.Context())
		if !ok {
			writeJSON(w, 401, map[string]string{"error": "missing client_id or auth token"})
			return
		}
		cid, err := srv.resolveClientID(r.Context(), uid)
		if err != nil {
			if err == errNoClientLink {
				writeJSON(w, 409, map[string]string{"error": "no ID_CLIENTE linked; call POST /me/client-id first"})
				return
			}
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		clientID = cid
	}
	k := defaultK
	if qs := r.URL.Query().Get("k"); qs != "" {
		if v, err := strconv.Atoi(qs); err == nil && v > 0 && v <= maxK {
			k = v
		}
	}

	t := time.Now()
	recs, err := srv.Recommend(clientID, k)
	latency := float64(time.Since(t).Microseconds()) / 1000.0

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, RecommendResponse{
		ClientID:        clientID,
		Recommendations: recs,
		LatencyMs:       latency,
	})
}

func (srv *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	productID := strings.TrimPrefix(r.URL.Path, "/similar/")
	if productID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing product_id"})
		return
	}
	k := defaultK
	if qs := r.URL.Query().Get("k"); qs != "" {
		if v, err := strconv.Atoi(qs); err == nil && v > 0 && v <= maxK {
			k = v
		}
	}

	t := time.Now()
	sims, err := srv.Similar(productID, k)
	latency := float64(time.Since(t).Microseconds()) / 1000.0

	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, SimilarResponse{
		ProductID: productID,
		Similar:   sims,
		LatencyMs: latency,
	})
}

// handleEvents receives a batch of interaction events from the Flutter app and
// persists them to Firestore (events/{eventId}). The identity (uid) comes from
// the verified Firebase token, or the legacy X-User-Id header while
// AUTH_REQUIRED=false — never from the body.
//
// Writes are idempotent (doc id = app eventId or hash of uid+product+kind+at):
// duplicates are skipped and do not double-count the UCB1 counters.
func (srv *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	userID, ok := srv.identify(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "missing identity (Bearer token or X-User-Id)"})
		return
	}
	if srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "event store not available"})
		return
	}

	var batch EventsBatch
	if !decodeJSONBody(w, r, &batch) {
		return
	}
	if len(batch.Events) > maxEventsPerBatch {
		writeJSON(w, 400, map[string]string{"error": "too many events in batch"})
		return
	}
	if len(batch.Events) == 0 {
		writeJSON(w, 200, map[string]int{"inserted": 0})
		return
	}

	clientVersion := r.Header.Get("X-Client-Version")
	ctx := r.Context()

	inserted := 0
	for _, ev := range batch.Events {
		if ev.ProductID == "" || ev.Kind == "" {
			continue
		}
		created, err := srv.writeEvent(ctx, userID, ev, clientVersion)
		if err != nil {
			log.Printf("event write error: %v", err)
			continue
		}
		if !created {
			continue // duplicate; already counted
		}
		inserted++
		if err := srv.incrementProductStats(ctx, ev.ProductID, ev.Kind, ev.Value); err != nil {
			log.Printf("product_stats increment error (product=%s): %v", ev.ProductID, err)
		}
	}

	writeJSON(w, 200, map[string]int{"inserted": inserted})
}

// handleClickout records a click-out — the strongest conversion signal (user
// tapped to buy / open the product). It is sugar over a single click_out event.
func (srv *Server) handleClickout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	userID, ok := srv.identify(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "missing identity (Bearer token or X-User-Id)"})
		return
	}
	if srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "event store not available"})
		return
	}

	var ev EventInput
	if !decodeJSONBody(w, r, &ev) {
		return
	}
	if ev.ProductID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing product_id"})
		return
	}
	ev.Kind = "click_out"
	if ev.Value == 0 {
		ev.Value = 1.0
	}
	if ev.At == "" {
		ev.At = time.Now().UTC().Format(time.RFC3339)
	}

	ctx := r.Context()
	created, err := srv.writeEvent(ctx, userID, ev, r.Header.Get("X-Client-Version"))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "clickout write: " + err.Error()})
		return
	}
	if created {
		if err := srv.incrementProductStats(ctx, ev.ProductID, ev.Kind, ev.Value); err != nil {
			log.Printf("clickout stats increment error: %v", err)
		}
	}
	writeJSON(w, 200, map[string]interface{}{"status": "ok", "recorded": created})
}

// handleTrends returns the top-k trending products (highest positive signals).
func (srv *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
	if srv.fb == nil {
		writeJSON(w, 200, map[string]interface{}{"trends": []TrendItem{}})
		return
	}
	k := defaultK
	if qs := r.URL.Query().Get("k"); qs != "" {
		if v, err := strconv.Atoi(qs); err == nil && v > 0 && v <= maxK {
			k = v
		}
	}
	items, err := srv.topTrends(r.Context(), k)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "trends query: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"trends": items})
}

// handleExperiments returns the A/B test variant and params for each active
// experiment the given user belongs to. Variant assignment is deterministic:
//
//	bucket = SHA-256(user_id + ":" + experiment_id)[0:4] / 0xFFFFFFFF
//
// If bucket ∈ [bucket_from, bucket_to) → user is in that variant.
func (srv *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	userID, ok := srv.identify(r)
	if !ok && !srv.authRequired {
		// Legacy callers passed the identity as a query param.
		userID = r.URL.Query().Get("user_id")
		ok = userID != ""
	}
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "missing identity (Bearer token or user_id)"})
		return
	}
	if srv.fb == nil {
		writeJSON(w, 200, ExperimentsResponse{Experiments: []ExperimentResult{}})
		return
	}

	experiments, err := srv.loadActiveExperiments(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "experiments query: " + err.Error()})
		return
	}

	results := make([]ExperimentResult, 0, len(experiments))
	for expID, exp := range experiments {
		bucket := userBucket(userID, expID)
		for _, v := range exp.Variants {
			if bucket >= v.BucketFrom && bucket < v.BucketTo {
				results = append(results, ExperimentResult{
					ExperimentID: expID,
					Variant:      v.Variant,
					Params:       v.Params,
				})
				break
			}
		}
	}
	writeJSON(w, 200, ExperimentsResponse{Experiments: results})
}

// userBucket deterministically maps (user_id, experiment_id) → [0, 1).
func userBucket(userID, experimentID string) float64 {
	h := sha256.Sum256([]byte(userID + ":" + experimentID))
	n := binary.BigEndian.Uint32(h[:4])
	return float64(n) / float64(0xFFFFFFFF)
}

// ── Identity (Phase 2) ────────────────────────────────────────────────────────

// handleMe returns (creating on first call) the authenticated user's profile.
// Requires a valid Firebase ID token — there is no legacy fallback here.
func (srv *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "auth not configured"})
		return
	}
	uid, ok := uidFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	profile, err := srv.getOrCreateUser(r.Context(), uid, emailFromContext(r.Context()))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "profile error: " + err.Error()})
		return
	}
	// Automatic ID_CLIENTE linking from the email directory. Only fires when the
	// user has no link yet; on a miss the app keeps the manual-entry flow.
	if profile.BemolClientID == "" {
		if linked, ok := srv.tryAutoLink(r.Context(), uid, profile.Email); ok {
			profile = linked
		}
	}
	writeJSON(w, 200, profile)
}

type clientIDRequest struct {
	BemolClientID string `json:"bemol_client_id"`
}

// handleSetClientID links a Bemol ID_CLIENTE to the authenticated user.
// The id is validated against the embedding index: an id without an embedding
// would yield no recommendations, so it is rejected with a helpful error.
// Self-declared links are stored with clientIdVerified=false; the future
// ID_CLIENTE:EMAIL directory will set it to true automatically.
func (srv *Server) handleSetClientID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if srv.fb == nil {
		writeJSON(w, 503, map[string]string{"error": "auth not configured"})
		return
	}
	uid, ok := uidFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}

	var req clientIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	clientID := strings.TrimSpace(req.BemolClientID)
	if clientID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing bemol_client_id"})
		return
	}

	srv.idxMu.RLock()
	_, exists := srv.idx.ClientMap[clientID]
	srv.idxMu.RUnlock()
	if !exists {
		writeJSON(w, 422, map[string]string{"error": "ID_CLIENTE not found in recommendation index"})
		return
	}

	profile, err := srv.setClientID(r.Context(), uid, clientID, false)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "link error: " + err.Error()})
		return
	}
	writeJSON(w, 200, profile)
}

// handleAdminReload hot-swaps the embedding index without restarting the server.
// After loading, UCB scores are recomputed against the new product list using
// the current in-memory stats.
func (srv *Server) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	// Guard: the endpoint is disabled unless ADMIN_TOKEN is configured, and the
	// caller must present it via X-Admin-Token. Constant-time compare avoids
	// leaking the token via timing.
	if srv.adminToken == "" {
		writeJSON(w, 503, map[string]string{"error": "admin endpoint disabled"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(srv.adminToken)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	log.Printf("Admin reload requested")
	t0 := time.Now()

	newIdx, err := loadIndex(srv.artifactsDir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "reload failed: " + err.Error()})
		return
	}

	// Rebuild UCB slice aligned with the new product list.
	srv.statsMu.RLock()
	stats := srv.productStats
	total := srv.totalImpressions
	srv.statsMu.RUnlock()

	newUCB := buildUCBScores(newIdx.ProductIDs, stats, total)

	srv.idxMu.Lock()
	srv.idx = newIdx
	srv.idxMu.Unlock()

	srv.statsMu.Lock()
	srv.ucbScores = newUCB
	srv.statsMu.Unlock()

	elapsed := time.Since(t0)
	log.Printf("Admin reload complete in %v: %d clients, %d products",
		elapsed, len(newIdx.ClientIDs), len(newIdx.ProductIDs))

	writeJSON(w, 200, map[string]string{
		"status":   "ok",
		"elapsed":  elapsed.String(),
		"clients":  strconv.Itoa(len(newIdx.ClientIDs)),
		"products": strconv.Itoa(len(newIdx.ProductIDs)),
	})
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	artifactsDir := os.Getenv("ARTIFACTS_DIR")
	if artifactsDir == "" {
		artifactsDir = "/data/artifacts"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Loading artifacts from %s ...", artifactsDir)
	t0 := time.Now()
	idx, err := loadIndex(artifactsDir)
	if err != nil {
		log.Fatalf("Failed to load index: %v", err)
	}
	log.Printf("Loaded in %.1fs", time.Since(t0).Seconds())

	srv := &Server{
		idx:                idx,
		artifactsDir:       artifactsDir,
		productStats:       make(map[string]ProductStats),
		clientIDCache:      make(map[string]string),
		authRequired:        os.Getenv("AUTH_REQUIRED") == "true",
		allowedEmailDomain:  os.Getenv("ALLOWED_EMAIL_DOMAIN"),
		adminToken:          os.Getenv("ADMIN_TOKEN"),
		directoryHMACSecret: os.Getenv("DIRECTORY_HMAC_SECRET"),
	}

	// Email→ID_CLIENTE directory (hashed artifact). Optional: absence just
	// disables auto-link. Path defaults to <artifacts>/email_cli.hashed.csv.
	dirPath := os.Getenv("DIRECTORY_FILE")
	if dirPath == "" {
		dirPath = artifactsDir + "/email_cli.hashed.csv"
	}
	if dir, err := loadDirectory(dirPath); err != nil {
		log.Printf("WARNING: email directory not loaded (%v) — auto-link disabled", err)
	} else {
		srv.directory = dir
		log.Printf("Email directory loaded: %d entries from %s", len(dir), dirPath)
	}

	// Magic-link sender (SendGrid). Optional: nil disables POST /auth/send-link.
	if srv.sendLink = newSendLinkService(); srv.sendLink != nil {
		log.Printf("Magic-link sender enabled (from=%s)", srv.sendLink.from)
	} else {
		log.Printf("Magic-link sender disabled (SENDGRID_API_KEY unset)")
	}

	// VTEX catalog proxy. Optional: nil disables /catalog/* (503).
	if srv.vtex = newVtexCatalog(); srv.vtex != nil {
		log.Printf("VTEX catalog proxy enabled (host=%s)", srv.vtex.host)
	} else {
		log.Printf("VTEX catalog proxy disabled (VTEX_APP_KEY/TOKEN unset)")
	}

	// Firebase (Auth + Firestore). Optional in degraded/local mode: when it
	// cannot be initialized the server still serves recommendations, but /me
	// and token verification are unavailable. If AUTH_REQUIRED=true the absence
	// of Firebase is fatal, since no request could authenticate.
	if fb, err := initFirebase(context.Background()); err != nil {
		if srv.authRequired {
			log.Fatalf("Firebase init failed and AUTH_REQUIRED=true: %v", err)
		}
		log.Printf("WARNING: Firebase unavailable (%v) — auth, events and UCB disabled", err)
	} else {
		srv.fb = fb
		log.Printf("Firebase initialized (auth_required=%v, allowed_domain=%q)",
			srv.authRequired, srv.allowedEmailDomain)
		// UCB1 stats are sourced from Firestore (product_stats), maintained
		// inline by handleEvents/handleClickout.
		srv.startStatsRefresh(statsRefreshRate)

		// UGC posts (photos/video). Optional: disabled if the bucket or the URL
		// signer SA cannot be resolved. Signing uses IAM signBlob (no key file),
		// so the runtime SA needs roles/iam.serviceAccountTokenCreator on itself.
		// UGC_SIGNER_SA must be the runtime SA email (used as the V4 GoogleAccessID).
		bucket := envOr("UGC_BUCKET", "bemoltok-dev-ugc")
		signerSA := os.Getenv("UGC_SIGNER_SA")
		if signerSA == "" {
			log.Printf("WARNING: UGC posts disabled (set UGC_SIGNER_SA to the runtime SA email)")
		} else if ugc, uerr := newUGCService(context.Background(), fb.Firestore, bucket, signerSA); uerr != nil {
			log.Printf("WARNING: UGC posts disabled (%v)", uerr)
		} else {
			srv.ugc = ugc
			log.Printf("UGC posts enabled (bucket=%s, signer=%s)", bucket, signerSA)
		}
	}

	mux := http.NewServeMux()
	// Public.
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/similar/", srv.handleSimilar)
	mux.HandleFunc("/trends", srv.handleTrends)
	mux.HandleFunc("/auth/send-link", srv.handleSendLink)
	mux.HandleFunc("/auth/request-code", srv.handleRequestCode)
	mux.HandleFunc("/auth/verify-code", srv.handleVerifyCode)
	mux.HandleFunc("/admin/reload", srv.handleAdminReload)

	// Identity-aware routes. authMiddleware verifies a Bearer token when present
	// and, while authRequired=false, lets legacy (X-User-Id / path) requests pass.
	mux.HandleFunc("/feed", srv.authMiddleware(srv.handleFeed))
	mux.HandleFunc("/recommend/", srv.authMiddleware(srv.handleRecommend)) // legacy /recommend/{id}
	mux.HandleFunc("/recommend", srv.authMiddleware(srv.handleRecommend))  // new token flow
	mux.HandleFunc("/events", srv.authMiddleware(srv.handleEvents))
	mux.HandleFunc("/clickout", srv.authMiddleware(srv.handleClickout))
	mux.HandleFunc("/experiments", srv.authMiddleware(srv.handleExperiments))

	// Auth-only.
	mux.HandleFunc("/me", srv.authMiddleware(srv.handleMe))
	mux.HandleFunc("/me/client-id", srv.authMiddleware(srv.handleSetClientID))
	mux.HandleFunc("/me/likes", srv.authMiddleware(srv.handleMeLikes))
	mux.HandleFunc("/me/likes/", srv.authMiddleware(srv.handleMeLikes))
	mux.HandleFunc("/me/bookmarks", srv.authMiddleware(srv.handleMeBookmarks))
	mux.HandleFunc("/me/bookmarks/", srv.authMiddleware(srv.handleMeBookmarks))
	mux.HandleFunc("/products/", srv.authMiddleware(srv.handleProductComments))

	// UGC posts (auth-only). /posts: POST creates + GET lists own; the subpath
	// handles POST /posts/{id}/complete.
	mux.HandleFunc("/posts", srv.authMiddleware(srv.handlePosts))
	mux.HandleFunc("/posts/", srv.authMiddleware(srv.handlePostsSub))

	// VTEX catalog proxy (auth-only). Keys stay on the BFF; the app uses Bearer.
	mux.HandleFunc("/catalog/sku/", srv.authMiddleware(srv.handleCatalogSKU))
	mux.HandleFunc("/catalog/search", srv.authMiddleware(srv.handleCatalogSearch))

	// Public user profiles (auth-only).
	mux.HandleFunc("/users/", srv.authMiddleware(srv.handleUsersSub))

	log.Printf("Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
