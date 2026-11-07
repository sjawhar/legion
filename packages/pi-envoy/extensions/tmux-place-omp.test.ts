import { afterEach, expect, test } from "bun:test";
import { readFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Cleanup,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// Where a session sits in tmux, as the real Oh My Pi reports it (envoy-client's `readTmuxPlace`,
// the cached reading extensions/envoy.ts takes a registration's place from without waiting on it,
// and the `tmux_place` transport.ts puts in the subscribe body): a case starts a tmux server of
// its own, opens a window of three panes and runs the binary in one of them, then reads the place
// off the registration bodies the binary sends the stand-in listener. Nothing short of that
// proves it: the reading takes `TMUX` and `TMUX_PANE` from the environment but accepts the
// answer only from the server `TMUX` names, and only for a pane whose own process is one of this
// process's ancestors, so a set of variables with no pane behind it is refused.
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server, as in
// card-delivery-omp.test.ts; a tmux on PATH is the third thing this file needs, since the session
// runs the one it finds there itself. A run without all three skips, except on GitHub Actions,
// where the pi-envoy job supplies them.
// This file imports no `nats`, so the package's shared test process serves it: envoy.test.ts's
// `mock.module("nats", …)` replaces that module for this process alone, and the session's NATS
// connection is the binary's own, in a process of its own, which no mock here reaches.
const omp = process.env.LEGION_TEST_OMP;
const natsUrl = process.env.LEGION_TEST_NATS_URL;
const tmuxBinary = Bun.which("tmux");
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = (omp === undefined || natsUrl === undefined || tmuxBinary === null) && !onActions;

/** The binary, NATS server and tmux a case runs against; throws on GitHub Actions without them. */
function setup(): { readonly binary: string; readonly nats: string; readonly tmux: string } {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  if (natsUrl === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
  if (tmuxBinary === null) throw new Error("no tmux is on PATH on GitHub Actions");
  return { binary: omp, nats: natsUrl, tmux: tmuxBinary };
}

/** The heartbeat the session re-registers on, each registration carrying whatever the cache
 *  holds as it is built. Three times the bound the reading holds its one tmux call to, so a
 *  loaded server still answers inside it, and short enough that a case waits seconds rather than
 *  the default half-minute. */
const HEARTBEAT_MS = 3_000;
/** How long one reading stands here, which is also the period the cache's own timer reads tmux
 *  on. It is short against `HEARTBEAT_MS`, so the window is read again within a fraction of a
 *  heartbeat after a split, and the registration after that reading carries the split window. */
const PLACE_CACHE_MS = 500;
/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 60_000;

/** What the case gives tmux, and holds the reported place to. The window is not the session's
 *  first and the pane is not the window's first, so an index reported from the wrong end of
 *  either shows up. */
const SESSION_NAME = "tmux-place-case";
const WINDOW_NAME = "the-window";
const WINDOW_INDEX = 4;
const PANE_INDEX = 1;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** `value` as one word of the shell tmux runs a pane's command through. */
function quote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/**
 * Runs one tmux command on the case's server, resolving with its stdout and failing on its
 * stderr. `environment` is what the first call's server runs under, and a pane inherits the
 * server's: this pod's own Envoy, profile and session variables must not reach the binary under
 * test. `-f /dev/null` keeps a tmux.conf on the machine out of the window's layout.
 */
async function tmuxCommand(
  binary: string,
  socket: string,
  args: readonly string[],
  environment: Record<string, string>
): Promise<string> {
  const child = Bun.spawn([binary, "-u", "-S", socket, "-f", "/dev/null", ...args], {
    env: environment,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [out, error, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`tmux ${args.join(" ")} failed (${code}): ${error.trim()}`);
  return out.replace(/\n$/, "");
}

/** The environment the case's tmux server, and with it every pane, runs under: the harness's own
 *  HOME and PATH, with tmux's directory first, since the reading runs `tmux` from inside the pane.
 *  `LD_LIBRARY_PATH` goes with it, as a tmux outside the system directories needs its libraries
 *  named the way whatever put it on PATH named them. */
function paneEnvironment(home: string, binary: string): Record<string, string> {
  const named = process.env.LD_LIBRARY_PATH;
  return {
    HOME: home,
    PATH: [path.dirname(binary), "/usr/local/bin", "/usr/bin", "/bin"].join(":"),
    ...(named === undefined ? {} : { LD_LIBRARY_PATH: named }),
  };
}

/**
 * What the pane runs: the pane's own `TMUX` and `TMUX_PANE` written out, so the case reads what
 * tmux told the session before the session reports it, and then the binary with only envoy.ts
 * loaded, the harness's flags (`spawnRpc`'s, which this pane stands in for) and its output in a
 * file a failure can show. `exec` leaves the binary as the pane's own first process, which is what
 * the reading accepts the pane as this session's by. RPC mode keeps the session sitting there with
 * its stdin on the pane's terminal: the case drives nothing, it only heartbeats.
 */
function paneCommand(options: {
  readonly binary: string;
  readonly extension: string;
  readonly workspace: string;
  readonly sessions: string;
  readonly listener: string;
  readonly nats: string;
  readonly environmentFile: string;
  readonly log: string;
}): string {
  const flags = [
    "--mode",
    "rpc",
    "--no-extensions",
    "-e",
    options.extension,
    "--no-skills",
    "--no-rules",
    "--no-lsp",
    "--no-title",
    "--session-dir",
    options.sessions,
    "--cwd",
    options.workspace,
  ];
  return [
    `printf '%s\\n%s\\n' "$TMUX" "$TMUX_PANE" > ${quote(options.environmentFile)}`,
    "&& exec env",
    ...Object.entries(sessionEnvironment(options.listener, options.nats)).map(
      ([name, value]) => `${name}=${quote(value)}`
    ),
    ...[options.binary, ...flags].map(quote),
    `> ${quote(options.log)} 2>&1`,
  ].join(" ");
}

/** What a case's session is told about Envoy, in the pane and outside it alike. */
function sessionEnvironment(listener: string, nats: string): Record<string, string> {
  return {
    ENVOY_URL: listener,
    ENVOY_NATS_URL: nats,
    ENVOY_HEARTBEAT_MS: String(HEARTBEAT_MS),
    ENVOY_TMUX_PLACE_CACHE_MS: String(PLACE_CACHE_MS),
  };
}

/**
 * One stand-in that is the model gateway, Dispatch and the Envoy listener, and a profile whose
 * every model role is on it. No case here prompts, so the registration route is the only one a
 * healthy run reaches; the others answer so that a session which does reach them is not held.
 */
async function standin(
  home: string,
  workspace: string
): Promise<{ readonly requests: Request[]; readonly base: string }> {
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "tmux-place-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);
  return { requests, base };
}

/** The registration bodies the stand-in has taken, in the order it took them. */
function registrations(requests: readonly Request[]): Record<string, unknown>[] {
  return requests
    .filter((request) => request.path === "/v1/interests/subscribe")
    .map((request) => request.body);
}

/** The place a registration carries, which a body without one fails on by name. */
function placeOf(body: Record<string, unknown>): Record<string, unknown> {
  const place = body.tmux_place;
  if (typeof place !== "object" || place === null) {
    throw new Error(`the registration carries no tmux_place: ${JSON.stringify(body)}`);
  }
  return place as Record<string, unknown>;
}

/** The layout a registration's place draws, which a place without one fails on by name. */
function layoutOf(body: Record<string, unknown>): string {
  const layout = placeOf(body).layout;
  if (typeof layout !== "string") {
    throw new Error(`the place names no layout: ${JSON.stringify(placeOf(body))}`);
  }
  return layout;
}

/** The layout a registration draws, where it carries a place at all: what a case waits on while
 *  registrations the session sent before a reading answered are still arriving. */
function layoutIfAny(body: Record<string, unknown>): string | undefined {
  const place = body.tmux_place;
  if (typeof place !== "object" || place === null) return undefined;
  const layout = (place as Record<string, unknown>).layout;
  return typeof layout === "string" ? layout : undefined;
}

/** The panes a `window_layout` string names, in its own order: every leaf cell is
 *  `<width>x<height>,<x>,<y>,<pane id>`, the id written without its `%`, while a cell holding
 *  other cells ends in `{` or `[` instead and names no pane. */
function layoutPanes(layout: string): string[] {
  return [...layout.matchAll(/\d+x\d+,\d+,\d+,(\d+)/g)].map(([, id]) => `%${id}`);
}

/** Polls `probe` until it returns a value, failing on `what` at the deadline with what `diagnose`
 *  has to say about the session that owed it. */
async function until<T>(
  what: string,
  probe: () => T | undefined,
  diagnose: () => Promise<string>
): Promise<T> {
  const deadline = Date.now() + DEADLINE_MS;
  for (;;) {
    const value = probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline)
      throw new Error(`timed out waiting for ${what}: ${await diagnose()}`);
    // A request reaching the stand-in has no event the case can await, so it is polled.
    await Bun.sleep(20);
  }
}

// The whole of what a pane's session reports: every member of the place against what the case gave
// tmux, and the layout against the window the case built. A member read from the wrong field of
// the tmux answer, or dropped on the way to the wire, shows here and nowhere else — `window_id`
// and `zoomed` are reported by nothing else at all. The split then proves the place is read afresh
// rather than worked out once: the window the session sits in grows a pane, and a later
// registration draws four. Which registration carries a place is not fixed: a registration takes
// whatever the cache holds when it is built and waits on no tmux call, so the session's first
// goes out before its first reading has answered and a heartbeat behind it carries the answer.
test.skipIf(skip)(
  "a session in a tmux pane registers the pane it sits in, and the split that follows it",
  async () => {
    const { binary, nats, tmux } = setup();
    const { root, home, workspace, sessions } = await ompRoot(binary, "tmux-place-omp-", cleanup);
    const { requests, base } = await standin(home, workspace);

    const socket = path.join(root, "tmux.sock");
    const environmentFile = path.join(root, "pane-environment");
    const log = path.join(root, "omp.log");
    const environment = paneEnvironment(home, tmux);
    const run = (args: readonly string[]): Promise<string> =>
      tmuxCommand(tmux, socket, args, environment);
    const window = `${SESSION_NAME}:${WINDOW_INDEX}`;
    const said = async (): Promise<string> =>
      `omp said ${JSON.stringify(await readFile(log, "utf8").catch(() => "nothing"))}`;

    await run(["new-session", "-d", "-s", SESSION_NAME, "-x", "200", "-y", "60"]);
    try {
      // The window the case holds the place to, and its three panes: the binary in the middle one,
      // so neither the window's nor the pane's index is the first tmux would report for anything.
      await run(["new-window", "-d", "-t", window, "-n", WINDOW_NAME, "sleep 600"]);
      await run([
        "split-window",
        "-d",
        "-t",
        `${window}.0`,
        paneCommand({
          binary,
          extension: path.join(import.meta.dir, "envoy.ts"),
          workspace,
          sessions,
          listener: base,
          nats,
          environmentFile,
          log,
        }),
      ]);
      await run(["split-window", "-d", "-t", `${window}.${PANE_INDEX}`, "sleep 600"]);

      const first = await until(
        "a registration carrying the session's place",
        () => registrations(requests).find((body) => body.tmux_place !== undefined),
        said
      );
      // What tmux told the pane, as the pane itself saw it: `TMUX` is
      // `<socket>,<server pid>,<session>`, and a socket path may hold a comma of its own.
      const [tmuxVariable = "", paneVariable = ""] = (
        await readFile(environmentFile, "utf8")
      ).split("\n");
      const serverPid = tmuxVariable.split(",").at(-2);
      const windowId = await run(["display-message", "-p", "-t", window, "#{window_id}"]);
      const threePanes = await run(["display-message", "-p", "-t", window, "#{window_layout}"]);
      expect(placeOf(first)).toEqual({
        socket,
        server_pid: serverPid,
        session_name: SESSION_NAME,
        window_id: windowId,
        window_index: WINDOW_INDEX,
        window_name: WINDOW_NAME,
        pane_id: paneVariable,
        pane_index: PANE_INDEX,
        layout: threePanes,
        zoomed: false,
      });
      expect(layoutPanes(layoutOf(first))).toEqual(
        (await run(["list-panes", "-t", window, "-F", "#{pane_id}"])).split("\n")
      );
      expect(layoutPanes(layoutOf(first))).toHaveLength(3);

      await run(["split-window", "-d", "-t", `${window}.2`, "sleep 600"]);
      // The registrations taken from here on read the window after the split: a reading stands
      // for `PLACE_CACHE_MS`, and this waits that out before it counts what the stand-in holds.
      // The cache reads the split window on its own timer inside that lifetime, so the first
      // registration past it carries what that reading found; a registration that overtakes the
      // reading carries the window as it was, and `until` waits for the one behind it.
      await Bun.sleep(PLACE_CACHE_MS);
      const taken = registrations(requests).length;
      const next = await until(
        "a registration carrying the window the split left",
        () =>
          registrations(requests)
            .slice(taken)
            .find((body) => {
              const layout = layoutIfAny(body);
              return layout !== undefined && layout !== threePanes;
            }),
        said
      );
      expect(layoutPanes(layoutOf(next))).toEqual(
        (await run(["list-panes", "-t", window, "-F", "#{pane_id}"])).split("\n")
      );
      expect(layoutPanes(layoutOf(next))).toHaveLength(4);
      // The pane the session sits in is the same one, under a layout that has moved around it.
      expect(placeOf(next).pane_id).toBe(paneVariable);
      expect(placeOf(next).pane_index).toBe(PANE_INDEX);
    } finally {
      await tmuxCommand(tmux, socket, ["kill-server"], environment).catch(() => undefined);
    }
  },
  180_000
);

// The same binary, started the way every other case of this package starts it: no pane, no `TMUX`,
// nothing to report. The place is left off the body rather than sent empty: the listener builds
// its entry fresh from each registration, so a body carrying no place leaves the session with
// none, which is what a session outside tmux is.
test.skipIf(skip)(
  "a session outside tmux registers no place at all",
  async () => {
    const { binary, nats } = setup();
    const { home, workspace, sessions } = await ompRoot(binary, "tmux-place-outside-omp-", cleanup);
    const { requests, base } = await standin(home, workspace);

    const closed: { error?: unknown } = {};
    const rpc = spawnRpc(
      binary,
      {
        extensions: [path.join(import.meta.dir, "envoy.ts")],
        home,
        workspace,
        sessions,
        env: sessionEnvironment(base, nats),
        onFrame: () => undefined,
      },
      cleanup
    );
    rpc.closed.catch((error: unknown) => {
      closed.error = error;
    });

    const first = await until(
      "the session's first registration with the listener",
      () => registrations(requests)[0],
      async () => String(closed.error ?? "omp is still running")
    );
    expect("tmux_place" in first).toBe(false);
    await rpc.end();
  },
  180_000
);
