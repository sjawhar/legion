import { afterEach, describe, expect, test } from "bun:test";
import type { Server, ServerWebSocket } from "bun";

import { databaseUrlRefusal, Interrupted, Supervisor, shellQuote } from "../local-instance-support";

/** True while any process of group `pgid` is alive: signal 0 to the group reaches every member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Polls `ready` until it holds or `ms` pass. Used only for what another process does — its write
 *  to a file, or the kernel reaping a killed process, which still answers `kill(pid, 0)` as a
 *  zombie until its parent or init collects it — with no event in this process to await. */
async function eventually(ready: () => boolean | Promise<boolean>, ms = 3_000): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await ready()) return true;
    await Bun.sleep(25);
  }
  return ready();
}

/** The pid a real child process writes to `path`. */
async function pidFrom(path: string): Promise<number> {
  const read = () =>
    Bun.file(path)
      .text()
      .catch(() => "");
  if (!(await eventually(async () => /^\d+\s*$/.test(await read())))) {
    throw new Error(`no pid in ${path}`);
  }
  return Number(await read());
}

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/** One macrotask turn: the runtime reports an unhandled rejection only after the microtask queue
 *  drains, so a test that waits for that report yields one turn rather than a guessed duration. */
const nextTurn = () => new Promise<void>((resolve) => setImmediate(resolve));

const supervisors: Supervisor[] = [];
function supervisor(): Supervisor {
  const made = new Supervisor();
  supervisors.push(made);
  return made;
}
afterEach(async () => {
  await Promise.all(supervisors.splice(0).map((made) => made.stop(100)));
});

describe("Supervisor.stop", () => {
  test("ends every process group it started, a grandchild included", async () => {
    const pidFile = `${process.env.TMPDIR ?? "/tmp"}/local-instance-support-${process.pid}.pid`;
    const made = supervisor();
    const leader = made.start(
      "leader",
      ["bash", "-c", `sleep 60 & echo $! > ${pidFile}; wait`],
      "/"
    );
    const quiet = made.start("quiet", ["sleep", "60"], "/");
    const grandchild = await pidFrom(pidFile);
    expect(groupAlive(leader.child.pid)).toBe(true);
    expect(groupAlive(quiet.child.pid)).toBe(true);

    await made.stop();

    expect(await eventually(() => !groupAlive(leader.child.pid))).toBe(true);
    expect(await eventually(() => !groupAlive(quiet.child.pid))).toBe(true);
    expect(await eventually(() => !processAlive(grandchild))).toBe(true);
  });

  test("returns as soon as the leaders exit, not after the force-kill delay", async () => {
    const made = supervisor();
    made.start("quiet", ["sleep", "60"], "/");
    const started = Date.now();
    await made.stop(30_000);
    expect(Date.now() - started).toBeLessThan(5_000);
  });

  test("ends a foreground step that a stop interrupts", async () => {
    const made = supervisor();
    const step = made.run(["sleep", "60"], "/");
    made.interrupt();
    await expect(step).rejects.toBeInstanceOf(Interrupted);
    const started = Date.now();
    await made.stop();
    expect(Date.now() - started).toBeLessThan(5_000);
  });
});

describe("Supervisor.untilInterrupted", () => {
  test("handles the step it was given after an interrupt, so nothing goes unhandled", async () => {
    const made = supervisor();
    made.interrupt();
    const unhandled: unknown[] = [];
    const record = (reason: unknown) => {
      unhandled.push(reason);
    };
    process.on("unhandledRejection", record);
    try {
      const { promise: step, reject } = Promise.withResolvers<never>();
      await expect(made.untilInterrupted(step)).rejects.toBeInstanceOf(Interrupted);
      reject(new Error("the step fails after the interrupt"));
      await nextTurn();
      await nextTurn();
    } finally {
      process.off("unhandledRejection", record);
    }
    expect(unhandled).toEqual([]);
  });

  test("answers the step's own result when no stop is under way", async () => {
    expect(await supervisor().untilInterrupted(Promise.resolve(7))).toBe(7);
  });
});

describe("databaseUrlRefusal", () => {
  test.each([
    "postgres://postgres@127.0.0.1:55439/dispatch_local?sslmode=disable",
    "postgres://u@LOCALHOST/d",
    "postgres://u@[::1]:5432/d",
    "postgres:///d?host=/var/run/postgresql",
    "postgresql://%2Fvar%2Frun%2Fpostgresql/d",
    "postgres:///d",
  ])("accepts %s", (url) => {
    expect(databaseUrlRefusal(url)).toBeUndefined();
  });

  test.each([
    ["postgres://u@db.example.com/x", "not db.example.com"],
    ["postgres://u@127.0.0.1/x?host=db.example.com", "not db.example.com"],
    ["postgres:///d?host=127.0.0.1,db.example.com", "not db.example.com"],
    ["postgres://u@127.0.0.1:5432,db.example.com:5432/d", "a single postgres:// URL"],
    ["host=db.example.com dbname=d", "a single postgres:// URL"],
  ])("refuses %s, saying %s", (url, said) => {
    expect(databaseUrlRefusal(url)).toContain(said);
  });
});

