package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Firestore collections (Phase 3).
const (
	eventsCollection       = "events"        // durable interaction log
	productStatsCollection = "product_stats"  // aggregate counters for UCB1
	experimentsCollection  = "experiments"    // A/B test config
)

// positiveKinds are the interaction kinds that count as a positive signal for
// UCB1 (matches the previous Postgres aggregation).
var positiveKinds = map[string]bool{
	"like": true, "bookmark": true, "share": true, "click_out": true, "comment": true,
}

// eventDoc is one interaction persisted to events/{eventId}.
type eventDoc struct {
	UID             string    `firestore:"uid"`
	ProductID       string    `firestore:"product_id"`
	Category        string    `firestore:"category"`
	Kind            string    `firestore:"kind"`
	Value           float64   `firestore:"value"`
	At              string    `firestore:"at"`
	Name            string    `firestore:"name,omitempty"`
	Price           *float64  `firestore:"price,omitempty"`
	Rank            *int      `firestore:"rank,omitempty"`
	Propensity      *float64  `firestore:"propensity,omitempty"`
	InjectionSource string    `firestore:"injection_source,omitempty"`
	ExperimentID    string    `firestore:"experiment_id,omitempty"`
	Variant         string    `firestore:"variant,omitempty"`
	SessionID       string    `firestore:"session_id,omitempty"`
	ImpressionID    string    `firestore:"impression_id,omitempty"`
	WeightsVersion  string    `firestore:"weights_version,omitempty"`
	ClientVersion   string    `firestore:"client_version,omitempty"`
	ReceivedAt      time.Time `firestore:"received_at,serverTimestamp"`
}

// eventID returns the document id used for idempotency: the app-provided
// eventId (UUID) when present, otherwise a deterministic hash of
// (uid, product_id, kind, at) — so retries never double-count.
func eventID(uid string, ev EventInput) string {
	if ev.EventID != "" {
		return ev.EventID
	}
	sum := sha256.Sum256([]byte(uid + "|" + ev.ProductID + "|" + ev.Kind + "|" + ev.At))
	return hex.EncodeToString(sum[:16])
}

// writeEvent stores one event idempotently. Returns created=false (no error)
// when the event already existed, so the caller skips counter increments.
func (srv *Server) writeEvent(ctx context.Context, uid string, ev EventInput, clientVersion string) (bool, error) {
	doc := srv.fb.Firestore.Collection(eventsCollection).Doc(eventID(uid, ev))
	_, err := doc.Create(ctx, eventDoc{
		UID:             uid,
		ProductID:       ev.ProductID,
		Category:        ev.Category,
		Kind:            ev.Kind,
		Value:           ev.Value,
		At:              ev.At,
		Name:            ev.Name,
		Price:           ev.Price,
		Rank:            ev.Rank,
		Propensity:      ev.Propensity,
		InjectionSource: ev.InjectionSource,
		ExperimentID:    ev.ExperimentID,
		Variant:         ev.Variant,
		SessionID:       ev.SessionID,
		ImpressionID:    ev.ImpressionID,
		WeightsVersion:  ev.WeightsVersion,
		ClientVersion:   clientVersion,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// incrementProductStats maintains the aggregate counters that feed UCB1, using
// atomic FieldValue increments. 'view' bumps impressions; positive kinds add to
// positive_signals (weighted by the event value).
func (srv *Server) incrementProductStats(ctx context.Context, productID, kind string, value float64) error {
	data := map[string]interface{}{"updated_at": time.Now()}
	if kind == "view" {
		data["impressions"] = firestore.Increment(int64(1))
	}
	if positiveKinds[kind] {
		data["positive_signals"] = firestore.Increment(value)
	}
	if len(data) == 1 {
		return nil // nothing countable in this event
	}
	doc := srv.fb.Firestore.Collection(productStatsCollection).Doc(productID)
	_, err := doc.Set(ctx, data, firestore.MergeAll)
	return err
}

// loadProductStats reads the whole product_stats collection into memory. Only
// products with activity have a document, so this stays small early on.
func (srv *Server) loadProductStats(ctx context.Context) (map[string]ProductStats, int64, error) {
	stats := make(map[string]ProductStats)
	var total int64

	iter := srv.fb.Firestore.Collection(productStatsCollection).Documents(ctx)
	defer iter.Stop()
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		var row struct {
			Impressions     int64   `firestore:"impressions"`
			PositiveSignals float64 `firestore:"positive_signals"`
		}
		if err := d.DataTo(&row); err != nil {
			continue
		}
		stats[d.Ref.ID] = ProductStats{
			Impressions:     row.Impressions,
			PositiveSignals: row.PositiveSignals,
		}
		total += row.Impressions
	}
	return stats, total, nil
}

// ── Experiments (Firestore) ────────────────────────────────────────────────

type fsExpVariant struct {
	Variant    string                 `firestore:"variant"`
	BucketFrom float64                `firestore:"bucket_from"`
	BucketTo   float64                `firestore:"bucket_to"`
	Params     map[string]interface{} `firestore:"params"`
}

type fsExperiment struct {
	Active   bool           `firestore:"active"`
	Variants []fsExpVariant `firestore:"variants"`
}

// loadActiveExperiments returns active experiments keyed by experiment id.
func (srv *Server) loadActiveExperiments(ctx context.Context) (map[string]fsExperiment, error) {
	out := make(map[string]fsExperiment)
	iter := srv.fb.Firestore.Collection(experimentsCollection).
		Where("active", "==", true).Documents(ctx)
	defer iter.Stop()
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var exp fsExperiment
		if err := d.DataTo(&exp); err != nil {
			continue
		}
		out[d.Ref.ID] = exp
	}
	return out, nil
}

// ── Trends (Firestore) ─────────────────────────────────────────────────────

// TrendItem is one product in the trending list.
type TrendItem struct {
	Rank            int     `json:"rank"`
	ProductID       string  `json:"product_id"`
	Name            string  `json:"name"`
	PositiveSignals float64 `json:"positive_signals"`
}

// topTrends returns the k products with the highest positive_signals. This is a
// simple "trending now" proxy; time-decayed windows arrive with Redis later.
func (srv *Server) topTrends(ctx context.Context, k int) ([]TrendItem, error) {
	iter := srv.fb.Firestore.Collection(productStatsCollection).
		OrderBy("positive_signals", firestore.Desc).Limit(k).Documents(ctx)
	defer iter.Stop()

	srv.idxMu.RLock()
	meta := srv.idx.Meta
	srv.idxMu.RUnlock()

	var items []TrendItem
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var row struct {
			PositiveSignals float64 `firestore:"positive_signals"`
		}
		if err := d.DataTo(&row); err != nil {
			continue
		}
		pid := d.Ref.ID
		items = append(items, TrendItem{
			Rank:            len(items) + 1,
			ProductID:       pid,
			Name:            meta[pid].Name,
			PositiveSignals: row.PositiveSignals,
		})
	}
	return items, nil
}
