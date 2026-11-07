import { expect, type Locator, type Page, test } from "@playwright/test";
import type { TmuxPlace } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import { resetDatabase } from "./seed";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * The machine pages (`/machines` and `/machines/<machine>`), which draw a machine's tmux layout
 * from the `tmux_place` each `Agent` of `GET /api/v1/agents` carries. One `test.describe` per
 * check of dispatch://INFRA-883/artifact/verification-md, each carrying that check's grep tag:
 * `@machines-draw` is k5, `@machines-honesty` k7, `@machines-readonly` k8, `@machines-width` k9
 * and `@machines-two-servers-one-window-id` k12. The width check is two cases, because
 * `packages/dispatch/AGENTS.md` refuses the `@phone` tag to a case that starts at a desktop
 * width and narrows itself: one untagged case at 1440 px in `chromium`, one `@phone` case at
 * the `iphone` project's own width, each measured against the ratios this file computes from
 * the same seeded layout string.
 */

/** A box of a tmux window, in terminal cells, as the layout string states it. The spec writes
 *  its own cells rather than reading the page's parser, so a drawing is measured against the
 *  seeded string and not against the code that drew it. */
interface Cell {
  readonly height: number;
  /** Carrying its `%`, as `TmuxPlace.pane_id` and the drawing's `data-pane` both do. */
  readonly paneId: string;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** A window's own size in cells: what every pane's share is taken over. */
interface WindowSize {
  readonly columns: number;
  readonly rows: number;
}

/** The three-pane window this design recorded from tmux 3.5a: `%0` over `%1` beside `%2`. tmux
 *  spends one cell on the divider and gives it to neither pane, so the two lower panes are 40
 *  and 39 wide in an 80-cell window. */
const THREE_PANE_SIZE: WindowSize = { columns: 80, rows: 24 };
const THREE_PANE_LAYOUT = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
const THREE_PANES: readonly Cell[] = [
  { height: 12, paneId: "%0", width: 80, x: 0, y: 0 },
  { height: 11, paneId: "%1", width: 40, x: 0, y: 13 },
  { height: 11, paneId: "%2", width: 39, x: 41, y: 13 },
];

/** A window of four panes, two over two, in a 200x50 window. */
const FOUR_PANE_SIZE: WindowSize = { columns: 200, rows: 50 };
const FOUR_PANE_LAYOUT =
  "b7c1,200x50,0,0[200x24,0,0{100x24,0,0,0,99x24,101,0,1},200x25,0,25{100x25,0,25,2,99x25,101,25,3}]";
const FOUR_PANES: readonly Cell[] = [
  { height: 24, paneId: "%0", width: 100, x: 0, y: 0 },
  { height: 24, paneId: "%1", width: 99, x: 101, y: 0 },
  { height: 25, paneId: "%2", width: 100, x: 0, y: 25 },
  { height: 25, paneId: "%3", width: 99, x: 101, y: 25 },
];

/** A window holding one pane, for a strip that needs a window more than it needs a drawing. */
function onePaneLayout(paneNumber: number): string {
  return `b262,80x24,0,0,${paneNumber}`;
}

/**
 * The 22-pane window the width check seeds: a 203x64 window of four rows of panes, three rows of
 * six and one of four. A real production session names a pane `agents:3.21`, so a window of 22
 * panes is one the page meets. tmux spends one cell on each divider and gives it to neither
 * neighbour, so six panes across 203 columns are 33 wide with five dividers between them
 * (6 x 33 + 5 = 203) and four are 50 wide with three (4 x 50 + 3 = 203); the rows are 15, 15, 15
 * and 16 cells tall with a divider row after each of the first three (15 + 1 + 15 + 1 + 15 + 1 +
 * 16 = 64).
 *
 * Which number decides the fallback, in the spec's own terms. A pane box is drawn at the
 * layout's own percentages inside a box `W` pixels wide and `W x 2 x rows / columns` tall, so
 * the box is `width x W / columns` pixels wide and `height x 2 x W / columns` pixels tall. The
 * **96 px** bound on a pane box's width is the one that decides here: the narrowest box is
 * `33 x W / 203`, which reaches 96 px only once the drawing is 591 px wide. A 390 px phone can
 * never be that wide - its drawing is the 390 px viewport less the shell's padding - so every
 * pane there is too narrow and the window is listed; at 1440 px the drawing is over a thousand
 * pixels wide and the same box is about 174 px, so the window is drawn. The 42 px bound on a
 * box's height never decides: the shortest box is `15 x 2 x W / 203`, which is over 42 px from a
 * drawing 285 px wide on, so it is met at both widths.
 */
const WIDE_SIZE: WindowSize = { columns: 203, rows: 64 };

/** Each row of that window: how many panes it holds, how tall it is, and where its top is. */
const WIDE_ROW_PLAN = [
  { height: 15, panes: 6, y: 0 },
  { height: 15, panes: 6, y: 16 },
  { height: 15, panes: 6, y: 32 },
  { height: 16, panes: 4, y: 48 },
] as const;

/** The layout string and the cells it names, built from one description so the string the test
 *  seeds and the proportions it asserts cannot drift apart. Pane ids run from `%100` in layout
 *  order, which is the order tmux writes them in. */
function wideWindow(): { readonly cells: readonly Cell[]; readonly layout: string } {
  const cells: Cell[] = [];
  const rows: string[] = [];
  let paneNumber = 100;
  for (const row of WIDE_ROW_PLAN) {
    const width = (WIDE_SIZE.columns - (row.panes - 1)) / row.panes;
    const written: string[] = [];
    for (let index = 0; index < row.panes; index += 1) {
      const x = index * (width + 1);
      cells.push({ height: row.height, paneId: `%${paneNumber}`, width, x, y: row.y });
      written.push(`${width}x${row.height},${x},${row.y},${paneNumber}`);
      paneNumber += 1;
    }
    rows.push(`${WIDE_SIZE.columns}x${row.height},0,${row.y}{${written.join(",")}}`);
  }
  return {
    cells,
    layout: `9f2a,${WIDE_SIZE.columns}x${WIDE_SIZE.rows},0,0[${rows.join(",")}]`,
  };
}

const WIDE = wideWindow();

test.beforeEach(async () => {
  await resetDatabase();
});

function machineSession(
  sessionId: string,
  title: string,
  options: { dir?: string; host: string; lastSeen: number; place?: TmuxPlace }
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: options.dir ?? "/workspaces/legion",
    host: options.host,
    last_seen: options.lastSeen,
    machine_id: options.host,
    roles: [],
    session_id: sessionId,
    title,
    tmux_place: options.place,
  };
}

