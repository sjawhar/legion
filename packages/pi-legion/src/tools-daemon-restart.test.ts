import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import net from "node:net";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { z } from "zod";
import { createLegionTool } from "./tools";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): string {
  return readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name),
    "utf8"
  );
}

const pi = {
  zod: z,
  sendMessage: () => undefined,
  appendEntry: () => undefined,
  getActiveTools: () => [],
  setActiveTools: async () => undefined,
  on: () => undefined,
  registerTool: () => undefined,
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
} as unknown as PiApi;

const context: SessionContext = {
  cwd: "/workspace",
  hasUI: true,
  setInterval: () => undefined,
  setTimeout: () => undefined,
  sessionManager: {
    getSessionId: () => "ses_208",
    getSessionFile: () => "/sessions/208.jsonl",
    ensureOnDisk: async () => undefined,
    getEntries: () => [],
  },
  ui: { notify: () => undefined },
};

const COMMIT = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const stops: Array<() => void> = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** The daemon after its restart: it answers each route from the golden fixtures, or `refuse`'s
 * status and body for the route it names, and records the path of every request it answered. */
function liveDaemon(refuse?: { path: string; status: number; body: object }) {
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const route = new URL(request.url).pathname;
      answered.push(route);
      if (refuse !== undefined && route === refuse.path) {
        return Response.json(refuse.body, { status: refuse.status });
      }
      const fixture = {
        "/legion/v1/state": "state.json",
        "/legion/v1/grants": "grant.json",
        "/legion/v1/handoff/complete": "handoff-complete.json",
        "/legion/v1/signoff": "signoff.json",
        "/legion/v1/gates/register": "gate-register.json",
      }[route];
      if (fixture === undefined) return new Response("no route", { status: 404 });
      return new Response(daemonFixture(fixture), {
        headers: { "Content-Type": "application/json" },
      });
    },
  });
  stops.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}`, answered };
}

/** A port nothing listens on: a connection to it is refused, as it is while a daemon restarts. */
function refusingOrigin(): string {
  const server = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response("") });
  const origin = `http://127.0.0.1:${server.port}`;
  server.stop(true);
  return origin;
}

/** A listener that accepts each connection and closes it once the request arrives, before any
 * response: the daemon going down mid-request. */
