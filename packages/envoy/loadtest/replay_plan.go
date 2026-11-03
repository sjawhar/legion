package main

// The replay request source's plan (loadgen run -source replay): a replay bundle's logged writes
// (window.jsonl) become the requests the dispatch CLI 10.25.1 sends for each one, at their offsets
// from the bundle's instant, with read actions before each write at a stated assumption. Planning
// is pure: one bundle, one set of inputs and one rng seed give one plan (planReplay), which loadgen
// replay-plan prints without a stack. Sending the plan is replay_run.go.
//
// Nothing here logs a body, title, quote or document text: routes are templates, failures are
// statuses, counts are counts.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReplayReadMix = "read=0.55,docread=0.30,commentref=0.10,search=0.05"
	// replayActionDeadline bounds every request of one action together; no request is retried.
	replayActionDeadline = 60 * time.Second
	// replayDrainCap bounds the wait, after the window, for the actions still in flight.
	replayDrainCap = 300 * time.Second
	// replayIdleBefore is the interval before the window over which Dispatch's idle CPU is read.
	replayIdleBefore = 30 * time.Second
	// replayReadSpacing separates the read actions before a write: read j of n is sent
	// (n-j) x replayReadSpacing before its write, never before the sub-window's start.
	replayReadSpacing = time.Second
	// replayRosterEntryBytes is the listener registration body each roster session is sized to.
	replayRosterEntryBytes = 458
	// replayDiffCap is the length at which production cut an artifact.version diff.
	replayDiffCap = 4094
	// replayDaemonPrefix starts the actor id of the Legion daemon's writes.
	replayDaemonPrefix = "legion-daemon:"
)

// Action kinds: the write kinds (summary.json replay.writes) and the read kinds. Each request's
// route in summary.json and TABLES.md is "<method> <pattern> [<kind>]".
const (
	rkAnchored      = "anchored"
	rkReply         = "reply"
	rkPlain         = "plain"
	rkUpload        = "upload"
	rkEdit          = "edit"
	rkEditUnnamed   = "edit-unnamed"
	rkStatus        = "status"
	rkDaemon        = "daemon"
	rkFields        = "fields"
	rkDaemonMessage = "daemon-message"
	rkMessage       = "message"
	rkIssueCreate   = "issue-create"
	rkDocCreate     = "doc-create"
	rkFileCreate    = "file-create"
	rkFileVersion   = "file-version"
	rkResolve       = "resolve"
	rkAskOpen       = "ask-open"
	rkAskAnswer     = "ask-answer"
	rkAskResolve    = "ask-resolve"
	rkClaim         = "claim"

	rkRead       = "read"
	rkDocRead    = "docread"
	rkCommentRef = "commentref"
	rkSearch     = "search"
)

var replayReadKinds = []string{rkRead, rkDocRead, rkCommentRef, rkSearch}

// replayDerived are the event types the server derives from another write: never sent.
var replayDerived = map[string]bool{
	evChildStatus: true, evCommentAnchorRefreshed: true, evCommentReopened: true,
	evMessageDelivery: true, evMessageAccepted: true,
}

// --- inputs ----------------------------------------------------------------------------------------

// readMix is the read actions' mix: kinds in flag order with their shares (summing to 1).
type readMix struct {
	Kinds  []string
	Shares []float64
}

// parseReadMix reads kind=weight,... over the read kinds; weights are normalized to shares.
func parseReadMix(s string) (readMix, error) {
	var m readMix
	sum := 0.0
	for part := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return m, fmt.Errorf("-replay-read-mix: %q is not kind=weight", part)
		}
		if !slices.Contains(replayReadKinds, k) {
			return m, fmt.Errorf("-replay-read-mix: unknown read kind %q (kinds: %s)", k, strings.Join(replayReadKinds, ", "))
		}
		if slices.Contains(m.Kinds, k) {
			return m, fmt.Errorf("-replay-read-mix: %s is given twice", k)
		}
		w, err := strconv.ParseFloat(v, 64)
		if err != nil || !finite(w) || w < 0 {
			return m, fmt.Errorf("-replay-read-mix: %s's weight %q is not a number of 0 or more", k, v)
		}
		m.Kinds = append(m.Kinds, k)
		m.Shares = append(m.Shares, w)
		sum += w
	}
	if sum <= 0 {
		return m, fmt.Errorf("-replay-read-mix: the weights sum to 0")
	}
	for i := range m.Shares {
		m.Shares[i] /= sum
	}
	return m, nil
}

// pick is the kind at u in [0,1).
func (m readMix) pick(u float64) string {
	acc := 0.0
	for i, s := range m.Shares {
		acc += s
		if u < acc {
			return m.Kinds[i]
		}
	}
	return m.Kinds[len(m.Kinds)-1]
}

func (m readMix) asMap() map[string]float64 {
	out := map[string]float64{}
	for i, k := range m.Kinds {
		out[k] = m.Shares[i]
	}
	return out
}

// replayOptions are the replay's inputs.
type replayOptions struct {
	inputs   string
	rate     float64
	reads    float64
	mix      readMix
	uploads  float64
	from, to time.Duration
	roster   int
	keys     map[string]bool // nil: every issue
	// docTexts is the current markdown of logged documents, read from the build under test
	// before the window (readBaseTexts). The client models a document this map names from that
	// text, not from the bundle's: a build renders a stored document its own way, and an upload
	// sends the client's whole text. nil (loadgen replay-plan) keeps the bundle's text.
	docTexts map[string]string
	rngSeed  uint64
}

// replayFlags are the replay's flags, shared by loadgen run and loadgen replay-plan.
type replayFlags struct {
	inputs, mix, keysFile *string
	rate, reads, uploads  *float64
	from, to              *time.Duration
	roster                *int
}

func registerReplayFlags(fs *flag.FlagSet) *replayFlags {
	return &replayFlags{
		inputs:   fs.String("replay-inputs", "", "replay bundle directory (required with -source replay)"),
		rate:     fs.Float64("replay-rate", 0.05, "rate factor F: the share of issues replayed, whole (0 < F <= 1)"),
		reads:    fs.Float64("replay-reads", 1, "read actions before each write (a fraction is a Bernoulli draw)"),
		mix:      fs.String("replay-read-mix", defaultReplayReadMix, "read action mix kind=weight,... over "+strings.Join(replayReadKinds, ", ")),
		uploads:  fs.Float64("replay-uploads", 0.25, "share of named versions with a diff re-sent as a whole-document upload"),
		from:     fs.Duration("replay-from", 0, "sub-window start, from the bundle's instant"),
		to:       fs.Duration("replay-to", 39*time.Minute, "sub-window end, from the bundle's instant"),
		roster:   fs.Int("replay-roster", 762, "sessions registered with the listener (the bundle's roster first)"),
		keysFile: fs.String("replay-keys-file", "", "replay only the issues this file lists"),
	}
}

