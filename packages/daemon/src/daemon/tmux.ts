import type { CommandRunnerOptions } from "../state/fetch";

export type TmuxRun = (
  cmd: string[],
  options?: CommandRunnerOptions
) => Promise<{ stdout: string; stderr?: string; exitCode: number }>;

/** The private tmux server this daemon owns. Every argv this module builds starts
 * `tmux -L <socket>`, so the server is forked by the daemon's own first command and inherits the
 * runner's allow-listed `paneEnv` (never a human's shell that may carry `DISPATCH_TOKEN`), and no
 * Legion pane ever shares a server with the operator's own sessions. The socket name equals the
 * session name (`legion-<project>`): attach with `tmux -L legion-<project> attach -t legion-<project>`. */
export interface TmuxServer {
  readonly run: TmuxRun;
  readonly socket: string;
}

/** stderr shapes meaning no server is behind the daemon's own socket: `no server running` (a
 * socket file left behind by an exited server) or `error connecting to … (No such file or
 * directory)` (the socket was never created — a first boot, or a reboot cleared `TMUX_TMPDIR`). */
export const NO_SERVER_STDERR =
  /no server running|error connecting to .*\(No such file or directory\)/;

function argv(server: TmuxServer, ...rest: string[]): string[] {
  return ["tmux", "-L", server.socket, ...rest];
}

/** An entry of `show-environment` is a line `NAME=value`: everything up to the first `=` is the
 * name, whatever characters it holds (a dashed bash function `BASH_FUNC_foo-bar%%`, npm's
 * `//registry.npmjs.org/:_authToken`) — a name this parser skipped would be a name the scrub
 * silently left in the server. Skipped on purpose: `-NAME` (tmux's marker for a variable it unsets
 * in new panes; it reaches none, and a name starting with `-` cannot be told apart from the marker
 * in this format), and a value's continuation lines — a bash exported function body, whose lines
 * start with a space or `}` — which are never a new entry even when they contain `=`. */
const ENVIRONMENT_ENTRY = /^([^\s=}-][^=]*)=/;

/** A pane inherits two tmux environment tables beneath its own `-e` pairs: the server's global
 * table (`-g`, the environment the server was forked with) and its session's table (`-t
 * <session>`, which `update-environment` fills from every attaching client — the operator's
 * `SSH_AUTH_SOCK`/`SSH_CONNECTION`/`DISPLAY` by default). `undefined` names the global table. */
export type EnvironmentTable = { readonly session: string } | undefined;

function tableFlags(table: EnvironmentTable): string[] {
  return table === undefined ? ["-g"] : ["-t", table.session];
}

/** `no server running` / socket never created (`NO_SERVER_STDERR`), or the named session is not
 * there: nothing behind the target to read or write. */
function targetAbsent(stderr: string | undefined): boolean {
  return NO_SERVER_STDERR.test(stderr ?? "") || (stderr ?? "").startsWith("no such session");
}

/** The failure detail for an error message: tmux's stderr only, never stdout — for
 * `show-environment` stdout is the value dump, and no value may reach a log or error string. */
function failure(result: { stderr?: string }): string {
  const detail = result.stderr?.trim();
  return detail ? `: ${detail}` : "";
}

/** The variable names in one of the server's environment tables (see `EnvironmentTable`), or
 * `undefined` when there is nothing to read: no server is running on this socket, or the named
 * session does not exist yet. Names only: the values are never kept. Any other failure throws with
 * tmux's stderr. */
export async function environmentNames(
  server: TmuxServer,
  table: EnvironmentTable
): Promise<string[] | undefined> {
  const flags = tableFlags(table);
  const result = await server.run(argv(server, "show-environment", ...flags));
  if (result.exitCode !== 0) {
    if (targetAbsent(result.stderr)) return undefined;
    throw new Error(
      `tmux show-environment ${flags.join(" ")} failed (exit ${result.exitCode})${failure(result)}`
    );
  }
  const names: string[] = [];
  for (const line of result.stdout.split("\n")) {
    const entry = ENVIRONMENT_ENTRY.exec(line);
    if (entry?.[1]) names.push(entry[1]);
  }
  return names;
}

/** Removes `name` from one of the server's environment tables (`-u`: the entry is gone, not merely
 * marked unset), so no pane opened afterwards inherits it. Panes already open keep their copy. */
