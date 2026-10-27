import { type ChildProcess, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { AgentEndEvent, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { logger } from "@oh-my-pi/pi-utils";

/**
 * The run record hook (README.md, "Run records"). A run is one prompt-bounded turn of a session,
 * from `agent_start` to `agent_end`. At each end the deployment's record builder appends one JSON
 * line for the run to the spool, `<RUN_RECORD_DIR>/<UTC day>/<session id>.jsonl`, and the
 * uploader (`scripts/run_record_upload.py`) copies each line to the operator's bucket.
 *
 * The hook computes nothing about git or the session file: it starts the builder as a detached
 * child, never waits for it, and returns at once, so no turn waits on the record. It is off, and
 * registers no handler, unless `RUN_RECORD_DIR` is set. When the builder cannot start (none
 * configured, its file missing, the interpreter missing) or exits non-zero, which means it wrote
 * no line, the hook appends the line itself with its own fields and the builder's left null. Every
 * path is fail-open: an error is logged and never reaches the turn.
 *
 * `extensions/legion.ts` registers it before anything else, outside `session_start`, so a `task`
 * subagent's instance of the plugin, which leaves `session_start` at its subagent check, records
 * its runs too, naming the parent's open run.
 */

/** A file or directory name the spool and the bucket take: the same rule the uploader holds. */
const SPOOL_NAME = /^[A-Za-z0-9._-]{1,128}$/;

/** The obligation rule a fallback end line names, as the builder's own lines do. */
const OBLIGATION_RULE = "syntactic_v12";

/**
 * The run each session file has open, process-wide: a `task` subagent loads its own instance of
 * this module in its parent's process, and finds the parent's open run here at its own
 * `agent_start` (the parent's session file is `<directory of the subagent's file>.jsonl`).
 */
const OPEN_RUNS_KEY = Symbol.for("legion.pi-legion.run-record.open-runs");

export type RunEndReason = "agent_end" | "shutdown";

/** The phase a successful `handoff_complete` completed, and the commit it reported. */
export interface RunHandoff {
  readonly phase: string;
  readonly commit: string;
}

export interface RunRecordHook {
  /** Told of a `handoff_complete` that succeeded in the open run; its end line carries it. */
  readonly handoffCompleted: (handoff: RunHandoff) => void;
}

export interface RunRecordOptions {
  /** The environment the configuration is read from, once: `process.env` unless a test's own. */
  readonly env?: Readonly<Record<string, string | undefined>>;
  readonly now?: () => Date;
  /** The run id: a UUIDv7 unless a test's own. */
  readonly newRunId?: () => string;
}

interface Identity {
  readonly project: string | null;
  readonly tree: string | null;
  readonly issue: string | null;
  readonly role: string | null;
  readonly generation: number | null;
  readonly workspace: string | null;
}

interface RunRecordConfig {
  readonly spoolDir: string;
  readonly builder: string | undefined;
  readonly python: string;
  readonly cacheDir: string;
  readonly host: string;
  readonly identity: Identity;
}

interface RunStart {
  readonly runId: string;
  readonly sessionId: string;
  readonly sessionFile: string;
  readonly startedAt: string;
  readonly parentRunId: string | null;
  readonly sessionOffset: number | null;
}

interface RunEnd {
  readonly endedAt: string;
  readonly endReason: RunEndReason;
  readonly lastResponseId: string | undefined;
  readonly handoff: RunHandoff | undefined;
}

interface OpenRun {
  readonly start: RunStart;
  handoff: RunHandoff | undefined;
}

const NO_HOOK: RunRecordHook = { handoffCompleted: () => undefined };

/**
 * Registers the run record hook on `pi`, or nothing when `RUN_RECORD_DIR` is unset. One call per
 * plugin instance, that is per session: the instance keeps the session's open run.
 */
export function registerRunRecordHook(
  pi: Pick<PiApi, "on">,
  options: RunRecordOptions = {}
): RunRecordHook {
  const config = readConfig(options.env ?? process.env);
  if (config === undefined) return NO_HOOK;
  const now = options.now ?? (() => new Date());
  const newRunId = options.newRunId ?? (() => Bun.randomUUIDv7());
  let open: OpenRun | undefined;

  /** Ends the open run, if there is one, at the clock's time now. */
  const endRun = (endReason: RunEndReason, lastResponse?: string): void => {
    const run = open;
    if (run === undefined) return;
    const endedAt = now().toISOString();
    open = undefined;
    const runs = openRuns();
    if (runs.get(run.start.sessionFile) === run.start.runId) runs.delete(run.start.sessionFile);
    const end: RunEnd = { endedAt, endReason, lastResponseId: lastResponse, handoff: run.handoff };
    launchBuilder(config, "end", endArguments(config, run.start, end), (error) =>
      appendFallbackLine(config, run.start, end, error)
    );
  };

  pi.on("agent_start", async (_event, context) => {
    try {
      const startedAt = now().toISOString();
      const start = beginRun(context, startedAt, newRunId);
      if (start === undefined) return;
      if (open !== undefined) {
        // No agent_end closed the last run; its start line stays without an end line.
        logger.warn("run record: a run started while the last one was still open", {
          openRun: open.start.runId,
          run: start.runId,
        });
      }
      openRuns().set(start.sessionFile, start.runId);
      open = { start, handoff: undefined };
      launchBuilder(config, "start", startArguments(config, start), (error) =>
        appendFallbackLine(config, start, undefined, error)
      );
    } catch (error) {
      logger.warn("run record: agent_start failed", { error: String(error) });
    }
  });

  pi.on("agent_end", async (event) => {
    try {
      endRun("agent_end", lastResponseId(event));
    } catch (error) {
      logger.warn("run record: agent_end failed", { error: String(error) });
    }
  });

  // Oh My Pi emits session_shutdown when it disposes the session, before it aborts a run still in
  // flight, so the run ends here and that run's later agent_end, if any, finds none open.
  pi.on("session_shutdown", async () => {
    try {
      endRun("shutdown");
    } catch (error) {
      logger.warn("run record: session_shutdown failed", { error: String(error) });
    }
  });

  return {
    handoffCompleted: (handoff) => {
      if (open !== undefined) open.handoff = handoff;
    },
  };
}

/** Clears the process-wide open runs, so one test's open run is never another's parent. */
export function resetRunRecordStateForTests(): void {
  delete (globalThis as Record<symbol, unknown>)[OPEN_RUNS_KEY];
}

function openRuns(): Map<string, string> {
  const global = globalThis as Record<symbol, unknown>;
  const existing = global[OPEN_RUNS_KEY];
  if (existing instanceof Map) return existing as Map<string, string>;
  const runs = new Map<string, string>();
  global[OPEN_RUNS_KEY] = runs;
  return runs;
}

function readConfig(
  env: Readonly<Record<string, string | undefined>>
): RunRecordConfig | undefined {
  const dir = nonEmpty(env.RUN_RECORD_DIR);
  if (dir === undefined) return undefined;
  const spoolDir = path.resolve(dir);
  const generation = nonEmpty(env.LEGION_GENERATION);
  return {
    spoolDir,
    builder: nonEmpty(env.RUN_RECORD_BUILDER),
    python: nonEmpty(env.RUN_RECORD_PYTHON) ?? "python3",
    cacheDir: path.resolve(nonEmpty(env.RUN_RECORD_CACHE_DIR) ?? path.join(spoolDir, ".cache")),
    host: nonEmpty(env.RUN_RECORD_HOST) ?? os.hostname(),
    identity: {
      project: nonEmpty(env.LEGION_PROJECT) ?? null,
      tree: nonEmpty(env.LEGION_TREE) ?? null,
      issue: nonEmpty(env.LEGION_ISSUE) ?? null,
      role: nonEmpty(env.LEGION_ROLE) ?? null,
      // The builder takes an integer; anything else is left out rather than failing its line.
      generation:
        generation !== undefined && /^\d{1,15}$/.test(generation) ? Number(generation) : null,
      workspace: nonEmpty(env.LEGION_WORKSPACE) ?? null,
    },
  };
}

function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/** The run `agent_start` opens, or undefined for a session with no transcript file to read. */
function beginRun(
  context: SessionContext,
  startedAt: string,
  newRunId: () => string
): RunStart | undefined {
  const sessionId = context.sessionManager.getSessionId();
  const sessionFile = context.sessionManager.getSessionFile();
  if (sessionId === "" || sessionFile === undefined || sessionFile === "") {
    logger.debug("run record: the session has no transcript file; its run is not recorded", {
      sessionId,
    });
    return undefined;
  }
  return {
    runId: newRunId(),
    sessionId,
    sessionFile,
    startedAt,
    parentRunId: openRuns().get(`${path.dirname(sessionFile)}.jsonl`) ?? null,
    sessionOffset: sessionOffset(sessionFile),
  };
}

/**
 * The session file's size now, from which the builder reads the run's own lines: 0 when Oh My Pi
 * has not written the file yet (every line it later holds is this run's or earlier bookkeeping),
 * and null, so the builder reads the whole file, when it cannot be read.
 */
function sessionOffset(sessionFile: string): number | null {
  try {
    return fs.statSync(sessionFile).size;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return 0;
    return null;
  }
}

/** The `responseId` of the run's last assistant message, when it has one. */
function lastResponseId(event: AgentEndEvent): string | undefined {
  const messages = event.messages ?? [];
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    if (message?.role !== "assistant") continue;
    const id = message.responseId;
    return typeof id === "string" && id !== "" ? id : undefined;
  }
  return undefined;
}

function startArguments(config: RunRecordConfig, start: RunStart): string[] {
  const { identity } = config;
  const args = [
    "--spool-dir",
    config.spoolDir,
    "--run-id",
    start.runId,
    "--session-id",
    start.sessionId,
    "--session-file",
    start.sessionFile,
    "--started-at",
    start.startedAt,
    "--host",
    config.host,
  ];
  const optional: readonly (readonly [string, string | number | null])[] = [
    ["--project", identity.project],
    ["--tree", identity.tree],
    ["--issue", identity.issue],
    ["--role", identity.role],
    ["--generation", identity.generation],
    ["--parent-run-id", start.parentRunId],
    ["--workspace", identity.workspace],
    ["--session-offset", start.sessionOffset],
  ];
  for (const [flag, value] of optional) {
    if (value !== null) args.push(flag, String(value));
  }
  return args;
}

function endArguments(config: RunRecordConfig, start: RunStart, end: RunEnd): string[] {
  const args = [
    ...startArguments(config, start),
    "--ended-at",
    end.endedAt,
    "--end-reason",
    end.endReason,
  ];
  if (end.lastResponseId !== undefined) args.push("--last-response-id", end.lastResponseId);
  if (end.handoff !== undefined) {
    args.push("--handoff-phase", end.handoff.phase, "--handoff-commit", end.handoff.commit);
  }
  args.push("--cache-dir", config.cacheDir);
  return args;
}

/**
 * Starts `<python> <builder> <command> <args>` detached, its stdio ignored and unreferenced, and
 * returns without waiting. `fallback` runs once, with the error code its line names, when the
 * builder never starts (`no_builder`) or exits non-zero (`builder_exit:<code>`), which the
 * builder does only when it wrote no line. A builder killed by a signal gets no fallback: it may
 * have written its line.
 */
function launchBuilder(
  config: RunRecordConfig,
  command: "start" | "end",
  args: readonly string[],
  fallback: (error: string) => void
): void {
  let settled = false;
  const fallBack = (error: string): void => {
    if (settled) return;
    settled = true;
    fallback(error);
  };
  const builder = config.builder;
  if (builder === undefined || !isFile(builder)) {
    fallBack("no_builder");
    return;
  }
  let child: ChildProcess;
  try {
    child = spawn(config.python, [builder, command, ...args], {
      detached: true,
      stdio: "ignore",
    });
  } catch {
    fallBack("no_builder");
    return;
  }
  child.once("error", () => fallBack("no_builder"));
  child.once("exit", (code) => {
    if (code === null || code === 0) settled = true;
    else fallBack(code > 0 ? `builder_exit:${code}` : "no_builder");
  });
  child.unref();
}

function isFile(file: string): boolean {
  try {
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

/**
 * Appends the line the builder did not write, in one `appendFile`, to the spool file of the UTC
 * day of the line's own time. It carries what the hook knows (identity, times, the end reason, the
 * handoff) and leaves every field the builder computes null.
 */
function appendFallbackLine(
  config: RunRecordConfig,
  start: RunStart,
  end: RunEnd | undefined,
  error: string
): void {
  const line = fallbackLine(config, start, end, error);
  const time = end === undefined ? start.startedAt : end.endedAt;
  appendLine(config.spoolDir, time.slice(0, 10), start.sessionId, line).catch((failure) => {
    logger.warn("run record: the fallback line could not be written", {
      run: start.runId,
      error: String(failure),
    });
  });
}

function fallbackLine(
  config: RunRecordConfig,
  start: RunStart,
  end: RunEnd | undefined,
  error: string
): Record<string, unknown> {
  const { identity } = config;
  const startFields: Record<string, unknown> = {
    schema: "run-record/1",
    kind: end === undefined ? "start" : "end",
    run_id: start.runId,
    host: config.host,
    project: identity.project,
    tree: identity.tree,
    issue: identity.issue,
    role: identity.role,
    generation: identity.generation,
    session_id: start.sessionId,
    session_file: start.sessionFile,
    parent_session_id: null,
    parent_run_id: start.parentRunId,
    harness: "omp",
    started_at: start.startedAt,
    repo: null,
    branch: identity.issue === null ? null : `legion/${identity.issue}`,
    base: null,
    session_offset: start.sessionOffset,
    producer: null,
  };
  if (end === undefined) return startFields;
  return {
    ...startFields,
    ended_at: end.endedAt,
    end_reason: end.endReason,
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
    handoff:
      end.handoff === undefined ? null : { phase: end.handoff.phase, commit: end.handoff.commit },
    obligations: { rule: OBLIGATION_RULE, evaluated: "error", error, changes: [] },
  };
}

async function appendLine(
  spoolDir: string,
  day: string,
  sessionId: string,
  line: Record<string, unknown>
): Promise<void> {
  if (!SPOOL_NAME.test(sessionId)) {
    throw new Error("the session id is not a name the spool takes");
  }
  const dir = path.join(spoolDir, day);
  await fs.promises.mkdir(dir, { recursive: true, mode: 0o755 });
  await fs.promises.appendFile(path.join(dir, `${sessionId}.jsonl`), `${JSON.stringify(line)}\n`);
}
