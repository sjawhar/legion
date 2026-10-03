#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/api.md from Dispatch's route table
// (packages/envoy/internal/dispatch/api/routes_table.go), read through `envoy-dispatch routes`,
// which prints the body GET /api/v1 serves without a database or a listener. It runs the
// subcommand with `go run`, so the page follows the checked-out source, never an installed
// binary; after the build compiles envoy-dispatch, the Go build cache leaves only the link.
import { join, resolve } from "node:path";
import { inline, writePage } from "./lib/markdown.ts";

const SOURCE = "packages/envoy/internal/dispatch/api/routes_table.go";
const GENERATOR = "docs/site/generators/dispatch-api.ts";
const REPO_ROOT = resolve(import.meta.dir, "../../..");
const COMMAND = ["go", "run", "./cmd/dispatch", "routes"];

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
  const run = Bun.spawnSync(COMMAND, {
    cwd: join(REPO_ROOT, "packages/envoy"),
    stdout: "pipe",
    stderr: "pipe",
  });
  if (run.exitCode !== 0) {
    throw new Error(`${COMMAND.join(" ")} exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const body = JSON.parse(run.stdout.toString()) as { routes?: unknown };
  if (!Array.isArray(body.routes) || body.routes.length === 0) {
    throw new Error(`${COMMAND.join(" ")} printed no routes`);
  }
  return body.routes.map((route: Record<string, unknown>) => {
    for (const field of ["method", "path", "auth", "description"]) {
      if (typeof route[field] !== "string" || route[field] === "") {
        throw new Error(`route ${JSON.stringify(route)} has no ${field}`);
      }
    }
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
    "---",
    "title: HTTP API",
    `description: ${JSON.stringify("Every route Dispatch serves under /api/v1: method, path, who may call it, and what it does.")}`,
    "editUrl: false",
    "---",
    "",
    `> Generated from \`${SOURCE}\` by \`${GENERATOR}\`. Edit the route table, not this page.`,
    "",
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

writePage(GENERATOR, "dispatch/reference/api.md", () => render(readRoutes()));
