import { expect, test } from "bun:test";

import type { Agent, TmuxPlace } from "../../api/types";
import {
  DRAWING_STALE_AFTER_MS,
  type MachineModel,
  type MachinePane,
  machineList,
  machineModel,
} from "./machine-model";
import { parseTmuxLayout } from "./tmux-layout";

const NOW = 1_762_000_000_000;
const SOCKET = "/tmp/tmux-1000/default";
/** `%0` over `%1` beside `%2`, recorded from tmux 3.5a. */
const LAYOUT = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";

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

function panesOf(model: MachineModel): MachinePane[] {
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
  const model = machineModel(
    [
      session({ session_id: "first", tmux_place: place({ pane_id: "%1", server_pid: "100" }) }),
      session({ session_id: "second", tmux_place: place({ pane_id: "%1", server_pid: "200" }) }),
      session({
        session_id: "third",
        tmux_place: place({ server_pid: "300", socket: "/tmp/tmux-1000/other" }),
      }),
    ],
    "devbox",
    NOW
  );

  expect(model.servers.map((server) => [server.socket, server.serverPid, server.showPid])).toEqual([
    [SOCKET, "100", true],
    [SOCKET, "200", true],
    ["/tmp/tmux-1000/other", "300", false],
  ]);
  // `%1` on one server is not `%1` on the other, so both are drawn and neither is a duplicate.
  expect(panesOf(model).map((pane) => [pane.session.session_id, pane.otherSessions])).toEqual([
    ["first", 0],
    ["second", 0],
    ["third", 0],
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

  const panes = panesOf(machineModel([older, newer], "devbox", NOW));

  expect(panes).toHaveLength(1);
  expect(panes[0].session.session_id).toBe("newer");
  expect(panes[0].otherSessions).toBe(1);
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
    expect(panesOf(machineModel(agents, "devbox", NOW))[0].session.session_id).toBe("aaa");
  }
});

test("a server's windows are ordered by window index, its tmux sessions by name", () => {
  const model = machineModel(
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
    "devbox",
    NOW
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
  const model = machineModel(
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
    "devbox",
    NOW
  );

  expect(model.servers[0].sessions[0].windows[0].layout).toBe(LAYOUT);
});

test("a window whose every session dropped its layout carries none, so the page lists its panes", () => {
  const model = machineModel(
    [
      session({ session_id: "one", tmux_place: place({ pane_id: "%0", pane_index: 0 }) }),
      session({ session_id: "two", tmux_place: place({ pane_id: "%1", pane_index: 1 }) }),
    ],
    "devbox",
    NOW
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

  expect(machineModel(agents, "devbox", NOW).age).toEqual({
    ageMs: 200_000,
    oldestSeen: NOW - 200_000,
    stale: true,
  });
  expect(DRAWING_STALE_AFTER_MS).toBe(180_000);
  expect(machineModel(agents, "devbox", NOW - 100_000).age?.stale).toBe(false);
});

test("a machine whose sessions all report no place says so instead of drawing a tmux server", () => {
  const model = machineModel(
    [session({ session_id: "one" }), session({ last_seen: NOW, session_id: "two" })],
    "devbox",
    NOW
  );

  expect(model.noneInTmux).toBe(true);
  expect(model.servers).toEqual([]);
  expect(model.age).toBeUndefined();
  expect(model.outsideTmux.map((agent) => agent.session_id)).toEqual(["two", "one"]);
  expect(model.summary).toEqual({ host: "devbox", inTmux: 0, sessions: 2 });
});

test("a machine whose sessions report two directories is marked as a name many hosts may share", () => {
  const sameDirectory = [
    session({ session_id: "one", tmux_place: place({ pane_id: "%0" }) }),
    session({ session_id: "two", tmux_place: place({ pane_id: "%1" }) }),
  ];
  const twoDirectories = [sameDirectory[0], { ...sameDirectory[1], dir: "/src/other" }];

  expect(machineModel(sameDirectory, "devbox", NOW).sharedName).toBe(false);
  expect(machineModel(twoDirectories, "devbox", NOW).sharedName).toBe(true);
});

test("the model names a pane as the layout parser does, so a drawing can match the two", () => {
  const model = machineModel(
    [
      session({ session_id: "top", tmux_place: place({ layout: LAYOUT, pane_id: "%0" }) }),
      session({ session_id: "left", tmux_place: place({ pane_id: "%1", pane_index: 1 }) }),
      session({ session_id: "right", tmux_place: place({ pane_id: "%2", pane_index: 2 }) }),
    ],
    "devbox",
    NOW
  );
  const window = model.servers[0].sessions[0].windows[0];

  expect(parseTmuxLayout(window.layout ?? "")?.panes.map((pane) => pane.paneId)).toEqual(
    window.panes.map((pane) => pane.paneId)
  );
});

test("a session in another machine's panes is no part of this machine's model", () => {
  const model = machineModel(
    [
      session({ session_id: "here", tmux_place: place() }),
      session({ host: "laptop", session_id: "there", tmux_place: place({ pane_id: "%7" }) }),
    ],
    "devbox",
    NOW
  );

  expect(panesOf(model).map((pane) => pane.session.session_id)).toEqual(["here"]);
  expect(model.summary.sessions).toBe(1);
});

test("a pane carries the zoomed flag of the session drawn in it", () => {
  const model = machineModel(
    [
      session({ session_id: "plain", tmux_place: place({ pane_id: "%0" }) }),
      session({ session_id: "zoomed", tmux_place: place({ pane_id: "%1", zoomed: true }) }),
    ],
    "devbox",
    NOW
  );

  expect(panesOf(model).map((pane) => [pane.paneId, pane.zoomed])).toEqual([
    ["%0", false],
    ["%1", true],
  ]);
});
