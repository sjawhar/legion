import type { Agent, TmuxPlace } from "../../api/types";

/**
 * The model the machine pages draw, built from the agent list alone: `/machines` lists what
 * `machineList` returns, `/machines/<machine>` draws what `machineModel` returns. Both are pure —
 * nothing here fetches, and nothing knows React — so the page is a drawing of this value and the
 * rules below are tested without a browser.
 */

/**
 * How old the sessions a drawing is made of may be before the page says the drawing is behind.
 * Three minutes is the age of the report, not of the pane: a registration is at most 90 s old,
 * the route roster 5 s, the page's poll 15 s and the plugin's place cache 30 s, which is 140 s
 * and inside three minutes, so an older drawing is one that stopped being refreshed rather than
 * one the hops are behind on.
 */
export const DRAWING_STALE_AFTER_MS = 3 * 60_000;

/** One machine on `/machines`. */
export interface MachineSummary {
  /** `Agent.host`: the session's own machine, which is what the pages key a machine by. */
  readonly host: string;
  readonly sessions: number;
  /** How many of those sessions reported a tmux place. */
  readonly inTmux: number;
}

/** One pane of a window, with the session drawn in it. */
export interface MachinePane {
  /** `TmuxPlace.pane_id`, carrying its `%`, which is what `parseTmuxLayout` answers too. */
  readonly paneId: string;
  readonly paneIndex: number;
  readonly zoomed: boolean;
  /** The session shown in the pane: the one heard from most recently that named it. */
  readonly session: Agent;
  /** How many further sessions named this same pane. Two live sessions in one pane means one of
   *  them moved or ended without the registry hearing, so the page says how many rather than
   *  drawing a pane twice or picking silently. */
  readonly otherSessions: number;
}

export interface MachineWindow {
  readonly windowId: string;
  readonly windowIndex: number;
  readonly windowName: string;
  /**
   * The layout string to draw the window from: the one from the session in the window heard from
   * most recently that carries one. Absent when every session reporting the window dropped its
   * layout for length, and the page then lists the panes below instead of drawing them.
   */
  readonly layout?: string;
  readonly panes: readonly MachinePane[];
}

export interface MachineTmuxSession {
  readonly name: string;
  readonly windows: readonly MachineWindow[];
}

export interface MachineTmuxServer {
  readonly socket: string;
  readonly serverPid: string;
  /** True where another server on this machine answers on the same socket, so the heading must
   *  name the process id to tell the two apart; a socket that names one server needs no pid. */
  readonly showPid: boolean;
  readonly sessions: readonly MachineTmuxSession[];
}

/** How far behind the drawing is: its oldest row, and whether that is past the bound above. */
export interface MachineDrawingAge {
  /** The oldest `last_seen`, in epoch ms, among the sessions the drawing shows. */
  readonly oldestSeen: number;
  readonly ageMs: number;
  readonly stale: boolean;
}

export interface MachineModel {
  readonly summary: MachineSummary;
  /** Grouped by tmux server, then by tmux session name, then by window. */
  readonly servers: readonly MachineTmuxServer[];
  /** The machine's sessions that reported no tmux place, freshest first. */
  readonly outsideTmux: readonly Agent[];
  /** True when no session on the machine is in tmux: the page says so rather than drawing an
   *  empty machine. */
  readonly noneInTmux: boolean;
  /**
   * True when the machine's sessions report more than one directory, which says the name is a
   * logical one covering many hosts rather than one box: `host` is `ENVOY_MACHINE_ID` wherever a
   * deployment sets it, and 454 live sessions share one such name today. It is a hint, not proof
   * — one person's two sessions in two repositories look the same — so the page marks the name
   * rather than drawing a different machine.
   */
  readonly sharedName: boolean;
  /** Absent when the model drew no pane, since nothing was drawn to be behind. */
  readonly age?: MachineDrawingAge;
}

interface PlacedSession {
  readonly agent: Agent;
  readonly place: TmuxPlace;
}

/**
 * Freshest first, ties broken on session id. Every choice this model makes between two reports
 * uses it, so each choice is total and the page does not change between two polls that carry the
 * same rows.
 */
function freshestFirst(left: Agent, right: Agent): number {
  return right.last_seen - left.last_seen || left.session_id.localeCompare(right.session_id);
}

function groupBy<T>(rows: readonly T[], key: (row: T) => string): Map<string, T[]> {
  const groups = new Map<string, T[]>();
  for (const row of rows) {
    const id = key(row);
    const group = groups.get(id);
    if (group === undefined) {
      groups.set(id, [row]);
    } else {
      group.push(row);
    }
  }
  return groups;
}

/**
 * A tmux server is the pair (socket, server pid), never the socket alone: a socket path is reused
 * when a server is started again, and a pane id names a pane only on the server that gave it out,
 * so panes of two servers that share a socket must not be matched to each other. A socket path
 * holds no NUL, so joining on one is unambiguous.
 */
function serverKey(place: TmuxPlace): string {
  return `${place.socket}\u0000${place.server_pid}`;
}