/** The quoting cases dev-postgres.sh's `quote` is held to as well, so the two copies of the rule
 *  cannot drift apart. */
const quoteCases = (await Bun.file(
  new URL("../../../envoy/scripts/fixtures/shell-quote.json", import.meta.url)
).json()) as { word: string; quoted: string }[];

describe("shellQuote", () => {
  test("the shared table has cases", () => {
    expect(quoteCases.length).toBeGreaterThan(0);
  });

  test.each(quoteCases)("quotes $word as $quoted, which reads back whole in bash", async (row) => {
    expect(shellQuote(row.word)).toBe(row.quoted);
    const echoed = await Bun.$`bash -c ${`printf %s ${shellQuote(row.word)}`}`.text();
    expect(echoed).toBe(row.word);
  });
});

// `agentViewRefusal` opens a real live socket, and this package's test preload replaces
// `WebSocket` with a stub for the web unit tests (`web/src/__tests__/setup.ts`), which never
// connects. So these rows run it in a child process, as `bun run dispatch:local` does, against a
// stand-in Dispatch this process serves.
const supportModule = new URL("../local-instance-support.ts", import.meta.url).pathname;

describe("agentViewRefusal", () => {
  const SESSION = "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01";
  const servers: Server<undefined>[] = [];
  afterEach(() => {
    for (const server of servers.splice(0)) server.stop(true);
  });

  /** The answer `agentViewRefusal` gives for `sessionId` against `baseUrl`, read from a process
   *  with the runtime's own WebSocket: its refusal, or null for none. */
  async function refusalFrom(
    baseUrl: string,
    headers: Record<string, string>,
    sessionId: string
  ): Promise<string | null> {
    const code =
      `const { agentViewRefusal } = await import(${JSON.stringify(supportModule)});\n` +
      `const answer = await agentViewRefusal(${JSON.stringify(baseUrl)}, ` +
      `${JSON.stringify(headers)}, ${JSON.stringify(sessionId)});\n` +
      "process.stdout.write(JSON.stringify(answer ?? null));\n";
    const child = Bun.spawn([process.execPath, "-e", code], { stderr: "pipe", stdout: "pipe" });
    const [code_, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    if (code_ !== 0) throw new Error(`the child exited ${code_}: ${stderr}`);
    return JSON.parse(stdout) as string | null;
  }

  /** A Dispatch whose live socket answers the agent view's subscribe with `answer`, recording the
   *  headers of the upgrade and every frame it was sent. */
  function fakeDispatch(answer: (socket: ServerWebSocket<undefined>, sub: string) => void): {
    readonly url: string;
    readonly cookies: (string | null)[];
    readonly received: unknown[];
  } {
    const cookies: (string | null)[] = [];
    const received: unknown[] = [];
    const server = Bun.serve({
      fetch(request, srv) {
        if (new URL(request.url).pathname !== "/ws/live") {
          return new Response("not found", { status: 404 });
        }
        cookies.push(request.headers.get("cookie"));
        return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
      },
      hostname: "127.0.0.1",
      port: 0,
      websocket: {
        message(socket, raw) {
          const frame = JSON.parse(String(raw)) as { readonly sub: string };
          received.push(frame);
          answer(socket, frame.sub);
        },
      },
    });
    servers.push(server);
    return { cookies, received, url: `http://127.0.0.1:${server.port}` };
  }

  test("an agent view Dispatch carries is no refusal, and it subscribes as the page does", async () => {
    const dispatch = fakeDispatch((socket, sub) =>
      socket.send(
        JSON.stringify({
          data: { responding: false, v: 1 },
          event: "responder",
          sub,
          type: "agent",
        })
      )
    );
    expect(await refusalFrom(dispatch.url, { Cookie: "dsession=alice" }, SESSION)).toBeNull();
    expect(dispatch.cookies).toEqual(["dsession=alice"]);
    expect(dispatch.received).toEqual([
      {
        sub: "local-instance-smoke",
        type: "subscribe",
        view: { kind: "agent", session_id: SESSION },
      },
    ]);
  });

  test("a session id Dispatch refuses is the refusal, named", async () => {
    const dispatch = fakeDispatch((socket, sub) =>
      socket.send(
        JSON.stringify({
          code: "SESSION_ID_INPUT",
          message: "session_id must be a session uuid",
          sub,
          type: "error",
        })
      )
    );
    const refusal = await refusalFrom(dispatch.url, {}, "planner-session");
    expect(refusal).toContain("SESSION_ID_INPUT");
    expect(refusal).toContain("session_id must be a session uuid");
  });

  test("a socket that closes before any answer says so", async () => {
    const dispatch = fakeDispatch((socket) => socket.close(1011, "internal error"));
    expect(await refusalFrom(dispatch.url, {}, SESSION)).toBe(
      "the live socket closed first (1011 internal error)"
    );
  });

  test("a live socket that cannot be opened at all says so", async () => {
    const dispatch = fakeDispatch(() => undefined);
    const port = new URL(dispatch.url).port;
    for (const server of servers.splice(0)) server.stop(true);
    expect(await refusalFrom(`http://127.0.0.1:${port}`, {}, SESSION)).toBe(
      "the live socket could not be opened"
    );
  });
});
