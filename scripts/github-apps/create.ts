// Registers GitHub Apps from manifests (docs/site/src/content/docs/legion/running-legion.md, "The
// GitHub Apps"). It serves a loopback page with one Create button per manifest; GitHub sends the
// browser back with a one-time code, and the code's conversion returns the App's id, private key,
// client secret and webhook secret. That whole answer is written owner-only to <out>/<label>.json,
// the label being the manifest file's name without `.json`, before anything else reads it.
//
// Usage is USAGE below. --org registers the Apps under that organization, which takes one of its
// owners or App managers to click Create; without it they are registered under the account that
// clicks. Every App is private, installable only on that owner, unless --public makes them
// installable by any account. A manifest with no name is named <prefix><label>. --webhook-url
// becomes the webhook of every manifest that subscribes to events and names no webhook of its
// own. --port fixes the loopback page's port (any free port by default). GitHub redirects the
// browser to this machine's loopback address; when whoever clicks Create is on another machine,
// that redirect fails on theirs, and the address it names is pasted into this page's form instead.
import { randomBytes } from "node:crypto";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  unlinkSync,
  writeSync,
} from "node:fs";
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
  "usage: bun scripts/github-apps/create.ts --out <dir> [--org <org>] [--public] [--name-prefix <prefix>] [--webhook-url <url>] [--port <port>] <manifest.json>...";

function fail(message: string): never {
  console.error(`error: ${message}\n${USAGE}`);
  process.exit(2);
}

// parseArgs throws on a malformed command line (`--port -1` reads as an unknown option); that is
// the same usage error as the checks below, so it exits the same way.
function parseCommandLine() {
  try {
    return parseArgs({
      options: {
        out: { type: "string" },
        org: { type: "string" },
        public: { type: "boolean", default: false },
        "name-prefix": { type: "string" },
        "webhook-url": { type: "string" },
        port: { type: "string", default: "0" },
      },
      allowPositionals: true,
      strict: true,
    });
  } catch (error) {
    return fail((error as Error).message);
  }
}

const { values, positionals } = parseCommandLine();
const out = resolve(
  values.out ?? fail("--out is required: a new directory for the Apps' credentials")
);
if (positionals.length === 0) fail("name at least one manifest file");
const port = Number(values.port);
if (!Number.isInteger(port) || port < 0 || port > 65535) fail("--port must be a TCP port");

