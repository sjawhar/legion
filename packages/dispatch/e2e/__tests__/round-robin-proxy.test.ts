import { afterAll, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Server, Subprocess } from "bun";

import type { DispatchProcess } from "../harness-processes";
import type { ProxiedRequest, ProxiedSocket } from "../round-robin-proxy";

// The test preload (web/src/__tests__/setup.ts) puts happy-dom's fetch, Response and Headers and a
// WebSocket stub on globalThis. So the proxy runs as the harness runs it, a `bun` process of its
// own; this file answers as the two Dispatch processes with Bun's own Response, asks with
// `Bun.fetch`, and opens websockets with Bun's `ws` client, none of which the preload replaces.
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;
const { WebSocket: WsClient } = require("ws") as {
  WebSocket: new (
    url: string,
    options: { readonly headers: Record<string, string> }
  ) => {
    close(code?: number): void;
    on(event: "close", listener: (code: number) => void): void;
    on(event: "message", listener: (data: Buffer) => void): void;
    send(data: string): void;
  };
};

const proxyScript = fileURLToPath(new URL("../round-robin-proxy.ts", import.meta.url));
const harnessPortsModule = fileURLToPath(new URL("../harness-ports.ts", import.meta.url));
const runServer = fileURLToPath(new URL("../run-server.sh", import.meta.url));
// run-server.sh runs the server from packages/envoy, without the trailing slash `pwd` drops.
const envoyDirectory = fileURLToPath(new URL("../../../envoy", import.meta.url));

/** What an upstream saw of one request. */
interface Seen {
  readonly body: string;
  readonly cookie: string | null;
  readonly host: string | null;
  readonly method: string;
  readonly origin: string | null;
  readonly path: string;
  readonly pin: string | null;
}

/** A Bun server standing in for one Dispatch process: it records every request it is sent and
 *  answers with its own name; a test hook answers the status the test sets, `/events` is an event
 *  stream the test writes to, and `/socket` a websocket that names the process, echoes, and
 *  closes with 1012 when asked to. */
function startUpstream(name: DispatchProcess) {
  const encoder = new TextEncoder();
  const upstream = {
    hookStatus: 200,
    seen: [] as Seen[],
    server: undefined as unknown as Server<undefined>,
    /** The open event stream's writer and its end, once a request opened it. */
    stream: undefined as { write: (text: string) => void; end: () => void } | undefined,
  };
  upstream.server = Bun.serve({
    hostname: "127.0.0.1",
    idleTimeout: 0,
    port: 0,
    async fetch(request, server) {
      const url = new URL(request.url);
      upstream.seen.push({
        body: await request.text(),
        cookie: request.headers.get("cookie"),
        host: request.headers.get("host"),
        method: request.method,
        origin: request.headers.get("origin"),
        path: `${url.pathname}${url.search}`,
        pin: request.headers.get("x-dispatch-e2e-process"),
      });
      if (url.pathname === "/socket") {
        return server.upgrade(request, { data: undefined })
          ? undefined
          : new NativeResponse("no upgrade", { status: 400 });
      }
      if (url.pathname.includes("/_test/")) {
        return new NativeResponse(`hook on ${name}`, { status: upstream.hookStatus });
      }
      if (url.pathname === "/events") {
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(encoder.encode(`data: opened on ${name}\n\n`));
            upstream.stream = {
              end: () => controller.close(),
              write: (text) => controller.enqueue(encoder.encode(text)),
            };
          },
        });
        return new NativeResponse(body, { headers: { "Content-Type": "text/event-stream" } });
      }
      return new NativeResponse(name);
    },
    websocket: {
      message(ws, message) {
        if (message === "close 1012") ws.close(1012, "restarting");
        else ws.send(`${name} echoes ${String(message)}`);
      },
      open(ws) {
        ws.send(`opened on ${name}`);
      },
    },
  });
  return upstream;
}

/** A port nothing listens on at the moment it is asked for. */
function freePort(): number {
  const probe = Bun.serve({ fetch: () => new NativeResponse(), hostname: "127.0.0.1", port: 0 });
  const port = Number(probe.url.port);
  probe.stop(true);
  return port;
}

const a = startUpstream("a");
const b = startUpstream("b");
const proxyPort = freePort();
const origin = `http://127.0.0.1:${proxyPort}`;
let proxy: Subprocess<"ignore", "pipe", "inherit">;

