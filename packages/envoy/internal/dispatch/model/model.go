// Package model defines Dispatch's persisted and wire entities.
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Actor identifies the user or agent session that caused a change. Service is
// the verified Kubernetes subject of the projected service-account token a
// session authenticated with ("system:serviceaccount:<namespace>:<name>"), set
// by the server from the token and never from a request body.
type Actor struct {
	Kind    string       `json:"kind"`
	ID      string       `json:"id"`
	Origin  *ActorOrigin `json:"origin,omitempty"`
	Owner   *string      `json:"owner,omitempty"`
	Service *string      `json:"service,omitempty"`
}

// ActorOrigin describes the client environment of a session actor.
type ActorOrigin struct {
	Host         string `json:"host,omitempty"`
	Machine      string `json:"machine,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Tmux         string `json:"tmux,omitempty"`
	Pane         string `json:"pane,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
}

// AgentToken is the safely displayable metadata for a human-owned agent token.
type AgentToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// Anchor identifies a document mark at a particular named version.
type Anchor struct {
	ArtifactID string  `json:"artifact_id"`
	BlockID    *string `json:"block_id"`
	MarkID     string  `json:"mark_id"`
	Version    int     `json:"version"`
	Quote      string  `json:"quote"`
	Orphaned   bool    `json:"orphaned"`
}

// AnchorInput selects document text by quote or identifies a browser-owned mark.
type AnchorInput struct {
	Artifact   string  `json:"artifact"`
	Quote      *string `json:"quote,omitempty"`
	Occurrence *int    `json:"occurrence,omitempty"`
	MarkID     *string `json:"mark_id,omitempty"`
}

// Project groups native issues under a short key.
type Project struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	OpenAsks  int       `json:"open_asks"`
	CreatedAt time.Time `json:"created_at"`
}

