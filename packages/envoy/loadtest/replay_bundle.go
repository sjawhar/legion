package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// A replay bundle is one project's Dispatch state at an instant `at` plus every write of its issues
// in the window [at, until), as replay-fetch reconstructs them from production's event logs. The
// seeder recreates the state on a load-test stack; the replay request source re-sends the window.
// The directory holds:
//
//	state.json                replayState: the project's issues at `at`, with artifacts, comments and asks
//	docs/<artifact id>.md     each document's markdown at its version at `at` (replayArtifact.TextPath)
//	window.jsonl              the window's events, one compact JSON event per line, as Dispatch served them
//	created-docs/<id>.md      version 1 of each document an artifact.created window event made, and
//	                          of each spec an issue.created window event made (replayMeta.CreatedSpecs)
//	roster.json               []replayRosterEntry: the sessions the replay acts as
//	bundle.meta.json          replayMeta: counts and the sha256 of every other file
//
// raw/ beside them is replay-fetch's cache of production responses, never part of the bundle.
// A bundle carries production text; it never goes in the repository.
const (
	bundleStateFile  = "state.json"
	bundleWindowFile = "window.jsonl"
	bundleRosterFile = "roster.json"
	bundleMetaFile   = "bundle.meta.json"
	bundleDocsDir    = "docs"
	bundleCreatedDir = "created-docs"
	bundleRawDir     = "raw"
)

// replayState is state.json.
type replayState struct {
	Project string    `json:"project"`
	At      time.Time `json:"at"`
	Until   time.Time `json:"until"`
	// Issues are in number order. Without a -keys subset they are every number from 1 to the
	// largest number created before At, so recreating them in order reproduces the keys; a number
	// with no issue at At (deleted since, or never reached) is a placeholder with Exists false.
	Issues []replayIssue `json:"issues"`
	// BlockMaps are, by document id, the current blocks of every document an anchored comment
	// or ask in the state or the window names (window-created documents included).
	BlockMaps map[string]*replayBlockMap `json:"block_maps,omitempty"`
}

// replayIssue is one issue as it stood strictly before At: the fields of its last issue.created,
// issue.updated or issue.closed snapshot, with every later issue.claimed and issue.released
// applied.
type replayIssue struct {
	Key    string `json:"key"`
	Number int    `json:"number"`
	// Exists is false for a placeholder, whose other fields are all empty.
	Exists   bool     `json:"exists"`
	Title    string   `json:"title,omitempty"`
	Status   string   `json:"status,omitempty"`
	Parent   *string  `json:"parent,omitempty"`
	Priority *int     `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	// Route is the issue's route, e.g. "session:<id>" or "role:<name>"; nil when unrouted.
	Route *string `json:"route,omitempty"`
	// Claim is the claim in force at At; nil when unclaimed (a release clears it).
	Claim             *replayClaim `json:"claim,omitempty"`
	BlockedBy         []string     `json:"blocked_by,omitempty"`
	PrimaryArtifactID string       `json:"primary_artifact_id,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	CreatedBy         *replayActor `json:"created_by,omitempty"`
	// Artifacts are the issue's artifacts that had a version before At and are still listed today.
	Artifacts []replayArtifact `json:"artifacts,omitempty"`
	// Comments are in creation order, resolved ones included.
	Comments []replayComment `json:"comments,omitempty"`
	// Asks are the issue's asks open at At (state open or answered), in creation order.
	Asks []replayAsk `json:"asks,omitempty"`
}

// replayClaim is who held an issue's claim and since when.
type replayClaim struct {
	Actor replayActor `json:"actor"`
	At    time.Time   `json:"at"`
}

// replayActor is a Dispatch actor: kind is session, user or service; Origin is where a session
// ran, when Dispatch recorded it.
type replayActor struct {
	Kind   string        `json:"kind"`
	ID     string        `json:"id"`
	Origin *replayOrigin `json:"origin,omitempty"`
}

