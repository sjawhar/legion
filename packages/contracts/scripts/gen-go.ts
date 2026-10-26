import { mkdirSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";

import { AGENT_STREAM_SUBJECT_PREFIX } from "../src/agent-stream";
import {
  DEFAULT_ISSUE_PAGE_LIMIT,
  DELIVERY_DUPLICATE_WINDOW_MS,
  DISPATCH_LOGIN_CHANGED,
  DISPATCH_LOGIN_HEADER,
  DOCUMENT_SCHEMA_CLOSE_CODE,
  DOCUMENT_SCHEMA_CLOSE_REASON,
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
} from "../src/dispatch-api";
import {
  DISPATCH_BODY_MAX,
  PROJECT_KEY_PATTERN,
  PROJECT_KEY_RULE,
  SEARCH_KIND_DEPTH,
  SEARCH_QUERY_HINT,
  SEARCH_QUERY_MAX,
} from "../src/dispatch-tools";
import { MINTED_DEDUPE_KEY_PATTERN } from "../src/envelope";
import { SUBJECT_SEGMENT_REPLACED } from "../src/subject";

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

// DeliveryDuplicateWindow is how long a repeated delivery is recognised as a duplicate, by the
// notification stream and by every host that subscribes over core NATS. Generated from
// DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts, whose doc comment states the retry promise
// this window bounds, so the stream, the hosts and the dashboard read one number.
const DeliveryDuplicateWindow = ${DELIVERY_DUPLICATE_WINDOW_MS} * time.Millisecond

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

// DispatchBodyMax is the longest comment, comment-edit, message and broadcast body the server
// accepts, in UTF-16 units. Generated from DISPATCH_BODY_MAX in packages/contracts so the server's
// refusal, the agent tools' pre-send refusal and the dashboard composer cannot drift apart.
const DispatchBodyMax = ${DISPATCH_BODY_MAX}

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

// DispatchLoginHeader is the request header a page puts on every write it makes, naming the login
// of the person who pressed it, and DispatchLoginChanged the code of the 409 for one whose caller
// is anyone else: nothing ran. Generated from DISPATCH_LOGIN_HEADER and DISPATCH_LOGIN_CHANGED in
// packages/contracts so the server's check and the page's header cannot drift apart.
const DispatchLoginHeader = ${JSON.stringify(DISPATCH_LOGIN_HEADER)}
const DispatchLoginChanged = ${JSON.stringify(DISPATCH_LOGIN_CHANGED)}

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}

// githubRepositoryPrefix begins every GitHub subject with its repository's owner and name, each one
// segment: a name may hold a dot, which a NATS subject splits on. Mirrored on the TS side as
// \`githubRepositoryPrefix\`.
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
// \`sanitizeSubjectSegment\` reads too, so what the listener publishes and what a consumer expects
// cannot drift apart.
func SanitizeSubjectSegment(value string) string {
	return subjectSegmentSanitizer.Replace(value)
}

var subjectSegmentSanitizer = strings.NewReplacer(${SUBJECT_SEGMENT_REPLACED.flatMap((char) => [JSON.stringify(char), '"_"']).join(", ")})

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + SanitizeSubjectSegment(threadTs) + "." + kind
}

func GithubPushSubject(owner, repo, refType, refName string) string {
	return githubRepositoryPrefix(owner, repo) + ".push." + refType + "." + SanitizeSubjectSegment(refName)
}

func GithubWorkflowSubject(owner, repo, workflowFilename, action string) string {
	return githubRepositoryPrefix(owner, repo) + ".workflow." + SanitizeSubjectSegment(workflowFilename) + "." + action
}

// GithubJobSubject names a finished GitHub Actions job on the default branch, of a run with no pull
// request, by its run. Mirrored on the TS side as \`githubJobSubject\`.
func GithubJobSubject(owner, repo, runID, action string) string {
	return githubRepositoryPrefix(owner, repo) + ".job." + SanitizeSubjectSegment(runID) + "." + action
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
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

${renderEnvelope(envelope)}

${keep}
`;
}

function renderDaemonDispatch() {
  return `package dispatch

// MessageBodyLimit is the longest body Dispatch accepts on a message, in UTF-16 code units as
// Dispatch counts them. Generated from DISPATCH_BODY_MAX in packages/contracts;
// record.MessagePostLimit subtracts the outbox marker the runner appends from it.
const MessageBodyLimit = ${DISPATCH_BODY_MAX}
`;
}

const envelope = (await Bun.file(envelopeFile).json()) as Schema;

// Every generated Go file this script owns. The daemon carries its own copy of the body cap
// rather than importing the envoy package: its production build imports nothing from
// github.com/sjawhar/envoy.
const outputs = [
  {
    path: resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go"),
    rendered: renderContracts(envelope),
  },
  {
    path: resolve(import.meta.dir, "../../daemon/internal/dispatch/generated.go"),
    rendered: renderDaemonDispatch(),
  },
];
const fmt = Bun.which("gofmt");

// `--check` pipes each render through gofmt and compares it with the committed file, writing
// nothing, so CI fails on a generated.go that no longer matches the TypeScript it comes from.
if (process.argv.includes("--check")) {
  if (fmt === null) {
    throw new Error("gen-go --check needs gofmt: the committed files are gofmt-formatted");
  }
  // GEN_GO_CHECK_ROOT points the comparison at copies of the outputs under another root, laid out
  // by their repository-relative paths, so a test can check a copy instead of a tracked file.
  // Unset, `--check` compares the committed files.
  const checkRoot = process.env.GEN_GO_CHECK_ROOT;
  const repoRoot = resolve(import.meta.dir, "../../..");
  for (const { path: output, rendered } of outputs) {
    const path = checkRoot ? resolve(checkRoot, relative(repoRoot, output)) : output;
    const gofmt = Bun.spawnSync({
      cmd: [fmt],
      stdin: new TextEncoder().encode(rendered),
      stderr: "inherit",
    });
    if (gofmt.exitCode !== 0) {
      throw new Error("gofmt failed");
    }
    if (gofmt.stdout.toString() !== (await Bun.file(path).text())) {
      Bun.spawnSync({
        cmd: ["diff", "-u", path, "-"],
        stdin: gofmt.stdout,
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

// One gofmt for every file written, rather than one spawn per output.
if (fmt) {
  const gofmt = Bun.spawnSync({
    cmd: [fmt, "-w", ...outputs.map(({ path }) => path)],
    stderr: "inherit",
    stdout: "inherit",
  });

  if (gofmt.exitCode !== 0) {
    throw new Error("gofmt failed");
  }
}
