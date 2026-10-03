#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/tools.md from dispatchToolSpecs
// (packages/contracts/src/dispatch-tools.ts): each tool's description, its arguments read from the
// JSON Schema an agent host is handed (lib/tool-schema.ts), its cross-field rule, and its example
// call. Contract: scripts/generate.ts.
import { resolve } from "node:path";
import type { z as Zod } from "zod";
import {
  dispatchToolSchema,
  dispatchToolSpecs,
} from "../../../packages/contracts/src/dispatch-tools.ts";
import { zodSchemaApi } from "../../../packages/contracts/src/tool-schema.ts";
import { inline, writePage } from "./lib/markdown.ts";
import { argumentLines, type JsonSchema } from "./lib/tool-schema.ts";

const CONTRACTS_DIR = resolve(import.meta.dir, "../../../packages/contracts");

// The zod @legion/contracts declares, resolved from that package at run time rather than imported
// statically from this file: a static import resolves nearest this file, where the site's own
// dependencies (Astro) can put an older zod, and the schema must be the one agent hosts build.
const { z } = (await import(Bun.resolveSync("zod", CONTRACTS_DIR))) as { z: typeof Zod };

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
    lines.push(`## ${spec.name}`, "", inline(spec.description), "");
    lines.push(...argumentLines(spec.name, schema));
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