// replayOrigin is the part of an actor's origin the replay re-sends.
type replayOrigin struct {
	SessionTitle string `json:"session_title,omitempty"`
	Machine      string `json:"machine,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
}

// replayArtifact is one artifact at At.
type replayArtifact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Kind      string    `json:"kind"` // doc, or a file kind (file, image, ...)
	Primary   bool      `json:"primary"`
	CreatedAt time.Time `json:"created_at"`
	// Version is the artifact's version at At: the largest number created before At.
	Version int `json:"version"`
	// Size is Version's size in bytes, MIME its type, when the listing gives them.
	Size *int    `json:"size,omitempty"`
	MIME *string `json:"mime,omitempty"`
	// TextPath is the document's markdown in the bundle, "docs/<id>.md"; empty for a file (whose
	// bytes are never fetched) and for a document no version of which up to Version was readable.
	TextPath string `json:"text_path,omitempty"`
	// TextVersion is the version TextPath holds: Version, or the nearest readable lower version
	// when Version was not readable; 0 when TextPath is empty.
	TextVersion int `json:"text_version,omitempty"`
	// VersionSizes are the listed sizes in bytes of versions 1..Version, index 0 = version 1;
	// -1 where the listing gives none.
	VersionSizes []int `json:"version_sizes,omitempty"`
}

// replayBlockMap is a document's blocks at its current version (production's GET
// /api/v1/artifacts/{id}/blocks, read after the window), each with its markdown in that version.
// Production's block ids are in no version's markdown, so this is how an anchor's logged block
// (anchor.block_id) is found in the text at At (chooseAnchor).
type replayBlockMap struct {
	Version int `json:"version"`
	// Blocks are the top-level blocks in document order.
	Blocks []replayBlock `json:"blocks"`
}

// replayBlock is one top-level block. Inner are a table's rows and cells: an anchor in a cell
// names the cell's id, which is found here with the cell's text.
type replayBlock struct {
	ID    string             `json:"id"`
	Type  string             `json:"type"`
	Text  string             `json:"text"`
	Inner []replayInnerBlock `json:"inner,omitempty"`
}

type replayInnerBlock struct {
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
}

// replayComment is one comment at At.
type replayComment struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// Actor is the comment.created event's actor, which carries the origin the comment's own
	// author field lacks.
	Actor replayActor `json:"actor"`
	// Body is the body at At, after every comment.edited before it.
	Body       string  `json:"body"`
	ArtifactID *string `json:"artifact_id,omitempty"`
	ReplyTo    *string `json:"reply_to,omitempty"`
	// ThreadRootID is the payload's thread_root_id, or else the end of the reply_to chain; nil for
	// a root comment.
	ThreadRootID *string `json:"thread_root_id,omitempty"`
	// Anchor is the anchor at At (anchor refreshes before At applied); nil when unanchored.
	Anchor *replayAnchor `json:"anchor,omitempty"`
	AskID  *string       `json:"ask_id,omitempty"`
	// Open is false once a resolve before At was not followed by a reopen before At.
	Open       bool         `json:"open"`
	ResolvedAt *string      `json:"resolved_at,omitempty"`
	ResolvedBy *replayActor `json:"resolved_by,omitempty"`
	// WindowRefs is true when a window event refers to the comment: a reply whose reply_to or
	// thread root is it, or its comment.resolved, reopened, anchor_refreshed or edited.
	WindowRefs bool `json:"window_refs"`
}

// replayAnchor is where an anchored comment or ask points in a document.
type replayAnchor struct {
	ArtifactID string  `json:"artifact_id"`
	Quote      string  `json:"quote"`
	BlockID    *string `json:"block_id,omitempty"`
	Orphaned   bool    `json:"orphaned"`
}

// replayAsk is one ask at At, with what re-creating it needs: an ask open or answered at At, or a
// typed ask block resolved before At whose block is in its document's text at At. POST
// /api/v1/issues/{key}/asks takes question, kind, options, multiple, urgency, anchor
// {artifact, quote} and actor; an approval ask is opened by the server when a document's review is
// requested (Approval), and a typed ask block lives in its document (BlockID, BlockArtifactID),
// so those two are re-created through the document rather than that POST.
type replayAsk struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`  // question or approval
	State     string            `json:"state"` // open, answered, or resolved (a typed ask block only)
	Question  string            `json:"question"`
	Options   []replayAskOption `json:"options,omitempty"`
	Multiple  bool              `json:"multiple"`
	Urgency   string            `json:"urgency,omitempty"`
	Author    replayActor       `json:"author"`
	CreatedAt time.Time         `json:"created_at"`
	// ArtifactID is the document the ask belongs to, when it is a document's ask.
	ArtifactID      *string            `json:"artifact_id,omitempty"`
	Anchor          *replayAnchor      `json:"anchor,omitempty"`
	BlockID         *string            `json:"block_id,omitempty"`
	BlockArtifactID *string            `json:"block_artifact_id,omitempty"`
	Answer          *replayAskAnswer   `json:"answer,omitempty"`
	Approval        *replayAskApproval `json:"approval,omitempty"`
	// AnswerVersion is, for an answered typed ask block, the version of its block's document whose
	// settlement carried the answer (the first listed version created at or after the answer); 0
	// when the listing names none at or below the document's Version.
	AnswerVersion int `json:"answer_version,omitempty"`
	// Resolution is a resolved typed ask block's resolution; ResolveVersion the version of its
	// block's document the resolve wrote (the first listed version created at or after the
	// resolve), 0 when the listing names none at or below the document's Version.
	Resolution     *replayAskResolution `json:"resolution,omitempty"`
	ResolveVersion int                  `json:"resolve_version,omitempty"`
}

