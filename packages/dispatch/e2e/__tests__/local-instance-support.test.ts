import { afterEach, describe, expect, test } from "bun:test";

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
