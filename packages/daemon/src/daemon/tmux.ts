import type { CommandRunnerOptions } from "../state/fetch";

export type TmuxRun = (
  cmd: string[],
  options?: CommandRunnerOptions
) => Promise<{ stdout: string; stderr?: string; exitCode: number }>;

const BOOTSTRAP_WINDOW = "__legion_bootstrap";

/**
 * Every tmux window this daemon opens carries an `@legion_owner` option matching `owner`
 * (the deployment's tmux session name today), so reconciliation can tell a Legion-managed
 * window from one a human opened by hand in the same session.
 */
async function markOwner(run: TmuxRun, target: string, owner: string, scope: "session" | "window") {
  const marker = await run([
    "tmux",
    "set-option",
    ...(scope === "window" ? ["-w"] : []),
    "-t",
    target,
    "@legion_owner",
    owner,
  ]);
  if (marker.exitCode !== 0) {
    if (scope === "window") {
      // Every window is either recorded (marked, then locator-assigned by the caller) or
      // reaped: this one never got its marker, so nothing will ever recognize or clean it up
      // later. Kill it now instead of leaving an orphan for `reconcileTmuxWindows` to find.
      await run(["tmux", "kill-window", "-t", target]);
    }
    throw new Error(
      `tmux ${scope} ownership marker failed (exit ${marker.exitCode}): ${marker.stdout}`
    );
  }
}

interface PaneReport {
  windowId?: string;
  paneId: string;
  pid: number;
}

/**
 * Parses a `-P -F` report from `new-window` (`"#{window_id} #{pane_id} #{pane_pid}"`, three
 * tokens) or `split-window` (`"#{pane_id} #{pane_pid}"`, two tokens) — the only difference is
 * whether a window id leads the line. Throws on any malformed/missing token so a launch failure
 * is loud rather than silently persisting a garbage locator.
 */
function parsePaneReport(stdout: string, context: string, expectWindow: boolean): PaneReport {
  const tokens = stdout.trim().split(/\s+/);
  const windowId = expectWindow ? tokens.shift() : undefined;
  if (expectWindow && (!windowId || !/^@\d+$/.test(windowId))) {
    throw new Error(`${context} did not report a window id: ${stdout}`);
  }
  const [paneId, pidToken] = tokens;
  if (!paneId || !/^%\d+$/.test(paneId)) {
    throw new Error(`${context} did not report a pane id: ${stdout}`);
  }
  const pid = Number(pidToken);
  if (!Number.isSafeInteger(pid) || pid <= 0) {
    throw new Error(`${context} did not report a pane pid: ${stdout}`);
  }
  return { windowId, paneId, pid };
}

/**
 * Opens a fresh window in `session` running `environmentAndCommand`, creating the session first
 * (via a disposable bootstrap window, immediately killed) if it does not already exist. Captures
 * the window id, pane id, and pane pid in the same `-P -F` invocation that creates the window —
 * tmux resolves that synchronously before the wrapped command starts, so a command that exits (or
 * fails to spawn) instantly can never race a later, separate discovery call.
 */
export async function openWindow(
  run: TmuxRun,
  session: string,
  name: string,
  environmentAndCommand: string[],
  owner: string
): Promise<{ windowId: string; paneId: string; pid: number }> {
  const sessionExists = (await run(["tmux", "has-session", "-t", session])).exitCode === 0;
  if (!sessionExists) {
    const create = await run([
      "tmux",
      "new-session",
      "-d",
      "-s",
      session,
      "-n",
      BOOTSTRAP_WINDOW,
      "sleep 3600",
    ]);
    if (create.exitCode !== 0) {
      throw new Error(`tmux new-session failed (exit ${create.exitCode}): ${create.stdout}`);
    }
    await markOwner(run, session, owner, "session");
  }

  const command = [
    "tmux",
    "new-window",
    "-P",
    "-F",
    "#{window_id} #{pane_id} #{pane_pid}",
    "-t",
    session,
    "-n",
    name,
    ...environmentAndCommand,
  ];
  const result = await run(command);
  if (!sessionExists) {
    const cleanup = await run(["tmux", "kill-window", "-t", `${session}:${BOOTSTRAP_WINDOW}`]);
    if (cleanup.exitCode !== 0) {
      throw new Error(
        `tmux bootstrap window cleanup failed (exit ${cleanup.exitCode}): ${cleanup.stdout}`
      );
    }
  }
  if (result.exitCode !== 0) {
    throw new Error(`tmux new-window failed (exit ${result.exitCode}): ${result.stdout}`);
  }
  const { windowId, paneId, pid } = parsePaneReport(result.stdout, "tmux new-window", true);
  if (!windowId) throw new Error(`tmux new-window did not report a window id: ${result.stdout}`);
  await markOwner(run, windowId, owner, "window");
  return { windowId, paneId, pid };
}

/** Splits a new pane into an existing window, tiling the layout afterward. Same single-invocation
 * capture rationale as `openWindow`. */
export async function splitWindow(
  run: TmuxRun,
  windowId: string,
  environmentAndCommand: string[]
): Promise<{ paneId: string; pid: number }> {
  const split = await run([
    "tmux",
    "split-window",
    "-t",
    windowId,
    "-P",
    "-F",
    "#{pane_id} #{pane_pid}",
    ...environmentAndCommand,
  ]);
  if (split.exitCode !== 0) {
    throw new Error(`tmux split-window failed (exit ${split.exitCode}): ${split.stdout}`);
  }
  const { paneId, pid } = parsePaneReport(split.stdout, "tmux split-window", false);
  await run(["tmux", "select-layout", "-t", windowId, "tiled"]);
  return { paneId, pid };
}

