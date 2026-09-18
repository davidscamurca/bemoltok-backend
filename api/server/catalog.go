package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// vtexCatalog proxies Bemol's VTEX Catalog API so AppKey/AppToken never ship
// in the mobile binary. The Flutter app calls these Bearer-protected routes
// and the BFF forwards to VTEX with server-side credentials.
//
// Env:
//
//	VTEX_APP_KEY    — X-VTEX-API-AppKey (required to enable)
//	VTEX_APP_TOKEN  — X-VTEX-API-AppToken (required to enable)
//	VTEX_HOST       — default bemol.vtexcommercestable.com.br
type vtexCatalog struct {
	host   string
	appKey string
	token  string
	client *http.Client
}

func newVtexCatalog() *vtexCatalog {
	key := strings.TrimSpace(os.Getenv("VTEX_APP_KEY"))
	tok := strings.TrimSpace(os.Getenv("VTEX_APP_TOKEN"))
	if key == "" || tok == "" {
		return nil
	}
	host := envOr("VTEX_HOST", "bemol.vtexcommercestable.com.br")
	return &vtexCatalog{
		host:   host,
		appKey: key,
		token:  tok,
		client: &http.Client{Timeout: 12 * time.Second},
	}
}

func (v *vtexCatalog) headers() http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("X-VTEX-API-AppKey", v.appKey)
	h.Set("X-VTEX-API-AppToken", v.token)
	return h
}

func (v *vtexCatalog) proxyGET(w http.ResponseWriter, path string, query url.Values) {
	u := url.URL{
		Scheme:   "https",
		Host:     v.host,
		Path:     path,
		RawQuery: query.Encode(),
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "bad upstream request"})
		return
	}
	req.Header = v.headers()

	res, err := v.client.Do(req)
	if err != nil {
		log.Printf("vtex proxy: %v", err)
		writeJSON(w, 502, map[string]string{"error": "vtex unreachable"})
		return
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20)) // 4 MiB
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "vtex read failed"})
		return
	}

	// Pass through status + JSON body (200/206/404 are meaningful to the app).
	ct := res.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(body)
}

// handleCatalogSKU serves GET /catalog/sku/{refId}
// → VTEX pvt stockkeepingunitbyalternateId/{refId}
func (srv *Server) handleCatalogSKU(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if srv.vtex == nil {
		writeJSON(w, 503, map[string]string{"error": "catalog not configured"})
		return
	}
	refID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/catalog/sku/"), "/")
	if refID == "" || strings.Contains(refID, "/") {
		writeJSON(w, 400, map[string]string{"error": "missing refId"})
		return
	}
	path := fmt.Sprintf("/api/catalog_system/pvt/sku/stockkeepingunitbyalternateId/%s", url.PathEscape(refID))
	srv.vtex.proxyGET(w, path, nil)
}

// handleCatalogSearch serves GET /catalog/search?_from=&_to=&O=
// → VTEX pub products/search (credentials still applied server-side).
func (srv *Server) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if srv.vtex == nil {
		writeJSON(w, 503, map[string]string{"error": "catalog not configured"})
		return
	}
	q := r.URL.Query()
	upstream := url.Values{}
	if v := q.Get("_from"); v != "" {
		upstream.Set("_from", v)
	}
	if v := q.Get("_to"); v != "" {
		upstream.Set("_to", v)
	}
	if v := q.Get("O"); v != "" {
		upstream.Set("O", v)
	}
	srv.vtex.proxyGET(w, "/api/catalog_system/pub/products/search/", upstream)
}
