package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The five write kinds of a seed.json step, each on its real route.
const (
	kindComment  = "comment"  // POST /api/v1/issues/{key}/comments {body}
	kindAnchored = "anchored" // the same with anchor {artifact: "spec", quote: <requirement cell>}
	kindReply    = "reply"    // the same with reply_to <seeded root comment id>
	kindEdit     = "edit"     // POST /api/v1/artifacts/{id}/edits: one replace of the revision token, with a summary
	kindUpload   = "upload"   // GET /api/v1/artifacts/{id}/text, then POST /api/v1/issues/{key}/artifacts {name, content, summary}
)

// The three write kinds of the Verified Linux spec loop, which a step sends against its own seed
// (-loop-seed, loopseed_export.go) and no other.
const (
	// kindVerdict is a new version of one function's review page: POST /api/v1/artifacts/{id}/edits
	// on a review page of the proposal, replacing the page's paragraph with one of the same size
	// drawn afresh, as the loop's verdict rewrites the review text it reads. The page, and so the
	// op's find and its with, average loopPageMean (9,954) bytes.
	kindVerdict = "verdict"
	// kindRequirement is a new requirement: an anchored thread on the proposal's last part
	// document, on a spare quote row of that document (loopSpareRow).
	kindRequirement = "requirement"
	// kindStatus is a status change: a reply in an open thread and then its resolve, two
	// requests, or - for loopReopenShare of them - a reply alone in a resolved thread, which
	// reopens it.
	kindStatus = "status"
)

// eventKinds is every kind in the fixed order the mix picks in. The loop's three follow the five,
// so a mix of the five picks exactly as it always has.
var eventKinds = []string{kindComment, kindAnchored, kindReply, kindEdit, kindUpload,
	kindVerdict, kindRequirement, kindStatus}

// primaryEventType is the event type each kind's write appends with its marker, the one the
// delivery join times. A write can append others (an anchored comment's document snapshot, a
// reply's comment.reopened); those are counted, not timed.
var primaryEventType = map[string]string{
	// internal/dispatch/api/comment_create.go:639 (createCommentFor appends comment.created for
	// a plain comment, an anchored comment and a reply alike).
	kindComment:  evCommentCreated,
	kindAnchored: evCommentCreated,
	kindReply:    evCommentCreated,
	// internal/dispatch/api/artifacts.go:751 (commitArtifactVersionEvent, which editArtifact
	// calls at :921 for a changed document with a summary).
	kindEdit: evArtifactVersion,
	// internal/dispatch/api/artifacts.go:493 (storeArtifact: an upload to an existing document is
	// artifact.version; artifact.created only for a new one).
	kindUpload: evArtifactVersion,
	// internal/dispatch/api/artifacts.go:751 again: an edit of a review page with a summary
	// writes a named version and commits artifact.version for it.
	kindVerdict: evArtifactVersion,
	// internal/dispatch/api/comment_create.go:856 (the comment.created every created comment
	// appends). A status change's reply in a resolved thread appends comment.reopened at :829
	// first, in the same transaction; that one is counted, not timed.
	kindRequirement: evCommentCreated,
	kindStatus:      evCommentCreated,
}

// The Verified Linux spec loop's own write sizes and shares, measured on the loop of 2026-10-26
// (the counts of proposals-64796913-byid.tsv and the issue's plan):
//
//   - a requirement line's body averages 1,205 bytes, the loop's measured per-line mean;
//   - a status note averages 124 bytes;
//   - 1.2% of status changes are a reply alone in a resolved thread, which reopens it
//     (internal/dispatch/api/comment_create.go:737-760 clears resolved in the reply's own
//     transaction, and :829 and :856 append comment.reopened and comment.created in it).
const (
	loopRequirementMeanBytes = 1_205
	loopStatusMeanBytes      = 124
	loopReopenShare          = 0.012
)

// replyAnchoredShare is the share of replies made in anchored threads: production had 1,217 of
// 1,576 replies in anchored threads (the 2026-10-26 production write counts, §2 of their replay
// report).
const replyAnchoredShare = 0.77