async function openMachine(page: Page, host: string): Promise<void> {
  await page.goto(`/machines/${encodeURIComponent(host)}`);
  await expect(page.getByRole("heading", { level: 1, name: host })).toBeVisible();
}

/**
 * Every drawn pane box of `drawing` (one `window-drawing` section), as a share of that
 * drawing's own box, against the share the seeded layout gives it - the check's "within one
 * percent", read as one percent of the expected share. Each pane sits at the layout's own
 * percentages with no border, padding or margin on the measured box, so what is compared is the
 * geometry and nothing else. The boxes are asserted to be in the layout's own order first, so a
 * drawing that holds the right rectangles in the wrong order fails here rather than passing.
 */
async function assertPaneProportions(
  drawing: Locator,
  cells: readonly Cell[],
  size: WindowSize
): Promise<void> {
  const whole = await drawing.locator('[data-testid="window-layout"]').boundingBox();
  if (whole === null) throw new Error("the drawing has no box");
  const boxes = drawing.locator('[data-testid="window-pane"]');
  expect(await boxes.evaluateAll((panes) => panes.map((pane) => pane.dataset.pane))).toEqual(
    cells.map((cell) => cell.paneId)
  );
  for (const cell of cells) {
    const box = await drawing
      .locator(`[data-testid="window-pane"][data-pane="${cell.paneId}"]`)
      .boundingBox();
    if (box === null) throw new Error(`pane ${cell.paneId} has no box`);
    const widthShare = cell.width / size.columns;
    const heightShare = cell.height / size.rows;
    expect(box.width / whole.width, `pane ${cell.paneId} width share`).toBeGreaterThanOrEqual(
      widthShare * 0.99
    );
    expect(box.width / whole.width, `pane ${cell.paneId} width share`).toBeLessThanOrEqual(
      widthShare * 1.01
    );
    expect(box.height / whole.height, `pane ${cell.paneId} height share`).toBeGreaterThanOrEqual(
      heightShare * 0.99
    );
    expect(box.height / whole.height, `pane ${cell.paneId} height share`).toBeLessThanOrEqual(
      heightShare * 1.01
    );
  }
}

// ---------------------------------------------------------------------------------------------
// k5: the machine page draws each tmux server's windows and panes.
// ---------------------------------------------------------------------------------------------

const DRAW_HOST = "sami-devbox";
/** The parts are in socket order, so the person's socket is the first part and the Legion
 *  daemon's private one the second. */
const PERSON_SOCKET = "/tmp/tmux-1000/default";
const DAEMON_SOCKET = "/var/run/legion/tmux-daemon.sock";

const drawIds = {
  builder: "019a1c22-7e5c-7f9b-8a4a-5b2c6d9e0f22",
  builds: "019a1c25-ab8f-7c2e-9d7d-8e5f9a2b3c25",
  daemon: "019a1c26-bc9a-7d3f-ae8e-9f6a0b3c4d26",
  outside: "019a1c27-cdab-7e40-bf9f-0a7b1c4d5e27",
  planner: "019a1c21-6d4b-7e8a-bf39-4a1b5c8d9e21",
  stray: "019a1c24-9a7e-7b1d-8c6c-7d4e8f1a2b24",
  tester: "019a1c23-8f6d-7a0c-9b5b-6c3d7e0f1a23",
} as const;

/** The place every seeded machine is drawn from: the first pane of the first window of the
 *  person's tmux server. Each case names what it is about and keeps the rest. */
function tmuxPlace(overrides: Partial<TmuxPlace> = {}): TmuxPlace {
  return {
    layout: FOUR_PANE_LAYOUT,
    pane_id: "%0",
    pane_index: 0,
    server_pid: "4242",
    session_name: "main",
    socket: PERSON_SOCKET,
    window_id: "@1",
    window_index: 1,
    window_name: "agents",
    zoomed: false,
    ...overrides,
  };
}

/**
 * One machine with two tmux servers: the person's socket, holding one tmux session of two
 * windows, and the Legion daemon's private socket, holding one. The first window's layout names
 * four panes and three of them hold live sessions; a fourth session names the pane `%2` already
 * holds, from an older heartbeat, so the page shows the freshest and counts the other. One more
 * session on the machine is in no pane at all, so the machine's two counts differ.
 */
async function seedDrawnMachine(now: number): Promise<void> {
  await setLiveSessions([
    machineSession(drawIds.planner, "Planner", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%0", pane_index: 0 }),
    }),
    machineSession(drawIds.builder, "Builder", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%1", pane_index: 1 }),
    }),
    machineSession(drawIds.tester, "Tester", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    // Heard from two minutes before the Tester, and naming the Tester's pane: one of the two
    // moved or ended without the registry hearing of it.
    machineSession(drawIds.stray, "Stray", {
      host: DRAW_HOST,
      lastSeen: now - 150_000,
      place: tmuxPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    machineSession(drawIds.builds, "Builds", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(4),
        pane_id: "%4",
        pane_index: 0,
        window_id: "@2",
        window_index: 2,
        window_name: "builds",
      }),
    }),
    machineSession(drawIds.daemon, "Legion daemon", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
      place: {
        layout: onePaneLayout(9),
        pane_id: "%9",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@7",
        window_index: 0,
        window_name: "daemon",
        zoomed: false,
      },
    }),
    // In Kubernetes rather than tmux: on the machine, in no pane.
    machineSession(drawIds.outside, "Archivist", {
      host: DRAW_HOST,
      lastSeen: now - 30_000,
    }),
  ]);
}

