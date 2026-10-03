#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a phase worker and the
 * `legion` command-line tool hit while a shell command runs: the worker's claim registration
 * (`claims/register`, `claims/ready`, the daemon's claim routes the plugin boots through), grant
 * minting, the two credential redemptions (git credential, GitHub token), and a phase completion
 * (`GET /legion/v1/state`, which `legion handoff complete` reads the issue's phase from, then
 * `handoff/complete`).
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): the
 * plugin's grant request, the CLI's redemptions and completion, and the state document. The grant
 * rule mirrors the real daemon's: a grant lives for 60 seconds and redeems any number of times
 * while it lives; an unknown or expired grant id answers 403 `{"error":"Invalid or expired
 * grant"}`. Nothing is single-use.
 *
 * Every request appends one JSON line to the log file: `{at, path, status, grantId?, sessionId?,
 * mintedGrantId?}`. The rig driver (`run.ts`) reads that log to count grant mints per shell
 * command and to check which redemptions succeeded.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> [<project> <issue> <role>]`
 * The role token defaults to project `l12rig`, issue `RIG-1`, role `implementer`; the skill
 * scenarios (`../skill-scenarios/rig.sh`) name their own. The state document holds that one issue,
 * in the phase its role works.
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open.
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";
import { isLegionRole, type LegionRole, roleToken } from "@legion/contracts";
import {
  LegionGrantCredentialRequest,
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  type LegionPhase,
  LegionStateResponse,
} from "@legion/contracts/legion-api";

const GRANT_TTL_MS = 60_000;
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const [portArg, logFile, bootTokenFile, project = "l12rig", issue = "RIG-1", role = "implementer"] =
  Bun.argv.slice(2);
if (!isLegionRole(role)) {
  console.error(`${role} is not a Legion role`);
  process.exit(2);
}
const ROLE_TOKEN = roleToken(project, issue, role);
const SESSION_SECRET = "rig-secret";
const GIT_TOKEN = "rig-token";

/** The workflow phase each role's worker runs in (`workflow.RoleFor`'s inverse). */
const ROLE_PHASE: Readonly<Record<LegionRole, LegionPhase>> = {
  architect: "admitted",
  planner: "planning",
  implementer: "implementing",
  tester: "testing",
  reviewer: "reviewing",
  merger: "merging",
};
const startedAt = new Date().toISOString();
/** `GET /legion/v1/state`: the one issue the worker holds, admitted in the phase its role works. */
const STATE = LegionStateResponse.parse({
  daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
  admission: { cap: 1, active: [issue], waiting: [] },
  issues: {
    [issue]: {
      key: issue,
      generation: 1,
      phase: ROLE_PHASE[role],
      status: "in_progress",
      workers: {},
    },
  },
  pendingStatusWrites: [],
});

interface LogLine {
  readonly at: string;
  readonly path: string;
  readonly status: number;
  readonly grantId?: string;
  readonly sessionId?: string;
  readonly mintedGrantId?: string;
}

if (portArg === undefined || logFile === undefined || bootTokenFile === undefined) {
  console.error(
    "usage: bun daemon-standin.ts <port> <log file> <boot token file> [<project> <issue> <role>]"
  );
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

/** Mirrors the real daemon's rule: present and not yet expired, nothing else. Each route parses
 * its body with its own contract schema first, exactly as the daemon's route table does, and
 * hands the parsed id here. */
function resolveGrant(grantId: string): Response | undefined {
  const grant = grants.get(grantId);
  if (!grant || grant.expiresAt <= Date.now()) return forbidden("Invalid or expired grant");
  return undefined;
}

function handle(path: string, body: unknown): { response: Response; mintedGrantId?: string } {
  switch (path) {
    case "/legion/v1/claims/register": {
      if (stringField(body, "bootToken") !== expectedBootToken) {
        return { response: forbidden("Invalid boot token") };
      }
      return {
        response: json(200, {
          claimToken: ROLE_TOKEN,
          tree: issue,
          issue,
          role,
          generation: 1,
          secret: SESSION_SECRET,
        }),
      };
    }
    case "/legion/v1/claims/ready": {
      if (stringField(body, "secret") !== SESSION_SECRET) {
        return { response: forbidden("Invalid session secret") };
      }
      return { response: new Response(null, { status: 204 }) };
    }
    case "/legion/v1/grants": {
      const parsed = LegionGrantRequest.safeParse(body);
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
    // The daemon reads both credential redemptions as `api.GrantCredentialRequest` (a strict
    // `{ grantId }`), so the stand-in refuses the same malformed bodies it would.
    case "/legion/v1/git-credential": {
      const parsed = LegionGrantCredentialRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      return {
        response: new Response(`username=x-access-token\npassword=${GIT_TOKEN}`, {
          headers: { "content-type": "text/plain; charset=utf-8" },
        }),
      };
    }
    case "/legion/v1/gh-token": {
      const parsed = LegionGrantCredentialRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      return { response: json(200, { token: GIT_TOKEN, appLogin: "rig[bot]" }) };
    }
    case "/legion/v1/handoff/complete": {
      const parsed = LegionHandoffCompleteRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
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
        : request.method === "GET" && path === "/legion/v1/state"
          ? { response: json(200, STATE) }
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
