// Shared by the generators that document agent tools from the JSON Schema an agent host is handed
// (dispatch-tools.ts, envoy-tools.ts). A schema keyword or shape the page does not render fails the
// run, so no part of a tool's contract is dropped from a page without notice.
import { inline } from "./markdown.ts";

export interface JsonSchema {
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

/** Every JSON Schema keyword a page renders. `additionalProperties` only as `false`, which is
 *  what a strict tool's own "Unknown arguments are refused" line says. */
const RENDERED_KEYWORDS: Record<string, true> = {
  type: true,
  description: true,
  enum: true,
  anyOf: true,
  items: true,
  properties: true,
  required: true,
  additionalProperties: true,
  minLength: true,
  maxLength: true,
  minimum: true,
  maximum: true,
  minItems: true,
  maxItems: true,
};

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
    if (!Object.hasOwn(RENDERED_KEYWORDS, keyword)) {
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

/** The lines documenting `tool`'s arguments from its input `schema`: a table with one row per
 *  argument, or "Takes no arguments.". Throws on a keyword or shape the page would drop. */
export function argumentLines(tool: string, schema: JsonSchema): string[] {
  typeOf(schema, tool);
  const rows = argumentRows(tool, schema, "");
  if (rows.length === 0) return ["Takes no arguments.", ""];
  return ["| Argument | Type | Required | Description |", "| --- | --- | --- | --- |", ...rows, ""];
}
