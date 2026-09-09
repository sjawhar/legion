import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";

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

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}

func GithubSubject(owner string, repo string, kind string) string {
	return "notifications.github." + owner + "." + repo + "." + kind
}

func SlackSubject(team string, channel string, kind string) string {
	return "notifications.slack." + team + "." + channel + "." + kind
}

// SanitizeSubjectSegment replaces dots in a NATS subject segment with underscores so the segment
// stays a single token. Mirrored on the TS side as \`sanitizeSubjectSegment\`.
func SanitizeSubjectSegment(value string) string {
	return strings.ReplaceAll(value, ".", "_")
}

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + SanitizeSubjectSegment(threadTs) + "." + kind
}

func GithubPushSubject(owner, repo, refType, refName string) string {
	return "notifications.github." + owner + "." + repo + ".push." + refType + "." + SanitizeSubjectSegment(refName)
}

func GithubWorkflowSubject(owner, repo, workflowFilename, action string) string {
	return "notifications.github." + owner + "." + repo + ".workflow." + SanitizeSubjectSegment(workflowFilename) + "." + action
}

func GithubResourceSubject(owner string, repo string, resourceType string, resourceNumber string) string {
	return "notifications.github." + owner + "." + repo + "." + resourceType + "." + resourceNumber
}

const GhostWisprTopicPrefix = "notifications.ghostwispr."

func GhostWisprSubject(sessionId string, kind string) string {
	return GhostWisprTopicPrefix + sessionId + "." + kind
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

const envelope = (await Bun.file(envelopeFile).json()) as Schema;
mkdirSync(dirname(out), { recursive: true });
await Bun.write(out, renderContracts(envelope));

const fmt = Bun.which("gofmt");

if (fmt) {
  const gofmt = Bun.spawnSync({
    cmd: [fmt, "-w", out],
    stderr: "inherit",
    stdout: "inherit",
  });

  if (gofmt.exitCode !== 0) {
    throw new Error("gofmt failed");
  }
}
