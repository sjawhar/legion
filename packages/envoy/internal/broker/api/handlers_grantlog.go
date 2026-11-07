// handlers_grantlog.go: GET /v1/grant-log — every audit row of one closed period, oldest first,
// for the quarterly access review. Part of the routes a listed service calls with its own
// ServiceAccount token (accountAuth), and the only route the readsGrantLog power reaches: the
// review reads the broker's own record of who asked for what and who said yes, and changes
// nothing in it.
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store"
)

const (
	// grantLogLimit is both the page a caller that names no size gets and the largest page the
	// broker answers. A limit above the cap is held at it rather than refused, because a review
	// asking for more is asking for the whole period and pages through it either way, and a
	// refusal would only make it guess the number this constant already fixes. A thousand
	// events keeps one answer under about a mebibyte, which is small enough that the broker
	// holds it, marshals it and writes it without a page of its own becoming the expensive
	// part of the read.
	grantLogLimit = 1000
	// grantLogSettleTime is how far before now a period's end must be for the period to count as
	// closed. An audit id comes from a bigserial, which hands it out when the row is inserted
	// and not when its transaction commits, so a row with a lower id can become visible after
	// one with a higher id: a reader paging by id could pass that row's id before the row was
	// there, and never answer it at all. An hour is far longer than any transaction the broker
	// runs, so every row of a period that ended an hour ago has committed before the review
	// reads it, and the review's answer is the whole period rather than nearly all of it.
	grantLogSettleTime = time.Hour
)

// grantLogResponse is GET /v1/grant-log's answer: one page of the period's audit rows, and the
// cursor the next page resumes after.
type grantLogResponse struct {
	// The period's audit rows this page holds, oldest first (audit id order, which is write
	// order), each as store.GrantLogEvent's own JSON tags name it; an empty array when the page
	// holds none.
	Events []store.GrantLogEvent `json:"events"`
	// The id to send as ?after= for the next page: the last event's id, or the request's own
	// ?after= when this page is empty, so a caller that has reached the end of the period and
	// asks again reads the same empty page rather than starting the period over.
	NextAfter int64 `json:"next_after"`
}

// listGrantLog answers one page of the broker's grant log: ?since and ?until bound the period
// (RFC 3339, half-open), ?after resumes after an audit id and ?limit bounds the page. The caller
// has read the period whole when a page comes back shorter than its limit.
//
// It logs nothing per row and holds no secret. Nothing per row, because the broker already wrote
// one store.GrantLogMessage line for each decision as it made it, so a line here would say the
// same thing a second time, once per row of a whole quarter, and drown the log the review's own
// pipeline keeps. No secret, because a store.GrantLogEvent carries none to hold: every field of
// it is a name, a login, an id, a state or a decision, and the audit detail it passes through is
// written with no value in it.
func (s *server) listGrantLog(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	since, ok := grantLogBound(w, query.Get("since"), "since")
	if !ok {
		return
	}
	until, ok := grantLogBound(w, query.Get("until"), "until")
	if !ok {
		return
	}
	if until.After(time.Now().Add(-grantLogSettleTime)) {
		writeError(w, http.StatusBadRequest, "PERIOD_NOT_CLOSED",
			"until must be at least an hour before now, so that every row of the period has committed before the review reads it")
		return
	}
	after, ok := grantLogNumber(w, query.Get("after"), "after", 0)
	if !ok {
		return
	}
	limit, ok := grantLogNumber(w, query.Get("limit"), "limit", grantLogLimit)
	if !ok {
		return
	}
	// A caller naming zero names no page at all, which store.ReadGrantLog refuses
	// (store.ErrGrantLogLimit). The route reads it as it reads a caller that named no limit,
	// and holds it at the cap, where it holds anything above the cap too: the review is
	// answered a page either way rather than told the number it should have sent.
	if limit == 0 || limit > grantLogLimit {
		limit = grantLogLimit
	}
	// The audit table is the request machine's own store, and this route only reads it, so it
	// reaches it through the machine the broker already wires rather than through a dependency
	// of its own.
	events, err := store.ReadGrantLog(r.Context(), s.deps.Machine.Store.Pool, store.GrantLogQuery{
		Since: since, Until: until, After: after, Limit: int(limit),
	})
	if err != nil {
		writeInternal(w, "read the grant log", err)
		return
	}
	resp := grantLogResponse{Events: events, NextAfter: after}
	if len(events) > 0 {
		resp.NextAfter = events[len(events)-1].ID
	}
	if resp.Events == nil {
		resp.Events = []store.GrantLogEvent{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// grantLogBound reads one end of the period. A bound that is missing, or that is not an RFC 3339
// time, is refused naming which one it was and the format it takes: the review asks for a period,
// and a broker that filled in a bound of its own would answer the rows of a period nobody asked
// for, which a reader of the answer could not tell from the one they wanted.
func grantLogBound(w http.ResponseWriter, raw, name string) (time.Time, bool) {
	if raw == "" {
		writeError(w, http.StatusBadRequest, "PERIOD_REQUIRED",
			name+" is required: an RFC 3339 time, such as 2026-01-01T00:00:00Z")
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PERIOD_INVALID",
			name+" is not an RFC 3339 time, such as 2026-01-01T00:00:00Z")
		return time.Time{}, false
	}
	return at, true
}

// grantLogNumber reads ?after or ?limit, fallback when the caller sends neither. A value that is
// not a whole number, or is negative, is refused rather than quietly taken as the fallback: a
// caller that mistyped its cursor would otherwise be answered the period's first page again and
// read the same rows forever without being told.
func grantLogNumber(w http.ResponseWriter, raw, name string, fallback int64) (int64, bool) {
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		writeError(w, http.StatusBadRequest, "PAGE_INVALID", name+" is not a whole number of zero or more")
		return 0, false
	}
	return value, true
}