// RepoProject assigns an external repository to a native Dispatch project.
type RepoProject struct {
	Repo      string    `json:"repo"`
	Project   string    `json:"project"`
	CreatedBy Actor     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ArchitectureSource names the repository and branch a project's architecture
// documents are imported from. InstallationID caches the GitHub App
// installation the access check resolved — the importer re-resolves it on
// sync — and is a server detail kept out of the JSON shape. The sync
// bookkeeping fields stay null until the importer runs.
type ArchitectureSource struct {
	Project        string     `json:"project"`
	Repo           string     `json:"repo"`
	Branch         string     `json:"branch"`
	Enabled        bool       `json:"enabled"`
	InstallationID int64      `json:"-"`
	CreatedBy      Actor      `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	LastSyncAt     *time.Time `json:"last_sync_at"`
	LastCommit     *string    `json:"last_commit"`
	LastError      *string    `json:"last_error"`
	// LastTreeSha is the architecture directory's tree object id at the last
	// successful import: importer bookkeeping for the unchanged-model
	// short-circuit, kept out of the JSON shape.
	LastTreeSha *string `json:"-"`
}

// ExternalLink connects a native issue to an external tracker item.
type ExternalLink struct {
	URL  string `json:"url"`
	Kind string `json:"kind,omitempty"`
}

// IssueComponents is an issue's effective component attachment, resolved on
// read: the nearest ancestor with its own attachment (either mode) decides,
// and InheritedFrom names it when that ancestor is not the issue itself. Mode
// "inherit" with no InheritedFrom means no issue on the parent chain chose —
// the issue is unassigned. IDs is the effective set of live component ids;
// Unknown the effective member ids a re-import has since retired (kept, never
// dropped). Reason is set for mode "none".
type IssueComponents struct {
	Mode          string   `json:"mode"`
	IDs           []string `json:"ids"`
	Unknown       []string `json:"unknown"`
	Reason        *string  `json:"reason"`
	InheritedFrom *string  `json:"inherited_from"`
}

// IssueClaim is the session or human currently working an issue: the actor that claimed it
// and when. At most one issue claim exists at a time. It is not the issue's Route (where
// messages on the issue are delivered, which may be a role) and not its Assignee (the human
// who answers its asks).
type IssueClaim struct {
	Actor Actor     `json:"actor"`
	At    time.Time `json:"at"`
}

// IssueClaimEventPayload is the payload of issue.claimed and issue.released. Claim is the
// claim now in force (nil on a release); Previous is the claim this one replaced or cleared,
// so a takeover names whose work was taken and the outbox can tell that session. Reason is
// "claimed", "takeover", "forced", "released", or "closed".
type IssueClaimEventPayload struct {
	Key      string      `json:"key"`
	Status   string      `json:"status"`
	Claim    *IssueClaim `json:"claim"`
	Previous *IssueClaim `json:"previous_claim,omitempty"`
	Reason   string      `json:"reason"`
}

// Issue is the complete native issue record.
type Issue struct {
	Key               string          `json:"key"`
	Project           string          `json:"project"`
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	Status            string          `json:"status"`
	Priority          *int            `json:"priority"`
	Rank              string          `json:"rank"`
	Labels            []string        `json:"labels"`
	Parent            *string         `json:"parent"`
	Assignee          *string         `json:"assignee"`
	Claim             *IssueClaim     `json:"claim"`
	Components        IssueComponents `json:"components"`
	ExternalLinks     []ExternalLink  `json:"external_links"`
	Route             *string         `json:"route"`
	CreatedBy         *Actor          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	ClosedAt          *time.Time      `json:"closed_at"`
	PrimaryArtifactID string          `json:"primary_artifact_id"`
	LastSeq           int             `json:"last_seq"`
}

// IssueSummary is the lightweight issue listing representation.
type IssueSummary struct {
	Key        string          `json:"key"`
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	Priority   *int            `json:"priority"`
	Rank       string          `json:"rank"`
	Labels     []string        `json:"labels"`
	Parent     *string         `json:"parent"`
	Assignee   *string         `json:"assignee"`
	Claim      *IssueClaim     `json:"claim"`
	Components IssueComponents `json:"components"`
	UpdatedAt  time.Time       `json:"updated_at"`
	LastSeq    int             `json:"last_seq"`
	OpenAsks   int             `json:"open_asks"`
}

// SearchOwner identifies the issue or standalone project document that owns a search result.
// An issue owner carries Key, Title and Status; a document owner carries Project, Slug,
// ArtifactID and Name.
type SearchOwner struct {
	Kind       string `json:"kind"`
	Key        string `json:"key,omitempty"`
	Title      string `json:"title,omitempty"`
	Status     string `json:"status,omitempty"`
	Project    string `json:"project,omitempty"`
	Slug       string `json:"slug,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// SearchArtifact identifies an artifact attached to a global search result.
type SearchArtifact struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// SearchIssue is the legacy issue-owner shape of a search hit.
type SearchIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// SearchResult is one ranked Dispatch search hit.
type SearchResult struct {
	Kind  string      `json:"kind"`
	Owner SearchOwner `json:"owner"`
	// Issue mirrors Owner for issue-owned hits so agent clients built before the owner-only
	// shape (#1119) keep rendering; remove once no installed pi-legion-envoy /
	// opencode-legion-envoy / claude-envoy-bridge predates it.
	Issue    *SearchIssue    `json:"issue,omitempty"`
	Artifact *SearchArtifact `json:"artifact,omitempty"`
	ID       string          `json:"id"`
	Snippet  string          `json:"snippet"`
	Rank     float64         `json:"rank"`
	Href     string          `json:"href"`
}

// SearchResponse is the ranked global search result set.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	TookMS  int64          `json:"took_ms"`
}

// DuplicateCandidate is a potential duplicate issue proposed before creation.
type DuplicateCandidate struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Snippet     string `json:"snippet"`
	SharedTerms int    `json:"shared_terms"`
	Href        string `json:"href"`
}

// IssueChild is a child item embedded in an issue detail response. The subtree counts
// include the child itself, every status (icebox included); done is `status = 'done'`.
// ActiveAt is the newest updated_at anywhere in the child's subtree.
type IssueChild struct {
	Key           string         `json:"key"`
	Title         string         `json:"title"`
	Status        string         `json:"status"`
	SubtreeDone   int            `json:"subtree_done"`
	SubtreeTotal  int            `json:"subtree_total"`
	ActiveAt      time.Time      `json:"active_at"`
	ExternalLinks []ExternalLink `json:"external_links"`
}

// ArchitectureTree is GET /api/v1/projects/{key}/architecture: the project's
// component model with the work attached to it. Counting rule: a component
// counts every distinct issue whose effective set names it or any component it
// contains (transitively over parent), parents and icebox included, each once;
// Own* restricts that to issues whose effective set names this component
// itself. Unassigned lists issues no ancestor chain attached; NotArchitectural
// those resolving to a `none` row; RetiredLinks those whose effective set names
// a component a re-import retired.
type ArchitectureTree struct {
	Source           ArchitectureTreeSource      `json:"source"`
	Totals           ArchitectureTreeTotals      `json:"totals"`
	Components       []ArchitectureTreeComponent `json:"components"`
	Unassigned       []ArchitectureTreeIssueRef  `json:"unassigned"`
	NotArchitectural []ArchitectureTreeNone      `json:"not_architectural"`
	RetiredLinks     []ArchitectureTreeRetired   `json:"retired_links"`
}

// ArchitectureTreeSource is the source row's sync bookkeeping.
type ArchitectureTreeSource struct {
	Repo       string     `json:"repo"`
	Branch     string     `json:"branch"`
	LastCommit *string    `json:"last_commit"`
	LastSyncAt *time.Time `json:"last_sync_at"`
	LastError  *string    `json:"last_error"`
}

