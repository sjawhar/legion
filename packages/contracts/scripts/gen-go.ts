import { mkdirSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";

import { AGENT_STREAM_SUBJECT_PREFIX } from "../src/agent-stream";
import {
  DEFAULT_ISSUE_PAGE_LIMIT,
  DELIVERY_DUPLICATE_WINDOW_MS,
  DISPATCH_LOGIN_CHANGED,
  DISPATCH_LOGIN_HEADER,
  DISPATCH_SIGN_IN_REQUIRED,
  DOCUMENT_SCHEMA_CLOSE_CODE,
  DOCUMENT_SCHEMA_CLOSE_REASON,
  ID_AMBIGUOUS,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_INVALID,
  IDEMPOTENCY_KEY_MAX,
  IDEMPOTENCY_KEY_PATTERN,
  IDEMPOTENCY_KEY_REUSED,
  IDEMPOTENCY_KEY_TTL_MS,
  IDEMPOTENCY_LEASE_MS,
  IDEMPOTENCY_REPLAYED_HEADER,
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  ISSUE_TITLE_MAX,
  LIVE_SOCKET_MAX_AGE_CLOSE,
  LIVE_SOCKET_MAX_AGE_MS,
  LIVE_SOCKET_MAX_SUBSCRIPTIONS,
  LIVE_SOCKET_MIN_AGE_MS,
  MAX_BROADCAST_RECIPIENTS,
  MAX_ISSUE_BLOCKERS,
  MAX_ISSUE_PAGE_LIMIT,
  PROJECT_NAME_MAX,
  RECEIPT_TIMEOUT_CAUSE,
  SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  SEARCH_OPERATOR_EXCLUDE,
  SEARCH_OPERATOR_OR,
  SEARCH_OPERATOR_PHRASE,
} from "../src/dispatch-api";
import {
  DISPATCH_BLOCK_UNITS,
  DISPATCH_EVENT_BODY_MAX,
  DISPATCH_HANDOFF_BODY_MAX,
  DISPATCH_ITEM_BLOCKS,
  dispatchToolSpecs,
  HANDOFF_PHASE_PATTERN,
  HANDOFF_PHASE_RULE_WORDS,
  PROJECT_KEY_PATTERN,
  PROJECT_KEY_RULE,
  SEARCH_KIND_DEPTH,
  SEARCH_QUERY_HINT,
  SEARCH_QUERY_MAX,
} from "../src/dispatch-tools";
import { MINTED_DEDUPE_KEY_PATTERN } from "../src/envelope";
import {
  LISTENER_CALL_ATTEMPTS,
  LISTENER_CALL_RETRY_DELAY_MS,
  LISTENER_CALL_TIMEOUT_MS,
  SESSION_CLAIM_STALE_MS,
  SESSION_HEARTBEAT_MS,
  SESSION_LIVE_WINDOW_MS,
} from "../src/envoy-session";
import {
  GOING_ANSWER_WORD_MAX,
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_NO_MODEL_CONFIGURED,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_END,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_TEXT,
  GOING_HIT_RUNE_MAX,
  GOING_MODEL_CALL_MAX,
  GOING_REQUEST_RUNE_MAX,
  GOING_REQUEST_TTL_MS,
  GOING_RUN_MS,
  GOING_SEARCH_MAX,
  GOING_SEARCH_TOOL,
  GOING_WORDS_MAX,
} from "../src/going";
import { SUBJECT_MAX_BYTES, SUBJECT_SEGMENT_REPLACED } from "../src/subject";
import type { SchemaApi, SchemaNode } from "../src/tool-schema";

type ScalarKind = "string" | "integer" | "boolean";

type Prop = {
  type: ScalarKind | "array" | "object";
  enum?: string[];
  minLength?: number;
  properties?: Record<string, Prop>;
  required?: string[];
  items?: Prop;
};

type Schema = {
  type: "object";
  required?: string[];
  properties: Record<string, Prop>;
  additionalProperties?: boolean;
};

const envelopeFile = resolve(import.meta.dir, "../schemas/envelope.schema.json");
const blockSchemaFile = resolve(import.meta.dir, "../schemas/block-schema.schema.json");

const map = {
  string: "string",
  integer: "int64",
  boolean: "bool",
} satisfies Record<ScalarKind, string>;

const keep = `func ValidateWire(raw []byte) error {
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

// GithubTopicPrefix begins every GitHub subject, before the repository's owner and name.
const GithubTopicPrefix = "notifications.github."

// DeliveryDuplicateWindow is how long a repeated delivery is recognised as a duplicate, by the
// notification stream and by every host that subscribes over core NATS. Generated from
// DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts, whose doc comment states the retry promise
// this window bounds, so the stream, the hosts and the dashboard read one number.
const DeliveryDuplicateWindow = ${DELIVERY_DUPLICATE_WINDOW_MS} * time.Millisecond

// IssueClaimUnresponsiveGrace is how long an issue claim is held for a holder the Envoy registry
// calls unresponsive: a session that missed its heartbeats keeps the issue it is working until it
// was last seen longer ago than this, and only then may another agent take or release the claim
// without a person's force. Generated from ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS in
// packages/contracts, whose doc comment states the rule, so the server's refusal and the wait a
// client predicts are one number.
const IssueClaimUnresponsiveGrace = ${ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS} * time.Millisecond

// MintedDedupeKeyPattern is the shape of a dedupe key minted once for its message, by the listener
// or by the shared transport's idempotency key, which DedupeKeyNamesTheUpstreamEvent counts as
// naming its event. Generated from MINTED_DEDUPE_KEY_PATTERN in packages/contracts, which the
// hosts' dedupe reads too, so the stream and the hosts answer one question the same way.
const MintedDedupeKeyPattern = ${JSON.stringify(MINTED_DEDUPE_KEY_PATTERN)}

// ReceiptTimeoutCause is what a delivery attempt records when the listener never answered its
// send. Generated from RECEIPT_TIMEOUT_CAUSE in packages/contracts so the string Dispatch writes
// and the string the dashboard keys its retry wording on cannot drift apart.
const ReceiptTimeoutCause = ${JSON.stringify(RECEIPT_TIMEOUT_CAUSE)}

// MaxBroadcastRecipients is the most sessions one broadcast sends to. Generated from
// MAX_BROADCAST_RECIPIENTS in packages/contracts so the server's limit and the dashboard's
// cannot drift apart.
const MaxBroadcastRecipients = ${MAX_BROADCAST_RECIPIENTS}

// SearchQueryMax is the longest GET /api/v1/search query, in UTF-16 units. Generated from
// SEARCH_QUERY_MAX in packages/contracts so the server's refusal and the \`dispatch search\`
// command's cannot drift apart.
const SearchQueryMax = ${SEARCH_QUERY_MAX}

// SearchQueryHint follows a refusal over SearchQueryMax, saying what to send instead. Generated
// from SEARCH_QUERY_HINT in packages/contracts so the server and the tool word it once.
const SearchQueryHint = ${JSON.stringify(SEARCH_QUERY_HINT)}

// ProjectKeyRule says the project-key pattern's rule in words; every refusal of a key that is not
// a project key gives it. Generated from PROJECT_KEY_RULE in packages/contracts so the server and
// the tool word it once.
const ProjectKeyRule = ${JSON.stringify(PROJECT_KEY_RULE)}

// ProjectKeyPattern is a whole project key, as the server creates them and the projects.key check
// constraint holds. Generated from PROJECT_KEY_PATTERN in packages/contracts, which the dispatch
// command checks a key against, so the two accept the same keys.
const ProjectKeyPattern = ${JSON.stringify(PROJECT_KEY_PATTERN.source)}

// SearchKindDepth is how many of its best matches each kind of content lists before
// GET /api/v1/search merges the kinds; a kind's later matches count in the total and no offset
// returns them. Generated from SEARCH_KIND_DEPTH in packages/contracts so the server's cut and the
// \`dispatch search\` command's account of it cannot drift apart.
const SearchKindDepth = ${SEARCH_KIND_DEPTH}

// SearchDegradedEmbedderUnavailable is GET /api/v1/search's SearchResponse.degraded value when
// meaning search could not run for this request and search fell back to keyword-only ranking.
// Generated from SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE in packages/contracts so the string
// Dispatch writes and the string search-answer.ts compares against cannot drift apart.
const SearchDegradedEmbedderUnavailable = ${JSON.stringify(SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE)}

// SearchOperatorPhrase, SearchOperatorOr and SearchOperatorExclude are the operator forms
// GET /api/v1/search reads in a person's words and names in its answer's query.operators: a
// quoted phrase, the OR between two words, and a -word it leaves out. Generated from
// SEARCH_OPERATOR_PHRASE, SEARCH_OPERATOR_OR and SEARCH_OPERATOR_EXCLUDE in packages/contracts so
// the strings Dispatch answers and the strings a client reads them by cannot drift apart.
const (
	SearchOperatorPhrase  = ${JSON.stringify(SEARCH_OPERATOR_PHRASE)}
	SearchOperatorOr      = ${JSON.stringify(SEARCH_OPERATOR_OR)}
	SearchOperatorExclude = ${JSON.stringify(SEARCH_OPERATOR_EXCLUDE)}
)

// The names of the frames one answer's stream carries: text as the answer arrives, item for each
// thing the answer cites, cut when the answer reached its word cap, changed when the server did
// not pass the answer on as the model wrote it, none when the model produced no answer text at
// all, and end and error, the two that end a stream. A text frame carries plain text, every
// ASCII punctuation byte of it backslash-escaped, which closes markdown's own constructs, so a
// renderer makes no link, image, tag or code span of any of it; an escape closes none of GFM's
// literal autolinks, so a word a renderer would link on its own is dropped from the answer
// rather than sent; and an item frame is where the links of an answer live, each carrying a
// cited item's own href.
// Generated from GOING_EVENT_TEXT, GOING_EVENT_ITEM, GOING_EVENT_CUT,
// GOING_EVENT_CHANGED, GOING_EVENT_NONE, GOING_EVENT_END and GOING_EVENT_ERROR in
// packages/contracts so the frames the server writes and the frames the page reads cannot drift
// apart.
const GoingEventText = ${JSON.stringify(GOING_EVENT_TEXT)}
const GoingEventItem = ${JSON.stringify(GOING_EVENT_ITEM)}
const GoingEventCut = ${JSON.stringify(GOING_EVENT_CUT)}
const GoingEventChanged = ${JSON.stringify(GOING_EVENT_CHANGED)}
const GoingEventNone = ${JSON.stringify(GOING_EVENT_NONE)}
const GoingEventEnd = ${JSON.stringify(GOING_EVENT_END)}
const GoingEventError = ${JSON.stringify(GOING_EVENT_ERROR)}

// The codes an error frame carries: the run reached its window without an answer, the deployment
// has no text model configured, and the model service refused or failed. Generated from
// GOING_ERROR_ANSWER_DID_NOT_ARRIVE, GOING_ERROR_NO_MODEL_CONFIGURED and
// GOING_ERROR_ANSWER_FAILED in packages/contracts so the code the server writes and the wording
// the page keys on it cannot drift apart.
const GoingErrorAnswerDidNotArrive = ${JSON.stringify(GOING_ERROR_ANSWER_DID_NOT_ARRIVE)}
const GoingErrorNoModelConfigured = ${JSON.stringify(GOING_ERROR_NO_MODEL_CONFIGURED)}
const GoingErrorAnswerFailed = ${JSON.stringify(GOING_ERROR_ANSWER_FAILED)}

// GoingSearchTool is the name of the one tool the model is given, another search of Dispatch.
// Generated from GOING_SEARCH_TOOL in packages/contracts so the name the server declares and the
// name the page explains cannot drift apart.
const GoingSearchTool = ${JSON.stringify(GOING_SEARCH_TOOL)}

// GoingAnswerWordMax is the most words an answer carries, past which the stream cuts it and says
// so. Generated from GOING_ANSWER_WORD_MAX in packages/contracts so the server's cut and the
// page's account of it cannot drift apart.
const GoingAnswerWordMax = ${GOING_ANSWER_WORD_MAX}

// GoingHitRuneMax is the most of one hit's own text the model is shown, and GoingRequestRuneMax
// the most hit text one run shows it in total; a hit past the total is still named to the model
// with no body, and stays something the answer may cite.
// Generated from GOING_HIT_RUNE_MAX and GOING_REQUEST_RUNE_MAX in packages/contracts so the
// server's bounds and the page's account of them cannot drift apart.
const GoingHitRuneMax = ${GOING_HIT_RUNE_MAX}
const GoingRequestRuneMax = ${GOING_REQUEST_RUNE_MAX}

// GoingSearchMax is the most searches one run makes, the server's own seeding search counting as
// the first, and GoingModelCallMax the most calls it makes to the model, which bounds a model
// that keeps searching instead of answering. Generated from GOING_SEARCH_MAX and
// GOING_MODEL_CALL_MAX in packages/contracts so the server's bounds and the page's account of
// them cannot drift apart.
const GoingSearchMax = ${GOING_SEARCH_MAX}
const GoingModelCallMax = ${GOING_MODEL_CALL_MAX}

// GoingWordsMax is the longest question the route accepts, in UTF-16 units. Generated from
// GOING_WORDS_MAX in packages/contracts so the server's refusal and the page's own cannot drift
// apart.
const GoingWordsMax = ${GOING_WORDS_MAX}

// GoingRunWindow is how long one run may take, measured from the POST that started it, and
// GoingRequestTTL how long a run's rows are kept, so a reader that reconnects still finds its
// answer. Generated from GOING_RUN_MS and GOING_REQUEST_TTL_MS in packages/contracts so the
// server's windows and the page's account of them cannot drift apart.
const GoingRunWindow = ${GOING_RUN_MS} * time.Millisecond
const GoingRequestTTL = ${GOING_REQUEST_TTL_MS} * time.Millisecond

// MaxIssueBlockers bounds blocked_by input before deduplication. Generated from
// MAX_ISSUE_BLOCKERS in packages/contracts so the server and tool schemas share the limit.
const MaxIssueBlockers = ${MAX_ISSUE_BLOCKERS}

// MaxIssuePageLimit is the most issues one page of GET /api/v1/issues holds, and
// DefaultIssuePageLimit the page size when a caller pages with offset alone. Generated from
// MAX_ISSUE_PAGE_LIMIT and DEFAULT_ISSUE_PAGE_LIMIT in packages/contracts so the server's bounds
// and the \`dispatch issues\` command's cannot drift apart.
const MaxIssuePageLimit = ${MAX_ISSUE_PAGE_LIMIT}
const DefaultIssuePageLimit = ${DEFAULT_ISSUE_PAGE_LIMIT}

// IssueTitleMax is the longest issue title, in UTF-16 units, that POST /api/v1/issues and
// PATCH /api/v1/issues/{key} accept. Generated from ISSUE_TITLE_MAX in packages/contracts so the
// server's refusal and the dashboard's title fields cannot drift apart.
const IssueTitleMax = ${ISSUE_TITLE_MAX}

// DispatchBlockUnits is the longest one block of a comment, comment edit, message, broadcast or
// ask part may be, in UTF-16 units. Generated from DISPATCH_BLOCK_UNITS in packages/contracts;
// the server enforces it, names it in its refusal, and publishes it through GET /api/v1/limits.
const DispatchBlockUnits = ${DISPATCH_BLOCK_UNITS}

// DispatchItemBlocks is the most blocks one body may hold. Generated from DISPATCH_ITEM_BLOCKS in
// packages/contracts; the server enforces it, names it in its refusal, and publishes it through
// GET /api/v1/limits.
const DispatchItemBlocks = ${DISPATCH_ITEM_BLOCKS}

// DispatchEventBodyMax is the longest body an event carries whole, in UTF-16 units, and the bound
// on an ask's question and its options together. Generated from DISPATCH_EVENT_BODY_MAX in
// packages/contracts; the server enforces it, names it in its refusal, and publishes it through
// GET /api/v1/limits.
const DispatchEventBodyMax = ${DISPATCH_EVENT_BODY_MAX}

// DispatchHandoffBodyMax is the longest phase handoff body, in UTF-8 bytes, that
// POST /api/v1/issues/{key}/handoffs/{phase} accepts. Generated from DISPATCH_HANDOFF_BODY_MAX in
// packages/contracts so the server's 413 and the \`dispatch handoff-write\` command's pre-send
// refusal cannot drift apart.
const DispatchHandoffBodyMax = ${DISPATCH_HANDOFF_BODY_MAX}

// HandoffPhaseRule says the handoff-phase pattern's rule in words; every refusal of a phase that is
// not a handoff phase gives it. Generated from HANDOFF_PHASE_RULE_WORDS in packages/contracts so
// the server and the tool word it once.
const HandoffPhaseRule = ${JSON.stringify(HANDOFF_PHASE_RULE_WORDS)}

// HandoffPhasePattern is a handoff's phase, the {phase} segment of the handoff routes, which the
// handoffs.phase check constraint holds too. Generated from HANDOFF_PHASE_PATTERN in
// packages/contracts, which the dispatch command checks a phase against, so the two accept the
// same phases.
const HandoffPhasePattern = ${JSON.stringify(HANDOFF_PHASE_PATTERN.source)}

// ProjectNameMax is the longest project name, in UTF-16 units, that POST /api/v1/projects
// accepts. Generated from PROJECT_NAME_MAX in packages/contracts so the server's refusal, the
// Settings name field and the dispatch command cannot drift apart.
const ProjectNameMax = ${PROJECT_NAME_MAX}

// DocumentSchemaCloseCode and DocumentSchemaCloseReason close a document websocket whose room is
// outside the Proof schema, before any sync. Generated from DOCUMENT_SCHEMA_CLOSE_CODE and
// DOCUMENT_SCHEMA_CLOSE_REASON in packages/contracts so the server's close and the dashboard's
// reading of it cannot drift apart.
const DocumentSchemaCloseCode = ${DOCUMENT_SCHEMA_CLOSE_CODE}
const DocumentSchemaCloseReason = ${JSON.stringify(DOCUMENT_SCHEMA_CLOSE_REASON)}

// LiveSocketMaxAgeClose is the close code Dispatch ends a live socket with once the socket reaches
// its age, a random point in [LiveSocketMinAge, LiveSocketMaxAge), and LiveSocketMaxSubscriptions
// the most subscriptions one live socket holds. Generated from LIVE_SOCKET_MAX_AGE_CLOSE,
// LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS and LIVE_SOCKET_MAX_SUBSCRIPTIONS in
// packages/contracts so the server's close and limit and the dashboard's reading of them cannot
// drift apart.
const LiveSocketMaxAgeClose = ${LIVE_SOCKET_MAX_AGE_CLOSE}
const LiveSocketMinAge = ${LIVE_SOCKET_MIN_AGE_MS} * time.Millisecond
const LiveSocketMaxAge = ${LIVE_SOCKET_MAX_AGE_MS} * time.Millisecond
const LiveSocketMaxSubscriptions = ${LIVE_SOCKET_MAX_SUBSCRIPTIONS}

// IdempotencyKeyHeader is the request header a write names its intent with, the same on every
// retry of that intent, and IdempotencyReplayedHeader the response header (?1) on an answer
// replayed from the first request under the key rather than run again. Generated from
// IDEMPOTENCY_KEY_HEADER and IDEMPOTENCY_REPLAYED_HEADER in packages/contracts so the server, the
// dashboard and the agents' client name them once.
const IdempotencyKeyHeader = ${JSON.stringify(IDEMPOTENCY_KEY_HEADER)}
const IdempotencyReplayedHeader = ${JSON.stringify(IDEMPOTENCY_REPLAYED_HEADER)}

// IdempotencyKeyMax is the longest key and IdempotencyKeyPattern a whole one. Generated from
// IDEMPOTENCY_KEY_MAX and IDEMPOTENCY_KEY_PATTERN in packages/contracts so the server's refusal and
// the keys every client mints cannot drift apart.
const IdempotencyKeyMax = ${IDEMPOTENCY_KEY_MAX}
const IdempotencyKeyPattern = ${JSON.stringify(IDEMPOTENCY_KEY_PATTERN.source)}

// The codes of a keyed write's refusals and of a replay whose first answer was not kept: a key
// used for another request (422), a key whose first request still runs (409), a key whose write
// landed and whose answer was lost (409), a header that is not a key (400), and a first answer too
// large to keep. Generated from IDEMPOTENCY_KEY_REUSED, IDEMPOTENCY_KEY_IN_FLIGHT,
// IDEMPOTENCY_KEY_COMMITTED, IDEMPOTENCY_KEY_INVALID and IDEMPOTENCY_ANSWER_NOT_KEPT in
// packages/contracts so the server's codes and the clients' reading of them cannot drift apart.
const IdempotencyKeyReused = ${JSON.stringify(IDEMPOTENCY_KEY_REUSED)}
const IdempotencyKeyInFlight = ${JSON.stringify(IDEMPOTENCY_KEY_IN_FLIGHT)}
const IdempotencyKeyCommitted = ${JSON.stringify(IDEMPOTENCY_KEY_COMMITTED)}
const IdempotencyKeyInvalid = ${JSON.stringify(IDEMPOTENCY_KEY_INVALID)}
const IdempotencyAnswerNotKept = ${JSON.stringify(IDEMPOTENCY_ANSWER_NOT_KEPT)}

// IdempotencyKeyTTL is how long Dispatch keeps a key's answer, and IdempotencyLease how long a
// key's first request holds it before a repeat may run it again. Generated from
// IDEMPOTENCY_KEY_TTL_MS and IDEMPOTENCY_LEASE_MS in packages/contracts so the server's windows and
// the clients' account of them cannot drift apart.
const IdempotencyKeyTTL = ${IDEMPOTENCY_KEY_TTL_MS} * time.Millisecond
const IdempotencyLease = ${IDEMPOTENCY_LEASE_MS} * time.Millisecond

// SessionHeartbeat is how often a pane's pi-envoy plugin refreshes its listener registration, and
// SessionLiveWindow how long a session's last registration keeps it live to the listener. Three
// heartbeats plus one whole listener call fit inside the live window, so two refreshes may fail
// and the third still completes before the session stops reading live. Generated from
// SESSION_HEARTBEAT_MS and SESSION_LIVE_WINDOW_MS in packages/contracts, whose doc comments state
// that arrangement, so the pane's refresh and the listener's window cannot drift apart.
const SessionHeartbeat = ${SESSION_HEARTBEAT_MS} * time.Millisecond
const SessionLiveWindow = ${SESSION_LIVE_WINDOW_MS} * time.Millisecond

// SessionClaimStale is how long a session's role claim may go unrefreshed before a reader treats
// the process that wrote it as gone, which allows three missed heartbeats at SessionHeartbeat.
// Generated from SESSION_CLAIM_STALE_MS in packages/contracts so session.ClaimStaleAfter and the
// Legion daemon's copy of it, which times its last re-hold of a notice, are one number.
const SessionClaimStale = ${SESSION_CLAIM_STALE_MS} * time.Millisecond

// ListenerCallTimeout is how long one attempt of an envoy-client call waits for the listener,
// ListenerCallRetryDelay the pause before the next attempt, and ListenerCallAttempts how many
// attempts one call makes. Together they are a whole call's budget, which the heartbeat interval
// is set to leave room for. Generated from LISTENER_CALL_TIMEOUT_MS, LISTENER_CALL_RETRY_DELAY_MS
// and LISTENER_CALL_ATTEMPTS in packages/contracts so the client's cost and every check of that
// budget are computed from one set of numbers.
const ListenerCallTimeout = ${LISTENER_CALL_TIMEOUT_MS} * time.Millisecond
const ListenerCallRetryDelay = ${LISTENER_CALL_RETRY_DELAY_MS} * time.Millisecond
const ListenerCallAttempts = ${LISTENER_CALL_ATTEMPTS}

// DispatchLoginHeader is the request header a page puts on every write it makes, naming the login
// of the person who pressed it, and DispatchLoginChanged the code of the 409 for one whose caller
// is anyone else: nothing ran. Generated from DISPATCH_LOGIN_HEADER and DISPATCH_LOGIN_CHANGED in
// packages/contracts so the server's check and the page's header cannot drift apart.
const DispatchLoginHeader = ${JSON.stringify(DISPATCH_LOGIN_HEADER)}
const DispatchLoginChanged = ${JSON.stringify(DISPATCH_LOGIN_CHANGED)}

// IDAmbiguous is the code of the 409 an id written as a prefix two items or more carry is
// refused with, on a page address, a dispatch:// reference and a dashboard address in text, and
// on a route that takes an id in its path; the refusal's candidates list the items it names.
// Generated from ID_AMBIGUOUS in packages/contracts so the server's refusal and the page that
// lists its candidates cannot drift apart.
const IDAmbiguous = ${JSON.stringify(ID_AMBIGUOUS)}

// DispatchSignInRequired is the code of the 403 for a write by a broker-issued access token that
// names a session and a service but no person: the machine holding it has signed nobody in.
// Generated from DISPATCH_SIGN_IN_REQUIRED in packages/contracts so the server's refusal and
// every client's reading of it cannot drift apart.
const DispatchSignInRequired = ${JSON.stringify(DISPATCH_SIGN_IN_REQUIRED)}

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}

// GithubRepositoryPrefix begins every GitHub subject with its repository's owner and name, each one
// segment: a name may hold a dot, which a NATS subject splits on. Mirrored on the TS side as
// \`githubRepositoryPrefix\`.
func GithubRepositoryPrefix(owner, repo string) string {
	return GithubTopicPrefix + SanitizeSubjectSegment(owner) + "." + SanitizeSubjectSegment(repo)
}

func GithubSubject(owner string, repo string, kind string) string {
	return GithubRepositoryPrefix(owner, repo) + "." + kind
}

func SlackSubject(team string, channel string, kind string) string {
	return "notifications.slack." + team + "." + channel + "." + kind
}

// SanitizeSubjectSegment makes a value one NATS subject segment: a dot, which a subject splits on,
// and whitespace and the wildcards * and >, which a published subject may not hold, each become an
// underscore. A slash is kept. Generated from SUBJECT_SEGMENT_REPLACED in packages/contracts, which
// \`sanitizeSubjectSegment\` reads too, so what the listener publishes and what a consumer expects
// cannot drift apart.
func SanitizeSubjectSegment(value string) string {
	return subjectSegmentSanitizer.Replace(value)
}

var subjectSegmentSanitizer = strings.NewReplacer(${SUBJECT_SEGMENT_REPLACED.flatMap((char) => [JSON.stringify(char), '"_"']).join(", ")})

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + SanitizeSubjectSegment(threadTs) + "." + kind
}

// SubjectMaxBytes is the longest subject the bus publishes on, in bytes: bus's maxSubjectBytes, the
// NATS server's 4 KiB max control line less the rest of the longest protocol line nats.go sends.
// The bus refuses a longer subject before sending it, since the server would close the connection
// over it. Generated from SUBJECT_MAX_BYTES in packages/contracts.
const SubjectMaxBytes = ${SUBJECT_MAX_BYTES}

// GithubPushSubject names a push by its ref name, one segment, sanitized. The ref is the one part of
// a GitHub subject that GitHub bounds only as git does, which is not at all, so a subject past
// SubjectMaxBytes keeps of the segment what fits, cut between runes, followed by ~ and the first 16
// hex digits of the SHA-256 of the whole ref name, and is then at most SubjectMaxBytes: the bus
// publishes it, and two refs that share the kept text still have two subjects. Git refuses ~ in a
// ref, so a cut segment is never another ref's whole one. A consumer that needs the exact ref reads
// the payload's ref. Mirrored on the TS side as \`githubPushSubject\`.
func GithubPushSubject(owner, repo, refType, refName string) string {
	prefix := GithubRepositoryPrefix(owner, repo) + ".push." + refType + "."
	segment := SanitizeSubjectSegment(refName)
	if len(prefix)+len(segment) <= SubjectMaxBytes {
		return prefix + segment
	}
	digest := sha256.Sum256([]byte(refName))
	suffix := "~" + hex.EncodeToString(digest[:8])
	keep := max(SubjectMaxBytes-len(prefix)-len(suffix), 0)
	for keep > 0 && !utf8.RuneStart(segment[keep]) {
		keep--
	}
	return prefix + segment[:keep] + suffix
}

func GithubWorkflowSubject(owner, repo, workflowFilename, action string) string {
	return GithubRepositoryPrefix(owner, repo) + ".workflow." + SanitizeSubjectSegment(workflowFilename) + "." + action
}

// GithubJobSubject names a finished GitHub Actions job on the default branch, of a run with no pull
// request, by its run. Mirrored on the TS side as \`githubJobSubject\`.
func GithubJobSubject(owner, repo, runID, action string) string {
	return GithubRepositoryPrefix(owner, repo) + ".job." + SanitizeSubjectSegment(runID) + "." + action
}

func GithubResourceSubject(owner string, repo string, resourceType string, resourceNumber string) string {
	return GithubRepositoryPrefix(owner, repo) + "." + resourceType + "." + resourceNumber
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
const AgentStreamSubjectPrefix = ${JSON.stringify(AGENT_STREAM_SUBJECT_PREFIX)}

func AgentStreamFramesSubject(sessionID string) string {
	return AgentStreamSubjectPrefix + sessionID + ".frames"
}

func AgentStreamControlSubject(sessionID string) string {
	return AgentStreamSubjectPrefix + sessionID + ".control"
}

func WhatsappSubject(phone, jid, kind string) string {
	return "notifications.whatsapp." + phone + "." + jid + "." + kind
}`;

function title(text: string) {
  if (text === "id") return "ID";
  return (text[0]?.toUpperCase() + text.slice(1)).replace(/Id$/, "ID");
}

function name(key: string) {
  return key.split("_").map(title).join("");
}

function envelopeKind(prop: Prop, req: Set<string>, key: string) {
  if (prop.type === "object") {
    if (!prop.properties) throw new Error(`missing object properties for ${key}`);
    return `*Envelope${name(key)}`;
  }
  if (prop.type === "array") {
    if (prop.items?.type !== "string") {
      throw new Error(`unsupported array items for ${key}`);
    }
    return "[]string";
  }
  const base = map[prop.type];
  if (req.has(key) || prop.type === "string") return base;
  return `*${base}`;
}

function envelopeField(key: string, prop: Prop, req: Set<string>, wide: number, types: number) {
  const n = name(key);
  const t = envelopeKind(prop, req, key);
  const tag = req.has(key) ? key : `${key},omitempty`;
  return `\t${n.padEnd(wide)} ${t.padEnd(types)} \`json:"${tag}"\``;
}

function check(access: string, path: string, prop: Prop, depth = 1) {
  const indent = "\t".repeat(depth);
  if (prop.type === "string") {
    return `${indent}if strings.TrimSpace(${access}) == "" {\n${indent}\treturn fmt.Errorf("${path} is required")\n${indent}}`;
  }
  if (prop.type === "integer") {
    return `${indent}if ${access} == 0 {\n${indent}\treturn fmt.Errorf("${path} must be set")\n${indent}}`;
  }
  throw new Error(`unsupported required envelope type for ${path}`);
}

function enums(key: string, prop: Prop, optional: boolean) {
  if (!prop.enum?.length) return "";
  const n = name(key);
  const indent = optional ? "\t\t" : "\t";
  const list = prop.enum.map((item) => `"${item}"`).join(", ");
  const body = [
    `${indent}switch e.${n} {`,
    `${indent}case ${list}:`,
    `${indent}default:`,
    `${indent}\treturn fmt.Errorf("${key} must be one of: ${prop.enum.join(", ")}")`,
    `${indent}}`,
  ].join("\n");
  if (!optional) return body;
  return `\tif e.${n} != "" {\n${body}\n\t}`;
}

function objectChecks(key: string, prop: Prop) {
  if (prop.type !== "object" || !prop.properties) {
    throw new Error(`missing object properties for ${key}`);
  }
  const access = `e.${name(key)}`;
  const checks = (prop.required ?? []).map((nestedKey) => {
    const nestedProp = prop.properties?.[nestedKey];
    if (!nestedProp) throw new Error(`missing property for required field ${key}.${nestedKey}`);
    return check(`${access}.${name(nestedKey)}`, `${key}.${nestedKey}`, nestedProp, 2);
  });
  if (!checks.length) return "";
  return `\tif ${access} != nil {\n${checks.join("\n")}\n\t}`;
}

function renderEnvelopeObject(key: string, prop: Prop) {
  if (prop.type !== "object" || !prop.properties) {
    throw new Error(`missing object properties for ${key}`);
  }
  const properties = prop.properties;
  const keys = Object.keys(properties);
  const req = new Set(prop.required ?? []);
  const wide = Math.max(...keys.map((nestedKey) => name(nestedKey).length));
  const types = Math.max(
    ...keys.map((nestedKey) => envelopeKind(properties[nestedKey], req, nestedKey).length)
  );
  const body = keys
    .map((nestedKey) => envelopeField(nestedKey, properties[nestedKey], req, wide, types))
    .join("\n");
  return `type Envelope${name(key)} struct {
${body}
}`;
}

function renderEnvelope(schema: Schema) {
  if (schema.type !== "object") throw new Error("envelope schema must be an object");
  const keys = Object.keys(schema.properties);
  const req = new Set(schema.required ?? []);
  const wide = Math.max(...keys.map((key) => name(key).length));
  const types = Math.max(
    ...keys.map((key) => envelopeKind(schema.properties[key], req, key).length)
  );
  const body = keys
    .map((key) => envelopeField(key, schema.properties[key], req, wide, types))
    .join("\n");
  const checks = (schema.required ?? [])
    .map((key) => {
      const prop = schema.properties[key];
      if (!prop) throw new Error(`missing property for required field ${key}`);
      return check(`e.${name(key)}`, key, prop);
    })
    .join("\n");
  const optionalStringChecks = keys
    .flatMap((key) => {
      const prop = schema.properties[key];
      if (req.has(key) || prop.type !== "string" || (prop.minLength ?? 0) < 1) return [];
      const field = `e.${name(key)}`;
      return [
        `\tif ${field} != "" && strings.TrimSpace(${field}) == "" {\n\t\treturn fmt.Errorf("${key} must not be empty")\n\t}`,
      ];
    })
    .join("\n");
  const enumChecks = keys
    .map((key) => enums(key, schema.properties[key], !req.has(key)))
    .filter(Boolean)
    .join("\n");
  const nestedChecks = keys
    .filter((key) => schema.properties[key].type === "object")
    .map((key) => objectChecks(key, schema.properties[key]))
    .filter(Boolean)
    .join("\n");
  const validate = [checks, optionalStringChecks, enumChecks, nestedChecks, "\treturn nil"]
    .filter(Boolean)
    .join("\n");
  const nested = keys
    .filter((key) => schema.properties[key].type === "object")
    .map((key) => renderEnvelopeObject(key, schema.properties[key]))
    .join("\n\n");
  return `type Envelope struct {
${body}
}

${nested}

func (e Envelope) Validate() error {
${validate}
}`;
}

function renderContracts(envelope: Schema) {
  return `package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

${renderEnvelope(envelope)}

${keep}
`;
}

// The slice of JSON Schema that schemas/block-schema.schema.json uses, which the block schema
// renderers below read. `x-ordered` marks an object whose keys carry their declaration order,
// `x-go-type` names the Go type holding such an object, and `x-go-constants` names the Go
// constant of each value of a string enum.
type BlockProp = {
  description?: string;
  type?: "object" | "array" | "string" | "integer" | "boolean";
  $ref?: string;
  const?: string;
  enum?: string[];
  items?: BlockProp;
  anyOf?: BlockProp[];
  properties?: Record<string, BlockProp>;
  required?: string[];
  additionalProperties?: BlockProp;
  "x-ordered"?: boolean;
  "x-go-type"?: string;
  "x-go-constants"?: Record<string, string>;
};

type BlockSchemaDocument = BlockProp & { title: string; $defs: Record<string, BlockProp> };

function blockRefName(ref: string) {
  const referenced = ref.split("/").pop();
  if (!referenced) throw new Error(`unsupported block schema $ref ${ref}`);
  return referenced;
}

function blockDescription(prop: BlockProp, what: string) {
  if (!prop.description) throw new Error(`block schema ${what} has no description`);
  return prop.description;
}

// A description as comment lines of at most 100 columns, each behind prefix.
function blockComment(text: string, prefix: string) {
  const lines: string[] = [];
  let line = "";
  for (const word of text.split(/\s+/)) {
    const candidate = line === "" ? word : `${line} ${word}`;
    if (line !== "" && prefix.length + candidate.length > 100) {
      lines.push(prefix + line);
      line = word;
    } else {
      line = candidate;
    }
  }
  if (line !== "") lines.push(prefix + line);
  return lines;
}

function blockDoc(text: string) {
  return ["/**", ...blockComment(text, " * "), " */"];
}

function blockGoType(prop: BlockProp): string {
  if (prop.$ref) return blockRefName(prop.$ref);
  // A value of several scalar shapes (an attribute's default) is held as it comes off the wire.
  if (prop.anyOf) return "any";
  switch (prop.type) {
    case "string":
      return "string";
    case "integer":
      return "int";
    case "boolean":
      return "bool";
    case "array":
      if (!prop.items) throw new Error("a block schema array has no items");
      return `[]${blockGoType(prop.items)}`;
    case "object": {
      const goType = prop["x-go-type"];
      if (!prop["x-ordered"] || !goType) {
        throw new Error("a block schema object property needs x-ordered and x-go-type");
      }
      return goType;
    }
    default:
      throw new Error(`unsupported block schema type ${prop.type}`);
  }
}

function blockTypeScriptType(prop: BlockProp): string {
  if (prop.$ref) return blockRefName(prop.$ref);
  if (prop.anyOf) return prop.anyOf.map(blockTypeScriptType).join(" | ");
  switch (prop.type) {
    case "string":
      return prop.const === undefined ? "string" : JSON.stringify(prop.const);
    case "integer":
      return "number";
    case "boolean":
      return "boolean";
    case "array":
      if (!prop.items) throw new Error("a block schema array has no items");
      return `readonly ${blockTypeScriptType(prop.items)}[]`;
    case "object": {
      const values = prop.additionalProperties;
      if (!prop["x-ordered"] || !values) {
        throw new Error("a block schema object property needs x-ordered and additionalProperties");
      }
      // A JavaScript object keeps the insertion order of its string keys, so the object the
      // server sends carries the declaration order the Go decoder keeps.
      return `Readonly<Record<string, ${blockTypeScriptType(values)}>>`;
    }
    default:
      throw new Error(`unsupported block schema type ${prop.type}`);
  }
}

function blockFields(typeName: string, prop: BlockProp) {
  if (!prop.properties) throw new Error(`block schema object ${typeName} has no properties`);
  const req = new Set(prop.required ?? []);
  return Object.entries(prop.properties).map(([key, field]) => ({
    key,
    field,
    required: req.has(key),
  }));
}

function renderBlockGoStruct(typeName: string, prop: BlockProp) {
  const fields = blockFields(typeName, prop).map(({ key, field, required }) => {
    const tag = required ? key : `${key},omitempty`;
    // A $ref carries its description on the definition it names, which is rendered there.
    const doc = field.$ref
      ? []
      : blockComment(blockDescription(field, `${typeName}.${key}`), "\t// ");
    return [...doc, `\t${name(key)} ${blockGoType(field)} \`json:"${tag}"\``].join("\n");
  });
  return [
    ...blockComment(blockDescription(prop, typeName), "// "),
    `type ${typeName} struct {`,
    ...fields,
    "}",
  ].join("\n");
}

function renderBlockGoEnum(typeName: string, prop: BlockProp) {
  const constants = prop["x-go-constants"];
  if (!prop.enum?.length || !constants) {
    throw new Error(`block schema enum ${typeName} needs enum values and x-go-constants`);
  }
  const values = prop.enum.map((value) => {
    const constant = constants[value];
    if (!constant) throw new Error(`block schema enum ${typeName} value ${value} has no constant`);
    return `\t${constant} ${typeName} = ${JSON.stringify(value)}`;
  });
  return [
    ...blockComment(blockDescription(prop, typeName), "// "),
    `type ${typeName} string`,
    "",
    "const (",
    ...values,
    ")",
  ].join("\n");
}

function renderBlockInterface(typeName: string, prop: BlockProp) {
  const fields = blockFields(typeName, prop).map(({ key, field, required }) => {
    const doc = field.$ref
      ? []
      : blockDoc(blockDescription(field, `${typeName}.${key}`)).map((line) => `  ${line}`);
    const optional = required ? "" : "?";
    return [...doc, `  readonly ${key}${optional}: ${blockTypeScriptType(field)};`].join("\n");
  });
  return [
    ...blockDoc(blockDescription(prop, typeName)),
    `export interface ${typeName} {`,
    ...fields,
    "}",
  ].join("\n");
}

function renderBlockUnion(typeName: string, prop: BlockProp) {
  if (!prop.enum?.length) throw new Error(`block schema enum ${typeName} has no values`);
  const values = prop.enum.map((value) => JSON.stringify(value));
  const line = `export type ${typeName} = ${values.join(" | ")};`;
  const body =
    line.length <= 100
      ? line
      : `export type ${typeName} =\n${values.map((value) => `  | ${value}`).join("\n")};`;
  return [...blockDoc(blockDescription(prop, typeName)), body].join("\n");
}

// The Go the server reads its own block schema with: the types blocks.json is decoded into, and
// the attribute order every renderer of a typed block follows. The BlockAttributes type that
// `x-go-type` names is hand-written beside the generated file, in pmdoc/block_attributes.go: the
// schema shapes the field, not the ordered decoding behind it.
function renderBlockSchemaGo(document: BlockSchemaDocument) {
  const definitions = Object.entries(document.$defs).map(([typeName, definition]) =>
    definition.enum
      ? renderBlockGoEnum(typeName, definition)
      : renderBlockGoStruct(typeName, definition)
  );
  return `// Code generated by packages/contracts/scripts/gen-go.ts from
// packages/contracts/schemas/block-schema.schema.json. DO NOT EDIT.

package pmdoc

${renderBlockGoStruct(document.title, document)}

${definitions.join("\n\n")}
`;
}

// The same shape for every TypeScript client: the browser editor, the SPA and the tools.
function renderBlockSchemaTypes(document: BlockSchemaDocument) {
  const definitions = Object.entries(document.$defs).map(([typeName, definition]) =>
    definition.enum
      ? renderBlockUnion(typeName, definition)
      : renderBlockInterface(typeName, definition)
  );
  return `// Code generated by scripts/gen-go.ts from schemas/block-schema.schema.json. DO NOT EDIT.

${renderBlockInterface(document.title, document)}

${definitions.join("\n\n")}
`;
}

// The slice of a schema the Go side needs to check a proposed call: what each argument holds,
// whether it is required, and the names an enum allows. gen-go runs every dispatch tool's
// arguments through a SchemaApi that records these calls instead of building zod, so the Go the
// server validates a call with is the TypeScript declaration itself rather than a second copy
// of it. Bounds (min, max) are left out on purpose: the server's own handler enforces them and
// names them in its refusal, and a bound carried here would be a number nothing checks.
type RecordedField = {
  kind: "string" | "number" | "boolean" | "enum" | "array" | "object" | "any";
  optional: boolean;
  nullable: boolean;
  description: string;
  values?: readonly string[];
  item?: RecordedField;
  fields?: [string, RecordedField][];
};

// One recorded node. Every modifier answers a new node, as zod's do, so a shared helper's
// `.optional()` never reaches the node another route built from it.
class RecordedNode implements SchemaNode<RecordedNode> {
  constructor(readonly field: RecordedField) {}

  optional() {
    return new RecordedNode({ ...this.field, optional: true });
  }

  nullable() {
    return new RecordedNode({ ...this.field, nullable: true });
  }

  describe(text: string) {
    return new RecordedNode({ ...this.field, description: text });
  }
}

function recordedNode(field: Omit<RecordedField, "optional" | "nullable" | "description">) {
  return new RecordedNode({ optional: false, nullable: false, description: "", ...field });
}

function recordedFields(shape: Record<string, RecordedNode>): [string, RecordedField][] {
  return Object.entries(shape).map(([name, node]) => [name, node.field]);
}

// The facade the tool specs' arguments are written against, recording what each call declares.
// `refineObject` records the object alone: a cross-field rule is the tool's and the route
// handler's to enforce, and the catalogue's check is of the arguments an action names, not of
// how they agree.
const recordingApi: SchemaApi<RecordedNode> = {
  string: () => recordedNode({ kind: "string" }),
  number: () => recordedNode({ kind: "number" }),
  boolean: () => recordedNode({ kind: "boolean" }),
  enum: (values) => recordedNode({ kind: "enum", values }),
  array: (item) => recordedNode({ kind: "array", item: item.field }),
  unknown: () => recordedNode({ kind: "any" }),
  object: (shape) => recordedNode({ kind: "object", fields: recordedFields(shape) }),
  refineObject: (shape) => recordedNode({ kind: "object", fields: recordedFields(shape) }),
};

const toolFieldKinds = {
  string: "ToolFieldString",
  number: "ToolFieldNumber",
  boolean: "ToolFieldBoolean",
  enum: "ToolFieldEnum",
  array: "ToolFieldArray",
  object: "ToolFieldObject",
  any: "ToolFieldAny",
} satisfies Record<RecordedField["kind"], string>;

// A Go string literal of text: JSON's escapes are Go's for every character JSON escapes.
function goString(text: string) {
  return JSON.stringify(text);
}

// One field as a composite literal with its type elided, the form gofmt leaves alone inside a
// []ToolField. An array's element is rendered with no name, since it is the array's shape
// rather than an argument a call names.
function renderToolField(name: string, field: RecordedField): string {
  const parts = name === "" ? [] : [`Name: ${goString(name)}`];
  parts.push(`Kind: ${toolFieldKinds[field.kind]}`);
  if (!field.optional) parts.push("Required: true");
  if (field.nullable) parts.push("Nullable: true");
  if (field.description !== "") parts.push(`Description: ${goString(field.description)}`);
  if (field.values) parts.push(`Values: []string{${field.values.map(goString).join(", ")}}`);
  if (field.item) parts.push(`Item: &ToolField${renderToolField("", field.item)}`);
  if (field.fields) {
    const nested = field.fields.map(([key, value]) => `${renderToolField(key, value)},`).join("\n");
    parts.push(`Fields: []ToolField{\n${nested}\n}`);
  }
  return `{${parts.join(", ")}}`;
}

// The Go the action catalogue is built from: every dispatch tool with its own description, its
// own arguments and a call those arguments accept. Which of them the catalogue holds is the
// server's to decide from its own route table, so nothing here says which tool writes: this is
// what the TypeScript knows and the Go cannot read.
function renderDispatchTools() {
  const tools = dispatchToolSpecs
    .map((spec) => {
      const fields = recordedFields(spec.arguments(recordingApi) as Record<string, RecordedNode>)
        .map(([name, field]) => `${renderToolField(name, field)},`)
        .join("\n");
      const parts = [
        `Name: ${goString(spec.name)}`,
        `Description: ${goString(spec.description)}`,
        `Arguments: []ToolField{\n${fields}\n}`,
        `Example: json.RawMessage(${goString(JSON.stringify(spec.example))})`,
      ];
      return `{\n${parts.join(",\n")},\n},`;
    })
    .join("\n");

  return `// Code generated by packages/contracts/scripts/gen-go.ts from
// packages/contracts/src/dispatch-tools.ts. DO NOT EDIT.

package contracts

import "encoding/json"

// ToolFieldKind is what one argument of a dispatch tool holds.
type ToolFieldKind string

const (
	ToolFieldString  ToolFieldKind = "string"
	ToolFieldNumber  ToolFieldKind = "number"
	ToolFieldBoolean ToolFieldKind = "boolean"
	ToolFieldEnum    ToolFieldKind = "enum"
	ToolFieldArray   ToolFieldKind = "array"
	ToolFieldObject  ToolFieldKind = "object"
	// ToolFieldAny is an argument whose shape the route reads for itself, such as a typed
	// block's attributes.
	ToolFieldAny ToolFieldKind = "any"
)

// ToolField is one argument a tool takes: an element of an array carries no name, and an
// object's own arguments hang from Fields.
type ToolField struct {
	Name        string        \`json:"name,omitempty"\`
	Kind        ToolFieldKind \`json:"kind"\`
	Required    bool          \`json:"required,omitempty"\`
	Nullable    bool          \`json:"nullable,omitempty"\`
	Description string        \`json:"description,omitempty"\`
	Values      []string      \`json:"values,omitempty"\`
	Item        *ToolField    \`json:"item,omitempty"\`
	Fields      []ToolField   \`json:"fields,omitempty"\`
}

// DispatchTool is one tool of the \`dispatch\` CLI as its spec states it: the name it is called
// by, the whole description a model chooses it on, the arguments it takes and a call those
// arguments accept.
type DispatchTool struct {
	Name        string          \`json:"name"\`
	Description string          \`json:"description"\`
	Arguments   []ToolField     \`json:"arguments"\`
	Example     json.RawMessage \`json:"example"\`
}

// DispatchTools is every dispatch tool, in the order dispatch-tools.ts states them.
var DispatchTools = []DispatchTool{
${tools}
}
`;
}

const envelope = (await Bun.file(envelopeFile).json()) as Schema;
const blockSchema = (await Bun.file(blockSchemaFile).json()) as BlockSchemaDocument;

// Every generated file this script owns, each marked with the language it is written in: a Go
// file is gofmt-formatted before it is written and before it is compared.
const outputs = [
  {
    path: resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go"),
    rendered: renderContracts(envelope),
    language: "go",
  },
  {
    path: resolve(import.meta.dir, "../../envoy/internal/dispatch/pmdoc/schema_generated.go"),
    rendered: renderBlockSchemaGo(blockSchema),
    language: "go",
  },
  {
    path: resolve(import.meta.dir, "../../envoy/internal/contracts/dispatch_tools_generated.go"),
    rendered: renderDispatchTools(),
    language: "go",
  },
  {
    path: resolve(import.meta.dir, "../src/block-schema.generated.ts"),
    rendered: renderBlockSchemaTypes(blockSchema),
    language: "ts",
  },
] satisfies { path: string; rendered: string; language: "go" | "ts" }[];
const fmt = Bun.which("gofmt");

// `--check` pipes each Go render through gofmt and compares every render with the committed
// file, writing nothing, so CI fails on a generated file that no longer matches the TypeScript or
// the schema it comes from.
if (process.argv.includes("--check")) {
  if (fmt === null) {
    throw new Error("gen-go --check needs gofmt: the committed Go files are gofmt-formatted");
  }
  // GEN_GO_CHECK_ROOT points the comparison at copies of the outputs under another root, laid out
  // by their repository-relative paths, so a test can check a copy instead of a tracked file.
  // Unset, `--check` compares the committed files.
  const checkRoot = process.env.GEN_GO_CHECK_ROOT;
  const repoRoot = resolve(import.meta.dir, "../../..");
  for (const { path: output, rendered, language } of outputs) {
    const path = checkRoot ? resolve(checkRoot, relative(repoRoot, output)) : output;
    let expected = rendered;
    if (language === "go") {
      const gofmt = Bun.spawnSync({
        cmd: [fmt],
        stdin: new TextEncoder().encode(rendered),
        stderr: "inherit",
      });
      if (gofmt.exitCode !== 0) {
        throw new Error("gofmt failed");
      }
      expected = gofmt.stdout.toString();
    }
    if (expected !== (await Bun.file(path).text())) {
      Bun.spawnSync({
        cmd: ["diff", "-u", path, "-"],
        stdin: new TextEncoder().encode(expected),
        stderr: "inherit",
        stdout: "inherit",
      });
      console.error(
        `${path} is stale: run \`bun run gen:go\` in packages/contracts and commit the result.`
      );
      process.exit(1);
    }
  }
  process.exit(0);
}

for (const { path, rendered } of outputs) {
  mkdirSync(dirname(path), { recursive: true });
  await Bun.write(path, rendered);
}

// One gofmt for every Go file written, rather than one spawn per output.
if (fmt) {
  const gofmt = Bun.spawnSync({
    cmd: [
      fmt,
      "-w",
      ...outputs.filter(({ language }) => language === "go").map(({ path }) => path),
    ],
    stderr: "inherit",
    stdout: "inherit",
  });

  if (gofmt.exitCode !== 0) {
    throw new Error("gofmt failed");
  }
}