/** Trusts no recorded window id until it is confirmed live, so a human-killed window falls back to a fresh one. */
export async function windowAlive(run: TmuxRun, windowId: string): Promise<boolean> {
  const probe = await run(["tmux", "list-panes", "-t", windowId, "-F", "#{pane_id}"]);
  return probe.exitCode === 0;
}

/** Reads the live pid of a window's (or pane's) first pane, or `undefined` if it cannot be read. */
export async function panePid(run: TmuxRun, target: string): Promise<number | undefined> {
  const panes = await run(["tmux", "list-panes", "-t", target, "-F", "#{pane_pid}"]);
  const pid = Number(panes.stdout.trim().split(/\s+/)[0]);
  return panes.exitCode === 0 && Number.isSafeInteger(pid) && pid > 0 ? pid : undefined;
}

/** Reads a window's own pane id (its first/sole pane), or `undefined` if it cannot be read.
 * Used to backfill a locator's `tmuxPaneId` once a window recorded before that field existed —
 * or written by some other pane-id-less path — is confirmed alive, so the reconciliation
 * sweep's pane-level check (see `listUnknownPanes`) eventually has a real id to compare against
 * instead of permanently exempting that window. */
export async function firstPaneId(run: TmuxRun, windowId: string): Promise<string | undefined> {
  const panes = await run(["tmux", "list-panes", "-t", windowId, "-F", "#{pane_id}"]);
  const paneId = panes.stdout.trim().split(/\s+/)[0];
  return panes.exitCode === 0 && paneId && /^%\d+$/.test(paneId) ? paneId : undefined;
}

export async function killWindow(run: TmuxRun, windowId: string): Promise<void> {
  await run(["tmux", "kill-window", "-t", windowId]);
}

/** Kills a single pane, leaving the rest of its window (and any sibling panes) intact. */
export async function killPane(run: TmuxRun, paneId: string): Promise<void> {
  await run(["tmux", "kill-pane", "-t", paneId]);
}

/** A window this daemon's session owns, not already known to the caller. */
export interface UnknownOwnedWindow {
  windowId: string;
  activityAt: number;
}

/**
 * Lists every window in `session` marked with `@legion_owner === owner` that isn't in `known` —
 * candidates for the reconciliation sweep to reap once they have been idle past its grace period.
 * Returns an empty array (rather than throwing) if the session itself no longer exists.
 */
export async function listUnknownOwnedWindows(
  run: TmuxRun,
  session: string,
  owner: string,
  known: ReadonlySet<string>
): Promise<UnknownOwnedWindow[]> {
  const windows = await run([
    "tmux",
    "list-windows",
    "-t",
    session,
    "-F",
    "#{window_id}\t#{@legion_owner}\t#{window_activity}",
  ]);
  if (windows.exitCode !== 0) return [];

  const unknown: UnknownOwnedWindow[] = [];
  for (const line of windows.stdout.split(/\r?\n/)) {
    const [windowId, windowOwner, activitySeconds] = line.split("\t");
    if (!windowId || !/^@\d+$/.test(windowId) || known.has(windowId) || windowOwner !== owner) {
      continue;
    }
    const activityAt = Number(activitySeconds) * 1000;
    if (!Number.isFinite(activityAt)) continue;
    unknown.push({ windowId, activityAt });
  }
  return unknown;
}

/** An unrecorded pane inside a Legion-owned window, running the worker-shim wrapper. */
export interface UnknownPane {
  paneId: string;
  windowId: string;
  activityAt: number;
}

/**
 * Lists every pane, anywhere on the tmux server, whose owning window is marked with
 * `@legion_owner === owner` (window options resolve through the pane's own window, exactly as
 * `listUnknownOwnedWindows` reads the same option via `list-windows`) whose pane id isn't in
 * `known`, and whose start command names the `legion worker-shim` wrapper every Legion process —
 * root, phase worker, or controller — runs inside its pane. A pane split into a *known* window
 * (so the window itself is never reaped by `listUnknownOwnedWindows`) but never recorded by any
 * tree/controller/role locator is a real, running process the daemon has otherwise completely
 * forgotten about — most likely a crash between opening the pane and persisting its locator.
 * `known` is checked unconditionally: a recorded pane id is never a candidate here regardless of
 * whether its window happens to look unowned. Returns an empty array (rather than throwing) if
 * the command itself fails.
 */
export async function listUnknownPanes(
  run: TmuxRun,
  owner: string,
  known: ReadonlySet<string>
): Promise<UnknownPane[]> {
  const panes = await run([
    "tmux",
    "list-panes",
    "-a",
    "-F",
    "#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{pane_activity}",
  ]);
  if (panes.exitCode !== 0) return [];

  const unknown: UnknownPane[] = [];
  for (const line of panes.stdout.split(/\r?\n/)) {
    const [paneId, windowId, windowOwner, startCommand, activitySeconds] = line.split("\t");
    if (
      !paneId ||
      !/^%\d+$/.test(paneId) ||
      !windowId ||
      known.has(paneId) ||
      windowOwner !== owner ||
      !startCommand?.includes("worker-shim")
    ) {
      continue;
    }
    const activityAt = Number(activitySeconds) * 1000;
    if (!Number.isFinite(activityAt)) continue;
    unknown.push({ paneId, windowId, activityAt });
  }
  return unknown;
}