// ArchitectureTreeTotals are project-level counts: every filed issue (icebox
// and closed included), and the sizes of the three side lists plus the
// components that count no work.
type ArchitectureTreeTotals struct {
	IssuesDone            int `json:"issues_done"`
	IssuesTotal           int `json:"issues_total"`
	Unassigned            int `json:"unassigned"`
	NotArchitectural      int `json:"not_architectural"`
	ComponentsWithoutWork int `json:"components_without_work"`
	RetiredLinks          int `json:"retired_links"`
}

// ArchitectureTreeComponent is one component with its counts and the issues
// counted, each saying how it qualified.
type ArchitectureTreeComponent struct {
	ID        string                  `json:"id"`
	Title     string                  `json:"title"`
	Parent    *string                 `json:"parent"`
	DependsOn []string                `json:"depends_on"`
	Paths     []string                `json:"paths"`
	External  bool                    `json:"external"`
	Prose     string                  `json:"prose"`
	Done      int                     `json:"done"`
	Total     int                     `json:"total"`
	OwnDone   int                     `json:"own_done"`
	OwnTotal  int                     `json:"own_total"`
	Issues    []ArchitectureTreeIssue `json:"issues"`
}

// ArchitectureTreeIssue is an issue counted for a component. Attached is how:
// "direct" (its own row names the component), "inherited" (an ancestor's row
// does), or "contained" (its effective set names a component this one
// contains; Via is that component). When several apply the strongest wins.
type ArchitectureTreeIssue struct {
	Key           string         `json:"key"`
	Title         string         `json:"title"`
	Status        string         `json:"status"`
	Priority      *int           `json:"priority"`
	Parent        *string        `json:"parent"`
	ExternalLinks []ExternalLink `json:"external_links"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Attached      string         `json:"attached"`
	Via           *string        `json:"via,omitempty"`
}

// ArchitectureTreeIssueRef is an issue in the unassigned list.
type ArchitectureTreeIssueRef struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// ArchitectureTreeNone is an issue declared not architectural, with the reason
// and, when the declaration is an ancestor's, that ancestor.
type ArchitectureTreeNone struct {
	Key           string  `json:"key"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason"`
	InheritedFrom *string `json:"inherited_from"`
}

// ArchitectureTreeRetired is an issue whose effective set names retired
// components; IDs are those ids.
type ArchitectureTreeRetired struct {
	Key    string   `json:"key"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	IDs    []string `json:"ids"`
}

// Artifact is an issue-attached document or binary blob, or an unlinked project document.
type Artifact struct {
	ID        string    `json:"id"`
	IssueKey  *string   `json:"issue_key"`
	Project   string    `json:"project"`
	RefKey    string    `json:"ref_key"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Primary   bool      `json:"primary"`
	CreatedBy Actor     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Versions  []Version `json:"versions"`
	// Approval is the document's approval as of its latest version, derived from
	// version-pinned reviews and any open approval ask; nil for non-documents.
	Approval *ArtifactApproval `json:"approval,omitempty"`
}

