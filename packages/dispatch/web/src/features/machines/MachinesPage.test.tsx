import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, TmuxPlace } from "../../api/types";
import { MachinesPage } from "./MachinesPage";

const PLACE: TmuxPlace = {
  layout: "b262,80x24,0,0,5",
  pane_id: "%5",
  pane_index: 0,
  server_pid: "4242",
  session_name: "legion",
  socket: "/tmp/tmux-1000/default",
  window_id: "@1",
  window_index: 0,
  window_name: "agents",
  zoomed: false,
};

function session(sessionId: string, host: string, place?: TmuxPlace): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host,
    last_activity: null,
    last_seen: Date.now() - 30_000,
    machine_id: host,
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title: `Agent ${sessionId}`,
    tmux_place: place,
  };
}

function renderMachines() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={["/machines"]}>
      <QueryClientProvider client={client}>
        <MachinesPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("every machine the agent list names is a row with its counts and a link to its page", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    session("a".repeat(16), "sami-devbox", PLACE),
    session("b".repeat(16), "sami-devbox", { ...PLACE, pane_id: "%6", pane_index: 1 }),
    session("c".repeat(16), "sami-devbox"),
    session("d".repeat(16), "legion-infra-daemon"),
  ]);
  const view = renderMachines();
  try {
    const rows = await screen.findAllByTestId("machine-row");

    expect(rows.map((row) => row.getAttribute("data-machine"))).toEqual([
      "legion-infra-daemon",
      "sami-devbox",
    ]);
    const [daemon, devbox] = rows;
    expect([daemon.getAttribute("data-sessions"), daemon.getAttribute("data-in-tmux")]).toEqual([
      "1",
      "0",
    ]);
    expect(within(daemon).getByText("1 session · 0 in a pane")).toBeDefined();
    expect([devbox.getAttribute("data-sessions"), devbox.getAttribute("data-in-tmux")]).toEqual([
      "3",
      "2",
    ]);
    expect(within(devbox).getByText("3 sessions · 2 in a pane")).toBeDefined();
    expect(within(devbox).getByRole("link").getAttribute("href")).toBe("/machines/sami-devbox");
    // The one shared registry read, through the one poller every page reads behind.
    expect(agents).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a machine name that needs encoding is encoded in its link", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    session("a".repeat(16), "example-host-devbox.example-tailnet.ts.net"),
    session("b".repeat(16), "box one/two"),
  ]);
  const view = renderMachines();
  try {
    const rows = await screen.findAllByTestId("machine-row");

    expect(rows.map((row) => within(row).getByRole("link").getAttribute("href"))).toEqual([
      "/machines/box%20one%2Ftwo",
      "/machines/example-host-devbox.example-tailnet.ts.net",
    ]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a list with no agent in it says that nobody is connected", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = renderMachines();
  try {
    expect(
      await screen.findByText("No agent is connected, so no machine is listed.")
    ).toBeDefined();
    expect(screen.queryByTestId("machine-list")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});