async function closingOrigin(): Promise<string> {
  const server = net.createServer((socket) => {
    socket.on("data", () => socket.destroy());
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  stops.push(() => server.close());
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("no TCP address");
  return `http://127.0.0.1:${address.port}`;
}

/** The real daemon client over a fetch that sends its first `outage` requests to `down` (each
 * failing there with the real fetch error) and every later one to the live daemon. */
function restartingClient(live: string, down: string, outage: number) {
  let sent = 0;
  const fetchFn = ((input: string | URL | Request, init?: RequestInit) => {
    sent += 1;
    const url = String(input);
    return fetch(sent <= outage ? url.replace(live, down) : url, init);
  }) as typeof fetch;
  return createLegionDaemonClient(live, fetchFn);
}

/** A virtual clock: each wait is recorded and moves time on at once. */
function virtualClock() {
  let time = 0;
  const waits: number[] = [];
  return {
    waits,
    now: () => time,
    sleep: async (ms: number) => {
      waits.push(ms);
      time += ms;
    },
  };
}

function toolFor(
  role: LegionRole,
  client: LegionDaemonClient,
  clock: { readonly now: () => number; readonly sleep: (ms: number) => Promise<void> },
  completed: string[] = [],
  tree = role === "architect" ? "LEGION-207" : "LEGION-208"
) {
  return createLegionTool({
    pi,
    daemon: () => client,
    session: () => ({
      kind: role === "architect" ? "architect" : "phase-worker",
      role,
      sessionID: "ses_208",
      tree,
      issue: "LEGION-208",
      secret: "claim-secret",
    }),
    controllerGrant: async () => {
      throw new Error("no controller grant expected");
    },
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    handoffCommit: async () => COMMIT,
    resolveDocument: async () => {
      throw new Error("no document lookup expected");
    },
    restartClock: { now: clock.now, sleep: clock.sleep },
  });
}

/** Runs one tool call, collecting each partial result the tool sends the agent. */
async function run(tool: RegisteredTool, parameters: Record<string, unknown>) {
  const updates: string[] = [];
  const onUpdate = (partial: { content: Array<{ text: string }> }) => {
    updates.push(partial.content.map((block) => block.text).join(""));
  };
  const result = await tool.execute("", parameters, undefined, onUpdate, context);
  return { result, updates };
}

test("handoff_complete waits out a daemon that refuses connections while it restarts, and tells the agent once", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, refusingOrigin(), 4),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBeUndefined();
  expect(result.details).toEqual(JSON.parse(daemonFixture("handoff-complete.json")));
  expect(completed).toEqual(["ses_208"]);
  expect(live.answered).toEqual([
    "/legion/v1/state",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(clock.waits).toEqual([500, 1000, 2000, 4000]);
  expect(updates).toHaveLength(1);
  expect(updates[0]).toContain("The Legion daemon is unreachable, most likely restarting");
  expect(updates[0]).toContain("handoff_complete is being retried for up to 10 minutes");
});

test("handoff_complete retries a connection the daemon closed before answering, and passes on the repeat's refusal marked as possibly following an applied attempt", async () => {
  // The first attempt's three requests are closed unanswered, so the completion may have landed;
  // the daemon answers the repeat ALREADY_RECORDED, which it also answers when that attempt was
  // refused, so the tool claims no success and leaves the phase open.
  const detail = `the implementer completion of phase implementing (round 0) at commit ${COMMIT} was already received; this call changed nothing`;
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: { error: detail, code: "HANDOFF_ALREADY_RECORDED" },
  });
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, await closingOrigin(), 3),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBe(true);
  expect(result.content[0]).toEqual({
    type: "text",
    text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_ALREADY_RECORDED: ${detail} (an earlier attempt of handoff_complete lost its connection before the daemon answered, and may have been applied: read the record before calling again)`,
  });
  expect(completed).toEqual([]);
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(updates).toHaveLength(1);
  expect(clock.waits).toEqual([500, 1000, 2000]);
});

test("a 409 after refused connections only is passed on unmarked, since a refused request was never sent", async () => {
  const detail = `the implementer reported commit ${COMMIT} for its previous phase of LEGION-208; write and commit this phase's handoff before completing`;
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: { error: detail, code: "HANDOFF_NOT_NEW" },
  });
  const clock = virtualClock();
  const tool = toolFor("implementer", restartingClient(live.url, refusingOrigin(), 2), clock);

  const { result } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.content[0]).toEqual({
    type: "text",
    text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: ${detail}`,
  });
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(clock.waits).toEqual([500, 1000]);
});

test("the daemon's own 502 or 503, which carries its code, is its answer and is not retried", async () => {
  const live = liveDaemon({
    path: "/legion/v1/signoff",
    status: 503,
    body: { error: "the daemon could not reach GitHub", code: "GITHUB_READ_FAILED" },
  });
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 0), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBe(true);
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/signoff"]);
  expect(clock.waits).toEqual([]);
  expect(updates).toEqual([]);
});

test("register_gate waits out a restarting daemon", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const client = restartingClient(live.url, refusingOrigin(), 2);
  const tool = toolFor("architect", client, clock, [], "LEGION-208");

  const { result, updates } = await run(tool, {
    op: "register_gate",
    issue: "LEGION-208",
    artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
    version: 1,
  });

  expect(result).toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/gates/register"]);
  expect(clock.waits).toEqual([500, 1000]);
  expect(updates).toHaveLength(1);
});

test("an aborted call stops waiting for the daemon", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const abort = new AbortController();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 1_000), {
    now: clock.now,
    sleep: async (ms) => {
      await clock.sleep(ms);
      if (clock.waits.length === 3) abort.abort();
    },
  });

  const result = await tool.execute(
    "",
    { op: "sign_off", issue: "LEGION-208" },
    abort.signal,
    undefined,
    context
  );

  expect(result.isError).toBe(true);
  const [block] = result.content;
  expect(block?.type === "text" ? block.text : "").toBe(
    "sign_off: the Legion daemon was unreachable (Unable to connect. Is the computer able to access the url?) and the call was aborted; call it again once the daemon is back"
  );
  expect(clock.waits).toEqual([500, 1000, 2000]);
  expect(live.answered).toEqual([]);
});

test("a 409 from the daemon is its answer and is never retried", async () => {
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: {
      error: `the implementer reported commit ${COMMIT} for its previous phase of LEGION-208; write and commit this phase's handoff before completing`,
      code: "HANDOFF_NOT_NEW",
    },
  });
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, refusingOrigin(), 0),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBe(true);
  expect(result.content).toEqual([
    {
      type: "text",
      text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: the implementer reported commit ${COMMIT} for its previous phase of LEGION-208; write and commit this phase's handoff before completing`,
    },
  ]);
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(clock.waits).toEqual([]);
  expect(updates).toEqual([]);
  expect(completed).toEqual([]);
});

