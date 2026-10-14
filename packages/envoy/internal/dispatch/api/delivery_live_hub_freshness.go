package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// applyFreshness applies one delivery_freshness payload, the settings row's freshness columns as
// migration 0100's trigger writes them, reading nothing.
func (h *deliveryHub) applyFreshness(payload string) {
	var row struct {
		LastEventAt     *time.Time `json:"last_event_at"`
		LastReconcileAt *time.Time `json:"last_reconcile_at"`
		LastError       *string    `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		slog.Warn("dispatch: delivery live hub could not read a freshness notification", "error", err)
		return
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fresh.lastEventAt, h.fresh.lastReconcileAt, h.fresh.lastError = row.LastEventAt, row.LastReconcileAt, row.LastError
	h.judgeLocked(now)
	// The threshold moved with the reconcile time: the worker re-arms its timer.
	signal(h.wakeWork)
}

// verdictLocked is the freshness verdict as of now.
func (h *deliveryHub) verdictLocked(now time.Time) deliveryFreshnessVerdict {
	stale := []string{}
	if last := h.fresh.lastReconcileAt; last != nil && now.Sub(*last) > deliveryStaleAfter {
		stale = append(stale, deliveryReconciledSources...)
	}
	return deliveryFreshnessVerdict{lastError: h.fresh.lastError, stale: stale, unfetchable: h.fresh.unfetchable}
}

// judgeLocked sends every open page a freshness frame when the verdict changed since the last one,
// and nothing otherwise; nothing at all before the first recompute set it.
func (h *deliveryHub) judgeLocked(now time.Time) {
	if !h.known {
		return
	}
	verdict := h.verdictLocked(now)
	if verdict.unfetchable == h.sent.unfetchable && slices.Equal(verdict.stale, h.sent.stale) &&
		equalStringPointers(verdict.lastError, h.sent.lastError) {
		return
	}
	h.sent = verdict
	frame := DeliveryLiveFreshness{
		Type: "freshness",
		Freshness: delivery.DeliveryFreshnessView{
			LastEventAt: h.fresh.lastEventAt, LastReconcileAt: h.fresh.lastReconcileAt,
			LastError: h.fresh.lastError, UnfetchableCount: h.fresh.unfetchable,
		},
		Stale: verdict.stale,
	}
	for conn := range h.conns {
		conn.Send(frame)
	}
}
