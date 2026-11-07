import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent } from "../../api/types";
import type { MachinePane, MachineWindow } from "./machine-model";
import { type PaneCounts, WindowDrawing } from "./WindowDrawing";

/** A viewer who is owed nothing by any session: the drawing takes the counts, it never reads
 *  them, so a case that is not about the first state hands it two empty maps. */
const NOTHING_OWED: PaneCounts = { asksForYou: new Map(), unreadReplies: new Map() };

/** The three-pane window this design recorded from tmux 3.5a: `%0` over `%1` beside `%2`. */
const THREE_PANES = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";

const NOW = 1_762_000_000_000;

function session(sessionId: string, title: string, lastSeen = NOW - 30_000): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "sami-devbox",
    last_activity: null,
    last_seen: lastSeen,
    machine_id: "sami-devbox",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

function pane(paneId: string, paneIndex: number, agent: Agent, extra?: Partial<MachinePane>) {
  return {
    otherSessions: [],
    paneId,
    paneIndex,
    session: agent,
    zoomed: false,
    ...extra,
  } satisfies MachinePane;
}

function window(panes: readonly MachinePane[], layout?: string): MachineWindow {
  return { layout, panes, windowId: "@1", windowIndex: 2, windowName: "agents" };
}

/** happy-dom lays nothing out and has no `ResizeObserver`, so every case drives the component
 *  through its `width` seam. */