// options validates the flags, each refusal naming its input.
func (f *replayFlags) options(rngSeed uint64) (replayOptions, error) {
	o := replayOptions{inputs: *f.inputs, rate: *f.rate, reads: *f.reads, uploads: *f.uploads, from: *f.from, to: *f.to, roster: *f.roster, rngSeed: rngSeed}
	switch {
	case o.inputs == "":
		return o, fmt.Errorf("-replay-inputs is required with -source replay")
	case !finite(o.rate) || o.rate <= 0 || o.rate > 1:
		return o, fmt.Errorf("-replay-rate must be more than 0 and at most 1")
	case !finite(o.reads) || o.reads < 0:
		return o, fmt.Errorf("-replay-reads must be 0 or more")
	case !finite(o.uploads) || o.uploads < 0 || o.uploads > 1:
		return o, fmt.Errorf("-replay-uploads must be between 0 and 1")
	case o.from < 0:
		return o, fmt.Errorf("-replay-from must be 0 or more")
	case o.to <= o.from:
		return o, fmt.Errorf("-replay-to must be after -replay-from")
	case o.roster < 1:
		return o, fmt.Errorf("-replay-roster must be at least 1")
	}
	mix, err := parseReadMix(*f.mix)
	if err != nil {
		return o, err
	}
	o.mix = mix
	if *f.keysFile != "" {
		if o.keys, err = readKeysFile(*f.keysFile); err != nil {
			return o, fmt.Errorf("-replay-keys-file: %w", err)
		}
		if len(o.keys) == 0 {
			return o, fmt.Errorf("-replay-keys-file %s lists no issue", *f.keysFile)
		}
	}
	return o, nil
}

// loadReplay reads the bundle and plans the replay, refusing a sub-window past the bundle's.
func loadReplay(o replayOptions) (*replayBundle, *replayPlan, error) {
	b, err := readBundle(o.inputs)
	if err != nil {
		return nil, nil, fmt.Errorf("-replay-inputs: %w", err)
	}
	if span := b.State.Until.Sub(b.State.At); o.to > span {
		return nil, nil, fmt.Errorf("-replay-to %s is past the bundle's window of %s", o.to, span)
	}
	p, err := planReplay(b, o)
	if err != nil {
		return nil, nil, err
	}
	return b, p, nil
}

// --- the plan --------------------------------------------------------------------------------------

// replayRef names a logged object: kind is issue, artifact, comment or ask (replay_ids kinds).
type replayRef struct {
	Kind   string
	Logged string
}

// replayArg is a path placeholder's value: a local id (Ref), the dispatch:// URI of an issue (Ref,
// URI) or of a comment of it (Ref, Sub, URI), query-escaped; the last event sequence an earlier
// response of the action named, less 10 (After); or a literal.
type replayArg struct {
	Ref   *replayRef
	Sub   *replayRef
	URI   bool
	After bool
	Lit   string
}

// replayPrevEditedAt in a body is the edited_at an earlier response of the action carried (the
// ask an answer must name as expected_edited_at).
type replayPrevEditedAt struct{}

// replayFile is a multipart upload's file: deterministic bytes of the logged size.
type replayFile struct {
	Name, MIME, Seed string
	Size             int
	Actor            map[string]any
}

// replayProduce maps a response field to the logged object the request created.
type replayProduce struct {
	Ref   replayRef
	Field string // id, key or primary_artifact_id
}

// replayReq is one request: Path is Dispatch's pattern with {placeholders}, which Params fill.
type replayReq struct {
	Method   string
	Path     string
	Params   map[string]replayArg
	Body     map[string]any
	File     *replayFile
	Person   string
	Produces []replayProduce
	// Tolerate is a status that does not fail the action (the 404 of architecture-sources at
	// 35efd326).
	Tolerate int
}

// replayAction is one CLI call: its stages run in order, the requests of a stage in parallel.
type replayAction struct {
	Offset  time.Duration
	Issue   string // the logged issue key
	Kind    string
	Write   bool
	EventID int64
	// Doc is the logged document the action's write lands on (an anchored root, a reply or
	// resolve in an anchored thread, an edit, upload or create, an anchored ask, a typed ask's
	// answer or resolve); "" when none. The run sends a document's writes in plan order.
	Doc    string
	Stages [][]*replayReq
	// What the plan derived: a substituted anchor quote, an edit from the logged diff or a
	// substitute, a version's content (edit or upload) from a substitute.
	QuoteSub, EditDiff, EditSub, ContentSub bool
	// text is an edit's document text once its ops applied, as the client holds it: what Dispatch
	// then holds.
	text string
}

// route is the request's name in summary.json and TABLES.md.
func (r *replayReq) route(kind string) string { return r.Method + " " + r.Path + " [" + kind + "]" }

