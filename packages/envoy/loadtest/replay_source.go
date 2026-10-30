package main

// The replay request source (loadgen run -source replay): it re-sends a replay bundle's logged
// writes (window.jsonl) at their offsets from the bundle's instant, as the dispatch CLI 10.25.1
// sends each one, with read actions before each write at a stated assumption. Planning is pure:
// one bundle, one set of inputs and one rng seed give one plan (planReplay), which loadgen
// replay-plan prints without a stack. Sending (runReplay) resolves each logged id to the local one
// (loadtest.replay_ids, or the response that created the object in this run) and sends each action
// on its own connection at winStart + (offset - from).
//
// Nothing here logs a body, title, quote or document text: routes are templates, failures are
// statuses, counts are counts.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
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
	"child.status": true, "comment.anchor_refreshed": true, "comment.reopened": true,
	"message.delivery": true, "message.accepted": true,
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
		case "artifact.version":
			var v rpVersion
			if json.Unmarshal(ev.Payload, &v) == nil && v.Version.Size != nil {
				if _, ok := pl.firstSize[v.ArtifactID]; !ok {
					pl.firstSize[v.ArtifactID] = *v.Version.Size
				}
			}
		case "artifact.created":
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
			if ev.Type == "issue.updated" {
				hasUpdate = true
			}
		}
		for _, ev := range g.evs {
			// issue.closed is its PATCH's event when no issue.updated shares the transaction.
			if replayDerived[ev.Type] || strings.HasPrefix(ev.Type, "user_state.") || (ev.Type == "issue.closed" && hasUpdate) {
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
			if ev.Type == "issue.created" {
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
	case "comment.created":
		var c rpComment
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
	case "artifact.version":
		var v rpVersion
		if err := json.Unmarshal(ev.Payload, &v); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
	case "issue.updated", "issue.closed":
		var cur map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &cur); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
		var c struct {
			Claim *replayClaim `json:"claim"`
		}
		if json.Unmarshal(ev.Payload, &c) == nil {
			pl.claim[key] = nil
			if c.Claim != nil {
				pl.claim[key] = &c.Claim.Actor
			}
		}
	case "issue.created":
		var is rpIssueCreated
		if err := json.Unmarshal(ev.Payload, &is); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
	case "artifact.created":
		var c rpArtifactCreated
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
	case "comment.resolved":
		var c rpComment
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
		}
		act.Kind = rkResolve
		r := &replayReq{Method: "POST", Path: "/api/v1/comments/{id}/resolve", Params: map[string]replayArg{"id": refArg(replayRef{idComment, c.ID})}, Person: person}
		if actor != nil {
			r.Body = map[string]any{"actor": actor}
		}
		act.Stages = [][]*replayReq{{r}}
		act.Doc = pl.threadDoc[c.ID]
	case "message.created":
		var m rpMessage
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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
	case "ask.opened", "ask.answered", "ask.resolved":
		var a rpAsk
		if err := json.Unmarshal(ev.Payload, &a); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
		}
		askParams := map[string]replayArg{"id": refArg(replayRef{idAsk, a.ID})}
		if a.BlockID != nil && a.BlockArtifact != nil {
			pl.askDoc[a.ID] = a.BlockArtifact.ID
		}
		switch ev.Type {
		case "ask.opened":
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
		case "ask.answered":
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
	case "issue.claimed":
		var c struct {
			Claim *replayClaim `json:"claim"`
		}
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, "", fmt.Errorf("event %d: %w", ev.ID, err)
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

// --- sending ---------------------------------------------------------------------------------------

// replaySetup is what loadgen run takes from the bundle before the step: the plan, the roster and
// the inputs record.
type replaySetup struct {
	opt    replayOptions
	plan   *replayPlan
	inputs replayInputs
	// bundle is the plan's bundle, kept so the run can replan it against the build's own
	// document texts (readBaseTexts).
	bundle *replayBundle
	// roster is the session ids registered with the listener: the bundle's roster ids the listener
	// accepts as keys (natsKey), then synthetic ones; rosterSkipped counts the roster ids it would
	// refuse (an actor id such as legion-daemon:LINUX is no session id); homes maps a roster session
	// to the kept issue its claim holds at the instant.
	roster        []string
	rosterSkipped int
	homes         map[string]string
}

// natsKeyAlphabet is the alphabet of a NATS KV key nats.go accepts (its validKeyRe); a key also
// neither starts nor ends with a dot. The listener keys a session by its id and refuses any other
// (internal/bus ErrInvalidKey).
var natsKeyAlphabet = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)

