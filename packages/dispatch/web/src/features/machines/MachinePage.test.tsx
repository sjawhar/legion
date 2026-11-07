import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, TmuxPlace, UserAgentStates } from "../../api/types";
import { MachinePage } from "./MachinePage";

const HOST = "sami-devbox";
const SOCKET_A = "/tmp/a-socket";
const SOCKET_B = "/tmp/b-socket";
const THREE_PANES = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";

const PLACE: TmuxPlace = {
  layout: THREE_PANES,
  pane_id: "%0",
  pane_index: 0,
  server_pid: "4242",
  session_name: "legion",
  socket: SOCKET_A,
  window_id: "@1",
  window_index: 0,
  window_name: "agents",
  zoomed: false,
};

function session(sessionId: string, place: TmuxPlace | undefined, extra?: Partial<Agent>): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: HOST,
    last_activity: null,
    last_seen: Date.now() - 30_000,
    machine_id: HOST,
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title: `Agent ${sessionId.slice(0, 4)}`,
    tmux_place: place,
    ...extra,
  };
}

function id(letter: string): string {
  return letter.repeat(16);
}

function renderMachine(agents: readonly Agent[], host = HOST, agentState: UserAgentStates = {}) {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([...agents]);
  // The two reads the page takes its `Needs you` count from, which the shell already makes.
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue(agentState);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/machines/${encodeURIComponent(host)}`]}>
      <QueryClientProvider client={client}>
        <MachinePage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    close: () => {
      view.unmount();
      for (const spy of [listAgents, getInbox, whoAmI, getMyAgentState]) {
        spy.mockRestore();
      }
    },
    listAgents,
  };
}

test("a machine draws one part per tmux server, each with its windows and one drawn", async () => {
  const machine = renderMachine([
    session(id("a"), PLACE),
    session(id("b"), { ...PLACE, pane_id: "%1", pane_index: 1 }),
    session(id("c"), {
      ...PLACE,
      layout: "b262,80x24,0,0,5",
      pane_id: "%5",
      window_id: "@2",
      window_index: 1,
      window_name: "builds",
    }),
    session(id("d"), {
      ...PLACE,
      layout: "b262,80x24,0,0,1",
      pane_id: "%1",
      server_pid: "99",
      session_name: "daemon",
      socket: SOCKET_B,
      window_id: "@7",
      window_index: 0,
      window_name: "shim",
    }),
  ]);
  try {
    const servers = await screen.findAllByTestId("machine-server");

    expect(servers.map((server) => server.getAttribute("data-socket"))).toEqual([
      SOCKET_A,
      SOCKET_B,
    ]);
    const [first] = servers;
    // One socket, one server: the heading names the socket and needs no process id.
    expect(within(first).getByRole("heading").textContent).toBe(SOCKET_A);
    expect(
      within(first)
        .getAllByTestId("window-pick")
        .map((pick) => [pick.getAttribute("data-window"), pick.textContent])
    ).toEqual([
      ["@1", "legion:0 agents"],
      ["@2", "legion:1 builds"],
    ]);
    // No `?window=` in the address: the server's first window is drawn.
    expect(within(first).getByTestId("window-drawing").getAttribute("data-window")).toBe("@1");
    expect(
      within(first)
        .getAllByTestId("window-pane")
        .map((box) => box.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
    expect(within(servers[1]).getByTestId("window-drawing").getAttribute("data-window")).toBe("@7");
  } finally {
    machine.close();
  }
});

test("picking a window puts it in the address and draws it", async () => {
  const machine = renderMachine([
    session(id("a"), PLACE),
    session(id("c"), {
      ...PLACE,
      layout: "b262,80x24,0,0,5",
      pane_id: "%5",
      window_id: "@2",
      window_index: 1,
      window_name: "builds",
    }),
  ]);
  try {
    const picks = await screen.findAllByTestId("window-pick");

    // The address names the server as well as the window: the socket, the server's pid and the
    // window id, form-encoded.
    expect(picks.map((pick) => pick.getAttribute("href"))).toEqual([
      "/machines/sami-devbox?window=%2Ftmp%2Fa-socket%3A4242%3A%401",
      "/machines/sami-devbox?window=%2Ftmp%2Fa-socket%3A4242%3A%402",
    ]);
    expect(picks.map((pick) => pick.getAttribute("data-picked"))).toEqual(["true", "false"]);

    fireEvent.click(picks[1]);

    await waitFor(() =>
      expect(screen.getByTestId("window-drawing").getAttribute("data-window")).toBe("@2")
    );
    expect(
      screen.getAllByTestId("window-pick").map((pick) => pick.getAttribute("data-picked"))
    ).toEqual(["false", "true"]);
  } finally {
    machine.close();
  }
});

test("a window id two servers both hold picks one window, on the server whose strip it was picked in", async () => {
  // Each server holds a `@0` and a `@1`, which is what tmux gives: it numbers windows per
  // server, so the ids of a person's own server and of a Legion daemon's beside it repeat.
  const machine = renderMachine([
    session(id("a"), {
      ...PLACE,
      layout: "b262,80x24,0,0,0",
      pane_id: "%0",
      window_id: "@0",
      window_index: 0,
      window_name: "shell",
    }),
    session(id("b"), {
      ...PLACE,
      layout: "b262,80x24,0,0,1",
      pane_id: "%1",
      window_id: "@1",
      window_index: 1,
      window_name: "agents",
    }),
    session(id("c"), {
      ...PLACE,
      layout: "b262,80x24,0,0,9",
      pane_id: "%9",
      server_pid: "9001",
      session_name: "daemon",
      socket: SOCKET_B,
      window_id: "@0",
      window_index: 0,
      window_name: "daemon",
    }),
    session(id("d"), {
      ...PLACE,
      layout: "b262,80x24,0,0,8",
      pane_id: "%8",
      server_pid: "9001",
      session_name: "daemon",
      socket: SOCKET_B,
      window_id: "@1",
      window_index: 1,
      window_name: "builds",
    }),
  ]);
  try {
    const servers = await screen.findAllByTestId("machine-server");
    const [person, daemon] = servers;
    expect(servers.map((server) => server.getAttribute("data-socket"))).toEqual([
      SOCKET_A,
      SOCKET_B,
    ]);
    // With nothing in the address, each server draws its own first window.
    expect(within(person).getByTestId("window-drawing").getAttribute("data-window")).toBe("@0");
    expect(within(daemon).getByTestId("window-drawing").getAttribute("data-window")).toBe("@0");

    fireEvent.click(within(person).getAllByTestId("window-pick")[1]);

    // The person's server draws its own `@1`: the pane that window holds, and no other.
    await waitFor(() =>
      expect(within(person).getByTestId("window-drawing").getAttribute("data-window")).toBe("@1")
    );
    expect(
      within(person)
        .getAllByTestId("window-pane")
        .map((box) => box.getAttribute("data-pane"))
    ).toEqual(["%1"]);
    // The daemon's server is where it was: its own `@1` was not picked, and its first window
    // is still drawn and still the highlighted one.
    expect(within(daemon).getByTestId("window-drawing").getAttribute("data-window")).toBe("@0");
    expect(
      within(daemon)
        .getAllByTestId("window-pick")
        .map((pick) => pick.getAttribute("data-picked"))
    ).toEqual(["true", "false"]);
  } finally {
    machine.close();
  }
});

test("two tmux servers on one socket are told apart by their process ids", async () => {
  const machine = renderMachine([
    session(id("a"), { ...PLACE, layout: "b262,80x24,0,0,0" }),
    session(id("b"), {
      ...PLACE,
      layout: "b262,80x24,0,0,3",
      pane_id: "%3",
      server_pid: "7777",
      window_id: "@9",
    }),
  ]);
  try {
    const servers = await screen.findAllByTestId("machine-server");

    expect(servers.map((server) => within(server).getByRole("heading").textContent)).toEqual([
      `${SOCKET_A} · pid 4242`,
      `${SOCKET_A} · pid 7777`,
    ]);
  } finally {
    machine.close();
  }
});

test("a machine whose sessions are in no pane says so and opens the Agents page for it", async () => {
  const machine = renderMachine([session(id("a"), undefined), session(id("b"), undefined)]);
  try {
    expect((await screen.findByTestId("machine-none-in-tmux")).textContent).toBe(
      "No session on this machine is in tmux, so there is no layout to draw."
    );
    expect(screen.getByRole("link", { name: "Agents on this machine" }).getAttribute("href")).toBe(
      "/agents?machine=sami-devbox"
    );
    expect(screen.queryByTestId("machine-server")).toBeNull();
    expect(screen.queryByTestId("machine-drawing-age")).toBeNull();
  } finally {
    machine.close();
  }
});

test("a machine no session names says that, rather than drawing nothing", async () => {
  const machine = renderMachine([session(id("a"), PLACE)], "other-box");
  try {
    expect((await screen.findByTestId("machine-unknown")).textContent).toBe(
      "No live session reports this machine, so there is nothing to draw."
    );
    expect(screen.queryByTestId("machine-server")).toBeNull();
  } finally {
    machine.close();
  }
});

test("a drawn server says it holds only the windows that hold an agent", async () => {
  const machine = renderMachine([session(id("a"), PLACE)]);
  try {
    expect((await screen.findByTestId("machine-windows-with-agents")).textContent).toBe(
      "This server shows only the windows that hold an agent: a window with no agent reports nothing."
    );
  } finally {
    machine.close();
  }
});

test("the page states the age of its oldest report and marks a stale drawing", async () => {
  const fresh = renderMachine([session(id("a"), PLACE, { last_seen: Date.now() - 30_000 })]);
  try {
    expect((await screen.findByTestId("machine-drawing-age")).textContent).toBe(
      "Oldest report drawn: just now."
    );
    expect(screen.queryByTestId("machine-drawing-stale")).toBeNull();
  } finally {
    fresh.close();
  }

  const stale = renderMachine([
    session(id("a"), PLACE, { last_seen: Date.now() - 4 * 60_000 }),
    session(id("b"), { ...PLACE, pane_id: "%1", pane_index: 1 }),
  ]);
  try {
    expect((await screen.findByTestId("machine-drawing-age")).textContent).toBe(
      "Oldest report drawn: 4 minutes ago."
    );
    expect((await screen.findByTestId("machine-drawing-stale")).textContent).toBe(
      "This drawing is stale: its oldest report is over three minutes old."
    );
  } finally {
    stale.close();
  }
});

test("a machine name several directories share is said to be a deployment's name", async () => {
  const machine = renderMachine([
    session(id("a"), PLACE, { dir: "/one" }),
    session(id("b"), { ...PLACE, pane_id: "%1", pane_index: 1 }, { dir: "/two" }),
  ]);
  try {
    expect((await screen.findByTestId("machine-shared-name")).textContent).toBe(
      "This machine name is one a deployment chose, not one host: its sessions report more than one directory."
    );
  } finally {
    machine.close();
  }
});

test("a pane whose session the viewer has unread replies from reads Needs you", async () => {
  const machine = renderMachine([session(id("a"), PLACE)], HOST, {
    [id("a")]: { unread_replies: 1 },
  });
  try {
    const state = await screen.findByTestId("window-pane-state");

    await waitFor(() => expect(state.getAttribute("data-state")).toBe("needs-you"));
    expect(state.textContent).toBe("Needs you");
  } finally {
    machine.close();
  }
});