// refs are the logged objects the action's requests name.
func (a *replayAction) refs() []replayRef {
	seen := map[replayRef]bool{}
	var out []replayRef
	add := func(r *replayRef) {
		if r != nil && !seen[*r] {
			seen[*r] = true
			out = append(out, *r)
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case replayRef:
			add(&x)
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, st := range a.Stages {
		for _, r := range st {
			for _, k := range slices.Sorted(maps.Keys(r.Params)) {
				add(r.Params[k].Ref)
				add(r.Params[k].Sub)
			}
			walk(r.Body)
		}
	}
	return out
}

// replayPlan is the replay's actions in send order, with what planning counted.
type replayPlan struct {
	Project string
	Actions []*replayAction
	Counts  replayPlanCounts
	// IssueKeys are the kept issues that existed at the bundle's instant, in key order.
	IssueKeys []string
	// BaseDocs is the text each document the plan modelled started from, by logged id: the
	// bundle's, or the build's own when the run read it (readBaseTexts). Documents the window
	// creates are not in it, since no stored text precedes them.
	BaseDocs map[string]string
}

// replayPlanCounts is what loadgen replay-plan prints: counts only.
type replayPlanCounts struct {
	IssuesKept int `json:"issues_kept"`
	// TransactionsPlanned counts the kept, in-window transactions with an event to send.
	TransactionsPlanned int            `json:"transactions_planned"`
	Actions             int            `json:"actions"`
	WriteActions        int            `json:"write_actions"`
	ReadActions         int            `json:"read_actions"`
	Requests            int            `json:"requests"`
	WritesByKind        map[string]int `json:"writes_by_kind"`
	ReadsByKind         map[string]int `json:"reads_by_kind"`
	RequestsByRoute     map[string]int `json:"requests_by_route"`
	// NotSent counts the kept, in-window events no request stands for, by reason.
	NotSent map[string]int `json:"not_sent"`
	// DroppedTransactions and OutsideWindow count the window's transactions sampling dropped and those
	// outside [from, to).
	DroppedTransactions int               `json:"dropped_transactions"`
	OutsideWindow       int               `json:"outside_window_transactions"`
	Substitutes         replaySubstitutes `json:"substitutes"`
	Edits               replayEdits       `json:"edits"`
	// Unanchorable counts anchored roots sent unanchored: no usable quote in the client's text.
	Unanchorable int `json:"unanchorable"`
	// PatchUnchanged counts issue.updated writes no PATCH field changed in (a rank, claim or
	// components change): sent as a PATCH of the unchanged status.
	PatchUnchanged int `json:"patch_unchanged"`
	// SpecSizes counts created issues' specs by where their text came from: version1_text is the
	// spec's version 1 in created-docs/; otherwise a synthetic text of the first logged version's
	// size (first_version) or of the window's created documents' median first version
	// (created_docs_median).
	SpecSizes map[string]int `json:"spec_sizes"`
}

// --- planning --------------------------------------------------------------------------------------

// rpDoc is the client's copy of a document: its text and the logged size of its last version.
type rpDoc struct {
	text string
	size int
	file bool
}

type replayPlanner struct {
	b   *replayBundle
	o   replayOptions
	p   *replayPlan
	at  time.Time
	key func(string) bool

	artifacts     map[string]*replayArtifact // state artifacts by logged id
	docs          map[string]*rpDoc
	issueDocs     map[string][]string // logged document ids by issue key, files apart
	issueComments map[string][]string
	snap          map[string]map[string]json.RawMessage
	stateIssue    map[string]*replayIssue
	claim         map[string]*replayActor
	firstSize     map[string]int // the first logged version size of each artifact in the window
	// bornAt is the offset of the write that creates each comment and document the window creates:
	// a read sent before it never names the object.
	bornAt      map[string]time.Duration
	specDefault int
	// threadDoc is the document of each anchored thread's comments (its root's anchor), by
	// logged comment id; askDoc the document of each typed ask block, by logged ask id.
	threadDoc map[string]string
	askDoc    map[string]string
}

// planReplay plans the bundle's window under o.
func planReplay(b *replayBundle, o replayOptions) (*replayPlan, error) {
	pl := &replayPlanner{
		b: b, o: o, at: b.State.At,
		p: &replayPlan{Project: b.State.Project, BaseDocs: map[string]string{}, Counts: replayPlanCounts{
			WritesByKind: map[string]int{}, ReadsByKind: map[string]int{}, RequestsByRoute: map[string]int{},
			NotSent: map[string]int{}, SpecSizes: map[string]int{},
		}},
		artifacts: map[string]*replayArtifact{}, docs: map[string]*rpDoc{}, issueDocs: map[string][]string{},
		issueComments: map[string][]string{}, snap: map[string]map[string]json.RawMessage{}, stateIssue: map[string]*replayIssue{},
		claim: map[string]*replayActor{}, firstSize: map[string]int{}, bornAt: map[string]time.Duration{},
		threadDoc: map[string]string{}, askDoc: map[string]string{},
	}
	pl.key = func(k string) bool { return keepIssue(o.rngSeed, k, o.rate) && (o.keys == nil || o.keys[k]) }
	kept := map[string]bool{}
	for i := range b.State.Issues {
		is := &b.State.Issues[i]
		if !is.Exists {
			continue
		}
		pl.stateIssue[is.Key] = is
		if is.Claim != nil {
			pl.claim[is.Key] = &is.Claim.Actor
		}
		for j := range is.Artifacts {
			a := &is.Artifacts[j]
			pl.artifacts[a.ID] = a
			if a.Kind == "doc" {
				pl.issueDocs[is.Key] = append(pl.issueDocs[is.Key], a.ID)
			}
		}
		for _, c := range is.Comments {
			pl.issueComments[is.Key] = append(pl.issueComments[is.Key], c.ID)
			if c.Anchor != nil && c.ReplyTo == nil && c.ThreadRootID == nil {
				pl.threadDoc[c.ID] = c.Anchor.ArtifactID
			}
		}
		for _, a := range is.Asks {
			if a.BlockID != nil && a.BlockArtifactID != nil {
				pl.askDoc[a.ID] = *a.BlockArtifactID
			}
		}
		if pl.key(is.Key) {
			kept[is.Key] = true
			pl.p.IssueKeys = append(pl.p.IssueKeys, is.Key)
		}
	}
	// A reply's thread is its root's.
	for i := range b.State.Issues {
		for _, c := range b.State.Issues[i].Comments {
			root := c.ThreadRootID
			if root == nil {
				root = c.ReplyTo
			}
			if root != nil && pl.threadDoc[*root] != "" {
				pl.threadDoc[c.ID] = pl.threadDoc[*root]
			}
		}
	}
	sort.Strings(pl.p.IssueKeys)

	events := slices.Clone(b.Window)
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].CreatedAt.Before(events[j].CreatedAt)
		}
		return events[i].ID < events[j].ID
	})
	// Sizes the plan needs before it reaches them: each artifact's first logged version size, and
	// the median first version of the window's created documents (a created spec's default).
	var createdSizes []int
	for _, ev := range events {
		switch ev.Type {
		case evArtifactVersion:
			var v rpVersion
			if json.Unmarshal(ev.Payload, &v) == nil && v.Version.Size != nil {
				if _, ok := pl.firstSize[v.ArtifactID]; !ok {
					pl.firstSize[v.ArtifactID] = *v.Version.Size
				}
			}
		case evArtifactCreated:
			var c rpArtifactCreated
			if json.Unmarshal(ev.Payload, &c) == nil && c.Artifact.Kind == "doc" && len(c.Artifact.Versions) > 0 && c.Artifact.Versions[0].Size != nil {
				createdSizes = append(createdSizes, *c.Artifact.Versions[0].Size)
			}
		}
	}
	pl.specDefault = 4096
	if len(createdSizes) > 0 {
		slices.Sort(createdSizes)
		pl.specDefault = createdSizes[len(createdSizes)/2]
	}

	// One logged write is one transaction: the events of one actor at one instant.
	type group struct{ evs []replayEvent }
	var groups []*group
	byKey := map[string]*group{}
	for _, ev := range events {
		k := ev.CreatedAt.Format(time.RFC3339Nano) + "|" + ev.Actor.ID
		g := byKey[k]
		if g == nil {
			g = &group{}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.evs = append(g.evs, ev)
	}
	issuesWithWrites := map[string]bool{}
	for _, g := range groups {
		offset := g.evs[0].CreatedAt.Sub(pl.at)
		sendable := g.evs[:0:0]
		hasUpdate := false
		for _, ev := range g.evs {
			if ev.Type == evIssueUpdated {
				hasUpdate = true
			}
		}
		for _, ev := range g.evs {
			// issue.closed is its PATCH's event when no issue.updated shares the transaction.
			if replayDerived[ev.Type] || strings.HasPrefix(ev.Type, evPrefixUserState) || (ev.Type == evIssueClosed && hasUpdate) {
				continue
			}
			sendable = append(sendable, ev)
		}
		key := ""
		if len(sendable) > 0 && sendable[0].IssueKey != nil {
			key = *sendable[0].IssueKey
		} else if g.evs[0].IssueKey != nil {
			key = *g.evs[0].IssueKey
		}
		if key == "" || !pl.key(key) {
			pl.p.Counts.DroppedTransactions++
			continue
		}
		if offset < o.from || offset >= o.to {
			pl.p.Counts.OutsideWindow++
			continue
		}
		for _, ev := range g.evs {
			if !slices.ContainsFunc(sendable, func(s replayEvent) bool { return s.ID == ev.ID }) {
				pl.p.Counts.NotSent["derived:"+ev.Type]++
			}
		}
		if len(sendable) == 0 {
			continue
		}
		pl.p.Counts.TransactionsPlanned++
		for _, ev := range sendable {
			if ev.IssueKey == nil {
				pl.p.Counts.NotSent["no_issue:"+ev.Type]++
				continue
			}
			// The reads before a write are planned before it, so they never pick the comment or
			// document the write creates, nor see the claim it changes; an issue.created's reads
			// are on its parent.
			readsOn := *ev.IssueKey
			if ev.Type == evIssueCreated {
				readsOn = ""
				var is rpIssueCreated
				if json.Unmarshal(ev.Payload, &is) == nil && is.Parent != nil {
					readsOn = *is.Parent
				}
			}
			var reads []*replayAction
			if readsOn != "" {
				reads = pl.planReads(readsOn, ev.ID, offset)
			}
			act, reason, err := pl.planWrite(ev, offset)
			if err != nil {
				return nil, err
			}
			if act == nil {
				pl.p.Counts.NotSent[reason]++
				continue
			}
			for _, r := range reads {
				pl.add(r)
			}
			pl.add(act)
			issuesWithWrites[act.Issue] = true
			if act.Kind == rkIssueCreate {
				kept[act.Issue] = true
			}
		}
	}
	pl.p.Counts.IssuesKept = len(kept)
	sort.SliceStable(pl.p.Actions, func(i, j int) bool { return pl.p.Actions[i].Offset < pl.p.Actions[j].Offset })
	for _, a := range pl.p.Actions {
		for _, st := range a.Stages {
			for _, r := range st {
				pl.p.Counts.Requests++
				pl.p.Counts.RequestsByRoute[r.route(a.Kind)]++
			}
		}
	}
	pl.p.Counts.Actions = len(pl.p.Actions)
	return pl.p, nil
}

