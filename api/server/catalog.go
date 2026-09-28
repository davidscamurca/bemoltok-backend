package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
	host       string
	appKey     string
	token      string
	client     *http.Client
	imageMu    sync.RWMutex
	imageCache map[string]string
}

func newVtexCatalog() *vtexCatalog {
	key := strings.TrimSpace(os.Getenv("VTEX_APP_KEY"))
	tok := strings.TrimSpace(os.Getenv("VTEX_APP_TOKEN"))
	if key == "" || tok == "" {
		return nil
	}
	host := envOr("VTEX_HOST", "bemol.vtexcommercestable.com.br")
	return &vtexCatalog{
		host:       host,
		appKey:     key,
		token:      tok,
		client:     &http.Client{Timeout: 12 * time.Second},
		imageCache: make(map[string]string),
	}
}

// skuImageURL resolves the first product image for a refId via VTEX SKU API.
// Results are cached in-memory for the lifetime of the process.
func (v *vtexCatalog) skuImageURL(refID string) string {
	refID = strings.TrimSpace(refID)
	if refID == "" {
		return ""
	}
	v.imageMu.RLock()
	if img, ok := v.imageCache[refID]; ok {
		v.imageMu.RUnlock()
		return img
	}
	v.imageMu.RUnlock()

	path := fmt.Sprintf(
		"/api/catalog_system/pvt/sku/stockkeepingunitbyalternateId/%s",
		url.PathEscape(refID),
	)
	body, status, err := v.fetchGET(path, nil)
	img := ""
	if err == nil && status == http.StatusOK {
		img = parseSkuImageURL(body)
	}
	v.imageMu.Lock()
	v.imageCache[refID] = img
	v.imageMu.Unlock()
	return img
}

func parseSkuImageURL(body []byte) string {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"ImageUrl", "imageUrl", "SkuImageUrl"} {
		if s, ok := raw[key].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeVtexImageURL(s)
		}
	}
	for _, listKey := range []string{"Images", "images", "SkuImages", "skuImages"} {
		list, ok := raw[listKey].([]interface{})
		if !ok {
			continue
		}
		for _, entry := range list {
			m, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			for _, key := range []string{"ImageUrl", "imageUrl", "Url", "url"} {
				if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
					return normalizeVtexImageURL(s)
				}
			}
		}
	}
	return ""
}

func normalizeVtexImageURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") {
		return "https://" + strings.TrimPrefix(s, "http://")
	}
	if strings.HasPrefix(s, "https://") {
		return s
	}
	if strings.HasPrefix(s, "//") {
		return "https:" + s
	}
	if strings.HasPrefix(s, "/") {
		return "https://bemol.vteximg.com.br" + s
	}
	return s
}