beforeAll(async () => {
  proxy = Bun.spawn(["bun", proxyScript], {
    env: {
      ...process.env,
      DISPATCH_E2E_PORT: String(proxyPort),
      DISPATCH_E2E_PORT_A: String(a.server.port),
      DISPATCH_E2E_PORT_B: String(b.server.port),
      DISPATCH_E2E_PROCESSES: "2",
    },
    stderr: "inherit",
    stdout: "pipe",
  });
  // The proxy logs one line once it listens.
  const reader = proxy.stdout.getReader();
  let logged = "";
  while (!logged.includes("round-robin proxy on")) {
    const { done, value } = await reader.read();
    if (done) throw new Error(`the proxy exited before it listened: ${logged}`);
    logged += new TextDecoder().decode(value);
  }
  reader.releaseLock();
});

afterAll(async () => {
  proxy.kill();
  await proxy.exited;
  a.server.stop(true);
  b.server.stop(true);
});

beforeEach(async () => {
  expect((await Bun.fetch(`${origin}/_proxy/reset`, { method: "POST" })).status).toBe(204);
  a.seen.length = 0;
  b.seen.length = 0;
  a.hookStatus = 200;
  b.hookStatus = 200;
});

const get = (path: string, headers: Record<string, string> = {}) =>
  Bun.fetch(`${origin}${path}`, { headers });
const answeredBy = async (path: string, headers: Record<string, string> = {}) =>
  (await get(path, headers)).text();
const proxyRequests = async () =>
  (await (await get("/_proxy/requests")).json()) as ProxiedRequest[];
const proxySockets = async () => (await (await get("/_proxy/sockets")).json()) as ProxiedSocket[];

/** A websocket to the proxy, the messages it has received, and its close code once it closes. */
function openSocket(headers: Record<string, string>) {
  const socket = new WsClient(`ws://127.0.0.1:${proxyPort}/socket`, { headers });
  const messages: string[] = [];
  const waiters: { count: number; resolve: () => void }[] = [];
  socket.on("message", (data) => {
    messages.push(String(data));
    for (const waiter of waiters) if (messages.length >= waiter.count) waiter.resolve();
  });
  const closed = new Promise<number>((resolve) => socket.on("close", resolve));
  const received = (count: number) =>
    new Promise<string[]>((resolve) => {
      if (messages.length >= count) resolve(messages);
      else waiters.push({ count, resolve: () => resolve(messages) });
    });
  return { closed, received, socket };
}

