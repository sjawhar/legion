package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The five write kinds, each on its real route.
const (
	kindComment  = "comment"  // POST /api/v1/issues/{key}/comments {body}
	kindAnchored = "anchored" // the same with anchor {artifact: "spec", quote: <requirement cell>}
	kindReply    = "reply"    // the same with reply_to <seeded root comment id>
	kindEdit     = "edit"     // POST /api/v1/artifacts/{id}/edits: one replace of the revision token, with a summary
	kindUpload   = "upload"   // GET /api/v1/artifacts/{id}/text, then POST /api/v1/issues/{key}/artifacts {name, content, summary}
)

// eventKinds is every kind in the fixed order the mix picks in.
var eventKinds = []string{kindComment, kindAnchored, kindReply, kindEdit, kindUpload}

// primaryEventType is the event type each kind's write appends with its marker, the one the
// delivery join times. A write can append others (an anchored comment's document snapshot, a
// reply's comment.reopened); those are counted, not timed.
var primaryEventType = map[string]string{
	// internal/dispatch/api/comment_create.go:639 (createCommentFor appends comment.created for
	// a plain comment, an anchored comment and a reply alike).
	kindComment:  "comment.created",
	kindAnchored: "comment.created",
	kindReply:    "comment.created",
	// internal/dispatch/api/artifacts.go:751 (commitArtifactVersionEvent, which editArtifact
	// calls at :921 for a changed document with a summary).
	kindEdit: "artifact.version",
	// internal/dispatch/api/artifacts.go:493 (storeArtifact: an upload to an existing document is
	// artifact.version; artifact.created only for a new one).
	kindUpload: "artifact.version",
}

// replyAnchoredShare is the share of replies made in anchored threads: production had 1,217 of
// 1,576 replies in anchored threads (INFRA-512 report §2).
const replyAnchoredShare = 0.77

var eventMixPresets = map[string]string{
	// INFRA-512's counts for LINUX 22:55-23:34Z (plain roots 153, anchored roots 1,495, replies
	// 1,576, document versions 1,329). The 3:1 edit/upload split of the versions is INFERENCE
	// from the production profile's write counts (editArtifact 6, uploadArtifact 2).
	"production-2026-10-26": "comment=3,anchored=33,reply=35,edit=22,upload=7",
	// The root report's mix, so its 1x row can be compared.
	"report-2026-10-27": "comment=50,anchored=30,edit=20",
}

const defaultEventMix = "production-2026-10-26"

// eventMixPresetNames lists the presets, sorted, for help and error text.
func eventMixPresetNames() string {
	return strings.Join(slices.Sorted(maps.Keys(eventMixPresets)), ", ")
}

// eventMix is a resolved --event-mix: a weight per kind.
type eventMix struct {
	Name    string             `json:"name"`
	Weights map[string]float64 `json:"weights"`
	total   float64
}

// eventMixError refuses one item of an event mix.
type eventMixError struct {
	Kind   string
	Reason string
}

func (e *eventMixError) Error() string {
	return fmt.Sprintf("event mix: %q %s", e.Kind, e.Reason)
}

// parseEventMix reads a preset name or "kind=weight,...". An unknown kind, a negative or
// non-numeric weight, a repeated kind and a mix whose weights are all zero are refused.
func parseEventMix(s string) (eventMix, error) {
	name := s
	if preset, ok := eventMixPresets[s]; ok {
		s = preset
	}
	m := eventMix{Name: name, Weights: map[string]float64{}}
	if strings.TrimSpace(s) == "" {
		return m, fmt.Errorf("event mix is empty: name a preset (%s) or give kind=weight,...", eventMixPresetNames())
	}
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		kind, value, ok := strings.Cut(item, "=")
		if !ok {
			return m, &eventMixError{Kind: item, Reason: "is neither a preset nor kind=weight"}
		}
		kind = strings.TrimSpace(kind)
		if !slices.Contains(eventKinds, kind) {
			return m, &eventMixError{Kind: kind, Reason: "is not a kind; kinds are " + strings.Join(eventKinds, ", ")}
		}
		if _, dup := m.Weights[kind]; dup {
			return m, &eventMixError{Kind: kind, Reason: "is given twice"}
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || !finite(w) {
			return m, &eventMixError{Kind: kind, Reason: fmt.Sprintf("has weight %q, which is not a number", value)}
		}
		if w < 0 {
			return m, &eventMixError{Kind: kind, Reason: fmt.Sprintf("has negative weight %g", w)}
		}
		m.Weights[kind] = w
		m.total += w
	}
	if m.total == 0 {
		return m, fmt.Errorf("event mix %q: every weight is zero", name)
	}
	return m, nil
}

// pick maps x in [0, 1) to a kind in proportion to the weights.
func (m eventMix) pick(x float64) string {
	x *= m.total
	last := ""
	for _, k := range eventKinds {
		w := m.Weights[k]
		if w <= 0 {
			continue
		}
		last = k
		if x < w {
			return k
		}
		x -= w
	}
	return last
}

// eventPlan is one write, fully decided by the event generator's RNG before it is sent.
type eventPlan struct {
	marker  int64
	kind    string
	issue   int // index into the seed's issues
	session int // index of the writing session
	human   bool
	quote   string
	replyTo string
}

