import { expect, test } from "bun:test";

import type { Agent, TmuxPlace } from "../../api/types";
import {
  chosenWindowId,
  DRAWING_STALE_AFTER_MS,
  type MachineGrouping,
  type MachinePane,
  machineDrawingAge,
  machineGrouping,
  machineList,
  windowChoice,
} from "./machine-model";
import { parseTmuxLayout } from "./tmux-layout";

const NOW = 1_762_000_000_000;
const SOCKET = "/tmp/tmux-1000/default";
/** `%0` over `%1` beside `%2`, recorded from tmux 3.5a. */
const LAYOUT = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
/** The same window with its lower right pane split in two: the pane `%7` is the new one. */
const SPLIT_LAYOUT =
  "c8d2,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13[39x5,41,13,2,39x5,41,19,7]}]";

function place(overrides: Partial<TmuxPlace> = {}): TmuxPlace {
  return {
    pane_id: "%0",
    pane_index: 0,
    server_pid: "4242",
    session_name: "work",
    socket: SOCKET,
    window_id: "@1",
    window_index: 1,
    window_name: "edit",
    zoomed: false,
    ...overrides,
  };
}

function session(overrides: Partial<Agent> & { session_id: string }): Agent {
  return {
    capabilities: [],
    dir: "/src/legion",
    host: "devbox",
    last_activity: null,
    last_seen: NOW - 30_000,
    machine_id: "listener",
    open_asks: 0,
    roles: [],
    title: overrides.session_id,
    ...overrides,
  };
}

function panesOf(model: MachineGrouping): MachinePane[] {
  return model.servers.flatMap((server) =>
    server.sessions.flatMap((tmuxSession) => tmuxSession.windows.flatMap((window) => window.panes))
  );
}

test("the machine list counts each machine's sessions and how many of them are in tmux", () => {
  const agents = [
    session({ session_id: "in-pane", tmux_place: place() }),
    session({ session_id: "no-pane" }),
    session({ host: "laptop", session_id: "elsewhere", tmux_place: place() }),
  ];

  expect(machineList(agents)).toEqual([
    { host: "devbox", inTmux: 1, sessions: 2 },
    { host: "laptop", inTmux: 1, sessions: 1 },
  ]);
});

test("two tmux servers on one machine are two groups, named by socket and by pid where they share one", () => {
  const model = machineGrouping(
    [
      session({ session_id: "first", tmux_place: place({ pane_id: "%1", server_pid: "100" }) }),
      session({ session_id: "second", tmux_place: place({ pane_id: "%1", server_pid: "200" }) }),
      session({
        session_id: "third",
        tmux_place: place({ server_pid: "300", socket: "/tmp/tmux-1000/other" }),
      }),
    ],
    "devbox"
  );

  expect(model.servers.map((server) => [server.socket, server.serverPid, server.showPid])).toEqual([
    [SOCKET, "100", true],
    [SOCKET, "200", true],
    ["/tmp/tmux-1000/other", "300", false],
  ]);
  // `%1` on one server is not `%1` on the other, so both are drawn and neither is a duplicate.
  expect(
    panesOf(model).map((pane) => [
      pane.session.session_id,
      pane.otherSessions.map((other) => other.session_id),
    ])
  ).toEqual([
    ["first", []],
    ["second", []],
    ["third", []],
  ]);
});

test("a pane two sessions name is drawn by the one heard from most recently, which counts the rest", () => {
  const older = session({
    last_seen: NOW - 120_000,
    session_id: "older",
    tmux_place: place({ pane_id: "%3" }),
  });
  const newer = session({
    last_seen: NOW - 20_000,
    session_id: "newer",
    tmux_place: place({ pane_id: "%3" }),
  });

  const panes = panesOf(machineGrouping([older, newer], "devbox"));

  expect(panes).toHaveLength(1);
  expect(panes[0].session.session_id).toBe("newer");
  expect(panes[0].otherSessions.map((other) => other.session_id)).toEqual(["older"]);
});

