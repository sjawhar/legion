package api

import (
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"reflect"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// The Go mirror of packages/contracts/src/dispatch-api.ts's DeliveryLive* frames: what an open
// Delivery or Pipeline page and Dispatch send each other over GET /ws/delivery (LEGION-690). A
// page subscribes to a view; Dispatch answers a snapshot (the matching GET route's body and a
// revision per key), then only deltas: list items by id, map entries by key, counts two levels
// deep, and the other fields whole.

// The three views a page can subscribe to, by the GET route each mirrors.
const (
	deliveryLiveTimeline = "timeline"
	deliveryLiveMeasures = "measures"
	deliveryLivePipeline = "pipeline"
)

// DeliveryLiveWindow is a window by preset (7, 14, 28 or 84 days), whose end stays at the
// present, or by the exact from/to of a custom range: exactly one of Preset and the From/To pair.
type DeliveryLiveWindow struct {
	Preset *int    `json:"preset,omitempty"`
	From   *string `json:"from,omitempty"`
	To     *string `json:"to,omitempty"`
}

// DeliveryLiveView is one view a page shows. A timeline or measures view names its Window and
// Query, the query string the page would send to the GET route without from/to (search and every
// facet); a pipeline view names at most an At.
type DeliveryLiveView struct {
	Kind   string              `json:"kind"`
	Window *DeliveryLiveWindow `json:"window,omitempty"`
	Query  *string             `json:"query,omitempty"`
	At     *string             `json:"at,omitempty"`
}

// DeliveryLiveRevs is what a page holds, as revisions (deliveryLiveRevision) in the frame's own
// sections: by list and item id, by map and key, by count map, facet and value, and by the name
// of each field sent whole.
type DeliveryLiveRevs struct {
	Items  map[string]map[string]string            `json:"items"`
	Maps   map[string]map[string]string            `json:"maps"`
	Counts map[string]map[string]map[string]string `json:"counts"`
	Fields map[string]string                       `json:"fields"`
}

// DeliveryLiveRevChanges is a delta's revision changes, in DeliveryLiveRevs' shape; nil (JSON
// null) for a key that left. A field sent whole never leaves, so Fields holds no null.
type DeliveryLiveRevChanges struct {
	Items  map[string]map[string]*string            `json:"items,omitempty"`
	Maps   map[string]map[string]*string            `json:"maps,omitempty"`
	Counts map[string]map[string]map[string]*string `json:"counts,omitempty"`
	Fields map[string]string                        `json:"fields,omitempty"`
}

// DeliveryLiveSubscribe names a view; Resume, from a reconnecting page, carries its last applied
// change (a decimal seq) and what it holds, and Dispatch answers the delta.
type DeliveryLiveSubscribe struct {
	Type   string              `json:"type"`
	Sub    string              `json:"sub"`
	View   DeliveryLiveView    `json:"view"`
	Resume *DeliveryLiveResume `json:"resume,omitempty"`
}

// DeliveryLiveResume is a reconnecting page's last applied change and its revisions.
type DeliveryLiveResume struct {
	Seq  string           `json:"seq"`
	Revs DeliveryLiveRevs `json:"revs"`
}

// DeliveryLiveUnsubscribe drops one subscription.
type DeliveryLiveUnsubscribe struct {
	Type string `json:"type"`
	Sub  string `json:"sub"`
}

// DeliveryLiveSnapshot is a subscription's first answer: exactly the matching GET route's body
// for the resolved window, the change position it reflects, when that change was recorded (nil
// while the log is empty), and the revision of every key the body holds.
type DeliveryLiveSnapshot struct {
	Type      string           `json:"type"`
	Sub       string           `json:"sub"`
	Seq       string           `json:"seq"`
	ChangedAt *time.Time       `json:"changed_at"`
	Body      json.RawMessage  `json:"body"`
	Revs      DeliveryLiveRevs `json:"revs"`
}

// DeliveryLiveItemChanges is one list's changes: each item new or changed, whole, in the body's
// order, and the ids of the items that left, sorted.
type DeliveryLiveItemChanges struct {
	Upsert []json.RawMessage `json:"upsert"`
	Remove []string          `json:"remove"`
}

// DeliveryLiveDelta is every later answer: only what differs from what the page holds. Set
// replaces fields whole, window among them whenever it moved, and never window alone; Maps and
// Counts carry a key's new value, or null when it left.
type DeliveryLiveDelta struct {
	Type      string                                `json:"type"`
	Sub       string                                `json:"sub"`
	Seq       string                                `json:"seq"`
	ChangedAt *time.Time                            `json:"changed_at"`
	Set       map[string]json.RawMessage            `json:"set"`
	Items     map[string]DeliveryLiveItemChanges    `json:"items"`
	Maps      map[string]map[string]json.RawMessage `json:"maps"`
	Counts    map[string]map[string]map[string]*int `json:"counts"`
	Revs      DeliveryLiveRevChanges                `json:"revs"`
}

// DeliveryLiveFreshness goes to every subscriber when the freshness row's verdict changes, and
// never otherwise: the freshness object and the sources past their threshold ("prs", "runs",
// "ci", "events").
type DeliveryLiveFreshness struct {
	Type      string                         `json:"type"`
	Freshness delivery.DeliveryFreshnessView `json:"freshness"`
	Stale     []string                       `json:"stale"`
}

// DeliveryLiveError refuses a frame or a subscription, by the code the matching GET route would
// answer (INVALID_QUERY, DELIVERY_NOT_CONFIGURED) or the socket's own (INVALID_FRAME).
type DeliveryLiveError struct {
	Type    string `json:"type"`
	Sub     string `json:"sub,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DeliveryLiveHeartbeat goes out every StreamHeartbeat beside the socket's control ping and carries
// nothing: a browser never shows a ping to the page's script, so this frame is how an open page
// tells a quiet socket from a dead one.
type DeliveryLiveHeartbeat struct {
	Type string `json:"type"`
}

// deliveryLiveHeartbeat is the one heartbeat frame.
var deliveryLiveHeartbeat = DeliveryLiveHeartbeat{Type: "heartbeat"}

// deliveryLiveClasses sorts one body kind's top-level JSON fields into how a delta sends them.
// Every field is in exactly one class (TestEveryResponseFieldHasOneClass), so a field added to a
// response is sent live or deliberately left out, never dropped unnoticed.
type deliveryLiveClasses struct {
	// body is the response struct the matching GET route writes.
	body reflect.Type
	// lists are sent item by item, each item by its "id" field.
	lists []string
	// maps are sent entry by entry, by key.
	maps []string
	// counts are {facet: {value: count}}, sent count by count.
	counts []string
	// whole fields are sent whole when their revision changes; each names the subfields its
	// revision leaves out (they are still sent, with the rest, when the field is).
	whole map[string][]string
	// withChanges are sent with any delta when they moved, but never make a delta by themselves.
	withChanges []string
	// out are never in a delta: computed_at moves on every compute, and freshness goes out as
	// its own frame.
	out []string
}

// deliveryLiveKinds is each view kind's classes. The timeline's waiting entries have no id, and
// are few, so they are sent whole; its measures are sent whole on a change of anything but their
// own computed_at, window and freshness.
var deliveryLiveKinds = map[string]deliveryLiveClasses{
	deliveryLiveTimeline: {
		body:        reflect.TypeFor[deliveryTimelineWithMeasures](),
		lists:       []string{"prs", "runs", "session_groups"},
		maps:        []string{"issue_titles", "session_titles", "session_pr_counts", "components"},
		counts:      []string{"facet_counts", "color_counts"},
		whole:       map[string][]string{"waiting": nil, "measures": {"computed_at", "window", "freshness"}},
		withChanges: []string{"window"},
		out:         []string{"freshness"},
	},
	deliveryLiveMeasures: {
		body:        reflect.TypeFor[deliveryMeasuresResponse](),
		whole:       map[string][]string{"measures": nil, "flags_source": nil, "unowned_p0": nil, "targets": nil, "status": nil},
		withChanges: []string{"window"},
		out:         []string{"computed_at", "freshness"},
	},
	deliveryLivePipeline: {
		body: reflect.TypeFor[deliveryPipelineResponse](),
		whole: map[string][]string{
			"days": nil, "pr_checks": nil, "staging_e2e": nil, "staging_apply": nil, "production_apply": nil,
			"merge_to_production": nil, "opened_to_merge": nil, "targets": nil,
		},
		out: []string{"computed_at", "freshness"},
	},
}

// deliveryLiveRevision is a value's revision: the 64-bit FNV-1a of its JSON encoding, as 16
// lowercase hex digits.
func deliveryLiveRevision(encoded []byte) string {
	hash := fnv.New64a()
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil))
}