function buildWindow(
  windowId: string,
  reports: PlacedSession[],
  panes: MachinePane[]
): MachineWindow {
  const [freshest] = reports;
  const withLayout = reports.find((report) => report.place.layout !== undefined);
  return {
    layout: withLayout?.place.layout,
    panes: [...panes].sort(
      (left, right) => left.paneIndex - right.paneIndex || left.paneId.localeCompare(right.paneId)
    ),
    windowId,
    windowIndex: freshest.place.window_index,
    windowName: freshest.place.window_name,
  };
}

function buildServer(key: string, reports: PlacedSession[]): Omit<MachineTmuxServer, "showPid"> {
  const [socket = "", serverPid = ""] = key.split("\u0000");
  const freshest = [...reports].sort((left, right) => freshestFirst(left.agent, right.agent));

  // One pane holds one session. Where two named it, the freshest is drawn and the rest counted.
  const panesByWindow = new Map<string, MachinePane[]>();
  for (const [paneId, named] of groupBy(freshest, (report) => report.place.pane_id)) {
    const [shown, ...others] = named;
    const pane: MachinePane = {
      otherSessions: others.length,
      paneId,
      paneIndex: shown.place.pane_index,
      session: shown.agent,
      zoomed: shown.place.zoomed,
    };
    const window = panesByWindow.get(shown.place.window_id);
    if (window === undefined) {
      panesByWindow.set(shown.place.window_id, [pane]);
    } else {
      window.push(pane);
    }
  }

  // A window's own fields come from every report naming it, the panes it draws from above: a
  // session whose pane another session took is still evidence of the window it sat in. A window
  // that only such reports name has no pane of its own to draw, and is left out.
  const windowsBySession = new Map<string, MachineWindow[]>();
  for (const [windowId, named] of groupBy(freshest, (report) => report.place.window_id)) {
    const panes = panesByWindow.get(windowId);
    if (panes === undefined) {
      continue;
    }
    const window = buildWindow(windowId, named, panes);
    const sessionName = named[0].place.session_name;
    const windows = windowsBySession.get(sessionName);
    if (windows === undefined) {
      windowsBySession.set(sessionName, [window]);
    } else {
      windows.push(window);
    }
  }

  const sessions = [...windowsBySession]
    .map(([name, windows]) => ({
      name,
      windows: windows.sort(
        (left, right) =>
          left.windowIndex - right.windowIndex || left.windowId.localeCompare(right.windowId)
      ),
    }))
    .sort((left, right) => left.name.localeCompare(right.name));
  return { serverPid, sessions, socket };
}

function drawingAge(
  servers: readonly MachineTmuxServer[],
  now: number
): MachineDrawingAge | undefined {
  let oldestSeen: number | undefined;
  for (const server of servers) {
    for (const session of server.sessions) {
      for (const window of session.windows) {
        for (const pane of window.panes) {
          if (oldestSeen === undefined || pane.session.last_seen < oldestSeen) {
            oldestSeen = pane.session.last_seen;
          }
        }
      }
    }
  }
  if (oldestSeen === undefined) {
    return undefined;
  }
  const ageMs = now - oldestSeen;
  return { ageMs, oldestSeen, stale: ageMs > DRAWING_STALE_AFTER_MS };
}

/** Every machine the agent list names, by name, with how many of its sessions are in tmux. */
export function machineList(agents: readonly Agent[]): MachineSummary[] {
  const counts = new Map<string, { inTmux: number; sessions: number }>();
  for (const agent of agents) {
    const count = counts.get(agent.host) ?? { inTmux: 0, sessions: 0 };
    count.sessions += 1;
    if (agent.tmux_place !== undefined) {
      count.inTmux += 1;
    }
    counts.set(agent.host, count);
  }
  return [...counts]
    .map(([host, count]) => ({ host, inTmux: count.inTmux, sessions: count.sessions }))
    .sort((left, right) => left.host.localeCompare(right.host));
}

/**
 * One machine's whole model: its servers, each server's tmux sessions and windows, and the
 * sessions on the machine that are in no pane. `now` is an argument so the drawing's age is the
 * caller's clock and the model stays pure.
 */
export function machineModel(agents: readonly Agent[], host: string, now: number): MachineModel {
  const onMachine = agents.filter((agent) => agent.host === host);
  const placed: PlacedSession[] = [];
  const outsideTmux: Agent[] = [];
  for (const agent of onMachine) {
    const place = agent.tmux_place;
    if (place === undefined) {
      outsideTmux.push(agent);
    } else {
      placed.push({ agent, place });
    }
  }
  outsideTmux.sort(freshestFirst);

  const built = [...groupBy(placed, (report) => serverKey(report.place))]
    .map(([key, reports]) => buildServer(key, reports))
    .sort(
      (left, right) =>
        left.socket.localeCompare(right.socket) || left.serverPid.localeCompare(right.serverPid)
    );
  const socketCounts = new Map<string, number>();
  for (const server of built) {
    socketCounts.set(server.socket, (socketCounts.get(server.socket) ?? 0) + 1);
  }
  const servers = built.map((server) => ({
    ...server,
    showPid: (socketCounts.get(server.socket) ?? 0) > 1,
  }));

  return {
    age: drawingAge(servers, now),
    noneInTmux: placed.length === 0,
    outsideTmux,
    servers,
    sharedName: new Set(onMachine.map((agent) => agent.dir)).size > 1,
    summary: { host, inTmux: placed.length, sessions: onMachine.length },
  };
}