function draw(node: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

function boxes(): HTMLElement[] {
  return screen.getAllByTestId("window-pane");
}

test("a window is drawn at its own aspect ratio, each pane at the layout's percentages", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    expect(screen.getByTestId("window-drawing").getAttribute("data-drawn")).toBe("layout");
    // 80 columns by 24 rows, in cells one wide and two tall: 1440 × 2 × 24 / 80.
    const layout = screen.getByTestId("window-layout");
    expect(layout.style.height).toBe("864px");
    expect([layout.getAttribute("data-columns"), layout.getAttribute("data-rows")]).toEqual([
      "80",
      "24",
    ]);

    const [top, left, right] = boxes();
    expect(boxes().map((box) => box.getAttribute("data-pane"))).toEqual(["%0", "%1", "%2"]);
    expect([top.style.left, top.style.top, top.style.width, top.style.height]).toEqual([
      "0%",
      "0%",
      "100%",
      `${(12 / 24) * 100}%`,
    ]);
    expect([left.style.left, left.style.top, left.style.width, left.style.height]).toEqual([
      "0%",
      `${(13 / 24) * 100}%`,
      "50%",
      `${(11 / 24) * 100}%`,
    ]);
    expect([right.style.left, right.style.width]).toEqual([
      `${(41 / 80) * 100}%`,
      `${(39 / 80) * 100}%`,
    ]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane names its session and its state; one with no session says so and shows its index", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder", NOW - 30 * 60_000)),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left, right] = boxes();
    expect(top.getAttribute("data-agent")).toBe("a".repeat(16));
    expect(within(top).getByText("Planner")).toBeDefined();
    expect(within(top).getByTestId("window-pane-state").textContent).toBe("Idle");
    // Unseen for half an hour: the registry's own silence, which is Quiet.
    expect(within(left).getByTestId("window-pane-state").textContent).toBe("Quiet");
    expect(right.getAttribute("data-agent")).toBeNull();
    expect(right.textContent).toBe("Pane 2No agent");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane with no session takes its index from the layout's order and the window's base", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 1, session("a".repeat(16), "Planner")),
          pane("%1", 2, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    expect(boxes().map((box) => box.getAttribute("data-pane-index"))).toEqual(["1", "2", "3"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a zoomed pane is marked, and a pane two sessions name says how many", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner"), { zoomed: true }),
          pane("%1", 1, session("b".repeat(16), "Builder"), {
            otherSessions: [session("c".repeat(16), "Stray")],
          }),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left] = boxes();
    expect(top.getAttribute("data-zoomed")).toBe("true");
    expect(within(top).getByTestId("window-pane-zoomed").textContent).toBe("Zoomed");
    expect(left.getAttribute("data-zoomed")).toBe("false");
    expect(within(left).getByTestId("window-pane-others").textContent).toBe(
      "1 other session names this pane"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane box too narrow or too short to read draws the window as a list, and says why", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [pane("%0", 0, session("a".repeat(16), "Planner"))];
  const view = draw(
    <>
      {/* 40 of 80 columns at 150 px is 75 px, under the 96 px a name and a state dot need. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={150}
        window={window(panes, THREE_PANES)}
      />
      {/* One row of 50 at 390 px is 9.75 px, under the 42 px an Agents row is given. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={390}
        window={window(panes, "b000,80x50,0,0[80x1,0,0,0,80x48,0,2,1]")}
      />
    </>
  );
  try {
    expect(screen.queryByTestId("window-pane")).toBeNull();
    const [narrow, short] = screen.getAllByTestId("window-pane-list");
    for (const list of [narrow, short]) {
      expect(list.getAttribute("data-reason")).toBe("panes-too-small");
      expect(within(list).getByTestId("window-pane-list-reason").textContent).toBe(
        "A pane of this window would be too small to read at this width, so its panes are listed in layout order instead."
      );
    }
    // The list is the layout's own order, which is the order the drawing holds the panes in.
    expect(
      within(narrow)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window with no layout, and one whose layout will not read, are lists with their reasons", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [
    pane("%7", 7, session("a".repeat(16), "Planner")),
    pane("%4", 4, session("b".repeat(16), "Builder")),
  ];
  const view = draw(
    <>
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes)} />
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={1440}
        window={window(panes, "b05b,80x24,0,0[80x12,0,0,0")}
      />
    </>
  );
  try {
    const [absent, refused] = screen.getAllByTestId("window-pane-list");
    expect(absent.getAttribute("data-reason")).toBe("no-layout");
    expect(within(absent).getByTestId("window-pane-list-reason").textContent).toBe(
      "This window reported no layout, so its panes are listed by pane index instead."
    );
    // With no layout to order them, the model's own order - by pane index - is the list's.
    expect(
      within(absent)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%7", "%4"]);
    expect(refused.getAttribute("data-reason")).toBe("unreadable-layout");
    expect(within(refused).getByTestId("window-pane-list-reason").textContent).toBe(
      "This window's layout could not be read, so its panes are listed by pane index instead."
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane whose session holds an open ask waiting on the viewer needs them", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={{ asksForYou: new Map([["a".repeat(16), 2]]), unreadReplies: new Map() }}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left] = boxes();

    expect(within(top).getByTestId("window-pane-state").getAttribute("data-state")).toBe(
      "needs-you"
    );
    expect(within(top).getByTestId("window-pane-state").textContent).toBe("Needs you");
    // The count is this session's alone: the pane beside it reads its own activity.
    expect(within(left).getByTestId("window-pane-state").getAttribute("data-state")).toBe("idle");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane whose session has replies the viewer has not read needs them too", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={{ asksForYou: new Map(), unreadReplies: new Map([["a".repeat(16), 1]]) }}
      now={NOW}
      width={1440}
      // Silent for half an hour, which is Quiet on the session's own activity: what the viewer
      // is owed wins over it, as `agentGroup`'s first-match order says.
      window={window(
        [pane("%0", 0, session("a".repeat(16), "Planner", NOW - 30 * 60_000))],
        THREE_PANES
      )}
    />
  );
  try {
    expect(within(boxes()[0]).getByTestId("window-pane-state").getAttribute("data-state")).toBe(
      "needs-you"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane the drawn layout does not name is named under the window, drawn or listed", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const split = session("c".repeat(16), "Just split");
  const panes = [
    pane("%0", 0, session("a".repeat(16), "Planner")),
    pane("%1", 1, session("b".repeat(16), "Builder")),
    pane("%2", 2, session("d".repeat(16), "Tester")),
    pane("%7", 3, split),
  ];
  const view = draw(
    <>
      {/* Drawn: the layout names three cells and the window holds four panes, which is what a
          pane split off after that layout was read looks like. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={1440}
        window={window(panes, THREE_PANES)}
      />
      {/* The same window too narrow to draw: the list walks the layout's cells as well. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={150}
        window={window(panes, THREE_PANES)}
      />
      {/* No layout at all: the list is the model's own panes, so none of them is left out. */}
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes)} />
    </>
  );
  try {
    const blocks = screen.getAllByTestId("window-unnamed-panes");
    expect(blocks).toHaveLength(2);
    for (const block of blocks) {
      expect(within(block).getByTestId("window-unnamed-panes-reason").textContent).toBe(
        "This window holds a pane the layout it was drawn from does not name, so the session in that pane is named here rather than above. Each session reports the layout it last read, and a pane split off after that reading is in no cell of it."
      );
      const rows = within(block).getAllByTestId("window-unnamed-pane-row");
      expect(rows.map((row) => row.getAttribute("data-pane"))).toEqual(["%7"]);
      expect(rows[0].getAttribute("data-agent")).toBe(split.session_id);
      expect(rows[0].getAttribute("data-pane-index")).toBe("3");
      expect(within(rows[0]).getByRole("link", { name: "Just split" })).toBeDefined();
    }
    // Neither the drawing nor the list holds `%7` itself: both walk the layout's own cells.
    expect(boxes().map((box) => box.getAttribute("data-pane"))).toEqual(["%0", "%1", "%2"]);
    const [tooSmall, noLayout] = screen.getAllByTestId("window-pane-list");
    expect(
      within(tooSmall)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
    expect(
      within(noLayout)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2", "%7"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a session whose pane a fresher session took is named in that pane's box", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const stray = session("c".repeat(16), "Stray");
  const moved = session("d".repeat(16), "Moved");
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder"), { otherSessions: [stray, moved] }),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const others = within(boxes()[1]).getByTestId("window-pane-others");
    expect(others.textContent).toBe("2 other sessions name this pane");
    expect(others.getAttribute("title")).toBe("Other sessions naming this pane: Stray, Moved");
    expect(
      within(others)
        .getAllByTestId("window-pane-other")
        .map((mark) => mark.getAttribute("data-agent"))
    ).toEqual([stray.session_id, moved.session_id]);
    // One pane draws one session: the others are carried, never drawn as a second name in the
    // box, which would read as two sessions drawn in one pane.
    expect(within(boxes()[1]).queryByText("Stray")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});