// natsKey says whether the listener accepts id as a session's key.
func natsKey(id string) bool {
	return natsKeyAlphabet.MatchString(id) && !strings.HasPrefix(id, ".") && !strings.HasSuffix(id, ".")
}

func newReplaySetup(o replayOptions) (*replaySetup, error) {
	b, p, err := loadReplay(o)
	if err != nil {
		return nil, err
	}
	s := &replaySetup{opt: o, plan: p, inputs: o.inputsRecord(b), bundle: b, homes: map[string]string{}}
	for _, r := range b.Roster {
		if len(s.roster) == o.roster {
			break
		}
		if !natsKey(r.SessionID) {
			s.rosterSkipped++
			continue
		}
		s.roster = append(s.roster, r.SessionID)
	}
	for i := len(s.roster); i < o.roster; i++ {
		s.roster = append(s.roster, fmt.Sprintf("ses-replay-%04d", i))
	}
	keys := map[string]bool{}
	for _, k := range p.IssueKeys {
		keys[k] = true
	}
	for i := range b.State.Issues {
		is := &b.State.Issues[i]
		if is.Exists && is.Claim != nil && is.Claim.Actor.Kind == "session" && keys[is.Key] {
			if _, ok := s.homes[is.Claim.Actor.ID]; !ok {
				s.homes[is.Claim.Actor.ID] = is.Key
			}
		}
	}
	if len(p.IssueKeys) == 0 {
		// No existing issue kept: the sessions watch the bundle's issues instead.
		for i := range b.State.Issues {
			if b.State.Issues[i].Exists {
				p.IssueKeys = append(p.IssueKeys, b.State.Issues[i].Key)
			}
		}
	}
	return s, nil
}

// newReplayStep lays out the roster: session i registers and subscribes its home issue (the issue
// its claim holds, else issue i mod the kept issues) and further issues 37 apart until its
// registration body reaches about replayRosterEntryBytes.
func newReplayStep(cfg runConfig, lg *log.Logger, nonce int64) *step {
	rs := cfg.replay
	sf := seedFile{Projects: []string{rs.plan.Project}}
	index := map[string]int{}
	for i, k := range rs.plan.IssueKeys {
		sf.Issues = append(sf.Issues, seedIssue{Key: k, Project: rs.plan.Project})
		index[k] = i
	}
	n := len(sf.Issues)
	st := &step{cfg: cfg, sf: sf, lg: lg, nonce: nonce, active: n, sessions: make([]session, len(rs.roster)), watchers: make([][]int, n)}
	for i, id := range rs.roster {
		if n == 0 {
			st.sessions[i] = session{id: id}
			continue
		}
		home := i % n
		if k, ok := rs.homes[id]; ok {
			home = index[k]
		}
		s := session{id: id, home: home, watched: []int{home}}
		size := len(st.registrationBody(s))
		for k := 1; k < n && size < replayRosterEntryBytes; k++ {
			w := (home + k*37) % n
			if slices.Contains(s.watched, w) {
				continue
			}
			next := append(slices.Clone(s.watched), w)
			nb := len(st.registrationBody(session{id: id, watched: next}))
			if nb > replayRosterEntryBytes+20 {
				break
			}
			s.watched, size = next, nb
		}
		for _, w := range s.watched {
			st.watchers[w] = append(st.watchers[w], i)
		}
		st.sessions[i] = s
	}
	return st
}

// replayFuture is a logged object the run creates: done closes once its action has finished.
type replayFuture struct {
	done  chan struct{}
	local string
	ok    bool
}

// replayTurn is an action's place in its document's write order: its write waits for prev (the
// previous planned write on the document; nil for the first), and done closes once it returned.
type replayTurn struct {
	prev <-chan struct{}
	done chan struct{}
}

// replayTurns chains each document's write actions in plan order.
func replayTurns(actions []*replayAction) map[*replayAction]*replayTurn {
	turns := map[*replayAction]*replayTurn{}
	last := map[string]chan struct{}{}
	for _, a := range actions {
		if !a.Write || a.Doc == "" {
			continue
		}
		t := &replayTurn{done: make(chan struct{}), prev: last[a.Doc]}
		last[a.Doc] = t.done
		turns[a] = t
	}
	return turns
}

