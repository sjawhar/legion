import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const probe = join(import.meta.dir, "dispatch-live-events.ts");
const servers: Server<undefined>[] = [];
const scratchDirectories: string[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
  for (const directory of scratchDirectories.splice(0))
    rmSync(directory, { force: true, recursive: true });
});

function scratch(): string {
  const directory = mkdtempSync(join(tmpdir(), "dispatch-live-events-"));
  scratchDirectories.push(directory);
  return directory;
}

async function run(
  ...args: string[]
): Promise<{ code: number; stderr: string; stdout: unknown[] }> {
  const child = Bun.spawn([process.execPath, probe, ...args], {
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return {
    code,
    stderr,
    stdout: stdout
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line)),
  };
}

test("the live event probe resumes after rotation and stops once an event matches", async () => {
  const upgrades: string[] = [];
  const subscriptions: unknown[] = [];
  let connections = 0;
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      upgrades.push(request.headers.get("x-dispatch-user") ?? "");
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        const frame = JSON.parse(String(raw));
        subscriptions.push(frame);
        connections += 1;
        if (connections === 1) {
          socket.send(JSON.stringify({ since: "5", sub: "events", type: "events_start" }));
          socket.send(
            JSON.stringify({
              event: { id: 6, type: "issue.updated" },
              id: "6",
              sub: "events",
              type: "event",
            })
          );
          socket.close(4001, "max age");
          return;
        }
        socket.send(JSON.stringify({ since: "6", sub: "events", type: "events_start" }));
        socket.send(
          JSON.stringify({
            event: { id: 7, note: "needle", type: "issue.updated" },
            id: "7",
            sub: "events",
            type: "event",
          })
        );
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--user",
    "alice@example.com",
    "--until",
    "needle",
    "--timeout",
    "5"
  );

  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  expect(upgrades).toEqual(["alice@example.com", "alice@example.com"]);
  expect(subscriptions).toEqual([
    { sub: "events", type: "subscribe", view: { kind: "events" } },
    { resume: { since: "6" }, sub: "events", type: "subscribe", view: { kind: "events" } },
  ]);
  expect(result.stdout).toEqual([
    { event: { id: 6, type: "issue.updated" } },
    { close: 4001, reason: "max age" },
    { resubscribed: "6" },
    { event: { id: 7, note: "needle", type: "issue.updated" } },
  ]);
});

test("the live event probe subscribes again from its cursor when Dispatch ends the feed", async () => {
  const subscriptions: unknown[] = [];
  let upgrades = 0;
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      upgrades += 1;
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        subscriptions.push(JSON.parse(String(raw)));
        if (subscriptions.length === 1) {
          socket.send(JSON.stringify({ since: "5", sub: "events", type: "events_start" }));
          socket.send(
            JSON.stringify({
              event: { id: 6, type: "issue.updated" },
              id: "6",
              sub: "events",
              type: "event",
            })
          );
          // The relay dropped the subscription: the socket stays open, silent for this feed.
          socket.send(JSON.stringify({ reason: "behind", sub: "events", type: "ended" }));
          return;
        }
        socket.send(JSON.stringify({ since: "6", sub: "events", type: "events_start" }));
        socket.send(
          JSON.stringify({
            event: { id: 7, note: "needle", type: "issue.updated" },
            id: "7",
            sub: "events",
            type: "event",
          })
        );
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--user",
    "alice@example.com",
    "--until",
    "needle",
    "--timeout",
    "2"
  );

  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  expect(upgrades).toBe(1);
  expect(subscriptions).toEqual([
    { sub: "events", type: "subscribe", view: { kind: "events" } },
    { resume: { since: "6" }, sub: "events", type: "subscribe", view: { kind: "events" } },
  ]);
  expect(result.stdout).toEqual([
    { event: { id: 6, type: "issue.updated" } },
    { ended: "behind", resubscribed: "6" },
    { event: { id: 7, note: "needle", type: "issue.updated" } },
  ]);
});