// replayAskOption is one answer choice.
type replayAskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// replayAskAnswer is a human's answer to an answered ask.
type replayAskAnswer struct {
	User     string    `json:"user"`
	Selected []string  `json:"selected,omitempty"`
	Text     *string   `json:"text,omitempty"`
	At       time.Time `json:"at"`
}

// replayAskResolution is how and by whom an ask was resolved: kind is resolved or retracted.
type replayAskResolution struct {
	Kind   string      `json:"kind"`
	Reason string      `json:"reason"`
	By     replayActor `json:"by"`
	At     time.Time   `json:"at"`
}

// replayAskApproval is the document version an approval ask asks a human to approve.
type replayAskApproval struct {
	ArtifactID       string `json:"artifact_id"`
	Version          int    `json:"version"`
	RequestedVersion int    `json:"requested_version"`
}

// replayRosterEntry is one session the replay acts as (roster.json is a list of them): claim
// holders at At in issue order, then routed issues' sessions, then the window's session writers
// in window order, each session once, under the first source that named it.
type replayRosterEntry struct {
	SessionID    string `json:"session_id"`
	Source       string `json:"source"` // claim, route or window
	SessionTitle string `json:"session_title,omitempty"`
	Machine      string `json:"machine,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
}

// replayMeta is bundle.meta.json. It names no URL or hostname.
type replayMeta struct {
	Project   string       `json:"project"`
	At        time.Time    `json:"at"`
	Until     time.Time    `json:"until"`
	FetchedAt time.Time    `json:"fetched_at"`
	Counts    replayCounts `json:"counts"`
	// CreatedSpecs maps each issue an issue.created window event made to its spec's id, whose
	// version 1 is created-docs/<id>.md; an issue whose spec version 1 was unreadable is absent.
	CreatedSpecs map[string]string `json:"created_specs,omitempty"`
	// Files maps each other bundle file's slash path to its hex sha256.
	Files map[string]string `json:"files"`
}

// replayCounts is what replay-fetch prints and records: counts only, never text.
type replayCounts struct {
	Issues            int `json:"issues"`       // existing issues at At
	Placeholders      int `json:"placeholders"` // numbers with no issue at At
	Documents         int `json:"documents"`
	DocBytes          int `json:"doc_bytes"`       // bytes of docs/ text written
	DocsFellBack      int `json:"docs_fell_back"`  // text from a version below the one at At
	DocsUnreadable    int `json:"docs_unreadable"` // no readable version at all
	Files             int `json:"files"`
	ArtifactsUnlisted int `json:"artifacts_unlisted"` // created before At, absent from today's listing
	CommentsOpen      int `json:"comments_open"`
	CommentsResolved  int `json:"comments_resolved"`
	AnchoredOpen      int `json:"anchored_open"`
	AsksOpen          int `json:"asks_open"` // asks open or answered at At
	// AsksResolvedBlocks counts the resolved typed ask blocks the state keeps.
	AsksResolvedBlocks int `json:"asks_resolved_blocks"`
	WindowEvents       int `json:"window_events"`
	// WindowByType counts window events per event type.
	WindowByType          map[string]int `json:"window_by_type"`
	CreatedDocs           int            `json:"created_docs"`
	CreatedDocsUnreadable int            `json:"created_docs_unreadable"`
	// CreatedSpecs counts the window's created issues with a readable spec version 1 in
	// created-docs/; CreatedSpecsUnreadable those without.
	CreatedSpecs           int `json:"created_specs"`
	CreatedSpecsUnreadable int `json:"created_specs_unreadable"`
	Roster                 int `json:"roster"`
	// WrittenDocs are the documents in the state that a window event writes to (an anchored
	// comment on it, or an artifact.version of it); the next two are over their open anchored
	// root comments at At.
	WrittenDocs                int     `json:"written_docs"`
	WrittenDocsOpenRootsMedian float64 `json:"written_docs_open_roots_median"`
	WrittenDocsOpenRootsSum    int     `json:"written_docs_open_roots_sum"`
	// VersionSizesMissing counts document versions 1..Version the listing gives no size for.
	VersionSizesMissing int `json:"version_sizes_missing"`
	// BlockMaps counts the documents with an anchor in the state or the window whose current
	// blocks were read; BlockMapsUnavailable those production would not serve.
	BlockMaps            int `json:"block_maps"`
	BlockMapsUnavailable int `json:"block_maps_unavailable"`
	// AnchorRules counts the state's and the window's anchors by the chooseAnchor branch each
	// takes on the text at At (logged_quote, in_logged_block, nearest_surviving_block,
	// no_block_map, unanchorable); an anchor on a document with no text at At (one not in the
	// state, say) counts as no_text.
	AnchorRules map[string]int `json:"anchor_rules"`
}

// The event types this package compares, as Dispatch's event log names them (replayEvent.Type).
// A literal spelt wrong here fails quietly: the planner falls to unhandled: and the fetch code
// skips the event, so every comparison in the package names one of these.
const (
	evIssueCreated  = "issue.created"
	evIssueUpdated  = "issue.updated"
	evIssueClosed   = "issue.closed"
	evIssueClaimed  = "issue.claimed"
	evIssueReleased = "issue.released"

	evCommentCreated         = "comment.created"
	evCommentEdited          = "comment.edited"
	evCommentResolved        = "comment.resolved"
	evCommentReopened        = "comment.reopened"
	evCommentAnchorRefreshed = "comment.anchor_refreshed"

	evArtifactCreated = "artifact.created"
	evArtifactVersion = "artifact.version"

	evAskOpened   = "ask.opened"
	evAskAnswered = "ask.answered"
	evAskResolved = "ask.resolved"

	evMessageCreated  = "message.created"
	evMessageDelivery = "message.delivery"
	evMessageAccepted = "message.accepted"

	evChildStatus = "child.status"

	// The prefixes a few sites take a whole family by.
	evPrefixIssue     = "issue."
	evPrefixAsk       = "ask."
	evPrefixUserState = "user_state."
)

// decodePayload decodes one event's payload, naming the event in the error.
func decodePayload[T any](ev replayEvent) (T, error) {
	var v T
	if err := json.Unmarshal(ev.Payload, &v); err != nil {
		return v, fmt.Errorf("event %d: %w", ev.ID, err)
	}
	return v, nil
}

// replayEvent is one window.jsonl line. Payload is left raw for the consumer to decode per Type;
// Raw is the whole line as served.
type replayEvent struct {
	ID         int64           `json:"id"`
	IssueKey   *string         `json:"issue_key"`
	ArtifactID *string         `json:"artifact_id"`
	Project    string          `json:"project"`
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Actor      replayActor     `json:"actor"`
	Notify     bool            `json:"notify"`
	CreatedAt  time.Time       `json:"created_at"`
	Payload    json.RawMessage `json:"payload"`
	Raw        []byte          `json:"-"`
}

// parseReplayEvent decodes one served event, keeping its compact bytes as Raw.
func parseReplayEvent(b []byte) (replayEvent, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return replayEvent{}, err
	}
	var ev replayEvent
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		return replayEvent{}, err
	}
	ev.Raw = buf.Bytes()
	return ev, nil
}

// replayBundle is a bundle read back by readBundle.
type replayBundle struct {
	Dir    string
	State  replayState
	Roster []replayRosterEntry
	Meta   replayMeta
	// MetaSHA256 is bundle.meta.json's sha256, as readBundle read it.
	MetaSHA256 string
	Window     []replayEvent
}

// readFile reads a bundle file by its slash path (a replayArtifact.TextPath, say).
func (b *replayBundle) readFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.Dir, filepath.FromSlash(rel)))
}

// readBundle loads a bundle directory, refusing it when a file bundle.meta.json lists is missing
// or its sha256 differs.
func readBundle(dir string) (*replayBundle, error) {
	b := &replayBundle{Dir: dir}
	metaBytes, err := os.ReadFile(filepath.Join(dir, bundleMetaFile))
	if err != nil {
		return nil, err
	}
	b.MetaSHA256 = sha256Hex(metaBytes)
	if err := json.Unmarshal(metaBytes, &b.Meta); err != nil {
		return nil, fmt.Errorf("%s: %w", bundleMetaFile, err)
	}
	for _, need := range []string{bundleStateFile, bundleRosterFile, bundleWindowFile} {
		if _, ok := b.Meta.Files[need]; !ok {
			return nil, fmt.Errorf("%s does not list %s", bundleMetaFile, need)
		}
	}
	paths := slices.Sorted(maps.Keys(b.Meta.Files))
	for _, p := range paths {
		data, err := b.readFile(p)
		if err != nil {
			return nil, fmt.Errorf("bundle file %s: %w", p, err)
		}
		if got := sha256Hex(data); got != b.Meta.Files[p] {
			return nil, fmt.Errorf("bundle file %s: sha256 %s, bundle.meta.json says %s", p, got, b.Meta.Files[p])
		}
		switch p {
		case bundleStateFile:
			err = json.Unmarshal(data, &b.State)
		case bundleRosterFile:
			err = json.Unmarshal(data, &b.Roster)
		case bundleWindowFile:
			b.Window, err = parseWindow(data)
		}
		if err != nil {
			return nil, fmt.Errorf("bundle file %s: %w", p, err)
		}
	}
	return b, nil
}

// parseWindow decodes window.jsonl.
func parseWindow(data []byte) ([]replayEvent, error) {
	var out []replayEvent
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; sc.Scan(); line++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		ev, err := parseReplayEvent(sc.Bytes())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// keepIssue says whether a replay sampled at fraction f keeps issueKey: the same seed and key
// always answer the same, so an issue is kept or dropped whole, with every event of it. The hash is
// FNV-1a 64 of the seed's 8 little-endian bytes then the key, passed through the splitmix64
// finalizer (FNV-1a alone kept 54% of sequential keys at f=0.5) and mapped to [0,1).
func keepIssue(rngSeed uint64, issueKey string, f float64) bool {
	if f >= 1 {
		return true
	}
	if f <= 0 || math.IsNaN(f) {
		return false
	}
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], rngSeed)
	_, _ = h.Write(seed[:])
	_, _ = h.Write([]byte(issueKey))
	x := splitmix64(h.Sum64())
	return float64(x>>11)/float64(1<<53) < f
}