// replayRun is one run's replay state.
type replayRun struct {
	setup   *replaySetup
	ids     map[[2]string]string // loadtest.replay_ids
	futures map[replayRef]*replayFuture
	pid     int
	pprof   string
	pg      string // the stack's Postgres URL
	out     string
	lg      *log.Logger
	// turns orders each document's writes (replayTurns); read-only once the run starts.
	turns map[*replayAction]*replayTurn

	mu      sync.Mutex
	writes  map[string]*replayWriteCount
	skipped map[string]int
	subs    replaySubstitutes
	edits   replayEdits
	// docWaits are the waits (s) of the writes that found the previous write on their document
	// still running; wrote the logged documents written to; endState what readEndState read.
	docWaits []float64
	wrote    map[string]bool
	endState *replayEndState
	// baseDocs records how the client's copy of the documents was matched to the build under
	// test (readBaseTexts); nil when the plan modelled no document.
	baseDocs *replayBaseDocs

	inflight      atomic.Int64
	timeouts      atomic.Int64
	inflightAtEnd int
	cpu           replayCPU
	meta          replayMetaRecord
	endRead       chan struct{}
}

// prepareReplay reads loadtest.replay_ids over the stack's Postgres URL, matches the client's copy
// of each document to the build under test (readBaseTexts) and records the run's inputs: the seed
// dump, Dispatch's GOMAXPROCS and build. It runs before the window opens, so its reads and any
// replan cost the measurement nothing.
func (st *step) prepareReplay(ctx context.Context, a *api) error {
	rs := st.cfg.replay
	rr := &replayRun{setup: rs, futures: map[replayRef]*replayFuture{}, pid: st.cfg.ep.pids["dispatch"], pprof: st.cfg.ep.pprof, pg: st.cfg.ep.pg,
		out: st.cfg.out, lg: st.lg, writes: map[string]*replayWriteCount{}, skipped: map[string]int{}, endRead: make(chan struct{}),
		wrote: map[string]bool{}}
	conn, err := pgx.Connect(ctx, st.cfg.ep.pg)
	if err != nil {
		return fmt.Errorf("replay: postgres: %w", err)
	}
	rr.ids, err = readReplayIDs(ctx, conn)
	conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("replay: read loadtest.replay_ids (is the replay seed restored?): %w", err)
	}
	if err := rr.readBaseTexts(ctx, a); err != nil {
		return err
	}
	rr.turns = replayTurns(rs.plan.Actions)
	for _, act := range rs.plan.Actions {
		if act.Write {
			wc := rr.writes[act.Kind]
			if wc == nil {
				wc = &replayWriteCount{}
				rr.writes[act.Kind] = wc
			}
			wc.Planned++
		}
		for _, stg := range act.Stages {
			for _, r := range stg {
				for _, p := range r.Produces {
					rr.futures[p.Ref] = &replayFuture{done: make(chan struct{})}
				}
			}
		}
	}
	rr.meta = collectReplayMeta(rs.inputs, rr.pid, loadtestRoot())
	st.lg.Printf("replay: %d actions (%d writes, %d reads), %d requests, %d issues kept; %d logged ids mapped; GOMAXPROCS %d",
		rs.plan.Counts.Actions, rs.plan.Counts.WriteActions, rs.plan.Counts.ReadActions, rs.plan.Counts.Requests, rs.plan.Counts.IssuesKept,
		len(rr.ids), rr.meta.Inputs.GOMAXPROCS)
	st.replay = rr
	return nil
}