// ArtifactReview is one human review of a document, pinned to the version it was
// given on: an approval, or a request for changes with a reason.
type ArtifactReview struct {
	ID         string    `json:"id"`
	ArtifactID string    `json:"artifact_id"`
	Version    int       `json:"version"`
	State      string    `json:"state"`
	Actor      Actor     `json:"actor"`
	Reason     *string   `json:"reason"`
	AskID      *string   `json:"ask_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// ArtifactApproval is a document's approval state as of its latest version:
// draft (never reviewed or requested), awaiting (an approval ask is open),
// approved (approved at the latest version), stale (approved at an older
// version), or changes_requested (the latest review asks for changes).
type ArtifactApproval struct {
	State         string  `json:"state"`
	LatestVersion int     `json:"latest_version"`
	Version       *int    `json:"version,omitempty"`
	By            *Actor  `json:"by,omitempty"`
	At            *string `json:"at,omitempty"`
	Reason        *string `json:"reason,omitempty"`
	AskID         *string `json:"ask_id,omitempty"`
	RequestedBy   *Actor  `json:"requested_by,omitempty"`
}

// ArtifactReviewEventPayload is the payload of artifact.approved and
// artifact.changes_requested: the review pinned to its version, and the approval
// ask it answered (nil when given from the document header with no request open).
type ArtifactReviewEventPayload struct {
	ArtifactID string  `json:"artifact_id"`
	Name       string  `json:"name"`
	Version    int     `json:"version"`
	Actor      Actor   `json:"actor"`
	Reason     *string `json:"reason"`
	AskID      *string `json:"ask_id"`
}

// ArtifactBlock is an addressable document block and its byte range in canonical markdown.
type ArtifactBlock struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	From          int             `json:"from"`
	To            int             `json:"to"`
	Token         string          `json:"token"`
	References    BlockReferences `json:"references"`
	DescendantIDs []string        `json:"-"`
}

// BlockReferences counts rows whose inline anchor is pinned to a document block;
// table blocks include anchors pinned to descendant cells.
type BlockReferences struct {
	Comments int `json:"comments"`
	Asks     int `json:"asks"`
}

// Version is an immutable artifact version.
type Version struct {
	Number    int       `json:"number"`
	Named     bool      `json:"named"`
	Summary   *string   `json:"summary"`
	Authors   []Actor   `json:"authors"`
	CreatedAt time.Time `json:"created_at"`
	Size      *int      `json:"size,omitempty"`
	MIME      *string   `json:"mime,omitempty"`
	SHA256    *string   `json:"sha256,omitempty"`
}

// Ask is a question with either a human answer or a recorded closure reason.
type Ask struct {
	ID         string  `json:"id"`
	IssueKey   *string `json:"issue_key"`
	ArtifactID *string `json:"artifact_id"`
	// BlockID is the typed block this indexed ask represents; nil is a row-only ask.
	BlockID *string `json:"block_id"`
	// BlockArtifactID identifies the document holding BlockID. It is internal to
	// Dispatch's answer write-back; row ownership remains issue or unlinked document.
	BlockArtifactID *string            `json:"-"`
	AnchorArtifact  *AskAnchorArtifact `json:"anchor_artifact,omitempty"`
	BlockArtifact   *AskBlockArtifact  `json:"block_artifact,omitempty"`
	Author          Actor              `json:"author"`
	// Kind is "question" for ordinary asks, whose asker chooses the options, and
	// "approval" for server-created document reviews.
	Kind          string         `json:"kind"`
	Question      string         `json:"question"`
	Options       []AskOption    `json:"options"`
	Multiple      bool           `json:"multiple"`
	Urgency       string         `json:"urgency"`
	Anchor        *Anchor        `json:"anchor"`
	State         string         `json:"state"`
	Answer        *AskAnswer     `json:"answer"`
	Resolution    *AskResolution `json:"resolution,omitempty"`
	OpenedEventID *int64         `json:"opened_event_id,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	// ReferencedByCount is batched with ask-list reads so a card can suppress an empty backlink
	// disclosure without issuing one graph request per ask. It is absent from mutation responses.
	ReferencedByCount *int    `json:"referenced_by_count,omitempty"`
	EditedAt          *string `json:"edited_at"`
	// Approval names the document an approval ask is about; nil for questions.
	Approval *AskApproval `json:"approval,omitempty"`
	// WaitingOn is whose reply an open ask needs next: "human" or "agent". It is the
	// Turn of the newest comment in the ask's thread, "human" when nobody has replied.
	// Set on ask reads only (inbox rows, ask lists, the ask detail), never on the
	// ask.* event payloads; empty for answered and resolved asks.
	WaitingOn string `json:"waiting_on,omitempty"`
}

// AskAnchorArtifact is the document containing an ask's quoted anchor.
type AskAnchorArtifact struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

// AskBlockArtifact is the document containing a typed ask block.
type AskBlockArtifact struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Primary bool   `json:"primary"`
}

// AskApproval is the document an approval ask asks about, at the version the
// request was made for.
type AskApproval struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
}

// AskEditPrevious is the mutable content of an ask before an edit.
type AskEditPrevious struct {
	Question string      `json:"question"`
	Options  []AskOption `json:"options"`
	Multiple bool        `json:"multiple"`
	Urgency  string      `json:"urgency"`
}

// AskEdit is one recorded rewording of an ask, read back from its ask.edited
// events: the question as it stood before the edit, who edited it, and when.
type AskEdit struct {
	Previous AskEditPrevious `json:"previous"`
	EditedBy Actor           `json:"edited_by"`
	At       string          `json:"at"`
}

// AskFollower is a session that receives an ask's answer, edits, resolution, and
// replies on its own topic: the asker, every session that replied, and any session
// a human added. A session may leave; a human may add or remove any session.
type AskFollower struct {
	SessionID string    `json:"session_id"`
	Since     time.Time `json:"since"`
}

// AskFollowerEventPayload is the payload of ask.follower_added and
// ask.follower_removed: which session joined or left which ask, and who did it.
type AskFollowerEventPayload struct {
	AskID     string `json:"ask_id"`
	SessionID string `json:"session_id"`
	By        Actor  `json:"by"`
}

// AskLastReply is the newest comment in an ask's thread, carried on inbox rows and
// on the issue detail's open asks so a human can see who spoke last. Whose turn it
// is comes from that comment's Turn (Ask.WaitingOn), not from its author.
type AskLastReply struct {
	Author    Actor  `json:"author"`
	CreatedAt string `json:"created_at"`
}

