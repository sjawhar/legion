// Registers GitHub Apps from manifests (docs/site/src/content/docs/legion/running-legion.md, "The
// two GitHub Apps"). It serves a loopback page with one Create button per manifest; GitHub sends
// the browser back with a one-time code, and the code's conversion returns the App's id, private
// key, client secret and webhook secret, which are written owner-only to <out>/<label>.json, the
// label being the manifest file's name without `.json`.
//
//   bun scripts/github-apps/create.ts --out <dir> [--org <org>] [--name-prefix <prefix>]
//     [--webhook-url <url>] <manifest.json>...
//
// --org registers the Apps under that organization, which takes one of its owners or App managers
// to click Create; without it they are registered under the account that clicks. A manifest with
// no name is named <prefix><label>. --webhook-url becomes the webhook of every manifest that
// subscribes to events and names no webhook of its own. GitHub redirects the browser to this
// machine's loopback address; when whoever clicks Create is on another machine, that redirect
// fails on theirs, and the address it names is pasted into this page's form instead.
import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { z } from "zod";

/** GitHub refuses an App name longer than this. */
const MAX_NAME_LENGTH = 34;

const Manifest = z.looseObject({
  name: z.string().optional(),
  url: z.url(),
  hook_attributes: z.looseObject({ url: z.url() }).optional(),
  default_events: z.array(z.string()).optional(),
  default_permissions: z.record(z.string(), z.enum(["read", "write", "admin"])),
});
type Manifest = z.infer<typeof Manifest>;

const Conversion = z.looseObject({
  id: z.number(),
  slug: z.string(),
  name: z.string(),
  owner: z.looseObject({ login: z.string() }),
  html_url: z.string(),
  client_id: z.string(),
  webhook_secret: z.string().nullable(),
  pem: z.string().startsWith("-----BEGIN"),
});
type Conversion = z.infer<typeof Conversion>;

interface App {
  label: string;
  manifest: Manifest;
  /** Set while a code is being converted, so a second callback for the App cannot race it. */
  converting: boolean;
  created?: Conversion;
}

const USAGE =
  "usage: bun scripts/github-apps/create.ts --out <dir> [--org <org>] [--name-prefix <prefix>] [--webhook-url <url>] <manifest.json>...";

function fail(message: string): never {
  console.error(`error: ${message}\n${USAGE}`);
  process.exit(2);
}

const { values, positionals } = parseArgs({
  options: {
    out: { type: "string" },
    org: { type: "string" },
    "name-prefix": { type: "string" },
    "webhook-url": { type: "string" },
    port: { type: "string", default: "0" },
  },
  allowPositionals: true,
  strict: true,
});
const outArg = values.out ?? fail("--out is required: a new directory for the Apps' credentials");
if (positionals.length === 0) fail("name at least one manifest file");
const port = Number(values.port);
if (!Number.isInteger(port) || port < 0 || port > 65535) fail("--port must be a TCP port");

/** One single-use state per App: the callback's state names the App its code belongs to. */
const apps = new Map<string, App>();
const labels = new Set<string>();
for (const file of positionals) {
  const label = basename(file, ".json");
  if (labels.has(label)) fail(`two manifests are named ${label}`);
  labels.add(label);
  const parsed = Manifest.safeParse(JSON.parse(readFileSync(file, "utf8")));
  if (!parsed.success) fail(`${file}: ${z.prettifyError(parsed.error)}`);
  const manifest = parsed.data;
  if (manifest.name === undefined && values["name-prefix"] !== undefined) {
    manifest.name = `${values["name-prefix"]}${label}`;
  }
  if (manifest.name !== undefined && manifest.name.length > MAX_NAME_LENGTH) {
    fail(`App name ${manifest.name} is longer than GitHub's ${MAX_NAME_LENGTH} characters`);
  }
  if ((manifest.default_events ?? []).length > 0 && manifest.hook_attributes === undefined) {
    const url = values["webhook-url"] ?? fail(`${file} subscribes to events: pass --webhook-url`);
    manifest.hook_attributes = { url, active: true };
  }
  apps.set(randomBytes(24).toString("hex"), { label, manifest, converting: false });
}

const out = resolve(outArg);
if (existsSync(out) && readdirSync(out).length > 0) fail(`${out} is not empty`);
mkdirSync(out, { recursive: true, mode: 0o700 });