test.describe("@machines-draw", () => {
  test("the machine page draws both tmux servers, its windows in index order, and each pane in its own proportion", async ({
    browser,
  }) => {
    await seedDrawnMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, DRAW_HOST);

      // One part per tmux server, in socket order: the person's first, the daemon's second.
      const servers = page.locator('[data-testid="machine-server"]');
      await expect(servers).toHaveCount(2);
      await expect(servers.nth(0)).toHaveAttribute("data-socket", PERSON_SOCKET);
      await expect(servers.nth(0)).toHaveAttribute("data-server-pid", "4242");
      await expect(servers.nth(1)).toHaveAttribute("data-socket", DAEMON_SOCKET);
      await expect(servers.nth(1)).toHaveAttribute("data-server-pid", "9001");

      // The first part's strip holds its two windows in window-index order, and the first is
      // the one drawn.
      const strip = servers.nth(0).locator('[data-testid="window-pick"]');
      await expect(strip).toHaveCount(2);
      expect(await strip.evaluateAll((picks) => picks.map((pick) => pick.dataset.window))).toEqual([
        "@1",
        "@2",
      ]);
      await expect(strip.nth(0)).toHaveAttribute("data-picked", "true");
      await expect(strip.nth(1)).toHaveAttribute("data-picked", "false");

      const drawn = servers.nth(0).locator('[data-testid="window-drawing"]');
      await expect(drawn).toHaveAttribute("data-window", "@1");
      await expect(drawn).toHaveAttribute("data-window-index", "1");
      await expect(drawn).toHaveAttribute("data-window-name", "agents");
      await expect(drawn).toHaveAttribute("data-drawn", "layout");

      // Four pane boxes, each at the layout's own proportion of the drawing.
      const boxes = drawn.locator('[data-testid="window-pane"]');
      await assertPaneProportions(drawn, FOUR_PANES, FOUR_PANE_SIZE);

      // A box whose pane holds a session shows that session's name and its state, and its link
      // is that session's live view.
      const planner = boxes.nth(0);
      await expect(planner).toHaveAttribute("data-agent", drawIds.planner);
      await expect(planner.getByRole("link", { name: "Planner" })).toHaveAttribute(
        "href",
        `/agents/${drawIds.planner}/live`
      );
      await expect(planner.locator('[data-testid="window-pane-state"]')).toHaveAttribute(
        "data-state",
        "idle"
      );
      await expect(planner.locator('[data-testid="window-pane-state"]')).toHaveText("Idle");
      await expect(boxes.nth(1)).toHaveAttribute("data-agent", drawIds.builder);
      await expect(boxes.nth(1).getByRole("link", { name: "Builder" })).toHaveAttribute(
        "href",
        `/agents/${drawIds.builder}/live`
      );

      // Two live sessions name `%2`: the page draws the one heard from most recently and says
      // how many others name it.
      const shared = boxes.nth(2);
      await expect(shared).toHaveAttribute("data-agent", drawIds.tester);
      await expect(shared.getByRole("link", { name: "Tester" })).toBeVisible();
      await expect(shared.getByText("Stray")).toHaveCount(0);
      await expect(shared.locator('[data-testid="window-pane-others"]')).toHaveText(
        "1 other session names this pane"
      );

      // The fourth pane holds no agent, and says so with its pane index.
      const empty = boxes.nth(3);
      await expect(empty).toHaveAttribute("data-pane", "%3");
      await expect(empty).toHaveAttribute("data-pane-index", "3");
      expect(await empty.getAttribute("data-agent")).toBeNull();
      await expect(empty).toContainText("Pane 3");
      await expect(empty).toContainText("No agent");

      // Picking the second window draws that window instead.
      await strip.nth(1).click();
      // The address names the server as well as the window (k12): tmux numbers windows per
      // server, so a bare `@2` would name a window on each server of this machine.
      await expect
        .poll(() => new URL(page.url()).searchParams.get("window"))
        .toBe(`${PERSON_SOCKET}:4242:@2`);
      await expect(drawn).toHaveAttribute("data-window", "@2");
      await expect(drawn).toHaveAttribute("data-window-name", "builds");
      await expect(strip.nth(1)).toHaveAttribute("data-picked", "true");
      await expect(servers.nth(0).locator('[data-testid="window-pane"]')).toHaveCount(1);
      await expect(servers.nth(0).locator('[data-testid="window-pane"]').first()).toHaveAttribute(
        "data-agent",
        drawIds.builds
      );

      // The daemon's own part is still its own window, whichever window the address names.
      await expect(servers.nth(1).locator('[data-testid="window-drawing"]')).toHaveAttribute(
        "data-window",
        "@7"
      );
    } finally {
      await alice.close();
    }
  });

  test("the machines list names the machine with its session count and how many of those are in a pane", async ({
    browser,
  }) => {
    await seedDrawnMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.goto("/machines");
      await expect(page.locator('[data-testid="machines-page"]')).toBeVisible();

      const rows = page.locator('[data-testid="machine-row"]');
      await expect(rows).toHaveCount(1);
      const row = rows.first();
      await expect(row).toHaveAttribute("data-machine", DRAW_HOST);
      // Seven sessions on the machine, six of them in a pane.
      await expect(row).toHaveAttribute("data-sessions", "7");
      await expect(row).toHaveAttribute("data-in-tmux", "6");
      await expect(row).toContainText("7 sessions · 6 in a pane");
      await expect(row.getByRole("link", { name: DRAW_HOST })).toHaveAttribute(
        "href",
        `/machines/${DRAW_HOST}`
      );

      await row.getByRole("link", { name: DRAW_HOST }).click();
      await expect(page.locator('[data-testid="machine-page"]')).toHaveAttribute(
        "data-machine",
        DRAW_HOST
      );
    } finally {
      await alice.close();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// k7: the page says what it does not know.
// ---------------------------------------------------------------------------------------------

const honestyIds = {
  drawn: "019a1c33-8f6d-7a0c-9b5b-6c3d7e0f1a33",
  elsewhere: "019a1c35-ab8f-7c2e-9d7d-8e5f9a2b3c35",
  here: "019a1c34-9a7e-7b1d-8c6c-7d4e8f1a2b34",
  kubeOne: "019a1c31-6d4b-7e8a-bf39-4a1b5c8d9e31",
  kubeTwo: "019a1c32-7e5c-7f9b-8a4a-5b2c6d9e0f32",
};

/** A single-pane place on a tmux server of its own, for a machine the page has to draw before
 *  it can say how old its drawing is. */
function lonePlace(paneNumber: number): TmuxPlace {
  return tmuxPlace({
    layout: onePaneLayout(paneNumber),
    pane_id: `%${paneNumber}`,
    server_pid: "7700",
    session_name: "work",
  });
}

test.describe("@machines-honesty", () => {
  test("a machine whose sessions are all outside tmux draws nothing and sends the reader to its agents", async ({
    browser,
  }) => {
    const host = "cluster-pool";
    const now = Date.now();
    await setLiveSessions([
      machineSession(honestyIds.kubeOne, "Runner one", { host, lastSeen: now - 30_000 }),
      machineSession(honestyIds.kubeTwo, "Runner two", { host, lastSeen: now - 30_000 }),
      // On another machine, so the Agents page the link opens has something to leave out.
      machineSession(honestyIds.elsewhere, "Devbox planner", {
        host: "some-devbox",
        lastSeen: now - 30_000,
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-none-in-tmux"]')).toHaveText(
        "No session on this machine is in tmux, so there is no layout to draw."
      );
      await expect(page.locator('[data-testid="machine-server"]')).toHaveCount(0);
      await expect(page.locator('[data-testid="window-drawing"]')).toHaveCount(0);

      const toAgents = page
        .locator('[data-testid="machine-page"]')
        .getByRole("link", { name: "Agents on this machine" });
      await expect(toAgents).toHaveAttribute("href", `/agents?machine=${host}`);
      await toAgents.click();
      await expect(page).toHaveURL(/\/agents\?machine=cluster-pool$/);
      // The Agents page opens on this machine's filter, so the session on another machine is
      // left out of the list. A row the filter excludes stays mounted and hidden.
      await expect(page.getByRole("combobox", { name: "Machine" })).toHaveValue(host);
      await expect(page.locator(`[data-agent-row="${honestyIds.elsewhere}"]`)).toBeHidden();
      await expect(page.locator(`[data-agent-row="${honestyIds.kubeOne}"]`)).toBeAttached();
    } finally {
      await alice.close();
    }
  });

  test("a drawn server says it holds only the windows that hold an agent, and the page dates its drawing", async ({
    browser,
  }) => {
    const host = "fresh-devbox";
    await setLiveSessions([
      machineSession(honestyIds.drawn, "Planner", {
        host,
        lastSeen: Date.now() - 30_000,
        place: lonePlace(3),
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-windows-with-agents"]')).toHaveText(
        "This server shows only the windows that hold an agent: a window with no agent reports nothing."
      );
      // Heard from half a minute ago: dated, and not stale.
      await expect(page.locator('[data-testid="machine-drawing-age"]')).toHaveText(
        "Oldest report drawn: just now."
      );
      await expect(page.locator('[data-testid="machine-drawing-stale"]')).toHaveCount(0);
    } finally {
      await alice.close();
    }
  });

  test("a drawing made from a report four minutes old is marked stale", async ({ browser }) => {
    const host = "stale-devbox";
    await setLiveSessions([
      machineSession(honestyIds.drawn, "Planner", {
        host,
        lastSeen: Date.now() - 4 * 60_000,
        place: lonePlace(3),
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-drawing-age"]')).toHaveText(
        "Oldest report drawn: 4 minutes ago."
      );
      await expect(page.locator('[data-testid="machine-drawing-stale"]')).toHaveText(
        "This drawing is stale: its oldest report is over three minutes old."
      );
    } finally {
      await alice.close();
    }
  });

  test("a machine name several sessions share with different directories is marked a logical name", async ({
    browser,
  }) => {
    const host = "legion-infra";
    const now = Date.now();
    await setLiveSessions([
      machineSession(honestyIds.here, "Planner", {
        dir: "/workspaces/cybermonklabs/legion/infra-883",
        host,
        lastSeen: now - 30_000,
        place: lonePlace(3),
      }),
      machineSession(honestyIds.elsewhere, "Reviewer", {
        dir: "/workspaces/cybermonklabs/legion/infra-812",
        host,
        lastSeen: now - 30_000,
        place: lonePlace(4),
      }),
    ]);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, host);

      await expect(page.locator('[data-testid="machine-shared-name"]')).toHaveText(
        "This machine name is one a deployment chose, not one host: its sessions report more than one directory."
      );
    } finally {
      await alice.close();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// k8: the page reads and never writes.
// ---------------------------------------------------------------------------------------------

/** One request the page made, as this check reads it. */
interface Recorded {
  readonly method: string;
  readonly path: string;
}

/** Every request the page makes from here on. The live stream rides a websocket, which
 *  Playwright reports as neither a request nor a response, so nothing here counts it. */
function recordRequests(page: Page): Recorded[] {
  const made: Recorded[] = [];
  page.on("request", (request) => {
    made.push({ method: request.method(), path: new URL(request.url()).pathname });
  });
  return made;
}

/** Every page of the app a control on these pages may open: the machine list, a machine, the
 *  Agents page and a session's live view. */
const APP_PAGE_PATH = /^\/machines(\/[^/]+)?$|^\/agents$|^\/agents\/[^/]+\/live$/;

/** A path a route of these pages' own would be named by. The pages read the agents list, the
 *  per-person agent state and the Inbox tally, all three of which the application shell already
 *  makes; what they must not do is ask for a route of their own. */
const OWN_ROUTE = /machine|tmux|pane|window/i;

test.describe("@machines-readonly", () => {
  test("every control on a drawn machine page is a link to a page of the app, and every request it makes is a read", async ({
    browser,
  }) => {
    await seedDrawnMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const made = recordRequests(page);
      await openMachine(page, DRAW_HOST);
      await expect(page.locator('[data-testid="window-layout"]').first()).toBeVisible();

      // A person picks each window of each server.
      const picks = page.locator('[data-testid="window-pick"]');
      const windows = await picks.evaluateAll((nodes) =>
        nodes.map((node) => node.dataset.window ?? "")
      );
      expect(windows.length).toBeGreaterThan(1);
      for (const window of windows) {
        await page.locator(`[data-testid="window-pick"][data-window="${window}"]`).click();
        await expect(
          page.locator(`[data-testid="window-drawing"][data-window="${window}"]`)
        ).toBeVisible();
      }

      // Every control of the page is a link, and every link opens a page of this app.
      const controls = await page
        .locator('[data-testid="machine-page"]')
        .locator("a, button")
        .evaluateAll((nodes) =>
          nodes.map((node) => ({
            href: node instanceof HTMLAnchorElement ? node.href : null,
            tag: node.tagName.toLowerCase(),
          }))
        );
      expect(controls.length).toBeGreaterThan(0);
      const origin = new URL(page.url()).origin;
      const notPages = controls.filter(
        (control) =>
          control.href === null ||
          new URL(control.href).origin !== origin ||
          !APP_PAGE_PATH.test(new URL(control.href).pathname)
      );
      expect(notPages, "every control is a link to a page of this app").toEqual([]);

      // Every request the page made while it drew and while the windows were picked is a read.
      expect(
        made.filter((request) => request.method !== "GET"),
        "the page made a request that is not a GET"
      ).toEqual([]);
      const read = [
        ...new Set(made.filter((r) => r.path.startsWith("/api/")).map((r) => r.path)),
      ].sort();
      // The agents list is the one read the drawing itself needs, and it is the shell's own.
      expect(read).toContain("/api/v1/agents");
      expect(
        read.filter((path) => OWN_ROUTE.test(path)),
        "the page asked for a route of its own"
      ).toEqual([]);
    } finally {
      await alice.close();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// k9: the drawing holds its proportions at a laptop width and a phone width.
// ---------------------------------------------------------------------------------------------

const WIDTH_HOST = "sami-laptop";

const widthIds = {
  agentsFirst: "019a1c53-8f6d-7a0c-9b5b-6c3d7e0f1a53",
  agentsLast: "019a1c55-ab8f-7c2e-9d7d-8e5f9a2b3c55",
  agentsSixth: "019a1c54-9a7e-7b1d-8c6c-7d4e8f1a2b54",
  deploy: "019a1c52-7e5c-7f9b-8a4a-5b2c6d9e0f52",
  review: "019a1c56-bc9a-7d3f-ae8e-9f6a0b3c4d56",
  writerOne: "019a1c51-6d4b-7e8a-bf39-4a1b5c8d9e51",
  writerThree: "019a1c57-cdab-7e40-bf9f-0a7b1c4d5e57",
  writerTwo: "019a1c58-dbac-7f51-8a0a-1b8c2d5e6f58",
};

/** A place on the machine the width cases draw: one tmux server, one tmux session, windows the
 *  caller names. */
function widthPlace(overrides: Partial<TmuxPlace>): TmuxPlace {
  return tmuxPlace({
    layout: THREE_PANE_LAYOUT,
    server_pid: "5150",
    session_name: "work",
    window_name: "release-notes",
    ...overrides,
  });
}

/**
 * One tmux server of four windows, so the strip has more picks than a phone's width holds and
 * fewer than a laptop's. Window 1 is the three-pane window, drawn at both widths; window 3 is
 * the 22-pane window, drawn at 1440 px and listed at 390 px; windows 2 and 4 hold one pane each
 * and are there for the strip.
 */
async function seedWidthMachine(now: number): Promise<void> {
  const lastSeen = now - 30_000;
  await setLiveSessions([
    machineSession(widthIds.writerOne, "Release writer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%0", pane_index: 0 }),
    }),
    machineSession(widthIds.writerTwo, "Release reviewer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%1", pane_index: 1 }),
    }),
    machineSession(widthIds.writerThree, "Release tester", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({ pane_id: "%2", pane_index: 2 }),
    }),
    machineSession(widthIds.deploy, "Deploy watcher", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: onePaneLayout(20),
        pane_id: "%20",
        pane_index: 0,
        window_id: "@2",
        window_index: 2,
        window_name: "deploy-watch",
      }),
    }),
    // `agents:3.0`, `agents:3.5` and `agents:3.21` of the 22-pane window.
    machineSession(widthIds.agentsFirst, "Agent one", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%100",
        pane_index: 0,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.agentsSixth, "Agent six", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%105",
        pane_index: 5,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.agentsLast, "Agent twenty-two", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: WIDE.layout,
        pane_id: "%121",
        pane_index: 21,
        window_id: "@3",
        window_index: 3,
        window_name: "agents",
      }),
    }),
    machineSession(widthIds.review, "Layout reviewer", {
      host: WIDTH_HOST,
      lastSeen,
      place: widthPlace({
        layout: onePaneLayout(30),
        pane_id: "%30",
        pane_index: 0,
        window_id: "@4",
        window_index: 4,
        window_name: "layout-review",
      }),
    }),
  ]);
}

/** The window strip, and whether it scrolls sideways inside itself. */
async function stripScrolls(page: Page): Promise<boolean> {
  return await page
    .locator('[data-testid="window-strip"]')
    .evaluate((strip) => strip.scrollWidth > strip.clientWidth + 0.5);
}

/** The picks of the strip all sit on one line. */
async function stripIsOneRow(page: Page): Promise<boolean> {
  return await page
    .locator('[data-testid="window-pick"]')
    .evaluateAll((picks) =>
      picks.every(
        (pick) =>
          Math.abs(pick.getBoundingClientRect().top - picks[0].getBoundingClientRect().top) < 1
      )
    );
}

test.describe("@machines-width", () => {
  // Untagged, and so `chromium`'s alone: it sets a laptop viewport of its own, which the
  // `iphone` project would only have to undo.
  test("at 1440 pixels the strip is one row, the drawing fills the width of the page, and every pane keeps its proportion", async ({
    browser,
  }) => {
    expect(WIDE.cells.length, "the seeded window holds 22 panes").toBe(22);
    await seedWidthMachine(Date.now());
    const alice = await asUser(browser, "alice", { viewport: { height: 900, width: 1440 } });
    try {
      const page = await alice.newPage();
      await openMachine(page, WIDTH_HOST);

      await expect(page.locator('[data-testid="window-pick"]')).toHaveCount(4);
      expect(await stripIsOneRow(page), "the strip is one row").toBe(true);
      expect(await stripScrolls(page), "the strip scrolls sideways").toBe(false);

      // The drawing fills the width of the page: nothing between it and the page takes any of
      // that width.
      const drawn = page.locator('[data-testid="window-drawing"]');
      const pageWidth = await page
        .locator('[data-testid="machine-page"]')
        .evaluate((section) => section.clientWidth);
      const whole = await page.locator('[data-testid="window-layout"]').boundingBox();
      if (whole === null) throw new Error("the drawing has no box");
      expect(Math.abs(whole.width - pageWidth), "the drawing fills the page").toBeLessThan(1);

      await expect(drawn).toHaveAttribute("data-drawn", "layout");
      await assertPaneProportions(drawn, THREE_PANES, THREE_PANE_SIZE);

      // At this width every box of the 22-pane window is wide enough to read, so the window is
      // drawn, and its boxes are in the layout's own order.
      await page.locator('[data-testid="window-pick"][data-window="@3"]').click();
      await expect(drawn).toHaveAttribute("data-window", "@3");
      await expect(drawn).toHaveAttribute("data-drawn", "layout");
      await assertPaneProportions(drawn, WIDE.cells, WIDE_SIZE);
    } finally {
      await alice.close();
    }
  });

  // The phone project's own, and only its own: every claim here is about what the phone layout
  // does at 390 px, which `chromium` at its own width would only contradict. A test that skips
  // every project but `iphone` is always tagged (`packages/dispatch/AGENTS.md`, R5).
  test("at a phone width the strip scrolls sideways, the drawn panes keep their proportions, and a 22-pane window is listed in layout order @phone", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "iphone", "the phone layout is the iphone project's");
    await seedWidthMachine(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, WIDTH_HOST);

      await expect(page.locator('[data-testid="window-pick"]')).toHaveCount(4);
      expect(await stripIsOneRow(page), "the strip is one row").toBe(true);
      expect(await stripScrolls(page), "the strip scrolls sideways").toBe(true);
      // The strip scrolls inside itself, so the page itself is no wider than the phone.
      await assertPageFits(page);

      // The three-pane window is still a drawing here, in the proportions the same seeded
      // layout string gives it.
      const firstWindow = page.locator('[data-testid="window-drawing"]');
      await expect(firstWindow).toHaveAttribute("data-drawn", "layout");
      await assertPaneProportions(firstWindow, THREE_PANES, THREE_PANE_SIZE);

      // The 22-pane window's narrowest box would be under 96 px here, so its panes are listed
      // in layout order instead, with the line that says why.
      await page.locator('[data-testid="window-pick"][data-window="@3"]').click();
      const drawn = page.locator('[data-testid="window-drawing"][data-window="@3"]');
      await expect(drawn).toHaveAttribute("data-drawn", "list");
      await expect(page.locator('[data-testid="window-pane"]')).toHaveCount(0);
      const list = drawn.locator('[data-testid="window-pane-list"]');
      await expect(list).toHaveAttribute("data-reason", "panes-too-small");
      await expect(list.locator('[data-testid="window-pane-list-reason"]')).toHaveText(
        "A pane of this window would be too small to read at this width, so its panes are listed in layout order instead."
      );
      const rows = list.locator('[data-testid="window-pane-row"]');
      await expect(rows).toHaveCount(22);
      expect(await rows.evaluateAll((items) => items.map((item) => item.dataset.pane))).toEqual(
        WIDE.cells.map((cell) => cell.paneId)
      );
      expect(
        await rows.evaluateAll((items) => items.map((item) => item.dataset.paneIndex))
      ).toEqual(WIDE.cells.map((_cell, index) => String(index)));
      await expect(rows.nth(21)).toHaveAttribute("data-agent", widthIds.agentsLast);
    } finally {
      await alice.close();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// The page accounts for every session it counts as being in a pane. This is R4's own rule - the
// page says what it does not know - read against the one thing the drawing can lose: a pane the
// layout string it draws from does not name.
//
// Each session reports its own copy of the window's layout, read at its own moment and held for
// `ENVOY_TMUX_PLACE_CACHE_MS` (30 s in production), while a window is drawn from the layout of
// the report heard from most recently that carries one (`buildWindow`, machine-model.ts). A
// registration that is fresher than the layout inside it therefore wins, so for up to one cache
// lifetime after a pane is split the window is drawn from a layout string written before that
// split. `drawnPanes` (WindowDrawing.tsx) walks `layout.panes` alone, so the session in the new
// pane is in neither the drawing nor the list, and no line of the page says so, while the
// header goes on counting it under "N in a pane".
//
// This test is tagged by nothing: no check of dispatch://INFRA-883/artifact/verification-md
// covers it, and the architect decides whether it earns one.
//
// A fix that draws the extra pane and a fix that lists it in a notice both satisfy this test,
// so long as the element that carries the session names it in `data-agent`, as every other
// pane box and pane row of this page already does.
// ---------------------------------------------------------------------------------------------

const UNDRAWN_HOST = "example-host-split";

/** The four-pane window above with its lower right pane split in two: the layout the session in
 *  the new pane `%7` reports, which the window is not drawn from while a fresher registration
 *  still carries the four-pane string. */
const FIVE_PANE_LAYOUT =
  "c8d2,200x50,0,0[200x24,0,0{100x24,0,0,0,99x24,101,0,1}," +
  "200x25,0,25{100x25,0,25,2,99x25,101,25[99x12,101,25,3,99x12,101,38,7]}]";

const undrawnIds = {
  builder: "019a1d11-0000-7000-8000-000000000002",
  planner: "019a1d11-0000-7000-8000-000000000001",
  reviewer: "019a1d11-0000-7000-8000-000000000004",
  split: "019a1d11-0000-7000-8000-000000000005",
  tester: "019a1d11-0000-7000-8000-000000000003",
} as const;

test.describe("@machines-accounts-for-every-session", () => {
  test("a session in a pane the drawn layout does not name is still accounted for", async ({
    browser,
  }) => {
    const now = Date.now();
    const place = (overrides: Partial<TmuxPlace>): TmuxPlace =>
      tmuxPlace({ layout: FOUR_PANE_LAYOUT, window_id: "@1", window_index: 1, ...overrides });
    await setLiveSessions([
      machineSession(undrawnIds.planner, "Planner", {
        host: UNDRAWN_HOST,
        // The freshest report of this window, and its layout was read from the cache before the
        // split below, so it names four panes and not five.
        lastSeen: now,
        place: place({ pane_id: "%0", pane_index: 0 }),
      }),
      machineSession(undrawnIds.builder, "Builder", {
        host: UNDRAWN_HOST,
        lastSeen: now - 20_000,
        place: place({ pane_id: "%1", pane_index: 1 }),
      }),
      machineSession(undrawnIds.tester, "Tester", {
        host: UNDRAWN_HOST,
        lastSeen: now - 20_000,
        place: place({ pane_id: "%2", pane_index: 2 }),
      }),
      machineSession(undrawnIds.reviewer, "Reviewer", {
        host: UNDRAWN_HOST,
        lastSeen: now - 20_000,
        place: place({ pane_id: "%3", pane_index: 3 }),
      }),
      // The session in the pane the split made, heard from a moment before the Planner above and
      // reporting the layout that names its own pane.
      machineSession(undrawnIds.split, "Just split", {
        host: UNDRAWN_HOST,
        lastSeen: now - 1_000,
        place: place({
          layout: FIVE_PANE_LAYOUT,
          pane_id: "%7",
          pane_index: 4,
        }),
      }),
    ]);
    const alice = await asUser(browser, "alice", { viewport: { height: 900, width: 1440 } });
    try {
      const page = await alice.newPage();
      await openMachine(page, UNDRAWN_HOST);
      await expect(page.locator('[data-testid="machine-server"]')).toHaveCount(1);

      // What the page says it holds.
      await expect(page.locator('[data-testid="machine-page"] header')).toContainText(
        "5 sessions · 5 in a pane"
      );

      // What the page shows. Every pane box and every pane row names its session in `data-agent`.
      const accounted = await page
        .locator('[data-testid="machine-page"] [data-agent]')
        .evaluateAll((nodes) => [
          ...new Set(nodes.map((node) => node.getAttribute("data-agent") ?? "")),
        ]);
      expect(
        accounted.sort(),
        "the page counts five sessions in a pane and must account for all five"
      ).toEqual([...Object.values(undrawnIds)].sort());
    } finally {
      await alice.close();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// k12: a window is addressed by its server and its id together, never by a bare id that repeats
// across servers.
// ---------------------------------------------------------------------------------------------

const REPEATED_HOST = "sami-twoservers";

const repeatedIds = {
  builds: "019a1e11-0000-7000-8000-000000000004",
  daemon: "019a1e11-0000-7000-8000-000000000003",
  editor: "019a1e11-0000-7000-8000-000000000002",
  shell: "019a1e11-0000-7000-8000-000000000001",
} as const;

/**
 * One machine running two tmux servers, the person's own and a Legion daemon's, each holding a
 * window `@0` and a window `@1`: tmux numbers windows from `@0` on each server it starts, so
 * the ids repeat across the two and `@1` alone names no window on this machine. Each window
 * holds one pane, and each pane's number is its own, so which window was drawn is read off the
 * pane in it.
 */
async function seedTwoServers(now: number): Promise<void> {
  await setLiveSessions([
    machineSession(repeatedIds.shell, "Shell", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(0),
        pane_id: "%0",
        pane_index: 0,
        window_id: "@0",
        window_index: 0,
        window_name: "shell",
      }),
    }),
    machineSession(repeatedIds.editor, "Editor", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(1),
        pane_id: "%1",
        pane_index: 0,
        window_id: "@1",
        window_index: 1,
        window_name: "agents",
      }),
    }),
    machineSession(repeatedIds.daemon, "Legion daemon", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(9),
        pane_id: "%9",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@0",
        window_index: 0,
        window_name: "daemon",
      }),
    }),
    machineSession(repeatedIds.builds, "Builds", {
      host: REPEATED_HOST,
      lastSeen: now - 30_000,
      place: tmuxPlace({
        layout: onePaneLayout(8),
        pane_id: "%8",
        pane_index: 0,
        server_pid: "9001",
        session_name: "legion",
        socket: DAEMON_SOCKET,
        window_id: "@1",
        window_index: 1,
        window_name: "builds",
      }),
    }),
  ]);
}