export async function unsetEnvironment(
  server: TmuxServer,
  table: EnvironmentTable,
  name: string
): Promise<void> {
  const flags = tableFlags(table);
  const result = await server.run(argv(server, "set-environment", ...flags, "-u", name));
  if (result.exitCode !== 0) {
    throw new Error(
      `tmux set-environment ${flags.join(" ")} -u ${name} failed (exit ${result.exitCode})${failure(result)}`
    );
  }
}

/** The `set-option` that empties a session's `update-environment`, so an attaching client (the
 * operator's `tmux -L legion-<project> attach`) no longer copies its `SSH_AUTH_SOCK`,
 * `SSH_CONNECTION`, `DISPLAY`, … into the session table every pane opened afterwards would
 * inherit. tmux's default list is a per-session option seeded from the server's global one, so it
 * is set on the session itself. */
function disableEnvironmentUpdatesArgs(session: string): string[] {
  return ["set-option", "-t", session, "update-environment", ""];
}

/** Empties an existing session's `update-environment` (see `disableEnvironmentUpdatesArgs`) —
 * every boot against a running server does this before it reads the session table, so an attach
 * landing meanwhile cannot slip a copy in behind the read. Returns `false`, having changed nothing,
 * when no server or no such session is there; throws on any other failure. */
export async function disableEnvironmentUpdates(
  server: TmuxServer,
  session: string
): Promise<boolean> {
  const result = await server.run(argv(server, ...disableEnvironmentUpdatesArgs(session)));
  if (result.exitCode === 0) return true;
  if (targetAbsent(result.stderr)) return false;
  throw new Error(
    `tmux set-option -t ${session} update-environment '' failed (exit ${result.exitCode})${failure(result)}`
  );
}

const BOOTSTRAP_WINDOW = "__legion_bootstrap";

/**
 * Every tmux window this daemon opens carries an `@legion_owner` option matching `owner`
 * (the deployment's tmux session name today), so reconciliation can tell a Legion-managed
 * window from one a human opened by hand in the same session.
 */