describe("round-robin proxy", () => {
  test("alternates requests between A and B, forwarding Host and Origin unchanged", async () => {
    const answers = [];
    for (let index = 0; index < 4; index++) {
      answers.push(await answeredBy("/who?n=1", { Origin: origin }));
    }
    expect(answers).toEqual(["a", "b", "a", "b"]);
    expect(await proxyRequests()).toEqual([
      { method: "GET", path: "/who?n=1", upstream: "a" },
      { method: "GET", path: "/who?n=1", upstream: "b" },
      { method: "GET", path: "/who?n=1", upstream: "a" },
      { method: "GET", path: "/who?n=1", upstream: "b" },
    ]);
    for (const seen of [...a.seen, ...b.seen]) {
      expect([seen.host, seen.origin]).toEqual([`127.0.0.1:${proxyPort}`, origin]);
    }
  });

  test("a pin header beats a pin cookie, a cookie pins on its own, and pins take no turn", async () => {
    expect(await answeredBy("/who", { Cookie: "x=1; dispatch-e2e-process=b" })).toBe("b");
    expect(await answeredBy("/who", { Cookie: "dispatch-e2e-process=b" })).toBe("b");
    expect(
      await answeredBy("/who", { Cookie: "dispatch-e2e-process=b", "X-Dispatch-E2E-Process": "a" })
    ).toBe("a");
    expect(await answeredBy("/who", { "X-Dispatch-E2E-Process": "b" })).toBe("b");
    // The alternation still starts at A: no pinned request took a turn.
    expect(await answeredBy("/who")).toBe("a");
    expect(await answeredBy("/who")).toBe("b");
    // The pin header is the proxy's and stops there; the cookie reaches the server as sent.
    expect([...a.seen, ...b.seen].map((seen) => seen.pin)).toEqual([
      null,
      null,
      null,
      null,
      null,
      null,
    ]);
    expect(b.seen[0]?.cookie).toBe("x=1; dispatch-e2e-process=b");
  });

  test("a pin that names no process is refused, never read as no pin", async () => {
    const refused: Record<string, string>[] = [
      { "X-Dispatch-E2E-Process": "c" },
      { Cookie: "dispatch-e2e-process=A" },
      { Cookie: "dispatch-e2e-process=b", "X-Dispatch-E2E-Process": "" },
      { Cookie: "dispatch-e2e-page=c" },
      // A pin of the other kind than the request's is refused too.
      { Cookie: "dispatch-e2e-api=b; dispatch-e2e-page=" },
    ];
    for (const headers of refused) {
      const response = await get("/who", headers);
      expect(response.status).toBe(400);
      expect(await response.json()).toMatchObject({
        error: expect.stringContaining('the processes are "a" and "b"'),
      });
    }
    expect([...a.seen, ...b.seen]).toEqual([]);
  });

  test("a page pin and an API pin send a context's page to one process and the rest to the other", async () => {
    const split = { Cookie: "dispatch-e2e-page=b; dispatch-e2e-api=a" };
    // The page: a navigation, index.html and every asset.
    expect(await answeredBy("/issues/CORE-1", { ...split, "Sec-Fetch-Mode": "navigate" })).toBe(
      "b"
    );
    expect(await answeredBy("/index.html", split)).toBe("b");
    expect(await answeredBy("/assets/index-abc.js", split)).toBe("b");
    // Everything else: the API, sign-in (a navigation included), a websocket.
    expect(await answeredBy("/api/v1/issues", split)).toBe("a");
    expect(
      await answeredBy("/auth/_dev/signin?login=alice", { ...split, "Sec-Fetch-Mode": "navigate" })
    ).toBe("a");
    expect(await answeredBy("/favicon.svg", split)).toBe("a");
    const socket = openSocket(split);
    expect(await socket.received(1)).toEqual(["opened on a"]);
    socket.socket.close();
    await socket.closed;
    // A pin of the request's kind beats the context pin, which still pins the other kind; the
    // header beats both.
    const both = "dispatch-e2e-process=a; dispatch-e2e-page=b";
    expect(await answeredBy("/assets/index-abc.js", { Cookie: both })).toBe("b");
    expect(await answeredBy("/api/v1/issues", { Cookie: both })).toBe("a");
    expect(await answeredBy("/api/v1/issues", { Cookie: "dispatch-e2e-api=b" })).toBe("b");
    expect(
      await answeredBy("/assets/index-abc.js", { ...split, "X-Dispatch-E2E-Process": "a" })
    ).toBe("a");
    // None of them took a turn.
    expect(await answeredBy("/who")).toBe("a");
    expect(await answeredBy("/who")).toBe("b");
  });

  test("a /_test/ request goes to both processes and answers A's, or a non-2xx of either", async () => {
    const hook = (headers: Record<string, string> = {}) =>
      Bun.fetch(`${origin}/api/v1/live/_test/disconnect`, {
        body: "payload",
        headers,
        method: "POST",
      });
    const pinned = await hook({ "X-Dispatch-E2E-Process": "b" });
    expect([pinned.status, await pinned.text()]).toEqual([200, "hook on a"]);
    for (const upstream of [a, b]) {
      expect(upstream.seen.map((seen) => [seen.method, seen.path, seen.body])).toEqual([
        ["POST", "/api/v1/live/_test/disconnect", "payload"],
      ]);
    }
    expect((await proxyRequests()).map((entry) => entry.upstream)).toEqual(["a", "b"]);

    b.hookStatus = 500;
    const bFailed = await hook();
    expect([bFailed.status, await bFailed.text()]).toEqual([500, "hook on b"]);
    a.hookStatus = 503;
    const bothFailed = await hook();
    expect([bothFailed.status, await bothFailed.text()]).toEqual([503, "hook on a"]);
    b.hookStatus = 200;
    a.hookStatus = 404;
    const aFailed = await hook();
    expect([aFailed.status, await aFailed.text()]).toEqual([404, "hook on a"]);
    // A fan-out took no turn either.
    expect(await answeredBy("/who")).toBe("a");
  });

  test("a /_proxy/ path is answered by the proxy and never forwarded", async () => {
    expect(await answeredBy("/who")).toBe("a");
    expect((await get("/_proxy/elsewhere")).status).toBe(404);
    expect((await get("/api/v1/_proxy/requests")).status).toBe(404);
    expect((await Bun.fetch(`${origin}/_proxy/requests`, { method: "DELETE" })).status).toBe(405);
    expect((await get("/_proxy/reset")).status).toBe(405);
    expect(await proxyRequests()).toEqual([{ method: "GET", path: "/who", upstream: "a" }]);
    expect([...a.seen, ...b.seen].map((seen) => seen.path)).toEqual(["/who"]);

    expect((await Bun.fetch(`${origin}/_proxy/reset`, { method: "POST" })).status).toBe(204);
    expect(await proxyRequests()).toEqual([]);
    expect(await answeredBy("/who")).toBe("a");
  });

  test("relays a websocket to its pinned process both ways, and logs it", async () => {
    const pinned = openSocket({ Cookie: "dispatch-e2e-process=b" });
    expect(await pinned.received(1)).toEqual(["opened on b"]);
    pinned.socket.send("hello");
    expect(await pinned.received(2)).toEqual(["opened on b", "b echoes hello"]);
    expect(await proxySockets()).toEqual([
      {
        id: expect.any(Number),
        open: true,
        openedAt: expect.any(Number),
        path: "/socket",
        upstream: "b",
      },
    ]);
    expect(b.seen.at(-1)?.host).toBe(`127.0.0.1:${proxyPort}`);
    pinned.socket.close();
    await pinned.closed;
    // The proxy marks the row when its own side of the socket closes, of which this process sees
    // no event; its log is read until it says so.
    const deadline = Date.now() + 3_000;
    while ((await proxySockets())[0]?.open !== false && Date.now() < deadline) await Bun.sleep(25);
    expect((await proxySockets())[0]?.open).toBe(false);

    // Unpinned upgrades alternate on a turn of their own, logged in open order, and a server's
    // close code reaches the browser.
    const first = openSocket({});
    expect(await first.received(1)).toEqual(["opened on a"]);
    const second = openSocket({ "X-Dispatch-E2E-Process": "a" });
    expect(await second.received(1)).toEqual(["opened on a"]);
    const third = openSocket({});
    expect(await third.received(1)).toEqual(["opened on b"]);
    const logged = await proxySockets();
    expect(logged.map((entry) => [entry.upstream, entry.open])).toEqual([
      ["b", false],
      ["a", true],
      ["a", true],
      ["b", true],
    ]);
    expect(logged.map((entry) => entry.id)).toEqual(
      logged.map((entry) => entry.id).sort((left, right) => left - right)
    );
    first.socket.send("close 1012");
    expect(await first.closed).toBe(1012);
    second.socket.close();
    third.socket.close();
    await Promise.all([second.closed, third.closed]);
  });

  test("streams an event stream from its pinned process as it is written", async () => {
    const response = await get("/events", { "X-Dispatch-E2E-Process": "b" });
    expect(response.headers.get("content-type")).toBe("text/event-stream");
    const reader = (response.body as ReadableStream<Uint8Array>).getReader();
    const decoder = new TextDecoder();
    const next = async () => decoder.decode((await reader.read()).value);
    expect(await next()).toBe("data: opened on b\n\n");
    // The second event is written only once the first has come through the proxy, so a proxy
    // that buffered the stream would never answer this read.
    b.stream?.write("data: second\n\n");
    expect(await next()).toBe("data: second\n\n");
    b.stream?.end();
    expect((await reader.read()).done).toBe(true);
    expect(a.seen).toEqual([]);
  });
});