func (pl *replayPlanner) add(a *replayAction) {
	pl.p.Actions = append(pl.p.Actions, a)
	c := &pl.p.Counts
	if a.Write {
		c.WriteActions++
		c.WritesByKind[a.Kind]++
	} else {
		c.ReadActions++
		c.ReadsByKind[a.Kind]++
	}
	a.tally(&c.Substitutes, &c.Edits)
}

// tally counts the action's substitutes and edit kinds into s and e.
func (a *replayAction) tally(s *replaySubstitutes, e *replayEdits) {
	if a.QuoteSub {
		s.Quote++
	}
	if a.ContentSub {
		s.Edit++
	}
	if a.EditDiff {
		e.Diff++
	}
	if a.EditSub {
		e.Substitute++
	}
}

// replayUnit is a uniform draw in [0,1) from the rng seed and the parts: FNV-1a 64 through the
// splitmix64 finalizer, as keepIssue.
func replayUnit(rngSeed uint64, parts ...string) float64 {
	return float64(replayHash(rngSeed, parts...)>>11) / float64(1<<53)
}

func replayHash(rngSeed uint64, parts ...string) uint64 {
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], rngSeed)
	_, _ = h.Write(seed[:])
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return splitmix64(h.Sum64())
}

// splitmix64 is the splitmix64 finalizer.
func splitmix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// doc is the client's copy of a logged document, read on first use from the build's own text when
// the run read it (o.docTexts), else from the bundle; nil when the bundle has none.
func (pl *replayPlanner) doc(id string) (*rpDoc, error) {
	if d, ok := pl.docs[id]; ok {
		return d, nil
	}
	a := pl.artifacts[id]
	if a == nil {
		return nil, nil
	}
	d := &rpDoc{file: a.Kind != "doc"}
	if a.Size != nil {
		d.size = *a.Size
	}
	if n := len(a.VersionSizes); n > 0 && a.VersionSizes[n-1] >= 0 {
		d.size = a.VersionSizes[n-1]
	}
	if !d.file {
		if a.TextPath == "" {
			return nil, nil
		}
		if text, ok := pl.o.docTexts[a.ID]; ok {
			d.text = text
		} else {
			raw, err := pl.b.readFile(a.TextPath)
			if err != nil {
				return nil, fmt.Errorf("bundle document %s: %w", a.ID, err)
			}
			d.text = string(raw)
		}
		if a.Size == nil && len(a.VersionSizes) == 0 {
			d.size = len(d.text)
		}
		pl.p.BaseDocs[a.ID] = d.text
	}
	pl.docs[id] = d
	return d, nil
}

// --- payloads --------------------------------------------------------------------------------------

type rpAnchor struct {
	ArtifactID string  `json:"artifact_id"`
	Quote      string  `json:"quote"`
	BlockID    *string `json:"block_id"`
}

type rpComment struct {
	ID      string    `json:"id"`
	Body    string    `json:"body"`
	ReplyTo *string   `json:"reply_to"`
	AskID   *string   `json:"ask_id"`
	Anchor  *rpAnchor `json:"anchor"`
}

type rpVersion struct {
	ArtifactID string  `json:"artifact_id"`
	Name       string  `json:"name"`
	Diff       *string `json:"diff"`
	Version    struct {
		Named   bool    `json:"named"`
		Size    *int    `json:"size"`
		MIME    *string `json:"mime"`
		Summary *string `json:"summary"`
	} `json:"version"`
}

type rpArtifactCreated struct {
	Artifact struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Versions []struct {
			Size    *int    `json:"size"`
			MIME    *string `json:"mime"`
			Summary *string `json:"summary"`
		} `json:"versions"`
	} `json:"artifact"`
}

