#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/api.md from Dispatch's route table
// (packages/envoy/internal/dispatch/api/routes_table.go), read through `envoy-dispatch routes`,
// which prints the body GET /api/v1 serves without a database or a listener. Contract:
// scripts/generate.ts; lib/binary-table.ts runs the binary.
import { readBinaryTable } from "./lib/binary-table.ts";
import { inline, writePage } from "./lib/markdown.ts";

/** Who may call a route, by the table's auth value (`routeAuth` in the source). */
const CALLERS: Record<string, { label: string; meaning: string }> = {
  public: { label: "Anyone", meaning: "no credential is needed." },
  any: {
    label: "Human or agent",
    meaning: "a signed-in human (browser session) or an agent (bearer token).",
  },
  human: {
    label: "Human only",
    meaning: "a signed-in human; an agent's bearer token is refused with 403 `HUMAN_ONLY`.",
  },
  bearer: {
    label: "Agent only",
    meaning: "an agent's bearer token; a browser session is refused.",
  },
};

interface Route {
  method: string;
  path: string;
  auth: string;
  description: string;
}

function readRoutes(): Route[] {
  return readBinaryTable("envoy-dispatch", "routes", "routes", [
    "method",
    "path",
    "auth",
    "description",
  ]).map((route) => {
    if (!Object.hasOwn(CALLERS, route.auth as string)) {
      throw new Error(`route ${route.method} ${route.path} has unknown auth ${route.auth}`);
    }
    return route as unknown as Route;
  });
}

function render(routes: Route[]): string {
  // A route belongs to the resource its path names after /api/v1: /api/v1/issues/{key}/asks is
  // an issues route, and /api/v1 itself is the index ("").
  const groups = Map.groupBy(routes, (route) => route.path.split("/")[3] ?? "");
  const lines = [
    `Dispatch serves ${routes.length} routes under \`/api/v1\`. A running server lists the same table at \`GET /api/v1\`, which needs no credential.`,
    "",
    "## Who may call a route",
    "",
    ...Object.values(CALLERS).map(({ label, meaning }) => `- **${label}**: ${meaning}`),
    "",
    "Path parameters are the `{name}` segments of a path.",
    "",
  ];
  for (const resource of [...groups.keys()].sort()) {
    const words = resource.replaceAll("-", " ");
    const title = resource === "" ? "Index" : words.charAt(0).toUpperCase() + words.slice(1);
    lines.push(`## ${title}`, "");
    for (const route of groups.get(resource) ?? []) {
      lines.push(`### ${route.method} ${route.path}`, "", inline(route.description), "");
      lines.push(`- **Who may call:** ${CALLERS[route.auth]?.label}`);
      const parameters = [...route.path.matchAll(/\{([^}]+)\}/g)].map((match) => `\`${match[1]}\``);
      if (parameters.length > 0) {
        lines.push(`- **Path parameters:** ${parameters.join(", ")}`);
      }
      lines.push("");
    }
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/api.md",
  title: "HTTP API",
  description:
    "Every route Dispatch serves under /api/v1: method, path, who may call it, and what it does.",
  source: "packages/envoy/internal/dispatch/api/routes_table.go",
  body: () => render(readRoutes()),
});
