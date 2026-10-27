import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { type FakeBuilder, fakeRunRecordBuilder, flagValue } from "../test/run-record-builder";
import {
  type RunRecordHook,
  type RunRecordOptions,
  registerRunRecordHook,
  resetRunRecordStateForTests,
} from "./run-record";

type Handler = (event: unknown, context: SessionContext) => Promise<unknown>;

/** One plugin instance's host: the handlers the hook registered, driven as Oh My Pi drives them.
 * `sessionFile` is the transcript path, or the session manager's own reader of it. */
function fakeSession(
  sessionId: string,
  sessionFile: string | undefined | (() => string | undefined)
) {
  const handlers = new Map<string, Handler[]>();
  const on = ((name: string, handler: Handler) => {
    handlers.set(name, [...(handlers.get(name) ?? []), handler]);
  }) as unknown as PiApi["on"];
  const getSessionFile = typeof sessionFile === "function" ? sessionFile : () => sessionFile;
  // Only the two session manager members the hook reads.
  const context = {
    sessionManager: { getSessionId: () => sessionId, getSessionFile },
  } as unknown as SessionContext;
  const emit = async (name: string, event: unknown = {}): Promise<void> => {
    for (const handler of handlers.get(name) ?? []) await handler(event, context);
  };
  return {
    handlers,
    install: (options: RunRecordOptions): RunRecordHook => registerRunRecordHook({ on }, options),
    agentStart: () => emit("agent_start"),
    agentEnd: (messages: readonly unknown[] = []) => emit("agent_end", { messages }),
    shutdown: () => emit("session_shutdown"),
  };
}

/** Each call answers the next time, in order. */
const clock = (...times: string[]) => {
  const queue = [...times];
  return () => {
    const next = queue.shift();
    if (next === undefined) throw new Error("the test clock ran out of times");
    return new Date(next);
  };
};

const sequentialRunIds = () => {
  let next = 0;
  return () => `run-${++next}`;
};

/** A spool line as JSON parses it. */
type SpoolLine = Record<string, unknown>;

/**
 * The spool file's complete lines once it holds `count`. The hook appends its own line without
 * a promise a test could await, and for a failed builder only once that separate process has
 * exited, so the file is polled until a deadline.
 */
async function spoolLines(file: string, count: number): Promise<SpoolLine[]> {
  const deadline = Date.now() + 4_000;
  for (;;) {
    const text = existsSync(file) ? await readFile(file, "utf8") : "";
    const lines = text.split("\n").slice(0, -1);
    if (lines.length >= count) return lines.map((line): SpoolLine => JSON.parse(line));
    if (Date.now() > deadline) throw new Error(`${file} holds ${lines.length} lines, not ${count}`);
    await Bun.sleep(20);
  }
}

let directory = "";
let spool = "";
let sessions = "";

beforeEach(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "run-record-"));
  spool = path.join(directory, "spool");
  sessions = path.join(directory, "sessions");
  await mkdir(sessions, { recursive: true });
  resetRunRecordStateForTests();
});

afterEach(async () => {
  resetRunRecordStateForTests();
  await rm(directory, { recursive: true, force: true });
});

const identity = {
  RUN_RECORD_HOST: "example-host-a",
  LEGION_PROJECT: "acme",
  LEGION_TREE: "ACME-1",
  LEGION_ISSUE: "ACME-2",
  LEGION_ROLE: "implementer",
  LEGION_GENERATION: "3",
  LEGION_WORKSPACE: "/workspaces/ACME-2",
};

const builderEnv = (builder: FakeBuilder) => ({
  RUN_RECORD_DIR: spool,
  RUN_RECORD_BUILDER: builder.script,
  RUN_RECORD_PYTHON: builder.interpreter,
  ...identity,
});

