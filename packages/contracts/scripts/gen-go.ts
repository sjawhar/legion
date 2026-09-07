import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { renderDispatchQuestionSchema } from "./dispatch-question-schema";

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
const questionOut = resolve(import.meta.dir, "../../envoy/internal/dispatch/core/generated.go");
const envelopeFile = resolve(import.meta.dir, "../schemas/envelope.schema.json");
const questionFile = resolve(import.meta.dir, "../schemas/dispatch-question.schema.json");

const map = {
  string: "string",
  integer: "int64",
  boolean: "bool",
} satisfies Record<ScalarKind, string>;

const nestedNames: Record<string, Record<string, string>> = {
  DispatchQuestion: {
    options: "DispatchQuestionOption",
  },
};

const keep = `const AgentTopicPrefix = "notifications.agent."
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

function optionalStringProperties(schema: Schema, req: Set<string>) {
  return Object.entries(schema.properties)
    .filter(([key, prop]) => !req.has(key) && prop.type === "string" && (prop.minLength ?? 0) >= 1)
    .map(([key]) => key);
}

function emptyFlag(key: string) {
  const field = name(key);
  return `${field[0].toLowerCase()}${field.slice(1)}WasEmpty`;
}

function optionalStringChecks(keys: string[]) {
  return keys
    .map((key) => {
      const field = name(key);
      return `\tif e.${emptyFlag(key)} && e.${field} == "" {\n\t\treturn fmt.Errorf("${key} must not be empty")\n\t}`;
    })
    .join("\n");
}

function renderOptionalStringUnmarshal(keys: string[]) {
  if (!keys.length) return "";
  const wireFields = keys.map((key) => `\t\t${name(key)} *string \`json:"${key}"\``).join("\n");
  const assignments = keys
    .map(
      (key) =>
        `\tif wire.${name(key)} != nil {\n\t\te.${name(key)} = *wire.${name(key)}\n\t\te.${emptyFlag(key)} = *wire.${name(key)} == ""\n\t}`
    )
    .join("\n");
  return `// UnmarshalJSON decodes an Envelope.
// Do not embed Envelope in a struct that is itself json-unmarshalled: the promoted UnmarshalJSON skips the outer fields. Compose it as a named field instead.
func (e *Envelope) UnmarshalJSON(data []byte) error {
\ttype envelopeAlias Envelope
\tvar decoded envelopeAlias
\tvar wire struct {
\t\t*envelopeAlias
${wireFields}
\t}
\twire.envelopeAlias = &decoded
\tif err := json.Unmarshal(data, &wire); err != nil {
\t\treturn err
\t}
\t*e = Envelope(decoded)
${assignments}
\treturn nil
}`;
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
  const optionalStrings = optionalStringProperties(schema, req);
  const wide = Math.max(...keys.map((key) => name(key).length));
  const types = Math.max(
    ...keys.map((key) => envelopeKind(schema.properties[key], req, key).length)
  );
  const body = [
    ...keys.map((key) => envelopeField(key, schema.properties[key], req, wide, types)),
    ...optionalStrings.map((key) => `\t${emptyFlag(key)} bool`),
  ].join("\n");
  const checks = (schema.required ?? [])
    .map((key) => {
      const prop = schema.properties[key];
      if (!prop) throw new Error(`missing property for required field ${key}`);
      return check(`e.${name(key)}`, key, prop);
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
  const optionalChecks = optionalStringChecks(optionalStrings);
  const validate = [checks, enumChecks, optionalChecks, nestedChecks, "\treturn nil"]
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

function nestedName(parent: string, key: string) {
  const nested = nestedNames[parent]?.[key];
  if (!nested) throw new Error(`unsupported nested question schema key ${parent}.${key}`);
  return nested;
}

function questionType(prop: Prop, required: boolean, parent: string, key: string) {
  if (prop.type === "array") {
    if (prop.items?.type !== "object") {
      throw new Error(`unsupported array items for ${key}`);
    }
    return `[]${nestedName(parent, key)}`;
  }
  const base = map[prop.type as ScalarKind];
  if (!base) throw new Error(`unsupported question schema type for ${key}`);
  if (prop.type === "boolean" && !required) return `*${base}`;
  return base;
}

function renderQuestionStruct(schema: Schema, typeName: string): string[] {
  if (schema.type !== "object") throw new Error(`${typeName} schema must be an object`);
  const keys = Object.keys(schema.properties);
  const required = new Set(schema.required ?? []);
  const fields = keys.map((key) => {
    const prop = schema.properties[key];
    const n = name(key);
    const t = questionType(prop, required.has(key), typeName, key);
    const tag = required.has(key) ? key : `${key},omitempty`;
    return { n, t, tag };
  });
  const wide = Math.max(...fields.map(({ n }) => n.length));
  const types = Math.max(...fields.map(({ t }) => t.length));
  const body = fields
    .map(
      ({ n, t, tag }) => `\t${n.padEnd(wide)} ${t.padEnd(types)} \`json:"${tag}" yaml:"${tag}"\``
    )
    .join("\n");
  const nested = keys.flatMap((key) => {
    const prop = schema.properties[key];
    if (prop.type === "array" && prop.items?.type === "object") {
      return renderQuestionStruct(prop.items as Schema, nestedName(typeName, key));
    }
    return [];
  });
  return [`type ${typeName} struct {\n${body}\n}`, ...nested];
}

function renderContracts(envelope: Schema) {
  const required = new Set(envelope.required ?? []);
  const optionalStrings = optionalStringProperties(envelope, required);
  const jsonImport = optionalStrings.length ? '\t"encoding/json"\n' : "";
  return `package contracts

import (
${jsonImport}\t"fmt"
\t"strings"
\t"time"
)

${renderEnvelope(envelope)}

${renderOptionalStringUnmarshal(optionalStrings)}

${keep}
`;
}

// The question types are generated into the dispatch core package, which owns
// them, rather than into contracts: contracts imports dispatch/core to read the
// origin session out of dispatch markers, so core must not import contracts.
function renderQuestion(question: Schema) {
  return `// Code generated by packages/contracts/scripts/gen-go.ts from
// packages/contracts/schemas/dispatch-question.schema.json. DO NOT EDIT.

package core

${renderQuestionStruct(question, "DispatchQuestion").join("\n\n")}
`;
}

// The question schema is emitted from the zod source first, so one run of this
// script refreshes the checked-in JSON Schema and both generated Go files.
const questionJson = renderDispatchQuestionSchema();
await Bun.write(questionFile, questionJson);
const envelope = (await Bun.file(envelopeFile).json()) as Schema;
const question = JSON.parse(questionJson) as Schema;

const outputs: Array<[string, string]> = [
  [out, renderContracts(envelope)],
  [questionOut, renderQuestion(question)],
];
for (const [path, content] of outputs) {
  mkdirSync(dirname(path), { recursive: true });
  await Bun.write(path, content);
}

const fmt = Bun.which("gofmt");

if (fmt) {
  const gofmt = Bun.spawnSync({
    cmd: [fmt, "-w", ...outputs.map(([path]) => path)],
    stderr: "inherit",
    stdout: "inherit",
  });

  if (gofmt.exitCode !== 0) {
    throw new Error("gofmt failed");
  }
}
