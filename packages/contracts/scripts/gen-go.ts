import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

import { AGENT_STREAM_SUBJECT_PREFIX } from "../src/agent-stream";
import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  MAX_BROADCAST_RECIPIENTS,
  RECEIPT_TIMEOUT_CAUSE,
} from "../src/dispatch-api";
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

const out = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");
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

// DeliveryDuplicateWindow is how long the notification stream recognises a repeated delivery as
// a duplicate. Generated from DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts so the stream's
// configuration and the dashboard's "retrying is safe" promise cannot drift apart.
const DeliveryDuplicateWindow = ${DELIVERY_DUPLICATE_WINDOW_MS} * time.Millisecond

// ReceiptTimeoutCause is what a delivery attempt records when the listener never answered its
// send. Generated from RECEIPT_TIMEOUT_CAUSE in packages/contracts so the string Dispatch writes
// and the string the dashboard keys its retry wording on cannot drift apart.
const ReceiptTimeoutCause = ${JSON.stringify(RECEIPT_TIMEOUT_CAUSE)}

// MaxBroadcastRecipients is the most sessions one broadcast sends to. Generated from
// MAX_BROADCAST_RECIPIENTS in packages/contracts so the server's limit and the dashboard's
// cannot drift apart.
const MaxBroadcastRecipients = ${MAX_BROADCAST_RECIPIENTS}

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

// `--check` renders to a scratch file and compares it with the committed one instead of writing
// it, so CI fails on a generated.go that no longer matches the TypeScript it comes from.
const checkOnly = process.argv.includes("--check");
const scratch = checkOnly ? mkdtempSync(join(tmpdir(), "gen-go-")) : undefined;
const target = scratch === undefined ? out : join(scratch, "generated.go");

const envelope = (await Bun.file(envelopeFile).json()) as Schema;
mkdirSync(dirname(target), { recursive: true });
await Bun.write(target, renderContracts(envelope));

const fmt = Bun.which("gofmt");
if (fmt === null && checkOnly) {
  throw new Error("gen-go --check needs gofmt: the committed file is gofmt-formatted");
}

if (fmt) {
  const gofmt = Bun.spawnSync({
    cmd: [fmt, "-w", target],
    stderr: "inherit",
    stdout: "inherit",
  });

  if (gofmt.exitCode !== 0) {
    throw new Error("gofmt failed");
  }
}

if (scratch !== undefined) {
  const fresh = await Bun.file(target).text();
  const committed = await Bun.file(out).text();
  if (fresh !== committed) {
    Bun.spawnSync({ cmd: ["diff", "-u", out, target], stderr: "inherit", stdout: "inherit" });
    rmSync(scratch, { force: true, recursive: true });
    console.error(
      `${out} is stale: run \`bun run gen:go\` in packages/contracts and commit the result.`
    );
    process.exit(1);
  }
  rmSync(scratch, { force: true, recursive: true });
}