const createUrl =
  values.org === undefined
    ? "https://github.com/settings/apps/new"
    : `https://github.com/organizations/${encodeURIComponent(values.org)}/settings/apps/new`;

function escapeHtml(text: string): string {
  return text
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function page(origin: string, error?: string): string {
  const rows = [...apps.entries()].map(([state, { label, manifest, created }]) => {
    if (created !== undefined) {
      const install = `https://github.com/apps/${created.slug}/installations/new`;
      return `<li>${escapeHtml(label)}: created <a href="${escapeHtml(created.html_url)}">${escapeHtml(created.name)}</a> (App ID ${created.id}). <a href="${escapeHtml(install)}">Install it</a>.</li>`;
    }
    const body = escapeHtml(JSON.stringify({ ...manifest, redirect_url: `${origin}/callback` }));
    const title = escapeHtml(manifest.name ?? label);
    return `<li><form action="${escapeHtml(`${createUrl}?state=${state}`)}" method="post"><input type="hidden" name="manifest" value="${body}"><button>Create ${title}</button></form></li>`;
  });
  const waiting = [...apps.values()].some((app) => app.created === undefined);
  const paste = waiting
    ? `<form action="/paste" method="get"><p>Clicked Create on another machine? Paste the address it was sent to:</p><input name="url" size="100"><button>Submit</button></form>`
    : "<p>Every App is registered; this page's server has stopped.</p>";
  const banner = error === undefined ? "" : `<p style="color:#b00">${escapeHtml(error)}</p>`;
  return `<!doctype html><meta charset="utf-8"><title>Create GitHub Apps</title><h1>Create GitHub Apps</h1>${banner}<ul>${rows.join("")}</ul>${paste}`;
}

function html(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/html; charset=utf-8" } });
}

async function complete(params: URLSearchParams, origin: string): Promise<Response> {
  const app = apps.get(params.get("state") ?? "");
  const code = params.get("code");
  if (app === undefined || code === null) {
    return html(page(origin, "That address names no App this page is waiting for."), 400);
  }
  if (app.created !== undefined) return Response.redirect(`${origin}/`, 303);
  if (app.converting) {
    return html(page(origin, `${app.label}: a code for this App is already being converted.`), 409);
  }

  app.converting = true;
  let response: Response;
  let body: string;
  try {
    response = await fetch(
      `https://api.github.com/app-manifests/${encodeURIComponent(code)}/conversions`,
      {
        method: "POST",
        headers: { Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
      }
    );
    body = await response.text();
  } finally {
    app.converting = false;
  }
  if (response.status !== 201) {
    const message = `${app.label}: GitHub refused the code (${response.status}): ${body.slice(0, 300)}`;
    console.error(message);
    return html(page(origin, message), 502);
  }
  const created = Conversion.parse(JSON.parse(body));
  const file = join(out, `${app.label}.json`);
  writeFileSync(file, body, { mode: 0o600, flag: "wx" });
  app.created = created;
  console.log(
    [
      `${app.label}: created ${created.name}`,
      `  app id     ${created.id}`,
      `  slug       ${created.slug} (bot ${created.slug}[bot])`,
      `  client id  ${created.client_id}`,
      `  owner      ${created.owner.login}`,
      `  webhook    ${created.webhook_secret === null ? "none" : "secret in the credentials file"}`,
      `  install    https://github.com/apps/${created.slug}/installations/new`,
      `  credentials ${file}`,
    ].join("\n")
  );
  return Response.redirect(`${origin}/`, 303);
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request): Promise<Response> {
    const url = new URL(request.url);
    const origin: string = `http://127.0.0.1:${server.port}`;
    switch (url.pathname) {
      case "/": {
        if ([...apps.values()].every((app) => app.created !== undefined)) {
          setTimeout(() => server.stop(), 500);
        }
        return html(page(origin));
      }
      case "/callback":
        return complete(url.searchParams, origin);
      case "/paste": {
        const pasted = URL.parse(url.searchParams.get("url") ?? "");
        if (pasted === null) return html(page(origin, "That is not a URL."), 400);
        return complete(pasted.searchParams, origin);
      }
      default:
        return new Response("not found", { status: 404 });
    }
  },
});

console.log(
  `Open http://127.0.0.1:${server.port}/ and create each App (GitHub signs you in first).`
);