func markerTag(marker int64) string { return fmt.Sprintf("lgm%dx", marker) }

func revisionToken(n int) string { return fmt.Sprintf("Revision token r%d", n) }

// currentRevision reads the first "Revision token r<N>" in text.
func currentRevision(text string) (int, bool) {
	const prefix = "Revision token r"
	i := strings.Index(text, prefix)
	if i < 0 {
		return 0, false
	}
	j := i + len(prefix)
	k := j
	for k < len(text) && text[k] >= '0' && text[k] <= '9' {
		k++
	}
	n, err := strconv.Atoi(text[j:k])
	return n, err == nil
}

// writer sends planned writes. Edits and uploads to one issue are serialized, and each issue's
// revision token is tracked from r0 (the seed's), re-read from the document when a response was
// lost.
type writer struct {
	a     *api
	sf    seedFile
	locks []sync.Mutex
	revs  []int
	lg    *log.Logger
}

func newWriter(a *api, sf seedFile, lg *log.Logger) *writer {
	return &writer{a: a, sf: sf, locks: make([]sync.Mutex, len(sf.Issues)), revs: make([]int, len(sf.Issues)), lg: lg}
}

// do sends one write and returns its record.
func (w *writer) do(ctx context.Context, p eventPlan) writeRec {
	is := w.sf.Issues[p.issue]
	rec := writeRec{Marker: p.marker, Kind: p.kind, Issue: is.Key}
	id := who{}
	if p.human {
		id.person = fmt.Sprintf("person-%d", p.session%7)
	}
	tag := markerTag(p.marker)
	r := rand.New(rand.NewPCG(uint64(p.marker), 7))
	var path string
	var body map[string]any
	switch p.kind {
	case kindComment:
		path, body = "/api/v1/issues/"+is.Key+"/comments", map[string]any{"body": tag + " " + sentence(r, 30)}
	case kindAnchored:
		path = "/api/v1/issues/" + is.Key + "/comments"
		body = map[string]any{"body": tag + " line comment: " + sentence(r, 20), "anchor": map[string]any{"artifact": "spec", "quote": p.quote}}
	case kindReply:
		path, body = "/api/v1/issues/"+is.Key+"/comments", map[string]any{"body": tag + " reply: " + sentence(r, 20), "reply_to": p.replyTo}
	case kindEdit:
		w.locks[p.issue].Lock()
		defer w.locks[p.issue].Unlock()
		old := w.revs[p.issue]
		path = "/api/v1/artifacts/" + is.ArtifactID + "/edits"
		body = map[string]any{
			"ops":     []map[string]any{{"op": "replace", "find": revisionToken(old), "with": revisionToken(old + 1)}},
			"summary": fmt.Sprintf("%s revision %d", tag, old+1),
		}
	case kindUpload:
		w.locks[p.issue].Lock()
		defer w.locks[p.issue].Unlock()
		text, markdown := w.specText(ctx, p.issue, id)
		old, found := currentRevision(markdown)
		if !found {
			rec.Arrival, rec.Response, rec.Status = text.start, text.end, text.status
			rec.TransportErr = fmt.Sprintf("read %s before upload: status %d, revision token found %t, %v", specName, text.status, found, text.err)
			w.lg.Printf("write %s %s: %s", p.kind, is.Key, rec.TransportErr)
			return rec
		}
		content := strings.Replace(markdown, revisionToken(old), revisionToken(old+1), 1)
		path = "/api/v1/issues/" + is.Key + "/artifacts"
		body = map[string]any{"name": specName, "content": content, "summary": fmt.Sprintf("%s upload revision %d", tag, old+1)}
		w.revs[p.issue] = old
	}
	if !p.human {
		body["actor"] = sessionActor(sessionID(p.session))
	}
	res := w.a.call(ctx, "POST", path, id, body)
	rec.Arrival, rec.Response, rec.Status, rec.ConnWait = res.arrival(), res.end, res.status, res.connWait
	if res.err != nil {
		rec.TransportErr = res.err.Error()
	}
	if p.kind == kindEdit || p.kind == kindUpload {
		if res.ok() {
			w.revs[p.issue]++
		} else {
			w.resync(ctx, p.issue)
		}
	}
	if !res.ok() {
		w.lg.Printf("write %s %s: %d %s %v", p.kind, is.Key, res.status, trunc(res.body), res.err)
	}
	return rec
}

// specText reads the issue's spec text: the GET's result and the document's markdown, "" when
// the read failed or its body did not decode.
func (w *writer) specText(ctx context.Context, issue int, id who) (callResult, string) {
	res := w.a.call(ctx, "GET", "/api/v1/artifacts/"+w.sf.Issues[issue].ArtifactID+"/text", id, nil)
	var doc struct {
		Markdown string `json:"markdown"`
	}
	if res.ok() {
		_ = json.Unmarshal(res.body, &doc)
	}
	return res, doc.Markdown
}

// resync reads the spec's current revision token after an edit or upload whose outcome the
// generator did not see (a client timeout on a write the server went on to commit).
func (w *writer) resync(ctx context.Context, issue int) {
	_, markdown := w.specText(ctx, issue, who{})
	if n, ok := currentRevision(markdown); ok {
		w.revs[issue] = n
	}
}