// AskEventPayload is the wire payload of every `ask.*` event that carries the ask row: the ask
// itself, flat, plus what the write moved in the reference graph. NewAskEventPayload is the only
// way to build one, and it takes the changes, so a producer whose question text can cite
// something cannot append an event that stays silent about it.
type AskEventPayload struct {
	Ask
	ReferenceChangesPayload
}

// NewReferenceChangesPayload states what a write moved on a typed event payload.
func NewReferenceChangesPayload(changes ReferenceChanges) ReferenceChangesPayload {
	return ReferenceChangesPayload{
		ReferencesChanged:          changes.Targets,
		ReferencesChangedTruncated: changes.Truncated,
	}
}

// NewAskEventPayload builds the payload of an `ask.*` event. A transition that writes no text
// (answered, resolved, an anchor refresh) passes an empty ReferenceChanges.
func NewAskEventPayload(ask Ask, changes ReferenceChanges) AskEventPayload {
	return AskEventPayload{
		Ask:                     ask,
		ReferenceChangesPayload: NewReferenceChangesPayload(changes),
	}
}

// IssueEventPayload is the wire payload of every `issue.*` event: the issue's own fields, flat,
// plus what the write moved in the reference graph. A new issue's spec text is indexed in the
// creating transaction, so `issue.created` names the nodes that body cited.
type IssueEventPayload struct {
	Issue
	ReferenceChangesPayload
}

// NewIssueEventPayload builds the payload of an `issue.*` event. A write that indexes no text
// (a patch, a close) passes an empty ReferenceChanges.
func NewIssueEventPayload(issue Issue, changes ReferenceChanges) IssueEventPayload {
	return IssueEventPayload{
		Issue:                   issue,
		ReferenceChangesPayload: NewReferenceChangesPayload(changes),
	}
}

// AskEditEventPayload is the full edited ask plus its prior mutable content, the actor who made
// the edit, and what the new question text moved in the reference graph.
type AskEditEventPayload struct {
	Ask
	ReferenceChangesPayload
	Previous AskEditPrevious `json:"previous"`
	EditedBy Actor           `json:"edited_by"`
}

// NewAskEditEventPayload builds the payload of an `ask.edited` event.
func NewAskEditEventPayload(
	ask Ask,
	previous AskEditPrevious,
	editedBy Actor,
	changes ReferenceChanges,
) AskEditEventPayload {
	return AskEditEventPayload{
		Ask:                     ask,
		ReferenceChangesPayload: NewReferenceChangesPayload(changes),
		Previous:                previous,
		EditedBy:                editedBy,
	}
}

// BlockRepairedEventPayload records one server-owned typed-block repair in a version.
type BlockRepairedEventPayload struct {
	BlockID     string `json:"block_id"`
	Version     int    `json:"version"`
	DisturbedBy Actor  `json:"disturbed_by"`
}

// BlockInvalidEventPayload records an ask block that browser editing left malformed.
type BlockInvalidEventPayload struct {
	BlockID     string `json:"block_id"`
	Version     int    `json:"version"`
	Reason      string `json:"reason"`
	DisturbedBy Actor  `json:"disturbed_by"`
}

// AskOption is an answer choice.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskAnswer is a human response to an ask.
type AskAnswer struct {
	User     string    `json:"user"`
	Selected []string  `json:"selected"`
	Text     *string   `json:"text"`
	At       time.Time `json:"at"`
}

// AskResolution records why a question no longer needs a human answer.
type AskResolution struct {
	Kind   string    `json:"kind"`
	Reason string    `json:"reason"`
	Actor  Actor     `json:"actor"`
	At     time.Time `json:"at"`
}