describe("DISPATCH_E2E_PROCESSES", () => {
  const run = async (argv: string[], env: Record<string, string>) => {
    const child = Bun.spawn(argv, {
      env: { PATH: process.env.PATH ?? "", ...env },
      stderr: "pipe",
      stdout: "pipe",
    });
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new NativeResponse(child.stdout).text(),
      new NativeResponse(child.stderr).text(),
    ]);
    return { code, stderr, stdout };
  };
  // A process of its own per import: the refusal is the module's evaluation, which runs once.
  const importPorts = (env: Record<string, string>) =>
    run(["bun", "-e", `await import(${JSON.stringify(harnessPortsModule)})`], env);

  test("every harness importer refuses a value other than 1, 2 or unset, naming those", async () => {
    for (const value of ["3", "two", " 2", "0"]) {
      const { code, stderr } = await importPorts({ DISPATCH_E2E_PROCESSES: value });
      expect(code).not.toBe(0);
      expect(stderr).toContain(
        `DISPATCH_E2E_PROCESSES must be "1", "2" or unset, not ${JSON.stringify(value)}`
      );
    }
    for (const value of ["", "1", "2"]) {
      expect((await importPorts({ DISPATCH_E2E_PROCESSES: value })).code).toBe(0);
    }
  });

  test("run-server.sh refuses it too, and two processes without one cookie key", async () => {
    const database = { DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened" };
    const unknown = await run(["bash", runServer], { ...database, DISPATCH_E2E_PROCESSES: "3" });
    expect(unknown.code).toBe(1);
    expect(unknown.stderr).toContain("DISPATCH_E2E_PROCESSES must be 1, 2 or unset, not '3'");
    const keyless = await run(["bash", runServer], { ...database, DISPATCH_E2E_PROCESSES: "2" });
    expect(keyless.code).not.toBe(0);
    expect(keyless.stderr).toContain("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY must give both processes");
  });

  test("run-server.sh runs DISPATCH_E2E_SERVER_BINARY, when set, as it runs the one it builds", async () => {
    const directory = mkdtempSync(join(tmpdir(), "run-server-binary-"));
    try {
      const binary = join(directory, "envoy-dispatch");
      // The binary reports the directory it runs in, the settings run-server.sh sweeps the
      // environment down to, and how it was told to listen: the socket it inherited, or the port
      // it was handed.
      writeFileSync(
        binary,
        [
          "#!/usr/bin/env bash",
          'fd="$DISPATCH_LISTEN_FD"',
          "socket=none",
          'if [ -n "$fd" ] && [ -S "/dev/fd/$fd" ]; then',
          "  socket=listening",
          "fi",
          '[ -n "$fd" ] || fd=none',
          'echo "given binary in $PWD with DISPATCH_TEST_HOOKS=$DISPATCH_TEST_HOOKS' +
            " DISPATCH_WEB_DIST=$DISPATCH_WEB_DIST DISPATCH_LISTEN_FD=$fd socket=$socket" +
            ' DISPATCH_PORT=$DISPATCH_PORT DISPATCH_SERVER_URL=$DISPATCH_SERVER_URL"',
          "",
        ].join("\n"),
        { mode: 0o755 }
      );
      // run-server.sh reads the fakes' ports, and binds the port it is given.
      const environment = {
        DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
        DISPATCH_E2E_PORT: "8777",
        DISPATCH_E2E_SERVER_BINARY: binary,
        DISPATCH_E2E_SERVER_WEB_DIST: "/given/web",
        FAKE_BROKER_PORT: "9024",
        FAKE_ENVOY_PORT: "9021",
        FAKE_GITHUB_PORT: "9022",
      };
      const { code, stdout } = await run(["bash", runServer], environment);
      expect(code).toBe(0);
      expect(stdout).toBe(
        `given binary in ${envoyDirectory} with DISPATCH_TEST_HOOKS=1 DISPATCH_WEB_DIST=/given/web ` +
          "DISPATCH_LISTEN_FD=none socket=none DISPATCH_PORT=8777 " +
          "DISPATCH_SERVER_URL=http://127.0.0.1:8777\n"
      );

      const missing = await run(["bash", runServer], {
        ...environment,
        DISPATCH_E2E_SERVER_BINARY: join(directory, "absent"),
      });
      expect(missing.code).not.toBe(0);
      expect(missing.stderr).toContain(
        `DISPATCH_E2E_SERVER_BINARY names ${join(directory, "absent")}, which is not an executable file`
      );

      // With no port of its own the given binary serves on the socket held for it, as a build
      // from this tree does: held-port.py hands it the listening socket as DISPATCH_LISTEN_FD and
      // names its port. A previous release adopts it (legion-envoy-v20.1.0 and later), so a
      // two-build run holds B's port like A's and sets none.
      const { DISPATCH_E2E_PORT: _, ...portless } = environment;
      const held = await run(["bash", runServer], portless);
      expect(held.code).toBe(0);
      const port = /DISPATCH_PORT=([1-9][0-9]*) /.exec(held.stdout)?.[1];
      expect(port).toBeDefined();
      expect(held.stdout).toBe(
        `given binary in ${envoyDirectory} with DISPATCH_TEST_HOOKS=1 DISPATCH_WEB_DIST=/given/web ` +
          `DISPATCH_LISTEN_FD=3 socket=listening DISPATCH_PORT=${port} ` +
          `DISPATCH_SERVER_URL=http://127.0.0.1:${port}\n`
      );
    } finally {
      rmSync(directory, { force: true, recursive: true });
    }
  });

  test("A's and B's ports join the collision check in two-process mode only", async () => {
    // The ports resolve, and a collision is refused, when `harnessPorts` is called, not on import.
    const resolve = [
      "bun",
      "-e",
      `(await import(${JSON.stringify(harnessPortsModule)})).harnessPorts()`,
    ];
    const clash = { DISPATCH_E2E_PORT_A: "9021", FAKE_ENVOY_PORT: "9021" };
    const twoProcesses = await run(resolve, { ...clash, DISPATCH_E2E_PROCESSES: "2" });
    expect(twoProcesses.code).not.toBe(0);
    expect(twoProcesses.stderr).toContain("DISPATCH_E2E_PORT_A and FAKE_ENVOY_PORT name port 9021");
    expect((await run(resolve, clash)).code).toBe(0);
  });
});
