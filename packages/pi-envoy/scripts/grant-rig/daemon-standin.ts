#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a phase worker and the
 * `legion` command-line tool hit while a shell command runs: the worker boot handshake, grant
 * minting, and the three grant redemptions (git credential, GitHub token, phase completion).
 *
 * Request and response shapes come from `LegionDaemonApi` in `@legion/contracts`, and the grant
 * rule mirrors `AuthState.resolveGrant` in the real daemon (`packages/daemon/src/daemon/api/auth.ts`):
 * a grant lives for 60 seconds and redeems any number of times while it lives; an unknown or
 * expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is single-use.
 *
 * Every request appends one JSON line to the log file: `{at, path, status, grantId?, sessionId?,
 * mintedGrantId?}`. The rig driver (`run.ts`) reads that log to count grant mints per shell
 * command and to check which redemptions succeeded.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file>`
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open.
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";
import { LegionDaemonApi, roleToken } from "@legion/contracts";

const GRANT_TTL_MS = 60_000;
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken("l12rig", "RIG-1", "implementer");
const SESSION_SECRET = "rig-secret";
const GIT_TOKEN = "rig-token";

interface LogLine {
  readonly at: string;
  readonly path: string;
  readonly status: number;
  readonly grantId?: string;
  readonly sessionId?: string;
  readonly mintedGrantId?: string;
}

const [portArg, logFile, bootTokenFile] = Bun.argv.slice(2);
if (portArg === undefined || logFile === undefined || bootTokenFile === undefined) {
  console.error("usage: bun daemon-standin.ts <port> <log file> <boot token file>");
  process.exit(2);
}
const expectedBootToken = (await Bun.file(bootTokenFile).text()).trim();
if (expectedBootToken.length === 0) {
  console.error(`boot token file ${bootTokenFile} is empty`);
  process.exit(2);
}

const grants = new Map<string, { readonly expiresAt: number }>();

function json(status: number, body: unknown): Response {
  return Response.json(body, { status });
}

function forbidden(message: string): Response {
  return json(403, { error: message });
}

function stringField(body: unknown, key: string): string | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "string" ? value : undefined;
}

/** Mirrors the real daemon's rule: present and not yet expired, nothing else. */
function resolveGrant(body: unknown): Response | undefined {
  const grantId = stringField(body, "grantId");
  if (grantId === undefined) return json(400, { error: "grantId is required" });
  const grant = grants.get(grantId);
  if (!grant || grant.expiresAt <= Date.now()) return forbidden("Invalid or expired grant");
  return undefined;
}

function handle(path: string, body: unknown): { response: Response; mintedGrantId?: string } {
  switch (path) {
    case "/legion/v1/worker/started": {
      const parsed = LegionDaemonApi.WorkerStarted.request.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      if (parsed.data.bootToken !== expectedBootToken) {
        return { response: forbidden("Invalid boot token") };
      }
      return {
        response: json(200, {
          roleToken: ROLE_TOKEN,
          secret: SESSION_SECRET,
          gitName: "Rig Worker",
          gitEmail: "rig@example.invalid",
        }),
      };
    }
    case "/legion/v1/worker/ready": {
      const parsed = LegionDaemonApi.WorkerReady.request.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      if (parsed.data.secret !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret") };
      }
      return { response: json(200, {}) };
    }
    case "/legion/v1/grants": {
      const parsed = LegionDaemonApi.Grant.request.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      if (parsed.data.secret !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret") };
      }
      const grantId = randomUUID();
      const expiresAt = Date.now() + GRANT_TTL_MS;
      grants.set(grantId, { expiresAt });
      return {
        response: json(200, { grantId, expiresAt: new Date(expiresAt).toISOString() }),
        mintedGrantId: grantId,
      };
    }
    case "/legion/v1/git-credential": {
      const refused = resolveGrant(body);
      if (refused) return { response: refused };
      return {
        response: new Response(`username=x-access-token\npassword=${GIT_TOKEN}`, {
          headers: { "content-type": "text/plain; charset=utf-8" },
        }),
      };
    }
    case "/legion/v1/gh-token": {
      const refused = resolveGrant(body);
      if (refused) return { response: refused };
      return { response: json(200, { token: GIT_TOKEN, appLogin: "rig[bot]" }) };
    }
    case "/legion/v1/phase/complete": {
      const parsed = LegionDaemonApi.PhaseComplete.request.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(body);
      if (refused) return { response: refused };
      return { response: json(200, {}) };
    }
    default:
      return { response: json(404, { error: `no route for ${path}` }) };
  }
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(portArg),
  async fetch(request) {
    const path = new URL(request.url).pathname;
    let body: unknown;
    if (request.method === "POST") {
      try {
        body = await request.json();
      } catch {
        body = undefined;
      }
    }
    const { response, mintedGrantId } =
      request.method === "POST"
        ? handle(path, body)
        : { response: json(404, { error: `no route for ${request.method} ${path}` }) };
    const line: LogLine = {
      at: new Date().toISOString(),
      path,
      status: response.status,
      ...(stringField(body, "grantId") === undefined
        ? {}
        : { grantId: stringField(body, "grantId") }),
      ...(stringField(body, "sessionId") === undefined
        ? {}
        : { sessionId: stringField(body, "sessionId") }),
      ...(mintedGrantId === undefined ? {} : { mintedGrantId }),
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