// Comment is a collaboration comment, optionally with an edit suggestion. A comment
// replies to at most one of another comment (ReplyTo) or an ask (AskID). Comment
// threads store their root ID in ReplyTo, and ask threads store their ask ID in
// AskID, so both thread forms remain flat.
// Mention is one canonical session or role target resolved when its comment is created.
type Mention struct {
	Target    string  `json:"target"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
}

// CommentDelivery records one attempt to deliver a mention to its resolved session.
type CommentDelivery struct {
	CommentID    string    `json:"comment_id"`
	Target       string    `json:"target"`
	Attempt      int       `json:"attempt"`
	Delivery     string    `json:"delivery"`
	SessionID    *string   `json:"session_id"`
	EnvelopeID   *string   `json:"envelope_id"`
	State        string    `json:"state"`
	Error        *string   `json:"error"`
	ResolveError *string   `json:"resolve_error"`
	ReplyID      *string   `json:"reply_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// CommentDeliveryEventPayload is the durable outcome of one mention delivery attempt.
type CommentDeliveryEventPayload struct {
	CommentID string `json:"comment_id"`
	// AskID is the ask whose thread holds the comment, so a consumer keyed on the ask
	// (the Inbox row, the ask card) sees a receipt that changes the thread it renders.
	// Nil when the comment belongs to no ask.
	AskID     *string `json:"ask_id"`
	Target    string  `json:"target"`
	Attempt   int     `json:"attempt"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
	State     string  `json:"state"`
	Error     string  `json:"error,omitempty"`
	ReplyID   *string `json:"reply_id"`
}

type Comment struct {
	ID         string  `json:"id"`
	IssueKey   *string `json:"issue_key"`
	ArtifactID *string `json:"artifact_id"`
	Author     Actor   `json:"author"`
	Body       string  `json:"body"`
	Anchor     *Anchor `json:"anchor"`
	ReplyTo    *string `json:"reply_to"`
	AskID      *string `json:"ask_id"`
	// Turn is set on a reply to an open ask (AskID non-nil) and names who holds the
	// turn after this comment: "human" when the human needs to act, "agent" when the
	// comment is a progress note and the asking agent still owes the next move. A
	// human's reply always hands the turn to the agent. Nil on replies under a closed
	// ask (nothing is waiting) and on every other comment.
	Turn       *string           `json:"turn"`
	Resolved   bool              `json:"resolved"`
	ResolvedBy *Actor            `json:"resolved_by"`
	ResolvedAt *string           `json:"resolved_at"`
	EditedAt   *string           `json:"edited_at"`
	Suggestion *Suggestion       `json:"suggestion"`
	CreatedAt  time.Time         `json:"created_at"`
	Mentions   []Mention         `json:"mentions"`
	Deliveries []CommentDelivery `json:"deliveries"`
}

// Suggestion carries a proposed document replacement and its resolution.
type Suggestion struct {
	ReplaceWith string `json:"replace_with"`
	Accepted    *bool  `json:"accepted"`
}

// ReferenceChanges is what one write moved in the reference graph, in the shape every event
// that reports it carries: the counted targets it touched, or Truncated when there were more
// than an event names and the reader refreshes its lists wholesale.
type ReferenceChanges struct {
	Targets   []ChangedReference
	Truncated bool
}

// The two wire keys every event that reports reference changes carries. A typed payload spells
// them in its struct tags through ReferenceChangesPayload and a map payload through
// NameReferenceChanges, and model_test.go pins the two spellings together.
const (
	ReferencesChangedKey          = "references_changed"
	ReferencesChangedTruncatedKey = "references_changed_truncated"
)

// ReferencedByCountKey is the wire key carrying how many graph_edges rows point at an ask. Ask
// struct tag, the issue detail's ask rows and the map-shaped event payloads all spell it here,
// and model/reference_changes_test.go pins the spelling against the struct.
const ReferencedByCountKey = "referenced_by_count"

// ReferenceChangesPayload is the pair of fields a typed event payload carries, embedded so every
// payload spells them once.
type ReferenceChangesPayload struct {
	ReferencesChanged          []ChangedReference `json:"references_changed,omitempty"`
	ReferencesChangedTruncated bool               `json:"references_changed_truncated,omitempty"`
}

// NameReferenceChanges records what a write moved on a map-shaped event payload, in the same
// keys the typed payloads carry.
func NameReferenceChanges(payload map[string]any, changes ReferenceChanges) {
	if len(changes.Targets) > 0 {
		payload[ReferencesChangedKey] = changes.Targets
	}
	if changes.Truncated {
		payload[ReferencesChangedTruncatedKey] = true
	}
}

// ChangedReference is a node whose inbound reference edges one write moved, addressed the way
// the lists that carry its backlink count are keyed: an issue by key, an ask by id together
// with the issue or document whose ask list holds its row.
type ChangedReference struct {
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	IssueKey   *string `json:"issue_key,omitempty"`
	ArtifactID *string `json:"artifact_id,omitempty"`
}

// CommentEventPayload is the wire payload of comment.* and suggestion.* events:
// the comment's own fields plus the anchored artifact's name. Comment is
// embedded so the JSON stays flat, which is the shape every consumer reads.
type CommentEventPayload struct {
	Comment
	ArtifactName string `json:"artifact_name"`
	// ProjectKey and ArtifactSlug identify the document owning an artifact comment,
	// including direct author routes whose topic no longer carries document details.
	ProjectKey   string `json:"project_key,omitempty"`
	ArtifactSlug string `json:"artifact_slug,omitempty"`
	// AskQuestion is the question text of the ask this comment replies to
	// (Comment.AskID), carried alongside the id-shaped in_reply_to so a
	// notification renderer can show "re: <question>" instead of a bare UUID.
	// Empty when the comment does not reply to an ask.
	AskQuestion string `json:"ask_question,omitempty"`
	// AskState is that ask's state when the comment was posted. A human reply
	// while the ask is still "open" is a request for clarification: the asker
	// answers in the thread or rewords the question. Empty when the comment does
	// not reply to an ask.
	AskState string `json:"ask_state,omitempty"`
	// AskWaitingOn is the open ask's Ask.WaitingOn once this comment is its newest
	// reply, so a stream consumer can move the ask between "waiting on you" and
	// "waiting on the agent" without re-reading it. Empty when the comment does not
	// reply to an open ask.
	AskWaitingOn string `json:"ask_waiting_on,omitempty"`
	// ThreadRootID is the id of the head of the thread the comment joined, which is also
	// what Comment.ReplyTo stores for it. Carried by comment.created and by the
	// comment.answered a delivery callback appends; empty when the comment replies to an
	// ask or is a root comment.
	ThreadRootID string `json:"thread_root_id,omitempty"`
	// SuppressRoute and SuppressedAuthors are resolved before the comment event commits,
	// so the outbox can deduplicate without consulting the live session registry.
	SuppressRoute            bool     `json:"suppress_route"`
	SuppressedRoute          string   `json:"suppressed_route,omitempty"`
	SuppressedRouteSessionID *string  `json:"suppressed_route_session_id,omitempty"`
	SuppressedAuthors        []string `json:"suppressed_authors"`
	// ReferenceChangesPayload names the nodes whose inbound reference edges this write moved,
	// so a reader carrying their batched backlink counts refreshes exactly those rows.
	ReferenceChangesPayload
}

// Message is a short update, optionally linked to an issue and threaded under another message.
type Message struct {
	ID         string            `json:"id"`
	IssueKey   *string           `json:"issue_key"`
	Author     Actor             `json:"author"`
	Body       string            `json:"body"`
	Target     *string           `json:"target"`
	InReplyTo  *string           `json:"in_reply_to"`
	CreatedAt  time.Time         `json:"created_at"`
	Deliveries []MessageDelivery `json:"deliveries"`
}

// MessageDelivery records one human-requested attempt to reach a live agent.
type MessageDelivery struct {
	MessageID  string    `json:"message_id"`
	Attempt    int       `json:"attempt"`
	Delivery   string    `json:"delivery"`
	SessionID  string    `json:"session_id"`
	EnvelopeID *string   `json:"envelope_id"`
	State      string    `json:"state"`
	Error      *string   `json:"error"`
	ReplyID    *string   `json:"reply_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// MessageEventPayload wraps a Message with the reply target's body preview (first 160
// characters, ReplyBody) so a reply can render "re: <preview>" without a second lookup.
type MessageEventPayload struct {
	Message
	ReplyBody string `json:"reply_body,omitempty"`
	// ThreadTarget is the target of the reply's thread root: the conversation the reply
	// lands in, for a consumer that groups a thread under its root. A session replying to a
	// message aimed at that session inherits no target of its own (the target would be
	// itself), so its own Target names no conversation. Empty on a root message, whose
	// Target already names one, and on a reply under an untargeted thread. It is the root's
	// target column verbatim, so it is whatever a target may be - "session:<id>" or the
	// "role:<role>" route the thread was aimed at - and never resolved to a session.
	ThreadTarget string `json:"thread_target,omitempty"`
	// ReferenceChangesPayload carries the same signal comment events do.
	ReferenceChangesPayload
}

// MessageDeliveryEventPayload is the user-visible result of one target delivery attempt.
type MessageDeliveryEventPayload struct {
	MessageID string `json:"message_id"`
	Attempt   int    `json:"attempt"`
	Delivery  string `json:"delivery"`
	SessionID string `json:"session_id"`
	Target    string `json:"target"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// ReferencedBy identifies a post or artifact that mentions an artifact.
type ReferencedBy struct {
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	IssueKey *string `json:"issue_key"`
	Project  string  `json:"project"`
	Excerpt  string  `json:"excerpt"`
	RefKey   string  `json:"ref_key,omitempty"`
}

// ReferenceVia identifies the item or artifact through which a reference was reached.
type ReferenceVia struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ReferenceMember is one artifact in an issue's reference closure.
type ReferenceMember struct {
	Artifact Artifact     `json:"artifact"`
	Depth    int          `json:"depth"`
	Via      ReferenceVia `json:"via"`
}

// IssueReferences is an issue's artifact reference closure.
type IssueReferences struct {
	Members   []ReferenceMember `json:"members"`
	Truncated bool              `json:"truncated"`
}

// OutgoingReference is a target edge from an artifact.
type OutgoingReference struct {
	Kind     string    `json:"kind"`
	ToID     string    `json:"to_id"`
	Artifact *Artifact `json:"artifact,omitempty"`
}

// ArtifactReferences describes an artifact's outgoing and incoming reference edges.
type ArtifactReferences struct {
	Outgoing     []OutgoingReference `json:"outgoing"`
	ReferencedBy []ReferencedBy      `json:"referenced_by"`
}

// GraphNode is one end of a reference-graph edge. IssueKey and Project locate it; Ref is its
// dispatch:// address and is empty for session nodes, which have none.
type GraphNode struct {
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	IssueKey *string `json:"issue_key,omitempty"`
	Project  string  `json:"project,omitempty"`
	Ref      string  `json:"ref,omitempty"`
}

// GraphExcerpt is the text shown for an edge: the block containing a document mention (with its
// block id), or the head of the other node's own text.
type GraphExcerpt struct {
	BlockID string `json:"block_id,omitempty"`
	Text    string `json:"text"`
}

// GraphEdge is one typed, timestamped edge of graph_edges seen from the queried node: Node is
// the other end, Direction is "in" for edges pointing at the queried node and "out" for edges
// it writes. SourceSeq is the events.id that introduced a mention; nil for structural edges.
type GraphEdge struct {
	Kind      string        `json:"kind"`
	Direction string        `json:"direction"`
	Node      GraphNode     `json:"node"`
	Excerpt   *GraphExcerpt `json:"excerpt,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	SourceSeq *int64        `json:"source_seq"`
}

// GraphReferences is the read of one node's edges, newest first.
type GraphReferences struct {
	Node  GraphNode   `json:"node"`
	Edges []GraphEdge `json:"edges"`
}

// Event is the immutable event-log record for a Dispatch mutation.
type Event struct {
	ID         int64     `json:"id"`
	IssueKey   *string   `json:"issue_key"`
	ArtifactID *string   `json:"artifact_id"`
	ProjectKey *string   `json:"-"`
	Project    string    `json:"project"`
	Seq        int       `json:"seq"`
	Type       string    `json:"type"`
	Actor      Actor     `json:"actor"`
	Notify     bool      `json:"notify"`
	CreatedAt  time.Time `json:"created_at"`
	Payload    any       `json:"payload"`
}

// EditPrecondition asks Dispatch to apply document operations only against the
// document or blocks the caller read.
type EditPrecondition struct {
	Document string                  `json:"document,omitempty"`
	Blocks   []EditBlockPrecondition `json:"blocks,omitempty"`
}

// EditBlockPrecondition identifies one stable block and its canonical-content token.
type EditBlockPrecondition struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// EditPreconditionMismatch names a stale document or block and its current token.
// Current is nil when the expected block no longer exists.
type EditPreconditionMismatch struct {
	Scope    string  `json:"scope"`
	BlockID  string  `json:"block_id,omitempty"`
	Expected string  `json:"expected"`
	Current  *string `json:"current"`
}

// EditOp is one agent document editing operation.
type EditOp struct {
	Op         string          `json:"op"`
	Find       string          `json:"find,omitempty"`
	With       string          `json:"with,omitempty"`
	Occurrence *int            `json:"occurrence,omitempty"`
	Markdown   string          `json:"markdown,omitempty"`
	After      string          `json:"after,omitempty"`
	Before     string          `json:"before,omitempty"`
	Block      string          `json:"block,omitempty"`
	Index      json.RawMessage `json:"index,omitempty"`
	Type       string          `json:"type,omitempty"`
	Attributes map[string]any  `json:"attributes,omitempty"`
}

// Route is a validated issue delivery route.
type Route struct {
	Kind string
	ID   string
}

// IssueStatuses is the canonical Legion issue lifecycle.
var IssueStatuses = []string{
	"triage",
	"icebox",
	"backlog",
	"todo",
	"in_progress",
	"testing",
	"needs_review",
	"retro",
	"done",
}

var issueStatusSet = func() map[string]struct{} {
	statuses := make(map[string]struct{}, len(IssueStatuses))
	for _, status := range IssueStatuses {
		statuses[status] = struct{}{}
	}
	return statuses
}()

// IsIssueStatus reports whether status belongs to the Legion lifecycle.
func IsIssueStatus(status string) bool {
	_, ok := issueStatusSet[status]
	return ok
}

var (
	roleRoutePattern    = regexp.MustCompile(`^role:([a-z0-9-]+)$`)
	sessionRoutePattern = regexp.MustCompile(`^session:([A-Za-z0-9_-]+)$`)
)

// ParseRoute validates and decomposes one supported issue delivery route.
func ParseRoute(s string) (Route, error) {
	if match := roleRoutePattern.FindStringSubmatch(s); match != nil {
		return Route{Kind: "role", ID: match[1]}, nil
	}
	if match := sessionRoutePattern.FindStringSubmatch(s); match != nil {
		return Route{Kind: "session", ID: match[1]}, nil
	}
	return Route{}, fmt.Errorf("invalid route %q", s)
}