test("two sessions heard from in the same millisecond settle on session id, so the page is steady", () => {
  const first = session({
    last_seen: NOW,
    session_id: "aaa",
    tmux_place: place({ pane_id: "%3" }),
  });
  const second = session({
    last_seen: NOW,
    session_id: "bbb",
    tmux_place: place({ pane_id: "%3" }),
  });

  for (const agents of [
    [first, second],
    [second, first],
  ]) {
    expect(panesOf(machineGrouping(agents, "devbox"))[0].session.session_id).toBe("aaa");
  }
});

test("a server's windows are ordered by window index, its tmux sessions by name", () => {
  const model = machineGrouping(
    [
      session({
        session_id: "third",
        tmux_place: place({ pane_id: "%3", window_id: "@9", window_index: 3 }),
      }),
      session({
        session_id: "first",
        tmux_place: place({ pane_id: "%1", window_id: "@2", window_index: 1 }),
      }),
      session({
        session_id: "second",
        tmux_place: place({ pane_id: "%2", window_id: "@7", window_index: 2 }),
      }),
      session({
        session_id: "other-tmux-session",
        tmux_place: place({ pane_id: "%4", session_name: "admin", window_id: "@4" }),
      }),
    ],
    "devbox"
  );

  expect(
    model.servers[0].sessions.map((tmuxSession) => [
      tmuxSession.name,
      tmuxSession.windows.map((window) => window.windowIndex),
    ])
  ).toEqual([
    ["admin", [1]],
    ["work", [1, 2, 3]],
  ]);
});

test("a window is drawn from the layout of the session in it heard from most recently that carries one", () => {
  const model = machineGrouping(
    [
      session({
        last_seen: NOW - 10_000,
        session_id: "freshest-without-layout",
        tmux_place: place({ pane_id: "%0" }),
      }),
      session({
        last_seen: NOW - 20_000,
        session_id: "fresher-layout",
        tmux_place: place({ layout: LAYOUT, pane_id: "%1" }),
      }),
      session({
        last_seen: NOW - 60_000,
        session_id: "older-layout",
        tmux_place: place({ layout: "b262,80x24,0,0,5", pane_id: "%2" }),
      }),
    ],
    "devbox"
  );

  expect(model.servers[0].sessions[0].windows[0].layout).toBe(LAYOUT);
});

test("a window whose every session dropped its layout carries none, so the page lists its panes", () => {
  const model = machineGrouping(
    [
      session({ session_id: "one", tmux_place: place({ pane_id: "%0", pane_index: 0 }) }),
      session({ session_id: "two", tmux_place: place({ pane_id: "%1", pane_index: 1 }) }),
    ],
    "devbox"
  );
  const window = model.servers[0].sessions[0].windows[0];

  expect(window.layout).toBeUndefined();
  expect(window.panes.map((pane) => pane.paneId)).toEqual(["%0", "%1"]);
});

test("the drawing's age is the oldest pane it drew, and over three minutes it is behind", () => {
  const agents = [
    session({ last_seen: NOW - 20_000, session_id: "fresh", tmux_place: place({ pane_id: "%0" }) }),
    session({
      last_seen: NOW - 200_000,
      session_id: "oldest-drawn",
      tmux_place: place({ pane_id: "%1" }),
    }),
    // Outside tmux and ancient: nothing was drawn for it, so it ages no drawing.
    session({ last_seen: NOW - 86_400_000, session_id: "outside" }),
  ];

  const grouping = machineGrouping(agents, "devbox");
  expect(machineDrawingAge(grouping, NOW)).toEqual({
    oldestSeen: NOW - 200_000,
    stale: true,
  });
  expect(DRAWING_STALE_AFTER_MS).toBe(180_000);
  expect(machineDrawingAge(grouping, NOW - 100_000)?.stale).toBe(false);
});

