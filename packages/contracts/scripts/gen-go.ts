import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";

type Kind = "string" | "integer";

type Prop = {
  type: Kind;
  enum?: string[];
};

type Schema = {
  type: "object";
  required?: string[];
  properties: Record<string, Prop>;
  additionalProperties?: boolean;
};

const out = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");

const file = resolve(import.meta.dir, "../schemas/envelope.schema.json");

const map = {
  string: "string",
  integer: "int64",
} satisfies Record<Kind, string>;

const keep = `const AgentTopicPrefix = "notifications.agent."

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

func SlackThreadSubject(team, channel, threadTs, kind string) string {
	return "notifications.slack." + team + "." + channel + ".thread." + strings.ReplaceAll(threadTs, ".", "_") + "." + kind
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
  return text[0]?.toUpperCase() + text.slice(1);
}

function name(key: string) {
  return key.split("_").map(title).join("");
}

function kind(prop: Prop, req: Set<string>, key: string) {
  const base = map[prop.type];
  if (!base) throw new Error(`unsupported schema type for ${key}`);
  if (req.has(key) || prop.type === "string") return base;
  return `*${base}`;
}

function field(key: string, prop: Prop, req: Set<string>, wide: number, types: number) {
  const n = name(key);
  const t = kind(prop, req, key);
  const tag = req.has(key) ? key : `${key},omitempty`;
  return `\t${n.padEnd(wide)} ${t.padEnd(types)} \`json:"${tag}"\``;
}

function check(key: string, prop: Prop) {
  const n = name(key);
  if (prop.type === "string") {
    return `\tif strings.TrimSpace(e.${n}) == "" {\n\t\treturn fmt.Errorf("${key} is required")\n\t}`;
  }
  if (prop.type === "integer") {
    return `\tif e.${n} == 0 {\n\t\treturn fmt.Errorf("${key} must be set")\n\t}`;
  }
  throw new Error(`unsupported required type for ${key}`);
}

function enums(key: string, prop: Prop) {
  if (!prop.enum?.length) return "";
  const n = name(key);
  const list = prop.enum.map((item) => `"${item}"`).join(", ");
  return [
    `\tswitch e.${n} {`,
    `\tcase ${list}:`,
    `\tdefault:`,
    `\t\treturn fmt.Errorf("${key} must be one of: ${prop.enum.join(", ")}")`,
    `\t}`,
  ].join("\n");
}

function render(schema: Schema) {
  if (schema.type !== "object") throw new Error("envelope schema must be an object");
  const keys = Object.keys(schema.properties);
  const req = new Set(schema.required ?? []);
  const wide = Math.max(...keys.map((key) => name(key).length));
  const types = Math.max(...keys.map((key) => kind(schema.properties[key], req, key).length));
  const body = keys.map((key) => field(key, schema.properties[key], req, wide, types)).join("\n");
  const checks = (schema.required ?? [])
    .map((key) => {
      const prop = schema.properties[key];
      if (!prop) throw new Error(`missing property for required field ${key}`);
      return check(key, prop);
    })
    .join("\n");
  const source = schema.properties.source;
  const extra = source ? enums("source", source) : "";
  const validate = [checks, extra, "\treturn nil"].filter(Boolean).join("\n");
  return `package contracts

import (
\t"fmt"
\t"strings"
\t"time"
)

type Envelope struct {
${body}
}

func (e Envelope) Validate() error {
${validate}
}

${keep}
`;
}

const schema = (await Bun.file(file).json()) as Schema;

mkdirSync(dirname(out), { recursive: true });
await Bun.write(out, render(schema));

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