test("sign_off waits out a restarting daemon with a fresh grant on each attempt, and a 409 is not retried", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 2), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result).toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/signoff"]);
  expect(clock.waits).toEqual([500, 1000]);
  expect(updates).toHaveLength(1);
  expect(updates[0]).toContain("sign_off is being retried");

  const refusing = liveDaemon({
    path: "/legion/v1/signoff",
    status: 409,
    body: {
      error: "LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing",
      code: "SIGNOFF_OUTSIDE_ACCEPTING",
    },
  });
  const quiet = virtualClock();
  const refused = await run(
    toolFor("architect", restartingClient(refusing.url, refusingOrigin(), 0), quiet),
    { op: "sign_off", issue: "LEGION-208" }
  );
  expect(refused.result.isError).toBe(true);
  expect(refused.result.content[0]).toMatchObject({
    text: expect.stringContaining("409 SIGNOFF_OUTSIDE_ACCEPTING"),
  });
  expect(refusing.answered.filter((route) => route === "/legion/v1/signoff")).toHaveLength(1);
  expect(quiet.waits).toEqual([]);
});

test("a 503 from a proxy in front of the daemon is retried like a refused connection", async () => {
  let unavailable = 2;
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const route = new URL(request.url).pathname;
      answered.push(route);
      if (unavailable > 0) {
        unavailable -= 1;
        return new Response("<html>503 Service Unavailable</html>", { status: 503 });
      }
      return new Response(
        daemonFixture(route === "/legion/v1/grants" ? "grant.json" : "signoff.json")
      );
    },
  });
  stops.push(() => server.stop(true));
  const clock = virtualClock();
  const tool = toolFor(
    "architect",
    createLegionDaemonClient(`http://127.0.0.1:${server.port}`),
    clock
  );

  const { result } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBeUndefined();
  expect(answered).toEqual([
    "/legion/v1/grants",
    "/legion/v1/grants",
    "/legion/v1/grants",
    "/legion/v1/signoff",
  ]);
  expect(clock.waits).toEqual([500, 1000]);
});

test("a daemon that stays down past the ten-minute budget fails the call with the reason", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 1_000), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBe(true);
  const [block] = result.content;
  expect(block?.type === "text" ? block.text : "").toBe(
    "sign_off: the Legion daemon was unreachable (Unable to connect. Is the computer able to access the url?) and stayed so for 10 minutes; call it again once the daemon is back"
  );
  expect(live.answered).toEqual([]);
  expect(clock.waits.reduce((sum, wait) => sum + wait, 0)).toBe(10 * 60_000);
  expect(Math.max(...clock.waits)).toBe(30_000);
  expect(updates).toHaveLength(1);
});
