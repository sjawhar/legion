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

// Anchor identifies a range of document text at a particular named version.
type Anchor struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
	Quote      string `json:"quote"`
	From       int    `json:"from"`
	To         int    `json:"to"`
	Orphaned   bool   `json:"orphaned"`
}

// AnchorInput selects document text by quote or by a browser-owned UTF-16 range.
type AnchorInput struct {
	Artifact   string  `json:"artifact"`
	Quote      *string `json:"quote,omitempty"`
	Occurrence *int    `json:"occurrence,omitempty"`
	From       *int    `json:"from,omitempty"`
	To         *int    `json:"to,omitempty"`
}

// Project groups native issues under a short key.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
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
	Parent    *string   `json:"parent"`
	UpdatedAt time.Time `json:"updated_at"`
	LastSeq   int       `json:"last_seq"`
	OpenAsks  int       `json:"open_asks"`
}

// IssueChild is a child item embedded in an issue detail response.
type IssueChild struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// Artifact is an issue-attached document or binary blob.
type Artifact struct {
	ID        string    `json:"id"`
	IssueKey  string    `json:"issue_key"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Primary   bool      `json:"primary"`
	CreatedBy Actor     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Versions  []Version `json:"versions"`
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
	ID         string         `json:"id"`
	IssueKey   string         `json:"issue_key"`
	Author     Actor          `json:"author"`
	Question   string         `json:"question"`
	Options    []AskOption    `json:"options"`
	Multiple   bool           `json:"multiple"`
	Urgency    string         `json:"urgency"`
	Anchor     *Anchor        `json:"anchor"`
	State      string         `json:"state"`
	Answer     *AskAnswer     `json:"answer"`
	Resolution *AskResolution `json:"resolution,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
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

// Comment is an issue comment, optionally with an edit suggestion. A comment
// replies to at most one of another comment (ReplyTo) or an ask (AskID): a
// top-level reply in an ask's thread sets AskID; a reply nested under that
// comment sets ReplyTo instead, so an ask's full thread is "AskID = the ask"
// plus the ReplyTo chains rooted at those comments.
type Comment struct {
	ID         string      `json:"id"`
	IssueKey   string      `json:"issue_key"`
	Author     Actor       `json:"author"`
	Body       string      `json:"body"`
	Anchor     *Anchor     `json:"anchor"`
	ReplyTo    *string     `json:"reply_to"`
	AskID      *string     `json:"ask_id"`
	Resolved   bool        `json:"resolved"`
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
}

// Message is a short issue update.
type Message struct {
	ID        string    `json:"id"`
	IssueKey  string    `json:"issue_key"`
	Author    Actor     `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ReferencedBy identifies a post that mentions an artifact.
type ReferencedBy struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	IssueKey string `json:"issue_key"`
	Excerpt  string `json:"excerpt"`
}

// Event is the immutable event-log record for an issue mutation.
type Event struct {
	ID        int64     `json:"id"`
	IssueKey  string    `json:"issue_key"`
	Seq       int       `json:"seq"`
	Type      string    `json:"type"`
	Actor     Actor     `json:"actor"`
	Notify    bool      `json:"notify"`
	CreatedAt time.Time `json:"created_at"`
	Payload   any       `json:"payload"`
}

// EditOp is one agent document editing operation.
type EditOp struct {
	Op         string `json:"op"`
	Find       string `json:"find,omitempty"`
	With       string `json:"with,omitempty"`
	Occurrence *int   `json:"occurrence,omitempty"`
	Markdown   string `json:"markdown,omitempty"`
	After      string `json:"after,omitempty"`
	Before     string `json:"before,omitempty"`
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
	sessionRoutePattern = regexp.MustCompile(`^session:([0-9a-f-]{16,})$`)
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