// readBaseTexts matches the client's copy of each document the plan modelled to the build under
// test: it reads the document's current markdown (GET /api/v1/artifacts/{id}/text, the agent
// bearer) and replans against the texts that differ from the bundle's. A build renders a stored
// document its own way, so the markdown an older build served can hold a construct this one
// refuses (a table row whose cells past the table's width come from an unescaped pipe, say), and
// an upload sends the client's whole text; planning against the build's own text keeps the replay
// measuring the build instead of that difference. Where the build serves the bundle's text, every
// document included, nothing is replanned and the plan stays byte for byte the bundle's.
func (rr *replayRun) readBaseTexts(ctx context.Context, a *api) error {
	rs := rr.setup
	if len(rs.plan.BaseDocs) == 0 {
		return nil
	}
	rec := &replayBaseDocs{Documents: []string{}}
	texts := map[string]string{}
	for _, logged := range slices.Sorted(maps.Keys(rs.plan.BaseDocs)) {
		local, ok := rr.ids[[2]string{idArtifact, logged}]
		if !ok {
			rec.Unread++
			continue
		}
		res := a.callRetry(ctx, "GET", "/api/v1/artifacts/"+url.PathEscape(local)+"/text", who{}, nil)
		var got struct {
			Markdown string `json:"markdown"`
		}
		if !res.ok() || json.Unmarshal(res.body, &got) != nil {
			rec.Unread++
			rr.lg.Printf("replay: base text of document %s not read: status %d %s %v", logged, res.status, res.errorCode(), res.err)
			continue
		}
		rec.Read++
		if got.Markdown != rs.plan.BaseDocs[logged] {
			rec.Differed++
			rec.Documents = append(rec.Documents, logged)
			texts[logged] = got.Markdown
		}
	}
	if rec.Read == 0 {
		return fmt.Errorf("replay: no document of the plan's %d could be read from Dispatch (base texts)", len(rs.plan.BaseDocs))
	}
	if len(texts) > 0 {
		rs.opt.docTexts = texts
		p, err := planReplay(rs.bundle, rs.opt)
		if err != nil {
			return fmt.Errorf("replay: replan against the build's document texts: %w", err)
		}
		// The sample of issues does not depend on a document's text, and the roster already
		// subscribes the setup's keys (newReplaySetup's fallback included), so they are kept.
		p.IssueKeys = rs.plan.IssueKeys
		rs.plan, rec.Replanned = p, true
	}
	rr.baseDocs = rec
	rr.lg.Printf("replay: base texts of %d documents read (%d unread), %d differ from the bundle, replanned %v",
		rec.Read, rec.Unread, rec.Differed, rec.Replanned)
	return nil
}

// runReplay sends the plan: at the window start one /proc read of Dispatch and heap-start.pprof,
// then each action at winStart + (offset - from) on its own goroutine; at the window end
// heap-end.pprof and a /proc read, then the drain, then the end state of the documents written.
// done closes once every action has returned and the drain and the end state are read.
func (st *step) runReplay(ctx context.Context, a *api) (load *requestLoad, done <-chan struct{}) {
	rr := st.replay
	load = &requestLoad{recs: newCollector[reqRec](1 << 16)}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		defer func() {
			select {
			case <-rr.endRead:
			default:
				close(rr.endRead)
			}
		}()
		idleFrom := st.winStart.Add(-replayIdleBefore)
		if idleFrom.Before(st.start) {
			idleFrom = st.start
		}
		if !sleepUntil(ctx, idleFrom) {
			return
		}
		rr.cpu.idle = rr.readCPU()
		if !sleepUntil(ctx, st.winStart) {
			return
		}
		rr.cpu.start = rr.readCPU()
		rr.meta.HeapStartAt = time.Now()
		fetchTo(rr.pprof+"/debug/pprof/heap", rr.out+"/heap-start.pprof", 60*time.Second, rr.lg)

		actionsDone := make(chan struct{})
		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			if !sleepUntil(ctx, st.winEnd) {
				close(rr.endRead)
				return
			}
			rr.meta.HeapEndAt = time.Now()
			fetchTo(rr.pprof+"/debug/pprof/heap", rr.out+"/heap-end.pprof", 60*time.Second, rr.lg)
			rr.cpu.end = rr.readCPU()
			rr.inflightAtEnd = int(rr.inflight.Load())
			close(rr.endRead)
			select {
			case <-actionsDone:
			case <-time.After(replayDrainCap):
			case <-ctx.Done():
			}
			rr.cpu.drain = rr.readCPU()
		}()
		var wg sync.WaitGroup
		from := rr.setup.opt.from
		for _, act := range rr.setup.plan.Actions {
			at := st.winStart.Add(act.Offset - from)
			if !sleepUntil(ctx, at) {
				break
			}
			wg.Go(func() { rr.do(ctx, a.base, a.token, act, at, load.recs) })
		}
		wg.Wait()
		close(actionsDone)
		<-endDone
		rr.readEndState(a)
	}()
	return load, closed
}

// readCPU is Dispatch's utime+stime now; ok is false without its pid or /proc entry.
func (rr *replayRun) readCPU() cpuReading {
	r := cpuReading{at: time.Now()}
	if rr.pid > 0 {
		if p, ok := readProcStat(rr.pid); ok {
			r.ticks, r.ok = p.utime+p.stime, true
		}
	}
	return r
}

