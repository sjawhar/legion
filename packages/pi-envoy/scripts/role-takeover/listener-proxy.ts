#!/usr/bin/env bun
/**
 * listener-proxy.ts — one client instance's HTTP path to the Envoy listener, for
 * `scripts/smoke-role-takeover.sh` only.
 *
 * Every client process in that smoke reaches the listener through a proxy of its own, so the
 * smoke reads what each client sent and what the listener answered it without instrumenting any
 * client code: the Oh My Pi extension and the Claude Code channel server run exactly as they
 * ship. Each request and its answer is appended to `--log` as one JSON line:
 *
 *   {"seq":1,"label":"omp-a","at":1761,"method":"POST","path":"/v1/roles/set","status":409,
 *    "request":{...},"response":{...},"ms":3}
 *
 * The proxy also holds a request where the smoke needs one held: `POST /__smoke/hold?path=<p>`
 * parks every later request whose path is `<p>` before it reaches the listener, which is how
 * A5's first scenario holds a fork's role claim back until another process has resumed and
 * registered its predecessor. `GET /__smoke/held?path=<p>` answers how many are parked,
 * `POST /__smoke/release?path=<p>` lets them through and disarms the hold. These three paths are
 * the proxy's own and never reach the listener.
 *
 * Usage: bun listener-proxy.ts --port <n> --upstream <url> --log <file> --label <name>
 */
import { appendFileSync } from "node:fs";

interface Options {
  readonly port: number;
  readonly upstream: string;
  readonly log: string;
  readonly label: string;
}

function parseOptions(argv: readonly string[]): Options {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (name === undefined || !name.startsWith("--") || value === undefined) {
      throw new Error(`listener-proxy: expected --name value pairs, got ${argv.join(" ")}`);
    }
    values.set(name.slice(2), value);
  }
  const port = Number(values.get("port"));
  const upstream = values.get("upstream");
  const log = values.get("log");
  const label = values.get("label");
  if (!Number.isInteger(port) || port <= 0) throw new Error("listener-proxy: --port is required");
  if (upstream === undefined) throw new Error("listener-proxy: --upstream is required");
  if (log === undefined) throw new Error("listener-proxy: --log is required");
  if (label === undefined) throw new Error("listener-proxy: --label is required");
  return { port, upstream: upstream.replace(/\/$/, ""), log, label };
}

const options = parseOptions(process.argv.slice(2));

/** Paths whose requests are parked, each with the parked requests' releases. */
const holds = new Map<string, (() => void)[]>();
let seq = 0;

/** The body as the log should carry it: parsed JSON where it is JSON, else the text, else null. */
function logBody(text: string): unknown {
  if (text === "") return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

function record(entry: Record<string, unknown>): void {
  appendFileSync(options.log, `${JSON.stringify(entry)}\n`);
}

function controlAnswer(path: string, url: URL): Response | undefined {
  const target = url.searchParams.get("path") ?? "";
  switch (path) {
    case "/__smoke/hold":
      if (!holds.has(target)) holds.set(target, []);
      return Response.json({ holding: target });
    case "/__smoke/held":
      return Response.json({ held: holds.get(target)?.length ?? 0 });
    case "/__smoke/release": {
      const parked = holds.get(target) ?? [];
      holds.delete(target);
      for (const release of parked) release();
      return Response.json({ released: parked.length });
    }
    default:
      return undefined;
  }
}

/** Parks the request until its hold is released, when its path is held. */
async function waitWhileHeld(path: string): Promise<void> {
  const parked = holds.get(path);
  if (parked === undefined) return;
  const gate = Promise.withResolvers<void>();
  parked.push(gate.resolve);
  await gate.promise;
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: options.port,
  idleTimeout: 0,
  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const control = controlAnswer(url.pathname, url);
    if (control !== undefined) return control;
    const body = request.method === "GET" || request.method === "HEAD" ? "" : await request.text();
    await waitWhileHeld(url.pathname);
    const headers = new Headers(request.headers);
    headers.delete("host");
    headers.delete("content-length");
    const started = Date.now();
    seq += 1;
    const entry = {
      seq,
      label: options.label,
      // The wall clock the smoke orders one client's requests against another's by.
      at: started,
      method: request.method,
      path: url.pathname,
      query: url.search,
      request: logBody(body),
    };
    let answer: Response;
    try {
      answer = await fetch(`${options.upstream}${url.pathname}${url.search}`, {
        method: request.method,
        headers,
        ...(body === "" ? {} : { body }),
      });
    } catch (error) {
      record({ ...entry, status: 0, response: String(error), ms: Date.now() - started });
      return Response.json({ error: "the listener did not answer the proxy" }, { status: 502 });
    }
    const text = await answer.text();
    record({ ...entry, status: answer.status, response: logBody(text), ms: Date.now() - started });
    const answerHeaders = new Headers(answer.headers);
    answerHeaders.delete("content-encoding");
    answerHeaders.delete("content-length");
    return new Response(text === "" ? null : text, {
      status: answer.status,
      headers: answerHeaders,
    });
  },
});

process.stdout.write(`listener-proxy ${options.label} ready on ${server.port}\n`);
