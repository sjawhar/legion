#!/usr/bin/env bun
// Writes <content dir>/envoy/reference/tools.md from envoyToolSpecs
// (packages/envoy-client/src/tool-contract.ts): each tool's description and its arguments, read
// from the JSON Schema an agent host builds from the spec (lib/tool-schema.ts). Contract:
// scripts/generate.ts.
import { resolve } from "node:path";
import type { z as Zod } from "zod";
import { zodSchemaApi } from "../../../packages/contracts/src/tool-schema.ts";
import { envoyToolSpecs } from "../../../packages/envoy-client/src/tool-contract.ts";
import { inline, writePage } from "./lib/markdown.ts";
import { argumentLines, type JsonSchema } from "./lib/tool-schema.ts";

const CLIENT_DIR = resolve(import.meta.dir, "../../../packages/envoy-client");

// The zod @legion/envoy-client declares, resolved from that package at run time rather than
// imported statically from this file: a static import resolves nearest this file, where the site's
// own dependencies (Astro) can put an older zod, and the schema must be the one agent hosts build.
const { z } = (await import(Bun.resolveSync("zod", CLIENT_DIR))) as { z: typeof Zod };

function render(): string {
  const lines = [
    `Agents use Envoy through ${envoyToolSpecs.length} tools. Each agent host registers them from the same specs, so the arguments below are the schema the agent is handed. [The clients](/legion/envoy/clients/) says which host registers which.`,
    "",
  ];
  for (const spec of envoyToolSpecs) {
    const { $schema: _dialect, ...schema } = z.toJSONSchema(
      z.object(spec.arguments(zodSchemaApi(z)) as Zod.ZodRawShape),
      { io: "input" }
    ) as JsonSchema & { $schema?: string };
    lines.push(`## ${spec.name}`, "", inline(spec.description), "");
    lines.push(...argumentLines(spec.name, schema));
  }
  return lines.join("\n");
}

writePage({
  path: "envoy/reference/tools.md",
  title: "Agent tools",
  description: "Every Envoy tool an agent can call: what it does and its arguments.",
  source: "packages/envoy-client/src/tool-contract.ts",
  body: render,
});