// A redial resumes after the cursor, so Dispatch replays nothing at or below it; a duplicate that
// arrives anyway (an older frame on the socket, or a replay after a redial) is noted, not written as
// an event, so the audits that count the event lines count it once, and no event moves the cursor
// back.
test("the live event probe writes a duplicate once and no event lowers its cursor", async () => {
  const subscriptions: unknown[] = [];
  const event = (id: number, extra: object = {}) =>
    JSON.stringify({
      event: { id, type: "issue.updated", ...extra },
      id: String(id),
      sub: "events",
      type: "event",
    });
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        subscriptions.push(JSON.parse(String(raw)));
        if (subscriptions.length === 1) {
          socket.send(JSON.stringify({ since: "5", sub: "events", type: "events_start" }));
          socket.send(event(6));
          socket.send(event(7));
          // An older event after a newer one: written once, and the resume stays at 7.
          socket.send(event(6));
          socket.close(1012, "service restart");
          return;
        }
        socket.send(JSON.stringify({ since: "7", sub: "events", type: "events_start" }));
        // The redial's replay repeats the last event before the next.
        socket.send(event(7));
        socket.send(event(8, { note: "needle" }));
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--user",
    "alice@example.com",
    "--until",
    "needle",
    "--timeout",
    "5"
  );

  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  expect(subscriptions).toEqual([
    { sub: "events", type: "subscribe", view: { kind: "events" } },
    { resume: { since: "7" }, sub: "events", type: "subscribe", view: { kind: "events" } },
  ]);
  expect(result.stdout).toEqual([
    { event: { id: 6, type: "issue.updated" } },
    { event: { id: 7, type: "issue.updated" } },
    { duplicate: "6" },
    { close: 1012, reason: "service restart" },
    { resubscribed: "7" },
    { duplicate: "7" },
    { event: { id: 8, note: "needle", type: "issue.updated" } },
  ]);
  // What the stage proofs' audits count: each event's line, once.
  const counted = result.stdout.flatMap((line) =>
    typeof line === "object" && line !== null && "event" in line ? [line.event] : []
  );
  expect(counted).toHaveLength(3);
});

// Dispatch starts a feed at what it was asked or below it, never above: below a `--since` past its
// head (clamped to the head), and below the resume a redial asks once its event log was reset. The
// probe takes the start as its cursor every time, so the events after it are written as events, not
// as duplicates of ids the probe asked past.
test("the live event probe takes every events_start as its cursor, below what it asked included", async () => {
  const subscriptions: unknown[] = [];
  const event = (id: number, extra: object = {}) =>
    JSON.stringify({
      event: { id, type: "issue.updated", ...extra },
      id: String(id),
      sub: "events",
      type: "event",
    });
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        subscriptions.push(JSON.parse(String(raw)));
        if (subscriptions.length === 1) {
          // Asked to start after 100, past the head: the feed starts at the head, 5.
          socket.send(JSON.stringify({ since: "5", sub: "events", type: "events_start" }));
          socket.send(event(6));
          socket.send(event(7));
          socket.close(1012, "service restart");
          return;
        }
        // The log was reset across the restart: the resume after 7 starts at the new head, 2.
        socket.send(JSON.stringify({ since: "2", sub: "events", type: "events_start" }));
        socket.send(event(3));
        socket.send(event(3));
        socket.send(event(4, { note: "needle" }));
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--user",
    "alice@example.com",
    "--since",
    "100",
    "--until",
    "needle",
    "--timeout",
    "2"
  );

  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  expect(subscriptions).toEqual([
    { resume: { since: "100" }, sub: "events", type: "subscribe", view: { kind: "events" } },
    { resume: { since: "7" }, sub: "events", type: "subscribe", view: { kind: "events" } },
  ]);
  expect(result.stdout).toEqual([
    { event: { id: 6, type: "issue.updated" } },
    { event: { id: 7, type: "issue.updated" } },
    { close: 1012, reason: "service restart" },
    { resubscribed: "7" },
    { event: { id: 3, type: "issue.updated" } },
    { duplicate: "3" },
    { event: { id: 4, note: "needle", type: "issue.updated" } },
  ]);
});

test("the live event probe prints Dispatch's refusal of the feed and exits 1", async () => {
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket) {
        socket.send(
          JSON.stringify({
            code: "STREAM_UNAVAILABLE",
            message: "no feed",
            sub: "events",
            type: "error",
          })
        );
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--user",
    "alice@example.com",
    "--timeout",
    "2"
  );

  expect(result.code).toBe(1);
  expect(result.stderr).toBe(
    "dispatch-live-events: Dispatch refused the events subscription: STREAM_UNAVAILABLE: no feed\n"
  );
  expect(result.stdout).toEqual([]);
});

test("the live event probe authenticates with a bearer file and exits at its timeout", async () => {
  const directory = scratch();
  const bearerFile = join(directory, "bearer");
  writeFileSync(bearerFile, "test-bearer\n", { mode: 0o600 });
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live")
        return new Response("not found", { status: 404 });
      if (request.headers.get("authorization") !== "Bearer test-bearer") {
        return new Response("forbidden", { status: 403 });
      }
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket) {
        socket.send(JSON.stringify({ since: "0", sub: "events", type: "events_start" }));
      },
    },
  });
  servers.push(server);

  const result = await run(
    "--url",
    `http://127.0.0.1:${server.port}`,
    "--bearer-file",
    bearerFile,
    "--timeout",
    "0.1"
  );

  expect(result.code).toBe(1);
  expect(result.stderr).toBe("");
  expect(result.stdout).toEqual([]);
});