func (v *vtexCatalog) fetchGET(path string, query url.Values) ([]byte, int, error) {
	u := url.URL{
		Scheme:   "https",
		Host:     v.host,
		Path:     path,
		RawQuery: query.Encode(),
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header = v.headers()
	res, err := v.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
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
	body, status, err := srv.vtex.fetchGET(path, nil)
	if err != nil {
		log.Printf("vtex sku %s: %v", refID, err)
		writeJSON(w, 502, map[string]string{"error": "vtex unreachable"})
		return
	}
	if status == http.StatusOK && skuPayloadComplete(body) {
		writeVtexBody(w, status, body)
		return
	}

	// O id do recomendador ora é RefId (alternate id), ora é o productId
	// interno da VTEX. O SKU privado só resolve o primeiro; o segundo
	// existe na pesquisa pública e traz foto + link da ficha.
	for _, fq := range []string{
		"alternateIds_RefId:" + refID,
		"productId:" + refID,
	} {
		q := url.Values{}
		q.Set("_from", "0")
		q.Set("_to", "4")
		q.Set("fq", fq)
		sbody, sstatus, serr := srv.vtex.fetchGET("/api/catalog_system/pub/products/search/", q)
		if serr != nil || (sstatus != http.StatusOK && sstatus != http.StatusPartialContent) {
			continue
		}
		merged, ok := synthesizeSKUFromSearch(sbody, refID)
		if !ok {
			continue
		}
		if status == http.StatusOK {
			merged = mergeSKUVisuals(body, merged)
		}
		writeVtexBody(w, http.StatusOK, merged)
		return
	}
	if status == 0 {
		status = http.StatusNotFound
	}
	writeVtexBody(w, status, body)
}

func writeVtexBody(w http.ResponseWriter, status int, body []byte) {
	if len(body) == 0 {
		body = []byte("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func skuPayloadComplete(body []byte) bool {
	return parseSkuImageURL(body) != "" && parseSkuDetailURL(body) != "" && parseSkuPrice(body) > 0
}

func parseSkuPrice(body []byte) float64 {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0
	}
	for _, key := range []string{"Sellers", "sellers"} {
		if sellers, ok := raw[key].([]interface{}); ok && len(sellers) > 0 {
			if first, ok := sellers[0].(map[string]interface{}); ok {
				for _, offKey := range []string{"CommertialOffer", "commertialOffer"} {
					if offer, ok := first[offKey].(map[string]interface{}); ok {
						for _, pKey := range []string{"Price", "price"} {
							if p, ok := offer[pKey].(float64); ok && p > 0 {
								return p
							}
						}
					}
				}
			}
		}
	}
	for _, offKey := range []string{"CommertialOffer", "commertialOffer"} {
		if offer, ok := raw[offKey].(map[string]interface{}); ok {
			for _, pKey := range []string{"Price", "price"} {
				if p, ok := offer[pKey].(float64); ok && p > 0 {
					return p
				}
			}
		}
	}
	return 0
}

func parseSkuDetailURL(body []byte) string {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"DetailUrl", "detailUrl"} {
		if s, ok := raw[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// synthesizeSKUFromSearch maps a VTEX /products/search hit into the private
// SKU shape the app already parses (ImageUrl, Images, DetailUrl, Sellers).
func synthesizeSKUFromSearch(body []byte, refID string) ([]byte, bool) {
	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil || len(list) == 0 {
		return nil, false
	}
	for _, product := range list {
		if !searchProductMatchesRef(product, refID) {
			continue
		}
		images := searchImageURLs(product)
		link, _ := product["link"].(string)
		name, _ := product["productName"].(string)
		ref, _ := product["productReference"].(string)
		if ref == "" {
			ref = refID
		}
		desc, _ := product["description"].(string)
		brand, _ := product["brand"].(string)
		pid := 0
		switch v := product["productId"].(type) {
		case string:
			fmt.Sscan(v, &pid)
		case float64:
			pid = int(v)
		}
		imgs := make([]map[string]string, 0, len(images))
		for _, u := range images {
			imgs = append(imgs, map[string]string{"ImageUrl": u})
		}
		root := ""
		if len(images) > 0 {
			root = images[0]
		}
		var sellers []interface{}
		items, _ := product["items"].([]interface{})
		if len(items) > 0 {
			if firstItem, ok := items[0].(map[string]interface{}); ok {
				if sList, ok := firstItem["sellers"].([]interface{}); ok {
					sellers = sList
				}
			}
		}
		out := map[string]interface{}{
			"ProductId":          pid,
			"NameComplete":       name,
			"ProductDescription": desc,
			"ProductRefId":       ref,
			"BrandName":          brand,
			"ImageUrl":           root,
			"Images":             imgs,
			"DetailUrl":          storePathFromLink(link),
			"ProductCategories":  map[string]string{},
			"Sellers":            sellers,
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, false
		}
		return encoded, root != "" || strings.TrimSpace(link) != ""
	}
	return nil, false
}

func searchProductMatchesRef(product map[string]interface{}, refID string) bool {
	if s, _ := product["productReference"].(string); s == refID {
		return true
	}
	if s, _ := product["productId"].(string); s == refID {
		return true
	}
	items, _ := product["items"].([]interface{})
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if s, _ := m["itemId"].(string); s == refID {
			return true
		}
		refs, _ := m["referenceId"].([]interface{})
		for _, r := range refs {
			rm, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			if v, _ := rm["Value"].(string); v == refID {
				return true
			}
		}
	}
	return false
}

func searchImageURLs(product map[string]interface{}) []string {
	items, _ := product["items"].([]interface{})
	if len(items) == 0 {
		return nil
	}
	first, ok := items[0].(map[string]interface{})
	if !ok {
		return nil
	}
	raw, _ := first["images"].([]interface{})
	out := make([]string, 0, len(raw))
	for _, img := range raw {
		m, ok := img.(map[string]interface{})
		if !ok {
			continue
		}
		u, _ := m["imageUrl"].(string)
		u = normalizeVtexImageURL(u)
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

func storePathFromLink(link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || u.Path == "" {
		return link
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// mergeSKUVisuals keeps the private SKU payload and fills image/link gaps
// from the public search synthesis.
func mergeSKUVisuals(skuBody, synthesized []byte) []byte {
	var sku map[string]interface{}
	var extra map[string]interface{}
	if err := json.Unmarshal(skuBody, &sku); err != nil {
		return synthesized
	}
	if err := json.Unmarshal(synthesized, &extra); err != nil {
		return skuBody
	}
	if parseSkuImageURL(skuBody) == "" {
		if v, ok := extra["ImageUrl"]; ok {
			sku["ImageUrl"] = v
		}
		if v, ok := extra["Images"]; ok {
			sku["Images"] = v
		}
	}
	if parseSkuDetailURL(skuBody) == "" {
		if v, ok := extra["DetailUrl"]; ok {
			sku["DetailUrl"] = v
		}
	}
	if parseSkuPrice(skuBody) == 0 {
		if v, ok := extra["Sellers"]; ok {
			sku["Sellers"] = v
		}
	}
	out, err := json.Marshal(sku)
	if err != nil {
		return skuBody
	}
	return out
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
	for key, vals := range q {
		if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
			continue
		}
		v := strings.TrimSpace(vals[0])
		// Compat: clientes antigos mandavam `alternateIds:123` (400 na VTEX).
		if key == "fq" && strings.HasPrefix(v, "alternateIds:") &&
			!strings.Contains(v, "alternateIds_") {
			v = "alternateIds_RefId:" + strings.TrimPrefix(v, "alternateIds:")
		}
		upstream.Set(key, v)
	}
	srv.vtex.proxyGET(w, "/api/catalog_system/pub/products/search/", upstream)
}