// resolve is the local id of a logged object: the response of the run's own action that created
// it (waited for), else loadtest.replay_ids.
func (rr *replayRun) resolve(ctx context.Context, ref replayRef) (string, bool) {
	if f, ok := rr.futures[ref]; ok {
		select {
		case <-f.done:
			return f.local, f.ok
		case <-ctx.Done():
			return "", false
		}
	}
	v, ok := rr.ids[[2]string{ref.Kind, ref.Logged}]
	return v, ok
}

// settle publishes what the action created; on failure its objects are missing to dependents.
func (rr *replayRun) settle(act *replayAction, produced map[replayRef]string) {
	for _, stg := range act.Stages {
		for _, r := range stg {
			for _, p := range r.Produces {
				f := rr.futures[p.Ref]
				if f == nil {
					continue
				}
				select {
				case <-f.done:
					continue
				default:
				}
				f.local, f.ok = produced[p.Ref], produced[p.Ref] != ""
				close(f.done)
			}
		}
	}
}

// do sends one action, scheduled at at, on its own connection under one deadline over all its
// requests (at + replayActionDeadline), with no retry; the first failed request ends it. A write
// on a document first waits, after its reads, for the previous planned write on that document to
// return; that wait is inside the deadline, and the write's latency counts from at.
func (rr *replayRun) do(ctx context.Context, base, token string, act *replayAction, at time.Time, recs *collector[reqRec]) {
	rr.inflight.Add(1)
	defer rr.inflight.Add(-1)
	turn := rr.turns[act]
	if turn != nil {
		defer close(turn.done)
	}
	ctx, cancel := context.WithDeadline(ctx, at.Add(replayActionDeadline))
	defer cancel()
	produced := map[replayRef]string{}
	defer func() { rr.settle(act, produced) }()
	local := map[replayRef]string{}
	for _, ref := range act.refs() {
		v, ok := rr.resolve(ctx, ref)
		if !ok {
			rr.mu.Lock()
			rr.skipped[act.Kind]++
			rr.mu.Unlock()
			return
		}
		local[ref] = v
	}
	if act.Write {
		rr.mu.Lock()
		rr.writes[act.Kind].Sent++
		act.tally(&rr.subs, &rr.edits)
		rr.mu.Unlock()
	}
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxIdleConnsPerHost: 4, IdleConnTimeout: replayActionDeadline}
	defer tr.CloseIdleConnections()
	ac := &api{base: base, token: token, client: &http.Client{Transport: tr}}
	var state replayActionState
	ok := true
	for _, stg := range act.Stages {
		from := int64(0) // when the stage's latency counts from; 0 for each request's arrival
		if turn != nil && writeStage(stg) {
			waited, alive := rr.awaitTurn(ctx, turn)
			turn = nil
			if !alive {
				ok = false
				break
			}
			if waited {
				from = at.UnixNano()
			}
			rr.mu.Lock()
			rr.wrote[act.Doc] = true
			rr.mu.Unlock()
		}
		results := make([]callResult, len(stg))
		if len(stg) == 1 {
			results[0] = rr.send(ctx, ac, stg[0], local, &state)
		} else {
			var wg sync.WaitGroup
			for i, r := range stg {
				wg.Go(func() { results[i] = rr.send(ctx, ac, r, local, &state) })
			}
			wg.Wait()
		}
		for i, res := range results {
			r := stg[i]
			start := res.arrival()
			if from != 0 {
				start = from
			}
			tolerated := !res.ok() && r.Tolerate != 0 && res.status == r.Tolerate && res.err == nil
			recs.add(reqRec{route: r.route(act.Kind), status: res.status, code: res.errorCode(), tolerated: tolerated, transport: res.err != nil,
				latency: time.Duration(res.end - start), connWait: res.connWait})
			if !res.ok() && !tolerated {
				ok = false
				continue
			}
			state.observe(res.body)
			for _, p := range r.Produces {
				if v := responseField(res.body, p.Field); v != "" {
					produced[p.Ref] = v
				}
			}
		}
		if !ok {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rr.timeouts.Add(1)
		ok = false
	}
	if act.Write && !ok {
		rr.mu.Lock()
		rr.writes[act.Kind].Failed++
		rr.mu.Unlock()
	}
	if !ok {
		clear(produced)
	}
}

// writeStage says whether a stage sends a write (any request but a GET).
func writeStage(stg []*replayReq) bool {
	return slices.ContainsFunc(stg, func(r *replayReq) bool { return r.Method != "GET" })
}

