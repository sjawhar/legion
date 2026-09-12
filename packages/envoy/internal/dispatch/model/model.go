// Package model defines Dispatch's persisted and wire entities.
package model

import (
	"fmt"
	"regexp"
	"time"
)

// Actor identifies the user or agent session that caused a change.
type Actor struct {
	Kind   string       `json:"kind"`
	ID     string       `json:"id"`
	Origin *ActorOrigin `json:"origin,omitempty"`
	Owner  *string      `json:"owner,omitempty"`
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
	ArtifactID string `json:"artifact_id"`
	MarkID     string `json:"mark_id"`
	Version    int    `json:"version"`
	Quote      string `json:"quote"`
	Orphaned   bool   `json:"orphaned"`
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

// ExternalLink connects a native issue to an external tracker item.
type ExternalLink struct {
	URL  string `json:"url"`
	Kind string `json:"kind,omitempty"`
}

// Issue is the complete native issue record.
type Issue struct {
	Key               string         `json:"key"`
	Project           string         `json:"project"`
	Number            int            `json:"number"`
	Title             string         `json:"title"`
	Status            string         `json:"status"`
	Rank              string         `json:"rank"`
	Labels            []string       `json:"labels"`
	Parent            *string        `json:"parent"`
	ExternalLinks     []ExternalLink `json:"external_links"`
	Route             *string        `json:"route"`
	CreatedBy         Actor          `json:"created_by"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	ClosedAt          *time.Time     `json:"closed_at"`
	PrimaryArtifactID string         `json:"primary_artifact_id"`
	LastSeq           int            `json:"last_seq"`
}

// IssueSummary is the lightweight issue listing representation.
type IssueSummary struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Rank      string    `json:"rank"`
	Labels    []string  `json:"labels"`
	Parent    *string   `json:"parent"`
	UpdatedAt time.Time `json:"updated_at"`
	LastSeq   int       `json:"last_seq"`
	OpenAsks  int       `json:"open_asks"`
}

// SearchIssue is legacy issue-shaped display metadata for a global search result.
type SearchIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// SearchOwner identifies the issue or standalone project document that owns a search result.
type SearchOwner struct {
	Kind       string `json:"kind"`
	Key        string `json:"key,omitempty"`
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

// SearchResult is one ranked Dispatch search hit.
type SearchResult struct {
	Kind     string          `json:"kind"`
	Owner    SearchOwner     `json:"owner"`
	Issue    SearchIssue     `json:"issue"`
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

// IssueChild is a child item embedded in an issue detail response.
type IssueChild struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
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
	ID   string `json:"id"`
	Type string `json:"type"`
	From int    `json:"from"`
	To   int    `json:"to"`
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
	BlockArtifactID *string           `json:"-"`
	BlockArtifact   *AskBlockArtifact `json:"block_artifact,omitempty"`
	Author          Actor             `json:"author"`
	// Kind is "question" for ordinary asks, "action" for fixed Done / Can't
	// human to-dos, and "approval" for server-created document reviews.
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
	EditedAt      *string        `json:"edited_at"`
	// Approval names the document an approval ask is about; nil for questions.
	Approval *AskApproval `json:"approval,omitempty"`
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

// AskLastReply is the newest comment in an ask's thread, carried on inbox rows so
// a human can see who spoke last: a human reply on an open ask means the asker
// owes the next turn.
type AskLastReply struct {
	Author    Actor  `json:"author"`
	CreatedAt string `json:"created_at"`
}

// AskEditEventPayload is the full edited ask plus its prior mutable content
// and the actor who made the edit.
type AskEditEventPayload struct {
	Ask
	Previous AskEditPrevious `json:"previous"`
	EditedBy Actor           `json:"edited_by"`
}

// BlockRepairedEventPayload records one server-owned typed-block repair in a version.
type BlockRepairedEventPayload struct {
	BlockID     string `json:"block_id"`
	Version     int    `json:"version"`
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
// replies to at most one of another comment (ReplyTo) or an ask (AskID): a
// top-level reply in an ask's thread sets AskID; a reply nested under that
// comment sets ReplyTo instead, so an ask's full thread is "AskID = the ask"
// plus the ReplyTo chains rooted at those comments.
type Comment struct {
	ID         string      `json:"id"`
	IssueKey   *string     `json:"issue_key"`
	ArtifactID *string     `json:"artifact_id"`
	Author     Actor       `json:"author"`
	Body       string      `json:"body"`
	Anchor     *Anchor     `json:"anchor"`
	ReplyTo    *string     `json:"reply_to"`
	AskID      *string     `json:"ask_id"`
	Resolved   bool        `json:"resolved"`
	ResolvedBy *Actor      `json:"resolved_by"`
	ResolvedAt *string     `json:"resolved_at"`
	EditedAt   *string     `json:"edited_at"`
	Suggestion *Suggestion `json:"suggestion"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Suggestion carries a proposed document replacement and its resolution.
type Suggestion struct {
	ReplaceWith string `json:"replace_with"`
	Accepted    *bool  `json:"accepted"`
}

// CommentEventPayload is the wire payload of comment.* and suggestion.* events:
// the comment's own fields plus the anchored artifact's name. Comment is
// embedded so the JSON stays flat, which is the shape every consumer reads.
type CommentEventPayload struct {
	Comment
	ArtifactName string `json:"artifact_name"`
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
	// ThreadRootID is the id of the comment thread's root (Comment.ReplyTo's
	// target) for a comment.created event that replies to another comment, so
	// a routed agent can reply under the same root humans use. Empty when the
	// comment does not reply to another comment.
	ThreadRootID string `json:"thread_root_id,omitempty"`
}

// Message is a short issue update, optionally threaded under another message.
type Message struct {
	ID         string            `json:"id"`
	IssueKey   string            `json:"issue_key"`
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
}

// MessageDeliveryEventPayload is the user-visible result of one target delivery attempt.
type MessageDeliveryEventPayload struct {
	MessageID string `json:"message_id"`
	Attempt   int    `json:"attempt"`
	Delivery  string `json:"delivery"`
	SessionID string `json:"session_id"`
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

// Event is the immutable event-log record for a Dispatch mutation.
type Event struct {
	ID         int64     `json:"id"`
	IssueKey   *string   `json:"issue_key"`
	ArtifactID *string   `json:"artifact_id"`
	Project    string    `json:"project"`
	Seq        int       `json:"seq"`
	Type       string    `json:"type"`
	Actor      Actor     `json:"actor"`
	Notify     bool      `json:"notify"`
	CreatedAt  time.Time `json:"created_at"`
	Payload    any       `json:"payload"`
}

// EditOp is one agent document editing operation.
type EditOp struct {
	Op         string         `json:"op"`
	Find       string         `json:"find,omitempty"`
	With       string         `json:"with,omitempty"`
	Occurrence *int           `json:"occurrence,omitempty"`
	Markdown   string         `json:"markdown,omitempty"`
	After      string         `json:"after,omitempty"`
	Before     string         `json:"before,omitempty"`
	Block      string         `json:"block,omitempty"`
	Type       string         `json:"type,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Route is a validated issue delivery route.
type Route struct {
	Kind string
	ID   string
}

// IssueStatuses is the canonical Legion issue lifecycle.
var IssueStatuses = map[string]struct{}{
	"triage":       {},
	"icebox":       {},
	"backlog":      {},
	"todo":         {},
	"in_progress":  {},
	"testing":      {},
	"needs_review": {},
	"retro":        {},
	"done":         {},
}

// IsIssueStatus reports whether status belongs to the Legion lifecycle.
func IsIssueStatus(status string) bool {
	_, ok := IssueStatuses[status]
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