describe("run record hook", () => {
  test("registers no handler and records nothing unless RUN_RECORD_DIR is set", () => {
    const session = fakeSession("ses-a", path.join(sessions, "a.jsonl"));
    const hook = session.install({ env: { ...identity, RUN_RECORD_BUILDER: "/bin/false" } });

    hook.handoffCompleted({ phase: "implementing", commit: "abc" });
    expect([...session.handlers.keys()]).toEqual([]);
  });

  test("agent_start starts the builder's start detached with the run's identity, and agent_end its end with every start argument and the end's", async () => {
    const builder = await fakeRunRecordBuilder(directory);
    const sessionFile = path.join(sessions, "2026-10-29T10-00-00-000Z_ses-a.jsonl");
    await writeFile(sessionFile, "0123456789\n");
    const session = fakeSession("ses-a", sessionFile);
    session.install({
      env: builderEnv(builder),
      now: clock("2026-10-29T10:00:00.123Z", "2026-10-29T10:41:07.900Z"),
    });

    await session.agentStart();
    const [start] = await builder.calls(1);
    if (start === undefined) throw new Error("no start call");
    const runId = flagValue(start.argv, "--run-id") ?? "";
    expect(runId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    const startArguments = [
      "--spool-dir",
      spool,
      "--run-id",
      runId,
      "--session-id",
      "ses-a",
      "--session-file",
      sessionFile,
      "--started-at",
      "2026-10-29T10:00:00.123Z",
      "--host",
      "example-host-a",
      "--project",
      "acme",
      "--tree",
      "ACME-1",
      "--issue",
      "ACME-2",
      "--role",
      "implementer",
      "--generation",
      "3",
      "--workspace",
      "/workspaces/ACME-2",
      "--session-offset",
      "11",
    ];
    expect(start.argv).toEqual(["start", ...startArguments]);
    // Detached: the builder leads a process group of its own, outside the pane's.
    expect(start.pgrp).toBe(start.pid);

    await session.agentEnd([
      { role: "user" },
      { role: "assistant", responseId: "msg_1" },
      { role: "toolResult" },
      { role: "assistant", responseId: "msg_2", stopReason: "stop" },
    ]);
    const [, end] = await builder.calls(2);
    expect(end?.argv).toEqual([
      "end",
      ...startArguments,
      "--ended-at",
      "2026-10-29T10:41:07.900Z",
      "--end-reason",
      "agent_end",
      "--last-response-id",
      "msg_2",
      "--cache-dir",
      path.join(spool, ".cache"),
    ]);
    expect(end?.pgrp).toBe(end?.pid);
    // The builder writes the lines; the hook wrote none of its own.
    expect(existsSync(spool)).toBe(false);
  });

  // Real time on purpose: what is under test is that the hook returns while a separate process,
  // the builder, is still running, which only the platform clock and that process can show.
  test("agent_end returns while the builder still runs", async () => {
    const builder = await fakeRunRecordBuilder(directory, { sleepSeconds: 1 });
    const session = fakeSession("ses-a", path.join(sessions, "a.jsonl"));
    session.install({ env: builderEnv(builder) });
    await session.agentStart();
    await builder.calls(1);

    const before = performance.now();
    await session.agentEnd();
    expect(performance.now() - before).toBeLessThan(500);
    const [, end] = await builder.calls(2);
    if (end === undefined) throw new Error("no end call");
    expect(await builder.finished(end.pid)).toBe(false);

    // Both sleeps end, so nothing this test started outlives it.
    expect((await builder.settled(2)).length).toBe(2);
  });

  test("a subagent's run names the run its parent's session has open, read through the process-wide record", async () => {
    const builder = await fakeRunRecordBuilder(directory);
    const parentFile = path.join(sessions, "parent.jsonl");
    await writeFile(parentFile, "");
    // Oh My Pi's layout: a subagent's transcript sits in a directory named after its parent's.
    const childFile = path.join(sessions, "parent", "child.jsonl");
    const runIds = sequentialRunIds();
    const parent = fakeSession("ses-parent", parentFile);
    parent.install({ env: builderEnv(builder), newRunId: runIds });
    const child = fakeSession("ses-child", childFile);
    child.install({ env: builderEnv(builder), newRunId: runIds });

    await parent.agentStart();
    await builder.calls(1);
    await child.agentStart();
    await builder.calls(2);
    await child.agentEnd([{ role: "assistant", responseId: "msg_1" }, { role: "assistant" }]);
    await builder.calls(3);
    await parent.agentEnd();
    await builder.calls(4);
    await child.agentStart();
    const calls = await builder.calls(5);

    const summary = calls.map(({ argv }) => ({
      command: argv[0],
      run: flagValue(argv, "--run-id"),
      parent: flagValue(argv, "--parent-run-id"),
      offset: flagValue(argv, "--session-offset"),
      response: flagValue(argv, "--last-response-id"),
    }));
    expect(summary).toEqual([
      { command: "start", run: "run-1", parent: undefined, offset: "0", response: undefined },
      // The child's transcript is not on disk yet: every line it will hold is this run's.
      { command: "start", run: "run-2", parent: "run-1", offset: "0", response: undefined },
      // The last assistant message carries no responseId, so none is named.
      { command: "end", run: "run-2", parent: "run-1", offset: "0", response: undefined },
      { command: "end", run: "run-1", parent: undefined, offset: "0", response: undefined },
      // The parent's run has ended: a later child run names none.
      { command: "start", run: "run-3", parent: undefined, offset: "0", response: undefined },
    ]);
  });

  test("session_shutdown ends an open run as shutdown, once, and does nothing with no run open", async () => {
    const builder = await fakeRunRecordBuilder(directory);
    const session = fakeSession("ses-a", path.join(sessions, "a.jsonl"));
    session.install({
      env: builderEnv(builder),
      newRunId: sequentialRunIds(),
      // The clock is read at a run's start and end only: the run-less shutdown and agent_end
      // below take no time from it.
      now: clock(
        "2026-10-29T10:00:00.000Z",
        "2026-10-29T10:05:00.000Z",
        "2026-10-29T10:06:00.000Z"
      ),
    });

    await session.shutdown();
    await session.agentStart();
    await builder.calls(1);
    await session.shutdown();
    await builder.calls(2);
    // The aborted run's own agent_end, after the shutdown, finds no run open; the next run's start
    // is spawned after anything that agent_end could have spawned.
    await session.agentEnd([{ role: "assistant", responseId: "msg_9" }]);
    await session.agentStart();
    const calls = await builder.settled(3);

    expect(calls.map(({ argv }) => [argv[0], flagValue(argv, "--run-id")])).toEqual([
      ["start", "run-1"],
      ["end", "run-1"],
      ["start", "run-2"],
    ]);
    const end = calls[1]?.argv ?? [];
    expect(flagValue(end, "--end-reason")).toBe("shutdown");
    expect(flagValue(end, "--ended-at")).toBe("2026-10-29T10:05:00.000Z");
    expect(flagValue(end, "--last-response-id")).toBeUndefined();
  });

  test("a handoff completed in the run reaches that run's end, and no later run's", async () => {
    const builder = await fakeRunRecordBuilder(directory);
    const session = fakeSession("ses-a", path.join(sessions, "a.jsonl"));
    const hook = session.install({ env: builderEnv(builder), newRunId: sequentialRunIds() });

    await session.agentStart();
    hook.handoffCompleted({ phase: "implementing", commit: "c0ffee" });
    await session.agentEnd();
    await session.agentStart();
    await session.agentEnd();
    const calls = await builder.calls(4);

    const ends = calls.filter(({ argv }) => argv[0] === "end");
    expect(
      ends.map(({ argv }) => [
        flagValue(argv, "--run-id"),
        flagValue(argv, "--handoff-phase"),
        flagValue(argv, "--handoff-commit"),
      ])
    ).toEqual([
      ["run-1", "implementing", "c0ffee"],
      ["run-2", undefined, undefined],
    ]);
  });

  test("with no builder configured the hook appends each line itself, into the spool file of its own UTC day", async () => {
    const sessionFile = path.join(sessions, "a.jsonl");
    await writeFile(sessionFile, "{}\n");
    const session = fakeSession("ses-a", sessionFile);
    const hook = session.install({
      env: { RUN_RECORD_DIR: spool, ...identity },
      newRunId: sequentialRunIds(),
      now: clock("2026-10-29T23:59:59.900Z", "2026-10-30T00:00:00.100Z"),
    });

    await session.agentStart();
    hook.handoffCompleted({ phase: "testing", commit: "f00d" });
    await session.agentEnd([{ role: "assistant", responseId: "msg_1" }]);

    const startFields = {
      schema: "run-record/1",
      kind: "start",
      run_id: "run-1",
      host: "example-host-a",
      project: "acme",
      tree: "ACME-1",
      issue: "ACME-2",
      role: "implementer",
      generation: 3,
      session_id: "ses-a",
      session_file: sessionFile,
      parent_session_id: null,
      parent_run_id: null,
      harness: "omp",
      started_at: "2026-10-29T23:59:59.900Z",
      repo: null,
      branch: "legion/ACME-2",
      base: null,
      session_offset: 3,
      producer: null,
    };
    expect(await spoolLines(path.join(spool, "2026-10-29", "ses-a.jsonl"), 1)).toEqual([
      startFields,
    ]);
    expect(await spoolLines(path.join(spool, "2026-10-30", "ses-a.jsonl"), 1)).toEqual([
      {
        ...startFields,
        kind: "end",
        ended_at: "2026-10-30T00:00:00.100Z",
        end_reason: "agent_end",
        stop_reason: null,
        model: null,
        turns: null,
        delivery: null,
        wall_seconds: null,
        model_seconds: null,
        models: null,
        calls: null,
        compactions: null,
        end: null,
        commits: null,
        handoff: { phase: "testing", commit: "f00d" },
        obligations: {
          rule: "syntactic_v12",
          evaluated: "error",
          error: "no_builder",
          changes: [],
        },
      },
    ]);
  });

  test("an interpreter that cannot start, or a builder that exits non-zero, gets the hook's own line", async () => {
    const cases = [
      {
        name: "missing interpreter",
        error: "no_builder",
        env: async () => {
          const builder = await fakeRunRecordBuilder(path.join(directory, "unused"));
          return {
            RUN_RECORD_BUILDER: builder.script,
            RUN_RECORD_PYTHON: path.join(directory, "none"),
          };
        },
      },
      {
        name: "builder exits 3",
        error: "builder_exit:3",
        env: async () => {
          const builder = await fakeRunRecordBuilder(path.join(directory, "failing"), {
            exitCode: 3,
          });
          return { RUN_RECORD_BUILDER: builder.script, RUN_RECORD_PYTHON: builder.interpreter };
        },
      },
    ];
    for (const [index, entry] of cases.entries()) {
      const sessionId = `ses-${index}`;
      const session = fakeSession(sessionId, path.join(sessions, `${sessionId}.jsonl`));
      session.install({
        env: { RUN_RECORD_DIR: spool, ...(await entry.env()) },
        now: clock("2026-10-29T10:00:00.000Z", "2026-10-29T10:01:00.000Z"),
      });
      await session.agentStart();
      await session.agentEnd();

      const lines = await spoolLines(path.join(spool, "2026-10-29", `${sessionId}.jsonl`), 2);
      expect({
        name: entry.name,
        kinds: lines.map((line) => String(line.kind)).sort(),
        obligations: lines.find((line) => line.kind === "end")?.obligations,
      }).toEqual({
        name: entry.name,
        kinds: ["end", "start"],
        obligations: { rule: "syntactic_v12", evaluated: "error", error: entry.error, changes: [] },
      });
    }
  });

  test("a failure reading the session never reaches the turn, and a session with no transcript file records nothing", async () => {
    const builder = await fakeRunRecordBuilder(directory);
    const throwing = fakeSession("ses-throws", () => {
      throw new Error("no session manager");
    });
    throwing.install({ env: builderEnv(builder) });
    const inMemory = fakeSession("ses-memory", undefined);
    inMemory.install({ env: builderEnv(builder) });
    const sentinel = fakeSession("ses-sentinel", path.join(sessions, "sentinel.jsonl"));
    sentinel.install({ env: builderEnv(builder) });

    expect(await throwing.agentStart()).toBeUndefined();
    expect(await throwing.agentEnd()).toBeUndefined();
    await inMemory.agentStart();
    await inMemory.agentEnd();
    // Spawned after anything the two sessions above could have spawned.
    await sentinel.agentStart();
    const calls = await builder.settled(1);

    expect(calls.map(({ argv }) => flagValue(argv, "--session-id"))).toEqual(["ses-sentinel"]);
  });
});