/** One random state per App: the callback's state names the App its code belongs to. */
const apps = new Map<string, App>();
const labels = new Set<string>();
for (const file of positionals) {
  const label = basename(file, ".json");
  if (labels.has(label)) fail(`two manifests are named ${label}`);
  labels.add(label);
  const parsed = Manifest.safeParse(JSON.parse(readFileSync(file, "utf8")));
  if (!parsed.success) fail(`${file}: ${z.prettifyError(parsed.error)}`);
  const manifest = { ...parsed.data, public: values.public };
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

if (existsSync(out) && readdirSync(out).length > 0) fail(`${out} is not empty`);
mkdirSync(out, { recursive: true, mode: 0o700 });

const createUrl =
  values.org === undefined
    ? "https://github.com/settings/apps/new"
    : `https://github.com/organizations/${encodeURIComponent(values.org)}/settings/apps/new`;

function page(error?: string): string {
  const rows = [...apps.entries()].map(([state, { label, manifest, created }]) => {
    if (created !== undefined) {
      const install = `https://github.com/apps/${created.slug}/installations/new`;
      return `<li>${Bun.escapeHTML(label)}: created <a href="${Bun.escapeHTML(created.html_url)}">${Bun.escapeHTML(created.name)}</a> (App ID ${created.id}). <a href="${Bun.escapeHTML(install)}">Install it</a>.</li>`;
    }
    const body = Bun.escapeHTML(
      JSON.stringify({ ...manifest, redirect_url: `${origin}/callback` })
    );
    const title = Bun.escapeHTML(manifest.name ?? label);
    return `<li><form action="${Bun.escapeHTML(`${createUrl}?state=${state}`)}" method="post"><input type="hidden" name="manifest" value="${body}"><button>Create ${title}</button></form></li>`;
  });
  const waiting = [...apps.values()].some((app) => app.created === undefined);
  const paste = waiting
    ? `<form action="/paste" method="get"><p>Clicked Create on another machine? Paste the address it was sent to:</p><input name="url" size="100"><button>Submit</button></form>`
    : "<p>Every App is registered; this page's server has stopped.</p>";
  const banner = error === undefined ? "" : `<p style="color:#b00">${Bun.escapeHTML(error)}</p>`;
  return `<!doctype html><meta charset="utf-8"><title>Create GitHub Apps</title><h1>Create GitHub Apps</h1>${banner}<ul>${rows.join("")}</ul>${paste}`;
}

function html(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/html; charset=utf-8" } });
}

/** The value `text` holds, or the text itself when it is not JSON, for the schema to refuse. */
function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** Back to the page, relative to whichever address the browser reached it at. */
function seeOther(): Response {
  return new Response(null, { status: 303, headers: { Location: "/" } });
}

async function complete(params: URLSearchParams): Promise<Response> {
  const app = apps.get(params.get("state") ?? "");
  const code = params.get("code");
  if (app === undefined || code === null) {
    return html(page("That address names no App this page is waiting for."), 400);
  }
  if (app.created !== undefined) return seeOther();
  if (app.converting) {
    return html(page(`${app.label}: a code for this App is already being converted.`), 409);
  }
  // A code converts once, so the file its answer goes to is claimed before GitHub is asked: no
  // write can fail after the conversion has happened.
  const file = join(out, `${app.label}.json`);
  let fd: number;
  try {
    fd = openSync(file, "wx", 0o600);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    return html(
      page(`${app.label}: GitHub's answer for this App is already saved in ${file}.`),
      409
    );
  }
  app.converting = true;
  // Only a code GitHub refused, or a request that never got an answer, gives the file back: once
  // GitHub has converted the code, whatever went wrong, the file stays for its owner to inspect.
  let converted = false;
  let body: string;
  try {
    const response = await fetch(
      `https://api.github.com/app-manifests/${encodeURIComponent(code)}/conversions`,
      {
        method: "POST",
        headers: { Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
      }
    );
    converted = response.status === 201;
    body = await response.text();
    if (!converted) {
      const message = `${app.label}: GitHub refused the code (${response.status}): ${body.slice(0, 300)}`;
      console.error(message);
      return html(page(message), 502);
    }
    const bytes = Buffer.from(body);
    for (let written = 0; written < bytes.length; ) {
      const count = writeSync(fd, bytes, written);
      if (count === 0) throw new Error(`${file}: the disk took none of GitHub's answer`);
      written += count;
    }
  } finally {
    app.converting = false;
    closeSync(fd);
    if (!converted) unlinkSync(file);
  }
  const parsed = Conversion.safeParse(parseJson(body));
  if (!parsed.success) {
    const message = `${app.label}: GitHub's answer is saved in ${file}, but this script cannot read it: ${z.prettifyError(parsed.error)}`;
    console.error(message);
    return html(page(message), 502);
  }
  const created = parsed.data;
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
  return seeOther();
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    // A page another site reaches through a rebound DNS name sends that name as its Host, and could
    // read every App's state off this page. The port may differ: a forwarded port reaches it too.
    const hostname = URL.parse(`http://${request.headers.get("host")}`)?.hostname;
    if (hostname !== "127.0.0.1" && hostname !== "localhost") {
      return new Response("this page answers only on its loopback address", { status: 421 });
    }
    switch (url.pathname) {
      case "/": {
        if ([...apps.values()].every((app) => app.created !== undefined)) server.stop();
        return html(page());
      }
      case "/callback":
        return complete(url.searchParams);
      case "/paste": {
        const pasted = URL.parse(url.searchParams.get("url") ?? "");
        if (pasted === null) return html(page("That is not a URL."), 400);
        return complete(pasted.searchParams);
      }
      default:
        return new Response("not found", { status: 404 });
    }
  },
});
const origin = `http://127.0.0.1:${server.port}`;

console.log(`Open ${origin}/ and create each App (GitHub signs you in first).`);