test.describe("@machines-two-servers-one-window-id", () => {
  test("picking a window id two servers share draws the window of the server it was picked on, and leaves the other server where it was", async ({
    browser,
  }) => {
    await seedTwoServers(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await openMachine(page, REPEATED_HOST);

      const servers = page.locator('[data-testid="machine-server"]');
      await expect(servers).toHaveCount(2);
      // In socket order: the person's own server first, the Legion daemon's second. Each part
      // names its server by its socket, in its heading and in `data-socket`.
      const person = servers.nth(0);
      const daemon = servers.nth(1);
      await expect(person).toHaveAttribute("data-socket", PERSON_SOCKET);
      await expect(person.getByRole("heading", { level: 2 })).toHaveText(PERSON_SOCKET);
      await expect(daemon).toHaveAttribute("data-socket", DAEMON_SOCKET);
      await expect(daemon.getByRole("heading", { level: 2 })).toHaveText(DAEMON_SOCKET);

      // Both servers hold a `@0` and a `@1`, and the same id in the two strips is two links:
      // each names the server its window is on.
      const personPicks = person.locator('[data-testid="window-pick"]');
      const daemonPicks = daemon.locator('[data-testid="window-pick"]');
      for (const picks of [personPicks, daemonPicks]) {
        expect(await picks.evaluateAll((all) => all.map((pick) => pick.dataset.window))).toEqual([
          "@0",
          "@1",
        ]);
      }
      expect(await personPicks.nth(1).getAttribute("href")).not.toBe(
        await daemonPicks.nth(1).getAttribute("href")
      );

      // With nothing in the address, each server draws its own first window.
      const personDrawing = person.locator('[data-testid="window-drawing"]');
      const daemonDrawing = daemon.locator('[data-testid="window-drawing"]');
      await expect(personDrawing).toHaveAttribute("data-window", "@0");
      await expect(daemonDrawing).toHaveAttribute("data-window", "@0");

      await personPicks.nth(1).click();

      // The address names the server and the window together, never the bare `@1`, which on
      // this machine would name a window on each of the two servers.
      await expect
        .poll(() => new URL(page.url()).searchParams.get("window"))
        .toBe(`${PERSON_SOCKET}:4242:@1`);

      // The person's server draws its own `@1`: the one pane that window holds and no other.
      await expect(personDrawing).toHaveAttribute("data-window", "@1");
      await expect(personDrawing).toHaveAttribute("data-window-name", "agents");
      const personPanes = personDrawing.locator('[data-testid="window-pane"]');
      await expect(personPanes).toHaveCount(1);
      await expect(personPanes.first()).toHaveAttribute("data-pane", "%1");
      await expect(personPanes.first()).toHaveAttribute("data-agent", repeatedIds.editor);
      await expect(personPicks.nth(1)).toHaveAttribute("data-picked", "true");

      // The daemon's server is where it was: its drawing is still its own `@0`, holding its own
      // pane, and its strip still highlights that window.
      await expect(daemonDrawing).toHaveAttribute("data-window", "@0");
      await expect(daemonDrawing).toHaveAttribute("data-window-name", "daemon");
      const daemonPanes = daemonDrawing.locator('[data-testid="window-pane"]');
      await expect(daemonPanes).toHaveCount(1);
      await expect(daemonPanes.first()).toHaveAttribute("data-agent", repeatedIds.daemon);
      await expect(daemonPicks.nth(0)).toHaveAttribute("data-picked", "true");
      await expect(daemonPicks.nth(1)).toHaveAttribute("data-picked", "false");
    } finally {
      await alice.close();
    }
  });

  test("a deep link to one server's window draws that window and leaves the other server at its first", async ({
    browser,
  }) => {
    await seedTwoServers(Date.now());
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const choice = encodeURIComponent(`${DAEMON_SOCKET}:9001:@1`);
      await page.goto(`/machines/${encodeURIComponent(REPEATED_HOST)}?window=${choice}`);
      await expect(page.getByRole("heading", { level: 1, name: REPEATED_HOST })).toBeVisible();

      const servers = page.locator('[data-testid="machine-server"]');
      // The link names the daemon's server, so the daemon's `@1` is drawn and the person's
      // server - which holds a `@1` of its own - is at its first window.
      await expect(servers.nth(1).locator('[data-testid="window-drawing"]')).toHaveAttribute(
        "data-window",
        "@1"
      );
      await expect(
        servers.nth(1).locator('[data-testid="window-drawing"] [data-testid="window-pane"]')
      ).toHaveAttribute("data-agent", repeatedIds.builds);
      await expect(servers.nth(0).locator('[data-testid="window-drawing"]')).toHaveAttribute(
        "data-window",
        "@0"
      );
      await expect(
        servers.nth(0).locator('[data-testid="window-drawing"] [data-testid="window-pane"]')
      ).toHaveAttribute("data-agent", repeatedIds.shell);
    } finally {
      await alice.close();
    }
  });
});