var eventMixPresets = map[string]string{
	// The orchestration project's writes of 2026-10-26 22:55-23:34Z (plain roots 153, anchored
	// roots 1,495, replies 1,576, document versions 1,329). The 3:1 edit/upload split of the
	// versions is INFERENCE from the production profile's write counts (editArtifact 6,
	// uploadArtifact 2).
	"production-2026-10-26": "comment=3,anchored=33,reply=35,edit=22,upload=7",
	// The root report's mix, so its 1x row can be compared.
	"report-2026-10-27": "comment=50,anchored=30,edit=20",
	// The Verified Linux spec loop's own writes over a driver day, as the issue's plan counts
	// them: 106 verdicts and 48 requirement lines for every 50 status changes. A step drives it
	// against the loop's own seed (-loop-seed), which is the only seed these kinds read.
	"spec-loop-2026-11-03": "verdict=106,requirement=48,status=50",
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

// mixSeed is the seed an event mix is checked against: today's seed.json (seedFile) or the
// Verified Linux spec loop's own exported seed (loopSeed, loopseed_export.go). Each says why it
// cannot serve one write kind, which is all the check asks of it.
type mixSeed interface {
	refuseKind(kind string) string
}

// checkSeedMix refuses a seed the mix cannot draw from, naming the kind and the record it holds
// none of. path names the seed. The kinds are walked in eventKinds order, so a seed that cannot
// serve two of them is refused by the first.
func checkSeedMix(s mixSeed, mix eventMix, path string) error {
	for _, kind := range eventKinds {
		if mix.Weights[kind] <= 0 {
			continue
		}
		if why := s.refuseKind(kind); why != "" {
			return fmt.Errorf("seed %s: %s", path, why)
		}
	}
	return nil
}

// refuseKind says why seed.json cannot serve one write kind, "" when it can (mixSeed). A reply
// needs a root comment to answer and an anchored comment a quote to anchor on, in every issue,
// since a write lands on any issue the step uses. The loop's three kinds read records of the
// loop's own seed - a review page, a spare quote row, a resolved requirement thread - which no
// issue of seed.json holds.
func (sf seedFile) refuseKind(kind string) string {
	switch kind {
	case kindVerdict:
		return "seed.json holds no review page to write a verdict on: the loop's kinds need the loop's own seed (-loop-seed)"
	case kindRequirement:
		return "seed.json holds no spare quote row to anchor a requirement thread on: the loop's kinds need the loop's own seed (-loop-seed)"
	case kindStatus:
		return "seed.json holds no resolved thread for a status change: the loop's kinds need the loop's own seed (-loop-seed)"
	}
	for _, is := range sf.Issues {
		if kind == kindReply && len(is.AnchoredCommentIDs)+len(is.PlainCommentIDs) == 0 {
			return fmt.Sprintf("issue %s has no root comment ids for replies; reseed with this loadgen", is.Key)
		}
		if kind == kindAnchored && len(is.Quotes) == 0 {
			return fmt.Sprintf("issue %s has no quotes for anchored comments", is.Key)
		}
	}
	return ""
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

// eventPlan is one write, fully decided by the event generator's RNG before it is sent. marker
// is the write's counter in the run (markerTag).
type eventPlan struct {
	marker  int64
	kind    string
	issue   int // index into the seed's issues
	session int // index of the writing session
	human   bool
	quote   string
	replyTo string
	// The loop mix's own draws, all zero without -loop-seed: page is where the review page a
	// verdict writes a new version of stands in the proposal's pages, body the byte size of the
	// write's text, and reopen makes a status change the reply-alone form that reopens a
	// resolved thread.
	page   int
	body   int
	reopen bool
}

// planEvent draws one write from the event stream: a session, one of its watched issues, a
// person or the session as author, a kind, and the kind's quote or reply root. Under a loop seed
// the kind's draws are the loop's (planLoopEvent), which replace the watched issue with the
// proposal the kind's own weighted draw lands on.
func planEvent(r *rand.Rand, cfg runConfig, sessions []session, sf seedFile, marker int64) eventPlan {
	sIdx := r.IntN(len(sessions))
	s := sessions[sIdx]
	p := eventPlan{marker: marker, session: sIdx, issue: s.watched[r.IntN(len(s.watched))]}
	p.human = r.Float64() < cfg.human
	p.kind = cfg.mix.pick(r.Float64())
	if cfg.loop != nil {
		return planLoopEvent(r, cfg.loop, p)
	}
	is := sf.Issues[p.issue]
	switch p.kind {
	case kindAnchored:
		p.quote = is.Quotes[r.IntN(len(is.Quotes))]
	case kindReply:
		anchoredRoot := r.Float64() < replyAnchoredShare
		roots := is.PlainCommentIDs
		if (anchoredRoot && len(is.AnchoredCommentIDs) > 0) || len(roots) == 0 {
			roots = is.AnchoredCommentIDs
		}
		p.replyTo = roots[r.IntN(len(roots))]
	}
	return p
}

// planLoopEvent draws the loop mix's write: the proposal it lands on, in proportion to its
// requirement ids among the proposals that hold what the kind needs (loopSeed.pick), and what the
// kind reads of that proposal. checkSeedMix has refused a mix whose kinds no proposal can serve,
// so every draw below has something to draw from.
func planLoopEvent(r *rand.Rand, ls *loopSeed, p eventPlan) eventPlan {
	switch p.kind {
	case kindVerdict:
		p.issue = ls.pick(loopNeedReviewPage, r.Float64())
		p.page = r.IntN(len(ls.Proposals[p.issue].ReviewPages))
	case kindRequirement:
		p.issue = ls.pick(loopNeedLastPart, r.Float64())
		p.body = loopDrawBytes(r, loopRequirementMeanBytes)
	case kindStatus:
		p.reopen = r.Float64() < loopReopenShare
		need := loopNeedOpenThread
		if p.reopen {
			need = loopNeedResolvedThread
		}
		p.issue = ls.pick(need, r.Float64())
		roots := ls.Proposals[p.issue].OpenThreads
		if p.reopen {
			roots = ls.Proposals[p.issue].ResolvedThreads
		}
		p.replyTo = roots[r.IntN(len(roots))]
		p.body = loopDrawBytes(r, loopStatusMeanBytes)
	default:
		p.issue = ls.pick(loopNeedAny, r.Float64())
	}
	return p
}

// loopDrawBytes draws one body size whose mean is mean, from the lognormal the planner draws the
// seed's own body sizes from (loopBodySigma): a measured mean is the mean of a spread, and the
// loop's lines are spread. A draw is at least one byte and at most loopBodyCap, which a longer
// body is refused by (400 CAP_EXCEEDED).
func loopDrawBytes(r *rand.Rand, mean float64) int {
	median := mean / math.Exp(loopBodySigma*loopBodySigma/2)
	return min(max(int(math.Round(median*math.Exp(loopBodySigma*r.NormFloat64()))), 1), loopBodyCap)
}

func revisionToken(n int) string { return fmt.Sprintf("Revision token r%d", n) }

// currentRevision reads the first "Revision token r<N>" in text.
func currentRevision(text string) (int, bool) {
	return firstNumberAfter(text, "Revision token r")
}

// firstNumberAfter reads the decimal number the first prefix in text is followed by: how both
// resyncs read back the state of a document they lost the answer to, the spec's revision token
// and a review page's generation (loopPageGeneration).
func firstNumberAfter(text, prefix string) (int, bool) {
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

// writer sends planned writes, each with the run's marker nonce. Edits and uploads to one issue
// are serialized, and each issue's revision token is tracked from r0 (the seed's), re-read from
// the document when a response was lost. Under a loop seed it also holds each proposal's
// inventory of spare quote rows, which the requirement kind takes one of per write, and each
// review page's generation, which the verdict kind writes the next of.
type writer struct {
	a     *api
	sf    seedFile
	loop  *loopSeed
	nonce int64
	locks []sync.Mutex
	revs  []int
	lg    *log.Logger
	// quotes is the requirement kind's state per proposal, by the index of the step's issues, and
	// pages the verdict kind's state per review page of each of them.
	quotes []loopQuotes
	pages  [][]loopPage

	// mu guards what the loop's writes record beyond their one writeRec: the requests each class
	// sent, since a status change sends two, and what the run met on the way.
	mu          sync.Mutex
	reqs        map[string]*loopRequests
	lat         map[string][]float64
	reopens     int
	capExceeded int
	newParts    int
	pageResyncs int
}

// loopQuotes is the requirement kind's inventory on one proposal: the part document its threads
// anchor in and the spare quote rows of that document no write has taken yet. It starts as the
// seed's last part document and the 64 rows the planner reserved on it (loopSpareQuotes); a
// document that answers 413 CAP_EXCEEDED, or whose rows are all taken, is replaced by a part
// document this run opens, with a fresh inventory of its own.
type loopQuotes struct {
	doc   string
	rows  []string
	parts int
	full  bool
}

// loopPage is the verdict kind's state on one review page: the generation of the text this run
// believes the page holds, which starts at 0 (what the seed writer wrote, loopPageText) and is
// re-read from the page when a verdict's op did not apply (resyncPage), and the lock that keeps
// two verdicts off one page, as the issue's lock keeps two edits off one spec. A verdict's find
// is the page's current text, so two verdicts that drew it at once would send the same find and
// the second would be refused for a page the first had already rewritten.
type loopPage struct {
	mu  sync.Mutex
	gen int
}

// The classes of request the loop's writes send. A write kind's own request and the extra
// request it sometimes needs are classes of their own, so what a step sent is read as it was
// sent: summary.json's events.by_kind counts one write per arrival and times the event its
// marker names, and these count the requests.
const (
	loopClassVerdict         = "verdict"
	loopClassRequirement     = "requirement"
	loopClassRequirementPart = "requirement-part"
	loopClassStatusReply     = "status-reply"
	loopClassStatusResolve   = "status-resolve"
)

// loopClasses is every class, in the order summary.json lists them.
var loopClasses = []string{loopClassVerdict, loopClassRequirement, loopClassRequirementPart,
	loopClassStatusReply, loopClassStatusResolve}

// loopRequests is one class of request the loop's writes sent: how many, how many were refused,
// the codes the refusals named, and how long the requests took from arrival to response.
type loopRequests struct {
	Class     string         `json:"class"`
	Requests  int            `json:"requests"`
	Failures  int            `json:"failures"`
	Codes     map[string]int `json:"codes,omitempty"`
	LatencyMs stat           `json:"latency_ms"`
}

// loopRun is what summary.json records of a loop step beyond its events: the requests each class
// sent, and what the requirement and status kinds met.
type loopRun struct {
	Requests []loopRequests `json:"requests"`
	// Reopens counts the status changes sent as a reply alone in a resolved thread
	// (loopReopenShare), which reopens it.
	Reopens int `json:"reopens"`
	// CapExceeded counts the requirement writes a part document refused with 413, and NewParts
	// the part documents those refusals, and the inventories they exhausted, made this run open.
	CapExceeded int `json:"cap_exceeded"`
	NewParts    int `json:"new_parts"`
	// PageResyncs counts the verdicts whose op did not apply and made the run re-read its review
	// page for the generation the page stands at (resyncPage): a response this run lost, or a
	// page another run has written since.
	PageResyncs int `json:"page_resyncs"`
}

func newWriter(a *api, sf seedFile, loop *loopSeed, nonce int64, lg *log.Logger) *writer {
	w := &writer{a: a, sf: sf, loop: loop, nonce: nonce, locks: make([]sync.Mutex, len(sf.Issues)),
		revs: make([]int, len(sf.Issues)), lg: lg,
		reqs: map[string]*loopRequests{}, lat: map[string][]float64{}}
	if loop == nil {
		return w
	}
	w.quotes = make([]loopQuotes, len(loop.Proposals))
	w.pages = make([][]loopPage, len(loop.Proposals))
	for i := range loop.Proposals {
		pr := &loop.Proposals[i]
		// A proposal whose last part document the seed does not hold starts with no inventory:
		// its first requirement write opens a part document of its own.
		if pr.serves(loopNeedLastPart) {
			w.quotes[i] = loopQuotes{doc: pr.Parts[pr.LastPart].ArtifactID, rows: pr.SpareQuotes}
		}
		w.pages[i] = make([]loopPage, len(pr.ReviewPages))
	}
	return w
}

// record counts one request of a class and times it from its arrival in Dispatch's socket, as
// the request generator times a read (summarizeRequests).
func (w *writer) record(class string, res callResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.reqs[class]
	if c == nil {
		c = &loopRequests{Class: class}
		w.reqs[class] = c
	}
	c.Requests++
	w.lat[class] = append(w.lat[class], float64(res.end-res.arrival())/1e6)
	if res.ok() {
		return
	}
	c.Failures++
	if c.Codes == nil {
		c.Codes = map[string]int{}
	}
	code := strconv.Itoa(res.status) + " " + res.errorCode()
	if res.err != nil {
		code = errClass(res.err)
	}
	c.Codes[strings.TrimSpace(code)]++
}

// loopRecord is what the loop's writes sent, for summary.json; nil for a step without a loop
// seed, which sends none of these classes.
func (w *writer) loopRecord() *loopRun {
	if w.loop == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := &loopRun{Requests: []loopRequests{}, Reopens: w.reopens,
		CapExceeded: w.capExceeded, NewParts: w.newParts, PageResyncs: w.pageResyncs}
	for _, class := range loopClasses {
		c := w.reqs[class]
		if c == nil {
			continue
		}
		row := *c
		row.Codes = maps.Clone(c.Codes)
		row.LatencyMs = mkStat(w.lat[class])
		out.Requests = append(out.Requests, row)
	}
	return out
}

// do sends one write and returns its record.
func (w *writer) do(ctx context.Context, p eventPlan) writeRec {
	is := w.sf.Issues[p.issue]
	rec := writeRec{Marker: p.marker, Kind: p.kind, Issue: is.Key}
	id := who{}
	if p.human {
		id.person = fmt.Sprintf("person-%d", p.session%7)
	}
	tag := markerTag(w.nonce, p.marker)
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
			rec.Sent, rec.Arrival, rec.Response, rec.Status = text.start, text.start, text.end, text.status
			rec.TransportErr = fmt.Sprintf("read %s before upload: status %d, revision token found %t, %v", specName, text.status, found, text.err)
			w.lg.Printf("write %s %s: %s", p.kind, is.Key, rec.TransportErr)
			return rec
		}
		content := strings.Replace(markdown, revisionToken(old), revisionToken(old+1), 1)
		path = "/api/v1/issues/" + is.Key + "/artifacts"
		body = map[string]any{"name": specName, "content": content, "summary": fmt.Sprintf("%s upload revision %d", tag, old+1)}
		w.revs[p.issue] = old
	case kindVerdict:
		// A verdict rewrites the page it lands on, as the loop's does: the op replaces the page's
		// paragraph with one of the same size drawn at the next generation, so the request
		// carries the page's own bytes twice - the text it finds and the text it writes - and the
		// version Dispatch stores stays the page the loop measured (loopPageMean, 9,954 bytes).
		// An insert would make the request a few hundred bytes and grow the stored page by every
		// verdict the step sends, which the loop never does.
		//
		// The page's lock is one verdict's at a time: two that drew the page's text together
		// would send one find, and the second would look for text the first had replaced.
		pg := &w.pages[p.issue][p.page]
		pg.mu.Lock()
		defer pg.mu.Unlock()
		pr := &w.loop.Proposals[p.issue]
		page := pr.ReviewPages[p.page]
		path = "/api/v1/artifacts/" + page.ArtifactID + "/edits"
		body = map[string]any{
			"ops": []map[string]any{{"op": "replace",
				"find": loopPageBody(pr.Index, page.Index, pg.gen, page.Bytes),
				"with": loopPageBody(pr.Index, page.Index, pg.gen+1, page.Bytes)}},
			"summary": tag + " verdict",
		}
	case kindRequirement:
		// The proposal's inventory is one write's at a time: two requirement writes must not
		// anchor on one row, and the part document a refusal replaces must be replaced once.
		w.locks[p.issue].Lock()
		defer w.locks[p.issue].Unlock()
		doc, quote, opened, ok := w.requirementTarget(ctx, p, id, tag)
		if !ok {
			rec.Sent, rec.Arrival, rec.Response, rec.Status = opened.start, opened.arrival(), opened.end, opened.status
			rec.TransportErr = fmt.Sprintf("open a part document for a requirement thread: status %d, %v", opened.status, opened.err)
			w.lg.Printf("write %s %s: %s", p.kind, is.Key, rec.TransportErr)
			return rec
		}
		path = "/api/v1/issues/" + is.Key + "/comments"
		body = map[string]any{
			"body":   loopWriteText(tag+" requirement: ", p.body),
			"anchor": map[string]any{"artifact": doc, "quote": quote},
		}
	case kindStatus:
		path = "/api/v1/issues/" + is.Key + "/comments"
		body = map[string]any{"body": loopWriteText(tag+" status: ", p.body), "reply_to": p.replyTo}
	}
	if !p.human {
		body["actor"] = sessionActor(sessionID(p.session))
	}
	res := w.a.call(ctx, "POST", path, id, body)
	rec.Sent, rec.Arrival, rec.Response, rec.Status, rec.ConnWait = res.start, res.arrival(), res.end, res.status, res.connWait
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
	switch p.kind {
	case kindVerdict:
		w.record(loopClassVerdict, res)
		// The page's lock is still held, so the generation and the page move together. A verdict
		// Dispatch answered holds the page at the generation it wrote; one it refused 429
		// EDIT_QUEUE_FULL never reached the document (internal/dispatch/api/server.go:832-834),
		// so the page is where this run left it; any other answer - a find Dispatch did not
		// match, a response this run lost - leaves the page where only the page can say.
		pg := &w.pages[p.issue][p.page]
		switch {
		case res.ok():
			pg.gen++
		case res.status != http.StatusTooManyRequests:
			w.resyncPage(ctx, pg, p, is.Key)
		}
	case kindRequirement:
		w.record(loopClassRequirement, res)
		// A part document whose margin is full answers 413: the next requirement write on this
		// proposal opens a new one rather than sending this refusal again. The proposal's lock
		// is still held, so the flag and the inventory move together.
		if res.status == http.StatusRequestEntityTooLarge {
			w.quotes[p.issue].full = true
			w.mu.Lock()
			w.capExceeded++
			w.mu.Unlock()
		}
	case kindStatus:
		w.record(loopClassStatusReply, res)
		if p.reopen {
			w.mu.Lock()
			w.reopens++
			w.mu.Unlock()
		} else if res.ok() {
			w.resolveThread(ctx, p, id)
		}
	}
	if !res.ok() {
		w.lg.Printf("write %s %s: %d %s %v", p.kind, is.Key, res.status, trunc(res.body), res.err)
	}
	return rec
}

// loopWriteText is the text of one loop write of n bytes: the marker and what the write is,
// then filler (loopFillText). A size under the head's own length would cut the marker off, and
// a write the run cannot find its event by is a write it cannot time, so the head is the floor.
func loopWriteText(head string, n int) string {
	return loopFillText(head, max(n, len(head)))
}

// requirementTarget is the part document a requirement thread anchors in and the spare quote row
// it takes. It opens a new part document on the proposal when the document it was anchoring in
// answered 413 or its spare rows are all taken; opened is that write's answer when it failed,
// which is why there is nothing to anchor on. The proposal's lock is the caller's.
func (w *writer) requirementTarget(ctx context.Context, p eventPlan, id who, tag string) (doc, quote string, opened callResult, ok bool) {
	q := &w.quotes[p.issue]
	if q.doc == "" || q.full || len(q.rows) == 0 {
		if opened, ok = w.openPart(ctx, p, id, tag, q); !ok {
			return "", "", opened, false
		}
	}
	quote, q.rows = q.rows[0], q.rows[1:]
	return q.doc, quote, opened, true
}

// openPart opens one part document on the proposal, carrying a fresh inventory of spare quote
// rows for the requirement threads this run writes into it. Its rows name the run's nonce, so
// they are rows of this document alone and an anchor resolves on exactly one line.
func (w *writer) openPart(ctx context.Context, p eventPlan, id who, tag string, q *loopQuotes) (callResult, bool) {
	pr := &w.loop.Proposals[p.issue]
	rows := make([]string, loopSpareQuotes)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s, run part %03d\n\n", pr.Name, q.parts+1)
	for i := range rows {
		rows[i] = loopRunSpareRow(w.nonce, pr.Index, q.parts, i)
		b.WriteString(rows[i])
		b.WriteString("\n\n")
	}
	body := map[string]any{
		"name":    loopRunPartName(w.nonce, q.parts),
		"content": b.String(),
		"summary": fmt.Sprintf("%s part document %d of the run", tag, q.parts+1),
	}
	if !p.human {
		body["actor"] = sessionActor(sessionID(p.session))
	}
	res := w.a.call(ctx, "POST", "/api/v1/issues/"+w.sf.Issues[p.issue].Key+"/artifacts", id, body)
	w.record(loopClassRequirementPart, res)
	if !res.ok() {
		w.lg.Printf("write %s %s: open a part document: %d %s %v", p.kind, w.sf.Issues[p.issue].Key, res.status, trunc(res.body), res.err)
		return res, false
	}
	q.doc, q.rows, q.full = idOf(res.body), rows, false
	q.parts++
	w.mu.Lock()
	w.newParts++
	w.mu.Unlock()
	return res, true
}

// loopRunPartName names a part document a run opened, and loopRunSpareRow one of its quote rows.
// Both carry the run's marker nonce, so two runs against one seed write their own documents and
// anchor on their own lines.
func loopRunPartName(nonce int64, part int) string {
	return fmt.Sprintf("part-run-%s-%03d.md", formatNonce(nonce), part+1)
}

func loopRunSpareRow(nonce int64, proposal, part, row int) string {
	return fmt.Sprintf("REQ-%04d-run-%s-%03d-%02d is a free line for a requirement written during the run.",
		proposal, formatNonce(nonce), part, row)
}

// resolveThread sends the second request of a status change: the resolve of the thread its reply
// answered. The resolve carries no text, so the comment.resolved it appends holds no marker and
// the delivery join cannot time it (commit-trigger.sql reads the marker out of the payload); the
// request is counted and timed as a class of its own instead (summary.json loop.requests).
func (w *writer) resolveThread(ctx context.Context, p eventPlan, id who) {
	body := map[string]any{}
	if !p.human {
		body["actor"] = sessionActor(sessionID(p.session))
	}
	res := w.a.call(ctx, "POST", "/api/v1/comments/"+p.replyTo+"/resolve", id, body)
	w.record(loopClassStatusResolve, res)
	if !res.ok() {
		w.lg.Printf("write %s resolve %s: %d %s %v", p.kind, w.sf.Issues[p.issue].Key, res.status, trunc(res.body), res.err)
	}
}

// specText reads the issue's spec text: the GET's result and the document's markdown, "" when
// the read failed or its body did not decode.
func (w *writer) specText(ctx context.Context, issue int, id who) (callResult, string) {
	return w.artifactText(ctx, w.sf.Issues[issue].ArtifactID, id)
}

// artifactText reads one document's text: the GET's result and the document's markdown, "" when
// the read failed or its body did not decode.
func (w *writer) artifactText(ctx context.Context, artifact string, id who) (callResult, string) {
	res := w.a.call(ctx, "GET", "/api/v1/artifacts/"+artifact+"/text", id, nil)
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

// resyncPage reads a review page's current text after a verdict whose op did not apply, and takes
// the generation the page's paragraph names as the one the next verdict finds, as resync takes
// the spec's revision token. Without it a page this run's generation had drifted from - a
// response the run lost, a page another run wrote - would refuse this run's every later verdict,
// since each would look for text the page no longer holds. A page whose text names no generation
// is one this run cannot reproduce, which it logs rather than guesses at. The page's lock is the
// caller's.
func (w *writer) resyncPage(ctx context.Context, pg *loopPage, p eventPlan, key string) {
	page := w.loop.Proposals[p.issue].ReviewPages[p.page]
	w.mu.Lock()
	w.pageResyncs++
	w.mu.Unlock()
	res, markdown := w.artifactText(ctx, page.ArtifactID, who{})
	n, ok := loopPageGeneration(markdown)
	if !ok {
		w.lg.Printf("write %s %s: read review page %s for its generation: %d %s %v",
			p.kind, key, page.ArtifactID, res.status, trunc(res.body), res.err)
		return
	}
	pg.gen = n
}
