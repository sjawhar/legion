#!/usr/bin/env bun
// Writes <content dir>/envoy/reference/api.md from the listener's route table
// (packages/envoy/cmd/listener/routes.go), read through `envoy-listener routes`, which prints every
// route the listener can serve without NATS or a running listener. Contract: scripts/generate.ts;
// lib/binary-table.ts runs the binary.
import { readBinaryTable } from "./lib/binary-table.ts";
import { inline, writePage } from "./lib/markdown.ts";

/** What a caller presents, by the table's auth value (`routeAuth` in the source). */
const CALLERS: Record<string, { label: string; meaning: string }> = {
  none: { label: "Anyone", meaning: "no credential is needed." },
  signature: {
    label: "Webhook signature",
    meaning:
      "the provider signs the body with the route's secret, which the listener checks; the [settings](/legion/envoy/reference/settings/) name each secret.",
  },
  bearer: {
    label: "Bearer token",
    meaning:
      "`Authorization: Bearer <token>`, where the token is `ENVOY_API_TOKEN` or a Kubernetes service-account token issued by `ENVOY_OIDC_ISSUER` for `ENVOY_OIDC_AUDIENCE`. A listener with neither configured takes every caller. A missing or wrong token is answered `401`.",
  },
};

interface Route {
  method: string;
  path: string;
  auth: string;
  when: string | null;
  description: string;
}

function readRoutes(): Route[] {
  return readBinaryTable("envoy-listener", "routes", "routes", [
    "method",
    "path",
    "auth",
    "description",
  ]).map((route) => {
    if (!Object.hasOwn(CALLERS, route.auth as string)) {
      throw new Error(`route ${route.method} ${route.path} has unknown auth ${route.auth}`);
    }
    if (route.when !== null && typeof route.when !== "string") {
      throw new Error(
        `route ${route.method} ${route.path} has a when that is neither text nor null`
      );
    }
    return route as unknown as Route;
  });
}

/** The section a route belongs to: the operational routes, the webhooks, or the /v1 resource its
 *  path names after /v1 (/v1/interests/subscribe is an Interests route). */
function section(route: Route): string {
  if (route.path.startsWith("/webhook/")) return "Webhooks";
  if (!route.path.startsWith("/v1/")) return "Health and metrics";
  const resource = route.path.split("/")[2] ?? "";
  return resource.charAt(0).toUpperCase() + resource.slice(1);
}

function render(routes: Route[]): string {
  const groups = Map.groupBy(routes, section);
  const lines = [
    `The Envoy listener serves the ${routes.length} routes below. \`envoy-listener routes\` prints the same table from the binary you run.`,
    "",
    'Every `/v1` answer is JSON, and an error is `{"error": "<message>", "expected": ["<field>"]}`, where `expected` names a field the caller must supply. While the listener starts, each webhook route answers `503` `service starting` until NATS is connected, and `/v1` until its session and interest caches have loaded too.',
    "",
    "## Who may call a route",
    "",
    ...Object.values(CALLERS).map(({ label, meaning }) => `- **${label}**: ${meaning}`),
    "",
    "Path parameters are the `{name}` segments of a path.",
    "",
  ];
  for (const [title, members] of groups) {
    lines.push(`## ${title}`, "");
    for (const route of members) {
      lines.push(`### ${route.method} ${route.path}`, "", inline(route.description), "");
      lines.push(`- **Who may call:** ${CALLERS[route.auth]?.label}`);
      if (route.when !== null) lines.push(`- **Mounted when:** ${inline(route.when)}`);
      const parameters = [...route.path.matchAll(/\{([^}]+)\}/g)].map((match) => `\`${match[1]}\``);
      if (parameters.length > 0) lines.push(`- **Path parameters:** ${parameters.join(", ")}`);
      lines.push("");
    }
  }
  return lines.join("\n");
}

writePage({
  path: "envoy/reference/api.md",
  title: "Listener HTTP API",
  description:
    "Every route the Envoy listener serves: method, path, who may call it, and what it does.",
  source: "packages/envoy/cmd/listener/routes.go",
  body: () => render(readRoutes()),
});