// awaitTurn waits until the previous write on the action's document has returned: waited is
// whether it was still running (the wait is then counted in docWaits), alive false when the
// action's deadline came first.
func (rr *replayRun) awaitTurn(ctx context.Context, t *replayTurn) (waited, alive bool) {
	if t.prev == nil {
		return false, true
	}
	select {
	case <-t.prev:
		return false, true
	default:
	}
	t0 := time.Now()
	select {
	case <-t.prev:
		alive = true
	case <-ctx.Done():
	}
	rr.mu.Lock()
	rr.docWaits = append(rr.docWaits, time.Since(t0).Seconds())
	rr.mu.Unlock()
	return true, alive
}

// replayActionState is what an action's earlier responses tell its later requests.
type replayActionState struct {
	mu       sync.Mutex
	lastSeq  *int64
	editedAt json.RawMessage
}

func (s *replayActionState) observe(body []byte) {
	var r struct {
		LastSeq  *int64          `json:"last_seq"`
		EditedAt json.RawMessage `json:"edited_at"`
		Ask      *struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	if json.Unmarshal(body, &r) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.LastSeq != nil {
		s.lastSeq = r.LastSeq
	}
	if r.Ask != nil && len(r.Ask.EditedAt) > 0 {
		s.editedAt = r.Ask.EditedAt
	} else if len(r.EditedAt) > 0 && s.editedAt == nil {
		s.editedAt = r.EditedAt
	}
}

// responseField is a created object's id (idOf), key or primary_artifact_id in a response.
func responseField(body []byte, field string) string {
	if field == "id" {
		return idOf(body)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(m[field], &s)
	return s
}

// send renders one request (path, body, file) with the local ids and sends it.
func (rr *replayRun) send(ctx context.Context, ac *api, r *replayReq, local map[replayRef]string, state *replayActionState) callResult {
	path := r.Path
	for _, name := range slices.Sorted(maps.Keys(r.Params)) {
		path = strings.ReplaceAll(path, "{"+name+"}", renderArg(r.Params[name], local, state))
	}
	w := who{person: r.Person}
	switch {
	case r.File != nil:
		ct, payload, err := multipartFile(rr.setup.opt.rngSeed, r.File)
		if err != nil {
			return callResult{err: err}
		}
		return ac.callRaw(ctx, r.Method, path, w, ct, payload)
	case r.Body != nil:
		return ac.call(ctx, r.Method, path, w, renderBody(r.Body, local, state))
	}
	return ac.call(ctx, r.Method, path, w, nil)
}

func renderArg(a replayArg, local map[replayRef]string, state *replayActionState) string {
	switch {
	case a.After:
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.lastSeq == nil {
			return "0"
		}
		return strconv.FormatInt(max(0, *state.lastSeq-10), 10)
	case a.URI && a.Sub != nil:
		return url.QueryEscape("dispatch://" + local[*a.Ref] + "/comment/" + local[*a.Sub])
	case a.URI:
		return url.QueryEscape("dispatch://" + local[*a.Ref])
	case a.Ref != nil:
		return url.PathEscape(local[*a.Ref])
	}
	return a.Lit
}

// renderBody is the body with each logged reference replaced by its local id.
func renderBody(v any, local map[replayRef]string, state *replayActionState) any {
	switch x := v.(type) {
	case replayRef:
		return local[x]
	case replayPrevEditedAt:
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.editedAt == nil {
			return json.RawMessage("null")
		}
		return state.editedAt
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = renderBody(e, local, state)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = renderBody(e, local, state)
		}
		return out
	}
	return v
}

// multipartFile is the multipart body of a file upload: name, actor and the file's bytes, under a
// boundary drawn from the rng seed and the file's seed (multipart.NewWriter's own is random), so
// one plan sends one body.
func multipartFile(rngSeed uint64, f *replayFile) (string, []byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	boundary := fmt.Sprintf("replay%016x%016x", replayHash(rngSeed, "boundary", f.Seed), replayHash(rngSeed, "boundary2", f.Seed))
	if err := mw.SetBoundary(boundary); err != nil {
		return "", nil, err
	}
	_ = mw.WriteField("name", f.Name)
	if f.Actor != nil {
		actor, _ := json.Marshal(f.Actor)
		_ = mw.WriteField("actor", string(actor))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", f.Name))
	h.Set("Content-Type", f.MIME)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", nil, err
	}
	if _, err := part.Write(fileBytes(rngSeed, f.Seed, f.Size)); err != nil {
		return "", nil, err
	}
	if err := mw.Close(); err != nil {
		return "", nil, err
	}
	return mw.FormDataContentType(), buf.Bytes(), nil
}
