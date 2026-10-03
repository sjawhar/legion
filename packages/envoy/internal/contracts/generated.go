package contracts

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Envelope struct {
	EventID        string          `json:"event_id"`
	Source         string          `json:"source"`
	SourceEventID  string          `json:"source_event_id"`
	SourceSession  string          `json:"source_session,omitempty"`
	Topic          string          `json:"topic"`
	DedupeKey      string          `json:"dedupe_key"`
	IssuedAt       int64           `json:"issued_at"`
	ExpiresAt      *int64          `json:"expires_at,omitempty"`
	PayloadSummary string          `json:"payload_summary"`
	PayloadRef     string          `json:"payload_ref,omitempty"`
	Payload        string          `json:"payload,omitempty"`
	TraceID        string          `json:"trace_id"`
	Sender         *EnvelopeSender `json:"sender,omitempty"`
	InReplyTo      string          `json:"in_reply_to,omitempty"`
	Supersedes     string          `json:"supersedes,omitempty"`
	Urgency        string          `json:"urgency,omitempty"`
	ExpectsReply   string          `json:"expects_reply,omitempty"`
}

type EnvelopeSender struct {
	SessionID string   `json:"session_id"`
	Machine   string   `json:"machine,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Title     string   `json:"title,omitempty"`
	Roles     []string `json:"roles,omitempty"`
}

func (e Envelope) Validate() error {
	if strings.TrimSpace(e.EventID) == "" {
		return fmt.Errorf("event_id is required")
	}
	if strings.TrimSpace(e.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(e.SourceEventID) == "" {
		return fmt.Errorf("source_event_id is required")
	}
	if strings.TrimSpace(e.Topic) == "" {
		return fmt.Errorf("topic is required")
	}
	if strings.TrimSpace(e.DedupeKey) == "" {
		return fmt.Errorf("dedupe_key is required")
	}
	if e.IssuedAt == 0 {
		return fmt.Errorf("issued_at must be set")
	}
	if strings.TrimSpace(e.PayloadSummary) == "" {
		return fmt.Errorf("payload_summary is required")
	}
	if strings.TrimSpace(e.TraceID) == "" {
		return fmt.Errorf("trace_id is required")
	}
	if e.InReplyTo != "" && strings.TrimSpace(e.InReplyTo) == "" {
		return fmt.Errorf("in_reply_to must not be empty")
	}
	if e.Supersedes != "" && strings.TrimSpace(e.Supersedes) == "" {
		return fmt.Errorf("supersedes must not be empty")
	}
	switch e.Source {
	case "agent", "human", "envoy", "github", "slack", "whatsapp", "ghostwispr", "dispatch":
	default:
		return fmt.Errorf("source must be one of: agent, human, envoy, github, slack, whatsapp, ghostwispr, dispatch")
	}
	if e.Urgency != "" {
		switch e.Urgency {
		case "low", "med", "high", "blocking":
		default:
			return fmt.Errorf("urgency must be one of: low, med, high, blocking")
		}
	}
	if e.ExpectsReply != "" {
		switch e.ExpectsReply {
		case "none", "optional", "required":
		default:
			return fmt.Errorf("expects_reply must be one of: none, optional, required")
		}
	}
	if e.Sender != nil {
		if strings.TrimSpace(e.Sender.SessionID) == "" {
			return fmt.Errorf("sender.session_id is required")
		}
	}
	return nil
}

func ValidateWire(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, field := range []string{"in_reply_to", "supersedes", "urgency", "expects_reply"} {
		if err := validateWireNonEmpty(fields, field, field); err != nil {
			return err
		}
	}
	senderRaw, found := fields["sender"]
	if !found {
		return nil
	}
	var sender map[string]json.RawMessage
	if err := json.Unmarshal(senderRaw, &sender); err != nil {
		return err
	}
	return validateWireNonEmpty(sender, "session_id", "sender.session_id")
}

func validateWireNonEmpty(fields map[string]json.RawMessage, field, path string) error {
	raw, found := fields[field]
	if !found {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("%s must not be empty", path)
	}
	return nil
}

const AgentTopicPrefix = "notifications.agent."
const RoleTopicPrefix = "notifications.role."

// DeliveryDuplicateWindow is how long a repeated delivery is recognised as a duplicate, by the
// notification stream and by every host that subscribes over core NATS. Generated from
// DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts, whose doc comment states the retry promise
// this window bounds, so the stream, the hosts and the dashboard read one number.
const DeliveryDuplicateWindow = 259200000 * time.Millisecond

// MintedDedupeKeyPattern is the shape of a dedupe key minted once for its message, by the listener
// or by the shared transport's idempotency key, which DedupeKeyNamesTheUpstreamEvent counts as
// naming its event. Generated from MINTED_DEDUPE_KEY_PATTERN in packages/contracts, which the
// hosts' dedupe reads too, so the stream and the hosts answer one question the same way.
const MintedDedupeKeyPattern = "^(?:envoy\\.role\\.forward\\.)?(?:publish|agent\\.[^.]+)\\.(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$"

// ReceiptTimeoutCause is what a delivery attempt records when the listener never answered its
// send. Generated from RECEIPT_TIMEOUT_CAUSE in packages/contracts so the string Dispatch writes
// and the string the dashboard keys its retry wording on cannot drift apart.
const ReceiptTimeoutCause = "The listener didn't answer within the send window; the message may already have been delivered."

// MaxBroadcastRecipients is the most sessions one broadcast sends to. Generated from
// MAX_BROADCAST_RECIPIENTS in packages/contracts so the server's limit and the dashboard's
// cannot drift apart.
const MaxBroadcastRecipients = 100

// SearchQueryMax is the longest GET /api/v1/search query, in UTF-16 units. Generated from
// SEARCH_QUERY_MAX in packages/contracts so the server's refusal and the dispatch_search tool's
// cannot drift apart.
const SearchQueryMax = 1000

// SearchQueryHint follows a refusal over SearchQueryMax, saying what to send instead. Generated
// from SEARCH_QUERY_HINT in packages/contracts so the server and the tool word it once.
const SearchQueryHint = "search with a short phrase of a few words, not a passage"

// MaxIssueBlockers bounds blocked_by input before deduplication. Generated from
// MAX_ISSUE_BLOCKERS in packages/contracts so the server and tool schemas share the limit.
const MaxIssueBlockers = 20

// MaxIssuePageLimit is the most issues one page of GET /api/v1/issues holds, and
// DefaultIssuePageLimit the page size when a caller pages with offset alone. Generated from
// MAX_ISSUE_PAGE_LIMIT and DEFAULT_ISSUE_PAGE_LIMIT in packages/contracts so the server's bounds
// and the dispatch_issues tool's cannot drift apart.
const MaxIssuePageLimit = 250
const DefaultIssuePageLimit = 50

// DocumentSchemaCloseCode and DocumentSchemaCloseReason close a document websocket whose room is
// outside the Proof schema, before any sync. Generated from DOCUMENT_SCHEMA_CLOSE_CODE and
// DOCUMENT_SCHEMA_CLOSE_REASON in packages/contracts so the server's close and the dashboard's
// reading of it cannot drift apart.
const DocumentSchemaCloseCode = 4409
const DocumentSchemaCloseReason = "DOC_SCHEMA"

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}

// githubRepositoryPrefix begins every GitHub subject with its repository's owner and name, each one
// segment: a name may hold a dot, which a NATS subject splits on. Mirrored on the TS side as
// `githubRepositoryPrefix`.
func githubRepositoryPrefix(owner, repo string) string {
	return "notifications.github." + SanitizeSubjectSegment(owner) + "." + SanitizeSubjectSegment(repo)
}

func GithubSubject(owner string, repo string, kind string) string {
	return githubRepositoryPrefix(owner, repo) + "." + kind
}

func SlackSubject(team string, channel string, kind string) string {
	return "notifications.slack." + team + "." + channel + "." + kind
}

// SanitizeSubjectSegment makes a value one NATS subject segment: a dot, which a subject splits on,
// and whitespace and the wildcards * and >, which a published subject may not hold, each become an
// underscore. A slash is kept. Generated from SUBJECT_SEGMENT_REPLACED in packages/contracts, which
// `sanitizeSubjectSegment` reads too, so what the listener publishes and what a consumer expects
// cannot drift apart.
func SanitizeSubjectSegment(value string) string {
	return subjectSegmentSanitizer.Replace(value)
}

var subjectSegmentSanitizer = strings.NewReplacer(".", "_", " ", "_", "\t", "_", "\r", "_", "\n", "_", "*", "_", ">", "_")

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + SanitizeSubjectSegment(threadTs) + "." + kind
}

func GithubPushSubject(owner, repo, refType, refName string) string {
	return githubRepositoryPrefix(owner, repo) + ".push." + refType + "." + SanitizeSubjectSegment(refName)
}

func GithubWorkflowSubject(owner, repo, workflowFilename, action string) string {
	return githubRepositoryPrefix(owner, repo) + ".workflow." + SanitizeSubjectSegment(workflowFilename) + "." + action
}

func GithubResourceSubject(owner string, repo string, resourceType string, resourceNumber string) string {
	return githubRepositoryPrefix(owner, repo) + "." + resourceType + "." + resourceNumber
}

const GhostWisprTopicPrefix = "notifications.ghostwispr."

func GhostWisprSubject(sessionId string, kind string) string {
	return GhostWisprTopicPrefix + sessionId + "." + kind
}

// AgentStreamSubjectPrefix roots the live agent conversation stream. Generated from
// AGENT_STREAM_SUBJECT_PREFIX in packages/contracts so the session that publishes its own turns
// and the Dispatch relay that forwards them to a browser cannot drift apart. It sits outside
// "notifications." deliberately: that family is what the notification stream captures, so every
// frame travels over core NATS and the bus retains none of it.
const AgentStreamSubjectPrefix = "agentstream."

func AgentStreamFramesSubject(sessionID string) string {
	return AgentStreamSubjectPrefix + sessionID + ".frames"
}

func AgentStreamControlSubject(sessionID string) string {
	return AgentStreamSubjectPrefix + sessionID + ".control"
}

func WhatsappSubject(phone, jid, kind string) string {
	return "notifications.whatsapp." + phone + "." + jid + "." + kind
}
