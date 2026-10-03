#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/tools.md from dispatchToolSpecs
// (packages/contracts/src/dispatch-tools.ts): each tool's description, its arguments read from the
// JSON Schema an agent host is handed, its cross-field rule, and its example call. A schema
// keyword or shape the page does not render fails the run, so no part of a tool's contract is
// dropped from the page without notice. Contract: scripts/generate.ts.
import { resolve } from "node:path";
import type { z as Zod } from "zod";
import {
  dispatchToolSchema,
  dispatchToolSpecs,
} from "../../../packages/contracts/src/dispatch-tools.ts";
import { zodSchemaApi } from "../../../packages/contracts/src/tool-schema.ts";
import { inline, writePage } from "./lib/markdown.ts";

const CONTRACTS_DIR = resolve(import.meta.dir, "../../../packages/contracts");

// The zod @legion/contracts declares, resolved from that package at run time rather than imported
// statically from this file: a static import resolves nearest this file, where the site's own
// dependencies (Astro) can put an older zod, and the schema must be the one agent hosts build.
const { z } = (await import(Bun.resolveSync("zod", CONTRACTS_DIR))) as { z: typeof Zod };

interface JsonSchema {
  type?: string;
  description?: string;
  enum?: string[];
  anyOf?: JsonSchema[];
  items?: JsonSchema;
  properties?: Record<string, JsonSchema>;
  required?: string[];
  additionalProperties?: unknown;
  minLength?: number;
  maxLength?: number;
  minimum?: number;
  maximum?: number;
  minItems?: number;
  maxItems?: number;
}

/** Every JSON Schema keyword the page renders. `additionalProperties` only as `false`, which is
 *  what a strict spec's own "Unknown arguments are refused" line says. */
const RENDERED_KEYWORDS = new Set<string>([
  "type",
  "description",
  "enum",
  "anyOf",
  "items",
  "properties",
  "required",
  "additionalProperties",
  "minLength",
  "maxLength",
  "minimum",
  "maximum",
  "minItems",
  "maxItems",
]);

/** " (1–40 characters)", " (at most 20 items)", " (0–3)", or "" when the schema sets no bound;
 *  `unit` is plural and loses its "s" for a bound of exactly 1. */
function bounds(low: number | undefined, high: number | undefined, unit: string): string {
  const count = (value: number) =>
    unit === "" ? `${value}` : `${value} ${value === 1 ? unit.slice(0, -1) : unit}`;
  if (low !== undefined && high !== undefined) return ` (${low}–${count(high)})`;
  if (high !== undefined) return ` (at most ${count(high)})`;
  if (low !== undefined) return ` (at least ${count(low)})`;
  return "";
}

/** The Type column for `node`, which sits at `where`. Throws on a keyword the page would drop, and
 *  on an object with fields anywhere under an `anyOf` (`inAnyOf`), whose fields would get no rows. */
function typeOf(node: JsonSchema, where: string, inAnyOf = false): string {
  for (const keyword of Object.keys(node)) {
    if (!RENDERED_KEYWORDS.has(keyword)) {
      throw new Error(`${where}: JSON Schema keyword "${keyword}" is not rendered`);
    }
  }
  if (node.additionalProperties !== undefined && node.additionalProperties !== false) {
    throw new Error(`${where}: additionalProperties other than false is not rendered`);
  }
  if (inAnyOf && node.properties) {
    throw new Error(`${where}: an object's fields under anyOf are not rendered`);
  }
  if (node.enum) return `one of ${node.enum.map((value) => `\`${value}\``).join(", ")}`;
  if (node.anyOf) {
    return node.anyOf
      .map((member, index) => typeOf(member, `${where} (anyOf ${index})`, true))
      .join(" or ");
  }
  switch (node.type) {
    case undefined:
      return "any";
    case "string":
      return `string${bounds(node.minLength, node.maxLength, "characters")}`;
    case "integer":
    case "number": {
      // zod gives every int() the safe-integer range; only a bound the spec sets is worth printing.
      const low = node.minimum === Number.MIN_SAFE_INTEGER ? undefined : node.minimum;
      const high = node.maximum === Number.MAX_SAFE_INTEGER ? undefined : node.maximum;
      return `${node.type}${bounds(low, high, "")}`;
    }
    case "array":
      return `array${bounds(node.minItems, node.maxItems, "items")} of ${node.items ? typeOf(node.items, `${where}[]`, inAnyOf) : "any"}`;
    case "boolean":
    case "null":
    case "object":
      return node.type;
    default:
      throw new Error(`${where}: JSON Schema type "${node.type}" is not rendered`);
  }
}

/** One table row per argument of `tool`. An object's fields follow it as `parent.field`, and the
 *  fields of an array's objects as `parent[].field`, at any depth. */
function argumentRows(tool: string, object: JsonSchema, prefix: string): string[] {
  const rows: string[] = [];
  for (const [name, node] of Object.entries(object.properties ?? {})) {
    const path = `${prefix}${name}`;
    const required = object.required?.includes(name) ? "yes" : "no";
    const description = inline(node.description ?? "", true).replace(/\s*\n\s*/g, " ");
    const type = inline(typeOf(node, `${tool} ${path}`), true);
    rows.push(`| \`${path}\` | ${type} | ${required} | ${description} |`);
    let nested: JsonSchema | undefined = node;
    let nestedPath = path;
    while (nested?.items) {
      nested = nested.items;
      nestedPath += "[]";
    }
    if (nested?.properties) rows.push(...argumentRows(tool, nested, `${nestedPath}.`));
  }
  return rows;
}

function render(): string {
  const lines = [
    `Agents work in Dispatch through ${dispatchToolSpecs.length} tools. Every agent host registers them from the same specs, so the arguments below are the schema the agent is handed. A required argument of an object argument is required when that object is given.`,
    "",
  ];
  for (const spec of dispatchToolSpecs) {
    const { $schema: _dialect, ...schema } = z.toJSONSchema(
      dispatchToolSchema(spec, zodSchemaApi(z)),
      { io: "input" }
    ) as JsonSchema & { $schema?: string };
    typeOf(schema, spec.name);
    lines.push(`## ${spec.name}`, "", inline(spec.description), "");
    const rows = argumentRows(spec.name, schema, "");
    if (rows.length === 0) {
      lines.push("Takes no arguments.", "");
    } else {
      lines.push("| Argument | Type | Required | Description |", "| --- | --- | --- | --- |");
      lines.push(...rows, "");
    }
    if ("validation" in spec && spec.validation) {
      lines.push(`**Rule:** ${inline(spec.validation.message)}`, "");
    }
    if ("strict" in spec && spec.strict) {
      lines.push("Unknown arguments are refused.", "");
    }
    lines.push("Example call:", "", "```json", JSON.stringify(spec.example, null, 2), "```", "");
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/tools.md",
  title: "Agent tools",
  description:
    "Every Dispatch tool an agent can call: what it does, its arguments, and an example call.",
  source: "packages/contracts/src/dispatch-tools.ts",
  body: render,
});