test("a machine whose sessions all report no place says so instead of drawing a tmux server", () => {
  const model = machineGrouping(
    [session({ session_id: "one" }), session({ last_seen: NOW, session_id: "two" })],
    "devbox"
  );

  expect(model.noneInTmux).toBe(true);
  expect(model.servers).toEqual([]);
  expect(machineDrawingAge(model, NOW)).toBeUndefined();
  expect(model.summary).toEqual({ host: "devbox", inTmux: 0, sessions: 2 });
});

test("a machine whose sessions report two directories is marked as a name many hosts may share", () => {
  const sameDirectory = [
    session({ session_id: "one", tmux_place: place({ pane_id: "%0" }) }),
    session({ session_id: "two", tmux_place: place({ pane_id: "%1" }) }),
  ];
  const twoDirectories = [sameDirectory[0], { ...sameDirectory[1], dir: "/src/other" }];

  expect(machineGrouping(sameDirectory, "devbox").sharedName).toBe(false);
  expect(machineGrouping(twoDirectories, "devbox").sharedName).toBe(true);
});

test("the model names a pane as the layout parser does, so a drawing can match the two", () => {
  const model = machineGrouping(
    [
      session({ session_id: "top", tmux_place: place({ layout: LAYOUT, pane_id: "%0" }) }),
      session({ session_id: "left", tmux_place: place({ pane_id: "%1", pane_index: 1 }) }),
      session({ session_id: "right", tmux_place: place({ pane_id: "%2", pane_index: 2 }) }),
    ],
    "devbox"
  );
  const window = model.servers[0].sessions[0].windows[0];

  expect(parseTmuxLayout(window.layout ?? "")?.panes.map((pane) => pane.paneId)).toEqual(
    window.panes.map((pane) => pane.paneId)
  );
});

test("a session in another machine's panes is no part of this machine's model", () => {
  const model = machineGrouping(
    [
      session({ session_id: "here", tmux_place: place() }),
      session({ host: "laptop", session_id: "there", tmux_place: place({ pane_id: "%7" }) }),
    ],
    "devbox"
  );

  expect(panesOf(model).map((pane) => pane.session.session_id)).toEqual(["here"]);
  expect(model.summary.sessions).toBe(1);
});

test("a pane carries the zoomed flag of the session drawn in it", () => {
  const model = machineGrouping(
    [
      session({ session_id: "plain", tmux_place: place({ pane_id: "%0" }) }),
      session({ session_id: "zoomed", tmux_place: place({ pane_id: "%1", zoomed: true }) }),
    ],
    "devbox"
  );

  expect(panesOf(model).map((pane) => [pane.paneId, pane.zoomed])).toEqual([
    ["%0", false],
    ["%1", true],
  ]);
});

test("a pane the layout the window is drawn from does not name is still one of the window's panes", () => {
  // Each session reports the layout it last read, so the report heard from most recently can
  // carry a layout older than the window: here the three-pane string, read before the split
  // that made `%7`. The window is drawn from that string, and the model still holds `%7`, so
  // the page has the session to name (dispatch://INFRA-883, k11).
  const model = machineGrouping(
    [
      session({
        last_seen: NOW,
        session_id: "top",
        tmux_place: place({ layout: LAYOUT, pane_id: "%0" }),
      }),
      session({
        last_seen: NOW - 20_000,
        session_id: "left",
        tmux_place: place({ layout: LAYOUT, pane_id: "%1", pane_index: 1 }),
      }),
      session({
        last_seen: NOW - 20_000,
        session_id: "right",
        tmux_place: place({ layout: LAYOUT, pane_id: "%2", pane_index: 2 }),
      }),
      session({
        last_seen: NOW - 1_000,
        session_id: "just-split",
        tmux_place: place({ layout: SPLIT_LAYOUT, pane_id: "%7", pane_index: 3 }),
      }),
    ],
    "devbox"
  );
  const window = model.servers[0].sessions[0].windows[0];

  expect(window.layout).toBe(LAYOUT);
  expect(parseTmuxLayout(window.layout ?? "")?.panes.map((pane) => pane.paneId)).toEqual([
    "%0",
    "%1",
    "%2",
  ]);
  expect(window.panes.map((pane) => [pane.paneId, pane.session.session_id])).toEqual([
    ["%0", "top"],
    ["%1", "left"],
    ["%2", "right"],
    ["%7", "just-split"],
  ]);
});