async function markOwner(
  server: TmuxServer,
  target: string,
  owner: string,
  scope: "session" | "window"
) {
  const marker = await server.run(
    argv(
      server,
      "set-option",
      ...(scope === "window" ? ["-w"] : []),
      "-t",
      target,
      "@legion_owner",
      owner
    )
  );
  if (marker.exitCode !== 0) {
    if (scope === "window") {
      // Every window is either recorded (marked, then locator-assigned by the caller) or
      // reaped: this one never got its marker, so nothing will ever recognize or clean it up
      // later. Kill it now instead of leaving an orphan for the runtime's orphan sweep
      // (`TmuxRuntime.reconcileOrphans`) to find.
      await server.run(argv(server, "kill-window", "-t", target));
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
  server: TmuxServer,
  session: string,
  name: string,
  environmentAndCommand: string[],
  owner: string
): Promise<{ windowId: string; paneId: string; pid: number }> {
  const sessionExists =
    (await server.run(argv(server, "has-session", "-t", session))).exitCode === 0;
  if (!sessionExists) {
    // One client invocation (`;` is tmux's command separator): the session exists with
    // `update-environment` already empty, so no attach can ever copy a client's environment into
    // it — not even one landing between the two commands.
    const create = await server.run(
      argv(
        server,
        "new-session",
        "-d",
        "-s",
        session,
        "-n",
        BOOTSTRAP_WINDOW,
        "sleep 3600",
        ";",
        ...disableEnvironmentUpdatesArgs(session)
      )
    );
    if (create.exitCode !== 0) {
      throw new Error(`tmux new-session failed (exit ${create.exitCode}): ${create.stdout}`);
    }
    await markOwner(server, session, owner, "session");
  }

  const command = argv(
    server,
    "new-window",
    "-P",
    "-F",
    "#{window_id} #{pane_id} #{pane_pid}",
    "-t",
    session,
    "-n",
    name,
    ...environmentAndCommand
  );
  const result = await server.run(command);
  if (!sessionExists) {
    const cleanup = await server.run(
      argv(server, "kill-window", "-t", `${session}:${BOOTSTRAP_WINDOW}`)
    );
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
  await markOwner(server, windowId, owner, "window");
  return { windowId, paneId, pid };
}

/** Splits a new pane into an existing window, tiling the layout afterward. Same single-invocation
 * capture rationale as `openWindow`. */
export async function splitWindow(
  server: TmuxServer,
  windowId: string,
  environmentAndCommand: string[]
): Promise<{ paneId: string; pid: number }> {
  const split = await server.run(
    argv(
      server,
      "split-window",
      "-t",
      windowId,
      "-P",
      "-F",
      "#{pane_id} #{pane_pid}",
      ...environmentAndCommand
    )
  );
  if (split.exitCode !== 0) {
    throw new Error(`tmux split-window failed (exit ${split.exitCode}): ${split.stdout}`);
  }
  const { paneId, pid } = parsePaneReport(split.stdout, "tmux split-window", false);
  await server.run(argv(server, "select-layout", "-t", windowId, "tiled"));
  return { paneId, pid };
}

/** What a tmux command's stderr says when the pane it targets provably does not exist -- and
 * neither does anything else on this daemon's private server: `can't find pane` (the pane was
 * reaped), or no server behind the private socket (`NO_SERVER_STDERR`: a socket file left behind
 * by an exited server, or a socket never created — a first boot after the upgrade runbook, or a
 * reboot that cleared `TMUX_TMPDIR`). No server on the daemon's own socket means no Legion pane.
 * Every other non-zero exit proves nothing about the pane. */
export const PANE_GONE_STDERR = new RegExp(`can't find pane|${NO_SERVER_STDERR.source}`);

/** `lookupPane`'s verdict. `absent` is a proof (the pane is not there); `failed` is the lack of
 * one -- the listing itself did not run to completion, so the pane may or may not exist -- and
 * a caller must never read it as either alive or gone. */
export type PaneLookup =
  | { status: "present"; pid: number }
  | { status: "absent" }
  | { status: "failed"; detail: string };

/** Looks up pane `paneId`'s live root pid. `list-panes -t` always lists the target's whole window
 * (a pane id resolves to its window; without `-a`/`-s` there is no single-pane listing), so the
 * pane-id column picks the row: a pane id absent from a successful listing is `absent`, never
 * approximated by a sibling's pid. A nonzero exit is `absent` only when its stderr matches
 * `PANE_GONE_STDERR`; any other nonzero exit (a client killed by the runner's timeout, a server
 * not responding) -- or a row whose pid does not parse -- is `failed`. */
export async function lookupPane(server: TmuxServer, paneId: string): Promise<PaneLookup> {
  const panes = await server.run(
    argv(server, "list-panes", "-t", paneId, "-F", "#{pane_id} #{pane_pid}")
  );
  if (panes.exitCode !== 0) {
    const stderr = panes.stderr?.trim() ?? "";
    if (PANE_GONE_STDERR.test(stderr)) return { status: "absent" };
    return {
      status: "failed",
      detail: `list-panes -t ${paneId} exited ${panes.exitCode}${stderr ? `: ${stderr}` : ""}`,
    };
  }
  const row = panes.stdout
    .split(/\r?\n/)
    .map((line) => line.trim().split(/\s+/))
    .find((r) => r[0] === paneId);
  if (!row) return { status: "absent" };
  const pid = Number(row[1]);
  if (!Number.isSafeInteger(pid) || pid <= 0) {
    return {
      status: "failed",
      detail: `list-panes -t ${paneId} reported an unparseable pid for ${paneId}: ${row.join(" ")}`,
    };
  }
  return { status: "present", pid };
}

export async function killWindow(server: TmuxServer, windowId: string): Promise<void> {
  await server.run(argv(server, "kill-window", "-t", windowId));
}

/** Kills a single pane, leaving the rest of its window (and any sibling panes) intact. The caller
 * decides whether/how to surface a non-zero exit — this never throws. */
export async function killPane(
  server: TmuxServer,
  paneId: string
): Promise<{ exitCode: number; stderr?: string }> {
  const result = await server.run(argv(server, "kill-pane", "-t", paneId));
  return { exitCode: result.exitCode, stderr: result.stderr };
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
  server: TmuxServer,
  session: string,
  owner: string,
  known: ReadonlySet<string>
): Promise<UnknownOwnedWindow[]> {
  const windows = await server.run(
    argv(
      server,
      "list-windows",
      "-t",
      session,
      "-F",
      "#{window_id}\t#{@legion_owner}\t#{window_activity}"
    )
  );
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
 * Lists every pane, anywhere on this daemon's private tmux server, whose owning window is marked
 * with `@legion_owner === owner` (window options resolve through the pane's own window, exactly as
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
  server: TmuxServer,
  owner: string,
  known: ReadonlySet<string>
): Promise<UnknownPane[]> {
  const panes = await server.run(
    argv(
      server,
      "list-panes",
      "-a",
      "-F",
      "#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{pane_activity}"
    )
  );
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
