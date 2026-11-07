// Where a session sits in tmux, read from the server `TMUX` names and sent with every
// registration, so Dispatch can draw a machine's tmux layout and say which pane runs which
// session. One bounded call per reading: a busy shared tmux server must not hold up a heartbeat.

import { TMUX_LAYOUT_MAX, type TmuxPlace } from "@legion/contracts";

import { type ExecFn, TMUX_PANE_LOOKUP_MS, tryExec } from "./dispatch-cwd";
import { isAncestor } from "./process-ancestry";
import { tmuxServer } from "./tmux-server";

/** A session's pane and the window and tmux session holding it, in the wire names the listener
 *  and Dispatch read: the contract's own type, which the listener and the pages read it by.
 *  `layout` is tmux's `window_layout` string, which draws the window's split; it is absent when
 *  the window is split so finely that the string outgrew what a registration carries.
 *  A place crosses the wire outwards only: a registration carries the one this module reads
 *  (`transport.ts`'s `SubscribeInput.place`), and nothing here parses one back, so there is no
 *  wire schema for it — the contract's type is what holds both ends together.
 *  `@legion/contracts`' `Agent.tmux_place` is the same place as Dispatch answers it. */
export type { TmuxPlace } from "@legion/contracts";

/**
 * What one reading came to. The two ways of having no place are kept apart because they mean
 * opposite things to the caller: a tmux server that died makes its client exit non-zero at once,
 * so `refused` is a session that is no longer in a pane and whose entry must be cleared, while
 * `timedOut` says only that a loaded server has not answered yet, and the caller keeps the place
 * it last reported rather than dropping the session off the layout for one slow call.
 */
export type TmuxPlaceReading =
  | { readonly kind: "place"; readonly place: TmuxPlace }
  | { readonly kind: "refused" }
  | { readonly kind: "timedOut" };

// `pid` is the answering server's: a pane id names a pane only on the server that gave it out,
// and a server started again on the same socket gives out the same ids, so an answer is this
// pane's only when the server answering is the one `TMUX` names. `pane_pid` is the pane's first
// process: tmux hands a popup, `run-shell` job or hook a `TMUX_PANE` left over from wherever the
// server was started, so a pane is this session's only when its process is one of this process's
// ancestors. Neither is reported; both decide whether the rest is.
const PLACE_FORMAT = [
  "#{session_name}",
  "#{window_id}",
  "#{window_index}",
  "#{window_name}",
  "#{pane_id}",
  "#{pane_index}",
  "#{pid}",
  "#{pane_pid}",
  "#{window_layout}",
  "#{window_zoomed_flag}",
].join("\t");

/** How many fields `PLACE_FORMAT` asks for; an answer with any other count is from a tmux that
 *  expanded it differently, and nothing in it can be trusted to be what it looks like. */
const PLACE_FIELDS = 10;

/** The variables a reading consults, over whatever else the environment holds. */
type TmuxEnvironment = {
  readonly TMUX?: string;
  readonly TMUX_PANE?: string;
} & Record<string, string | undefined>;

/** A tmux index (`window_index`, `pane_index`) as a number; undefined for anything that is not
 *  one, an empty field included, which `Number` would read as 0. */
function tmuxIndex(value: string): number | undefined {
  return /^\d+$/.test(value) ? Number(value) : undefined;
}

/**
 * Where this session sits in tmux, from `TMUX` and `TMUX_PANE` and one
 * `tmux -u -S <socket> display-message -p -t <pane>`. Outside tmux nothing is run at all.
 * `-u` because the format is tab-separated and tmux prints a tab as `_` to a client that has
 * neither `TMUX` nor a UTF-8 locale in its environment.
 */
export async function readTmuxPlace(env: TmuxEnvironment, exec: ExecFn): Promise<TmuxPlaceReading> {
  const { serverPid, socket } = tmuxServer(env.TMUX);
  const pane = env.TMUX_PANE?.trim() ?? "";
  if (socket === "" || serverPid === "" || pane === "") return { kind: "refused" };

  // Spawned in the root, never the session's own directory: tmux reads nothing from the cwd here
  // — the socket `TMUX` names is absolute — while a directory removed under a live session would
  // make the spawn fail, which `tryExec` reports as `refused` and a caller reads as a session
  // that has left its pane. The root outlives every session, and a caller's `process.cwd()`
  // throws for a deleted one.
  const answer = await tryExec(
    exec,
    "tmux",
    ["-u", "-S", socket, "display-message", "-p", "-t", pane, PLACE_FORMAT],
    "/",
    TMUX_PANE_LOOKUP_MS
  );
  if (!answer.ok) return answer.failure === "timedOut" ? { kind: "timedOut" } : { kind: "refused" };

  const fields = answer.stdout.replace(/\r?\n$/, "").split("\t");
  if (fields.length !== PLACE_FIELDS) return { kind: "refused" };
  const [
    sessionName = "",
    windowId = "",
    windowIndex = "",
    windowName = "",
    paneId = "",
    paneIndex = "",
    pid = "",
    panePid = "",
    layout = "",
    zoomedFlag = "",
  ] = fields;
  if (pid !== serverPid || paneId === "" || !isAncestor(panePid)) return { kind: "refused" };
  const window = tmuxIndex(windowIndex);
  const index = tmuxIndex(paneIndex);
  if (window === undefined || index === undefined) return { kind: "refused" };

  return {
    kind: "place",
    place: {
      socket,
      server_pid: serverPid,
      session_name: sessionName,
      window_id: windowId,
      window_index: window,
      window_name: windowName,
      pane_id: paneId,
      pane_index: index,
      // Past the contract's `TMUX_LAYOUT_MAX` the place is still reported, without its layout:
      // the one cap this client and the page that parses a layout both hold to.
      ...(layout.length > TMUX_LAYOUT_MAX ? {} : { layout }),
      zoomed: zoomedFlag === "1",
    },
  };
}