type rpIssueCreated struct {
	Title             string       `json:"title"`
	Parent            *string      `json:"parent"`
	PrimaryArtifactID string       `json:"primary_artifact_id"`
	Claim             *replayClaim `json:"claim"`
}

type rpMessage struct {
	Body string `json:"body"`
}

// rpClaimed is the claim an issue.claimed or issue.updated payload carries.
type rpClaimed struct {
	Claim *replayClaim `json:"claim"`
}

type rpAsk struct {
	ID       string            `json:"id"`
	Question string            `json:"question"`
	Kind     string            `json:"kind"`
	Options  []replayAskOption `json:"options"`
	Multiple bool              `json:"multiple"`
	Urgency  string            `json:"urgency"`
	Anchor   *rpAnchor         `json:"anchor"`
	// BlockID and BlockArtifact name a typed ask block and its document.
	BlockID       *string `json:"block_id"`
	BlockArtifact *struct {
		ID string `json:"id"`
	} `json:"block_artifact"`
	Answer     *replayAskAnswer `json:"answer"`
	Resolution *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"resolution"`
}

// replayPatchFields are the issue fields an issue.updated is re-sent with when they changed.
var replayPatchFields = []string{"title", "status", "labels", "priority", "route", "assignee", "parent", "blocked_by", "repo", "external_links"}

// --- writes ----------------------------------------------------------------------------------------

// actorFor is how a logged actor writes: a session through the bearer with the session in the
// body, a person through the identity header.
func actorFor(a replayActor) (person string, actor map[string]any) {
	if a.Kind == "user" {
		return a.ID, nil
	}
	actor = map[string]any{"kind": "session", "id": a.ID}
	if a.Origin != nil {
		actor["origin"] = a.Origin
	}
	return "", actor
}

func issueRef(key string) replayRef { return replayRef{Kind: idIssue, Logged: key} }

func refArg(r replayRef) replayArg { return replayArg{Ref: &r} }

func keyParams(key string) map[string]replayArg {
	return map[string]replayArg{"key": refArg(issueRef(key))}
}

func getIssue(key string) *replayReq {
	return &replayReq{Method: "GET", Path: "/api/v1/issues/{key}", Params: keyParams(key)}
}

// planWrite plans one logged write; a nil action is not sent, for the reason returned.
func (pl *replayPlanner) planWrite(ev replayEvent, offset time.Duration) (*replayAction, string, error) {
	key := *ev.IssueKey
	act := &replayAction{Offset: offset, Issue: key, Write: true, EventID: ev.ID}
	person, actor := actorFor(ev.Actor)
	eid := strconv.FormatInt(ev.ID, 10)
	switch ev.Type {
	case evCommentCreated:
		c, err := decodePayload[rpComment](ev)
		if err != nil {
			return nil, "", err
		}
		body := withActor(map[string]any{"body": c.Body}, actor)
		post := &replayReq{Method: "POST", Path: "/api/v1/issues/{key}/comments", Params: keyParams(key), Body: body, Person: person,
			Produces: []replayProduce{{Ref: replayRef{idComment, c.ID}, Field: "id"}}}
		switch {
		case c.ReplyTo != nil:
			act.Kind = rkReply
			body["reply_to"] = replayRef{idComment, *c.ReplyTo}
			act.Stages = [][]*replayReq{{post}}
			if doc := pl.threadDoc[*c.ReplyTo]; doc != "" {
				act.Doc, pl.threadDoc[c.ID] = doc, doc
			}
		case c.Anchor != nil:
			act.Kind = rkAnchored
			d, err := pl.doc(c.Anchor.ArtifactID)
			if err != nil {
				return nil, "", err
			}
			quote, sub := "", false
			if d != nil && !d.file {
				blockID := ""
				if c.Anchor.BlockID != nil {
					blockID = *c.Anchor.BlockID
				}
				quote, sub = anchorQuote(d.text, pl.b.State.BlockMaps[c.Anchor.ArtifactID], blockID, c.Anchor.Quote, pl.o.rngSeed, c.Anchor.ArtifactID, c.ID)
			}
			if quote == "" {
				pl.p.Counts.Unanchorable++
			} else {
				body["anchor"] = map[string]any{"artifact": replayRef{idArtifact, c.Anchor.ArtifactID}, "quote": quote}
				act.QuoteSub = sub
				act.Doc, pl.threadDoc[c.ID] = c.Anchor.ArtifactID, c.Anchor.ArtifactID
			}
			act.Stages = [][]*replayReq{{getIssue(key)}, {post}}
		default:
			act.Kind = rkPlain
			if c.AskID != nil {
				body["ask_id"] = replayRef{idAsk, *c.AskID}
			}
			act.Stages = [][]*replayReq{{post}}
		}
		pl.issueComments[key] = append(pl.issueComments[key], c.ID)
		pl.bornAt[c.ID] = offset
	case evArtifactVersion:
		v, err := decodePayload[rpVersion](ev)
		if err != nil {
			return nil, "", err
		}
		if ev.Actor.Kind == "user" {
			return nil, "browser:artifact.version", nil
		}
		act.Doc = v.ArtifactID
		d, err := pl.doc(v.ArtifactID)
		if err != nil {
			return nil, "", err
		}
		size := 0
		if v.Version.Size != nil {
			size = *v.Version.Size
		}
		if (d != nil && d.file) || v.Version.MIME != nil {
			act.Kind = rkFileVersion
			act.Stages = [][]*replayReq{{fileUpload(key, v.Name, v.Version.MIME, eid, size, actor, person, nil)}}
			if d != nil {
				d.size = size
			}
			return act, "", nil
		}
		if v.Version.Named && v.Diff == nil {
			return nil, "settlement:artifact.version", nil
		}
		if d == nil {
			return nil, "unknown_document:artifact.version", nil
		}
		delta := size - d.size
		diff := ""
		if v.Diff != nil {
			diff = *v.Diff
		}
		ops, next, fromDiff, ok := planEdit(d.text, diff, delta, pl.o.rngSeed, v.ArtifactID, eid)
		if !ok {
			return nil, "no_edit:artifact.version", nil
		}
		summary := ""
		if v.Version.Summary != nil {
			summary = *v.Version.Summary
		}
		act.ContentSub = !fromDiff
		upload := v.Version.Named && v.Diff != nil && replayUnit(pl.o.rngSeed, "upload", eid) < pl.o.uploads
		switch {
		case upload:
			act.Kind = rkUpload
			act.Stages = [][]*replayReq{{{Method: "POST", Path: "/api/v1/issues/{key}/artifacts", Params: keyParams(key), Person: person,
				Body: withActor(map[string]any{"name": v.Name, "content": next, "summary": summary}, actor)}}}
		default:
			act.Kind = rkEdit
			body := withActor(map[string]any{"ops": ops}, actor)
			if v.Version.Named {
				body["summary"] = summary
			} else {
				act.Kind = rkEditUnnamed
			}
			act.EditDiff, act.EditSub, act.text = fromDiff, !fromDiff, next
			act.Stages = [][]*replayReq{{getIssue(key)}, {{Method: "POST", Path: "/api/v1/artifacts/{id}/edits",
				Params: map[string]replayArg{"id": refArg(replayRef{idArtifact, v.ArtifactID})}, Person: person, Body: body}}}
		}
		d.text, d.size = next, size
	case evIssueUpdated, evIssueClosed:
		cur, err := decodePayload[map[string]json.RawMessage](ev)
		if err != nil {
			return nil, "", err
		}
		prev := pl.snap[key]
		if prev == nil {
			prev = stateSnapshot(pl.stateIssue[key])
		}
		body := map[string]any{}
		statusChanged := false
		for _, f := range replayPatchFields {
			was, known := prev[f]
			if !known {
				continue
			}
			now := cur[f]
			if sameJSON(was, now) {
				continue
			}
			if f == "parent" {
				var p *string
				_ = json.Unmarshal(now, &p)
				if p != nil {
					body[f] = issueRef(*p)
					continue
				}
			}
			body[f] = nullIfEmpty(now)
			statusChanged = statusChanged || f == "status"
		}
		if len(body) == 0 {
			pl.p.Counts.PatchUnchanged++
			body["status"] = nullIfEmpty(cur["status"])
		}
		withActor(body, actor)
		switch {
		case strings.HasPrefix(ev.Actor.ID, replayDaemonPrefix):
			act.Kind = rkDaemon
		case statusChanged && ev.Actor.Kind == "session":
			act.Kind = rkStatus
		default:
			act.Kind = rkFields
		}
		act.Stages = [][]*replayReq{{getIssue(key)}, {{Method: "PATCH", Path: "/api/v1/issues/{key}", Params: keyParams(key), Person: person, Body: body}}}
		pl.snap[key] = cur
		var c rpClaimed
		if json.Unmarshal(ev.Payload, &c) == nil {
			pl.claim[key] = nil
			if c.Claim != nil {
				pl.claim[key] = &c.Claim.Actor
			}
		}
	case evIssueCreated:
		is, err := decodePayload[rpIssueCreated](ev)
		if err != nil {
			return nil, "", err
		}
		act.Kind = rkIssueCreate
		docID := is.PrimaryArtifactID
		if docID == "" {
			docID = pl.b.Meta.CreatedSpecs[key]
		}
		var spec string
		var size int
		if id, ok := pl.b.Meta.CreatedSpecs[key]; ok {
			raw, err := pl.b.readFile(bundleCreatedDir + "/" + id + ".md")
			if err != nil {
				return nil, "", fmt.Errorf("bundle created spec %s: %w", key, err)
			}
			spec, size = string(raw), len(raw)
			pl.p.Counts.SpecSizes["version1_text"]++
		} else {
			var ok bool
			if size, ok = pl.firstSize[docID]; ok {
				pl.p.Counts.SpecSizes["first_version"]++
			} else {
				size = pl.specDefault
				pl.p.Counts.SpecSizes["created_docs_median"]++
			}
			spec = replaySynthDoc(size, replayHash(pl.o.rngSeed, "spec", docID))
		}
		body := withActor(map[string]any{"project": pl.p.Project, "title": is.Title, "spec": spec, "force": true}, actor)
		if is.Parent != nil {
			body["parent"] = issueRef(*is.Parent)
		}
		project := map[string]replayArg{"project": {Lit: url.PathEscape(pl.p.Project)}}
		act.Stages = [][]*replayReq{
			{{Method: "POST", Path: "/api/v1/issues", Body: body, Person: person, Produces: []replayProduce{
				{Ref: issueRef(key), Field: "key"}, {Ref: replayRef{idArtifact, docID}, Field: "primary_artifact_id"}}}},
			{{Method: "GET", Path: "/api/v1/projects/{project}/architecture-sources", Params: project, Tolerate: http.StatusNotFound}},
			{{Method: "GET", Path: "/api/v1/projects/{project}/architecture-source", Params: project}},
		}
		act.Doc = docID
		pl.docs[docID] = &rpDoc{text: spec, size: size}
		pl.issueDocs[key] = append(pl.issueDocs[key], docID)
		pl.bornAt[docID] = offset
		var snap map[string]json.RawMessage
		_ = json.Unmarshal(ev.Payload, &snap)
		pl.snap[key] = snap
		if is.Claim != nil {
			pl.claim[key] = &is.Claim.Actor
		}
	case evArtifactCreated:
		c, err := decodePayload[rpArtifactCreated](ev)
		if err != nil {
			return nil, "", err
		}
		a := c.Artifact
		size, summary := 0, ""
		var mime *string
		if len(a.Versions) > 0 {
			if a.Versions[0].Size != nil {
				size = *a.Versions[0].Size
			}
			if a.Versions[0].Summary != nil {
				summary = *a.Versions[0].Summary
			}
			mime = a.Versions[0].MIME
		}
		produce := []replayProduce{{Ref: replayRef{idArtifact, a.ID}, Field: "id"}}
		act.Doc = a.ID
		if a.Kind != "doc" {
			act.Kind = rkFileCreate
			act.Stages = [][]*replayReq{{fileUpload(key, a.Name, mime, a.ID, size, actor, person, produce)}}
			pl.docs[a.ID] = &rpDoc{file: true, size: size}
			break
		}
		act.Kind = rkDocCreate
		text := ""
		if raw, err := pl.b.readFile(bundleCreatedDir + "/" + a.ID + ".md"); err == nil {
			text = string(raw)
		} else if errors.Is(err, os.ErrNotExist) {
			text = replaySynthDoc(size, replayHash(pl.o.rngSeed, "created", a.ID))
			act.ContentSub = true
		} else {
			return nil, "", fmt.Errorf("bundle created document %s: %w", a.ID, err)
		}
		body := withActor(map[string]any{"name": a.Name, "content": text}, actor)
		if summary != "" {
			body["summary"] = summary
		}
		act.Stages = [][]*replayReq{{{Method: "POST", Path: "/api/v1/issues/{key}/artifacts", Params: keyParams(key), Person: person, Body: body, Produces: produce}}}
		pl.docs[a.ID] = &rpDoc{text: text, size: size}
		pl.issueDocs[key] = append(pl.issueDocs[key], a.ID)
		pl.bornAt[a.ID] = offset
	case evCommentResolved:
		c, err := decodePayload[rpComment](ev)
		if err != nil {
			return nil, "", err
		}
		act.Kind = rkResolve
		r := &replayReq{Method: "POST", Path: "/api/v1/comments/{id}/resolve", Params: map[string]replayArg{"id": refArg(replayRef{idComment, c.ID})}, Person: person}
		if actor != nil {
			r.Body = map[string]any{"actor": actor}
		}
		act.Stages = [][]*replayReq{{r}}
		act.Doc = pl.threadDoc[c.ID]
	case evMessageCreated:
		m, err := decodePayload[rpMessage](ev)
		if err != nil {
			return nil, "", err
		}
		post := &replayReq{Method: "POST", Path: "/api/v1/issues/{key}/messages", Params: keyParams(key), Person: person,
			Body: withActor(map[string]any{"body": m.Body}, actor)}
		if strings.HasPrefix(ev.Actor.ID, replayDaemonPrefix) {
			act.Kind = rkDaemonMessage
			act.Stages = [][]*replayReq{{{Method: "GET", Path: "/api/v1/issues/{key}/events?order=desc&limit=200", Params: keyParams(key)}}, {post}}
		} else {
			act.Kind = rkMessage
			act.Stages = [][]*replayReq{{post}}
		}
	case evAskOpened, evAskAnswered, evAskResolved:
		a, err := decodePayload[rpAsk](ev)
		if err != nil {
			return nil, "", err
		}
		askParams := map[string]replayArg{"id": refArg(replayRef{idAsk, a.ID})}
		if a.BlockID != nil && a.BlockArtifact != nil {
			pl.askDoc[a.ID] = a.BlockArtifact.ID
		}
		switch ev.Type {
		case evAskOpened:
			act.Kind = rkAskOpen
			options := make([]any, 0, len(a.Options))
			for _, o := range a.Options {
				options = append(options, map[string]any{"label": o.Label, "description": o.Description})
			}
			body := withActor(map[string]any{"question": a.Question, "kind": a.Kind, "options": options, "multiple": a.Multiple, "urgency": a.Urgency}, actor)
			if a.Anchor != nil {
				body["anchor"] = map[string]any{"artifact": replayRef{idArtifact, a.Anchor.ArtifactID}, "quote": a.Anchor.Quote}
				act.Doc = a.Anchor.ArtifactID
			}
			act.Stages = [][]*replayReq{{{Method: "POST", Path: "/api/v1/issues/{key}/asks", Params: keyParams(key), Person: person, Body: body,
				Produces: []replayProduce{{Ref: replayRef{idAsk, a.ID}, Field: "id"}}}}}
		case evAskAnswered:
			act.Kind = rkAskAnswer
			who := ev.Actor.ID
			body := map[string]any{"selected": []string{}, "expected_edited_at": replayPrevEditedAt{}}
			if a.Answer != nil {
				who = a.Answer.User
				if a.Answer.Selected != nil {
					body["selected"] = a.Answer.Selected
				}
				if a.Answer.Text != nil {
					body["text"] = *a.Answer.Text
				}
			}
			act.Stages = [][]*replayReq{
				{{Method: "GET", Path: "/api/v1/asks/{id}", Params: askParams, Person: who}},
				{{Method: "POST", Path: "/api/v1/asks/{id}/answer", Params: askParams, Person: who, Body: body}},
			}
			act.Doc = pl.askDoc[a.ID]
		default:
			act.Kind = rkAskResolve
			body := map[string]any{}
			if a.Resolution != nil {
				body["kind"], body["reason"] = a.Resolution.Kind, a.Resolution.Reason
			}
			act.Stages = [][]*replayReq{{{Method: "POST", Path: "/api/v1/asks/{id}/resolve", Params: askParams, Person: person, Body: withActor(body, actor)}}}
			act.Doc = pl.askDoc[a.ID]
		}
	case evIssueClaimed:
		c, err := decodePayload[rpClaimed](ev)
		if err != nil {
			return nil, "", err
		}
		act.Kind = rkClaim
		claimer := ev.Actor
		if c.Claim != nil {
			claimer = c.Claim.Actor
			pl.claim[key] = &c.Claim.Actor
		}
		cp, ca := actorFor(claimer)
		act.Stages = [][]*replayReq{{{Method: "POST", Path: "/api/v1/issues/{key}/claim", Params: keyParams(key), Person: cp, Body: withActor(map[string]any{}, ca)}}}
	default:
		return nil, "unhandled:" + ev.Type, nil
	}
	return act, "", nil
}

// fileUpload is a multipart upload of deterministic bytes of the logged size.
func fileUpload(key, name string, mime *string, seed string, size int, actor map[string]any, person string, produce []replayProduce) *replayReq {
	m := "application/octet-stream"
	if mime != nil && *mime != "" {
		m = *mime
	}
	return &replayReq{Method: "POST", Path: "/api/v1/issues/{key}/artifacts", Params: keyParams(key), Person: person, Produces: produce,
		File: &replayFile{Name: name, MIME: m, Seed: seed, Size: size, Actor: actor}}
}

// stateSnapshot is the issue's fields at the bundle's instant, those the state carries.
func stateSnapshot(is *replayIssue) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if is == nil {
		return out
	}
	put := func(k string, v any) {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	put("title", is.Title)
	put("status", is.Status)
	put("parent", is.Parent)
	put("priority", is.Priority)
	put("labels", nonNil(is.Labels))
	put("route", is.Route)
	put("blocked_by", nonNil(is.BlockedBy))
	return out
}

// sameJSON compares two JSON values by their compact bytes; absent is null.
func sameJSON(a, b json.RawMessage) bool {
	norm := func(x json.RawMessage) string {
		if len(bytes.TrimSpace(x)) == 0 {
			return "null"
		}
		var buf bytes.Buffer
		if json.Compact(&buf, x) != nil {
			return string(x)
		}
		return buf.String()
	}
	return norm(a) == norm(b)
}

func nullIfEmpty(x json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(x)) == 0 {
		return json.RawMessage("null")
	}
	return x
}

// anchorQuote is the quote an anchored root comment sends: the logged quote when the client's text
// holds it exactly once and its rendered text (renderedLine) occurs at most once in the document's
// (a quote Dispatch matches in two places is 409 TARGET_AMBIGUOUS), else substituteAnchor's; ""
// when the text has no usable quote.
func anchorQuote(text string, bm *replayBlockMap, blockID, quote string, rngSeed uint64, docID, itemID string) (string, bool) {
	if quote != "" && strings.Count(text, quote) == 1 && strings.Count(scanMarkdown(text).rendered, renderedLine(quote)) <= 1 {
		return quote, false
	}
	q, _, err := substituteAnchor(text, bm, blockID, quote, rngSeed, docID, itemID)
	if err != nil {
		return "", false
	}
	return q, true
}

// replaySynthDoc is n bytes of synthetic markdown: numbered sentences of synthetic words in
// paragraphs, so fallbackCandidates finds unique sentences in it.
func replaySynthDoc(n int, salt uint64) string {
	if n <= 0 {
		return ""
	}
	r := rand.New(rand.NewPCG(salt, 0x7265706c6179))
	var b strings.Builder
	for k := 0; b.Len() < n; k++ {
		if k > 0 {
			if k%5 == 0 {
				b.WriteString("\n\n")
			} else {
				b.WriteByte(' ')
			}
		}
		fmt.Fprintf(&b, "Replay note %d", k)
		for range 6 + r.IntN(6) {
			b.WriteByte(' ')
			b.WriteString(words[r.IntN(len(words))])
		}
		b.WriteByte('.')
	}
	s := b.String()[:n]
	if last := s[len(s)-1]; last == ' ' || last == '\n' {
		s = s[:n-1] + "x"
	}
	return s
}

// --- reads -----------------------------------------------------------------------------------------

// planReads plans the read actions before a write on key at offset: the integer part of the reads
// per write plus a Bernoulli draw for its fraction, each of a kind drawn from the mix.
func (pl *replayPlanner) planReads(key string, eventID int64, offset time.Duration) []*replayAction {
	eid := strconv.FormatInt(eventID, 10)
	whole, frac := math.Modf(pl.o.reads)
	n := int(whole)
	if frac > 0 && replayUnit(pl.o.rngSeed, "reads", eid) < frac {
		n++
	}
	reads := make([]*replayAction, 0, n)
	for j := range n {
		u := replayUnit(pl.o.rngSeed, "read-kind", eid, strconv.Itoa(j))
		at := max(pl.o.from, offset-time.Duration(n-j)*replayReadSpacing)
		reads = append(reads, pl.readAction(key, pl.o.mix.pick(u), eid, j, at))
	}
	return reads
}

func (pl *replayPlanner) readAction(key, kind, eid string, j int, at time.Duration) *replayAction {
	act := &replayAction{Offset: at, Issue: key, Kind: kind}
	pick := func(ids []string, what string) string {
		// ids are in log order: those born at or after the read are a suffix.
		n := len(ids)
		for n > 0 {
			if born, ok := pl.bornAt[ids[n-1]]; !ok || born < at {
				break
			}
			n--
		}
		if n == 0 {
			return ""
		}
		return ids[int(replayHash(pl.o.rngSeed, what, eid, strconv.Itoa(j))%uint64(n))]
	}
	params := keyParams(key)
	issueURI := map[string]replayArg{"key_ref": {Ref: new(issueRef(key)), URI: true}}
	switch kind {
	case rkDocRead:
		if doc := pick(pl.issueDocs[key], "docread"); doc != "" {
			withDoc := map[string]replayArg{"key": params["key"], "id": refArg(replayRef{idArtifact, doc})}
			act.Stages = [][]*replayReq{{getIssue(key)}, {
				{Method: "GET", Path: "/api/v1/artifacts/{id}/text", Params: withDoc},
				{Method: "GET", Path: "/api/v1/issues/{key}/comments?artifact={id}", Params: withDoc},
			}}
			return act
		}
	case rkCommentRef:
		if c := pick(pl.issueComments[key], "commentref"); c != "" {
			cref := replayRef{idComment, c}
			uri := map[string]replayArg{"comment_ref": {Ref: new(issueRef(key)), Sub: &cref, URI: true}}
			act.Stages = [][]*replayReq{
				{{Method: "GET", Path: "/api/v1/comments/{id}", Params: map[string]replayArg{"id": refArg(cref)}}},
				{{Method: "GET", Path: "/api/v1/references?to={comment_ref}", Params: uri}, {Method: "GET", Path: "/api/v1/references?from={comment_ref}", Params: uri}},
			}
			return act
		}
	case rkSearch:
		h := replayHash(pl.o.rngSeed, "search", eid, strconv.Itoa(j))
		q := words[h%uint64(len(words))] + "+" + words[(h>>20)%uint64(len(words))]
		act.Stages = [][]*replayReq{{{Method: "GET", Path: "/api/v1/search?q={q}&project={project}&limit=20&offset=0",
			Params: map[string]replayArg{"q": {Lit: q}, "project": {Lit: url.QueryEscape(pl.p.Project)}}}}}
		return act
	}
	// read, and a docread or commentref on an issue with no document or comment to read.
	act.Kind = rkRead
	last := []*replayReq{{Method: "GET", Path: "/api/v1/references?to={key_ref}", Params: issueURI}, {Method: "GET", Path: "/api/v1/references?from={key_ref}", Params: issueURI}}
	if c := pl.claim[key]; c != nil && c.Kind == "session" {
		last = append(last, &replayReq{Method: "GET", Path: "/api/v1/agents"})
	}
	act.Stages = [][]*replayReq{
		{getIssue(key), {Method: "GET", Path: "/api/v1/issues/{key}/references", Params: params}},
		{{Method: "GET", Path: "/api/v1/issues/{key}/events?after={after}&limit=10", Params: map[string]replayArg{"key": params["key"], "after": {After: true}}}},
		last,
	}
	return act
}

// --- loadgen replay-plan ---------------------------------------------------------------------------

// replayPlanOutput is what loadgen replay-plan prints.
type replayPlanOutput struct {
	Inputs replayInputs     `json:"inputs"`
	Counts replayPlanCounts `json:"counts"`
}

func runReplayPlan(args []string) error {
	fs := flag.NewFlagSet("replay-plan", flag.ContinueOnError)
	rf := registerReplayFlags(fs)
	rngSeed := fs.Uint64("rng-seed", 1, "seed of the issue sampling, the reads and the upload split")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	o, err := rf.options(*rngSeed)
	if err != nil {
		return err
	}
	b, p, err := loadReplay(o)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(replayPlanOutput{Inputs: o.inputsRecord(b), Counts: p.Counts})
}

// inputsRecord is the inputs as summary.json records them; the stack's fields stay empty.
func (o replayOptions) inputsRecord(b *replayBundle) replayInputs {
	return replayInputs{RateFactor: o.rate, ReadsPerWrite: o.reads, ReadMix: o.mix.asMap(), UploadShare: o.uploads, RNGSeed: o.rngSeed,
		FromS: o.from.Seconds(), ToS: o.to.Seconds(), BundleSHA256: b.MetaSHA256}
}