test("every session the model counts in a pane is a pane's own session or one of that pane's others", () => {
  // The whole of R4's accounting rule in one claim: the header's `N in a pane` is
  // `summary.inTmux`, and these two are the only places the model puts such a session, so a
  // page that names both names every session it counts. The rows are one of each case: two
  // panes of one window, a pane of a second window, a pane two sessions name, and a session in
  // no pane at all.
  const agents = [
    session({ session_id: "top", tmux_place: place({ layout: LAYOUT, pane_id: "%0" }) }),
    session({ session_id: "left", tmux_place: place({ pane_id: "%1", pane_index: 1 }) }),
    session({
      last_seen: NOW - 10_000,
      session_id: "shown-in-%2",
      tmux_place: place({ pane_id: "%2", pane_index: 2 }),
    }),
    session({
      last_seen: NOW - 150_000,
      session_id: "also-names-%2",
      tmux_place: place({ pane_id: "%2", pane_index: 2 }),
    }),
    session({
      session_id: "other-window",
      tmux_place: place({ pane_id: "%4", window_id: "@2", window_index: 2 }),
    }),
    session({ session_id: "outside-tmux" }),
  ];
  const model = machineGrouping(agents, "devbox");

  const accounted = panesOf(model).flatMap((pane) => [
    pane.session.session_id,
    ...pane.otherSessions.map((other) => other.session_id),
  ]);
  expect([...accounted].sort()).toEqual([
    "also-names-%2",
    "left",
    "other-window",
    "shown-in-%2",
    "top",
  ]);
  expect(accounted).toHaveLength(model.summary.inTmux);
});

test("a window choice names its own server, so one window id on two servers picks one window", () => {
  // The machine the spec describes: a person's own server and a Legion daemon's beside it,
  // each with a window `@1` of its own, because tmux numbers windows per server.
  const person = windowChoice(SOCKET, "100", "@1");
  const daemon = windowChoice("/var/run/legion/tmux-daemon.sock", "200", "@1");

  expect(person).not.toBe(daemon);
  expect(chosenWindowId(person, SOCKET, "100")).toBe("@1");
  // The other server reads no choice of its own from it, so it draws its first window.
  expect(chosenWindowId(person, "/var/run/legion/tmux-daemon.sock", "200")).toBeUndefined();
  // Two servers on one socket are told apart by their pids here too.
  expect(chosenWindowId(person, SOCKET, "200")).toBeUndefined();
});

test("a window choice survives the address, socket path and all, and anything else names no window", () => {
  // A socket path holds `/`, and may hold a comma or a colon; the value is written and read
  // back through the address, so what the page compares is what the link wrote.
  const socket = "/tmp/tmux-1000/odd,name:2";
  const choice = windowChoice(socket, "4242", "@11");
  const roundTripped = new URLSearchParams(`?${new URLSearchParams({ window: choice })}`).get(
    "window"
  );

  expect(roundTripped).toBe(choice);
  expect(chosenWindowId(roundTripped ?? "", socket, "4242")).toBe("@11");
  // An absent choice, one of another shape, and one naming the server but no window: each
  // leaves the server to draw its first window.
  for (const value of ["", "@11", `${socket}:4242:`, `${socket}:4242`]) {
    expect(chosenWindowId(value, socket, "4242")).toBeUndefined();
  }
});
