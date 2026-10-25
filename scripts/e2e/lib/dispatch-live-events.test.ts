import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const probe = join(import.meta.dir, "dispatch-live-events.ts");
const servers: Server<undefined>[] = [];
const scratchDirectories: string[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
  for (const directory of scratchDirectories.splice(0)) rmSync(directory, { force: true, recursive: true });
});

function scratch(): string {
  const directory = mkdtempSync(join(tmpdir(), "dispatch-live-events-"));
  scratchDirectories.push(directory);
  return directory;
}

async function run(...args: string[]): Promise<{ code: number; stderr: string; stdout: unknown[] }> {
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
      if (new URL(request.url).pathname !== "/ws/live") return new Response("not found", { status: 404 });
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

test("the live event probe authenticates with a bearer file and exits at its timeout", async () => {
  const directory = scratch();
  const bearerFile = join(directory, "bearer");
  writeFileSync(bearerFile, "test-bearer\n", { mode: 0o600 });
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live") return new Response("not found", { status: 404 });
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
