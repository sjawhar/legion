import {
  type WebSocket as BrowserWebSocket,
  expect,
  type Page,
  type Request,
  test,
  type WebSocketRoute,
} from "@playwright/test";

import type { LiveClientFrame, LiveServerFrame } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  createComment,
  createIssue,
  createProject,
  patchIssue,
  publishAgentStreamFrame,
  rotateLiveSockets,
  setAgentStreamResponder,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { asUser } from "./users";

const agent: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/live-socket",
  machine_id: "live-socket-host",
  roles: [],
  session_id: "01a0e090-4848-7473-acc5-fc96e6a646d4",
  title: "Live Socket Agent",
};
const bob = { actor: { id: "bob", kind: "session" as const }, as: "agent" as const };

function isLiveSocket(socket: BrowserWebSocket): boolean {
  return new URL(socket.url()).pathname === "/ws/live";
}

/** Lets the routed socket use Dispatch's own messages and preserves its close code. */
function connect(route: WebSocketRoute): void {
  const server = route.connectToServer();
  server.onClose((code, reason) =>
    route.close({
      ...(code === undefined ? {} : { code }),
      ...(reason === undefined ? {} : { reason }),
    })
  );
}

/** What a page's live sockets did: those open now, every one it opened, every frame it sent, and
 *  the type of every frame it received, in order. */
interface LiveSockets {
  readonly open: Set<BrowserWebSocket>;
  readonly opened: BrowserWebSocket[];
  readonly sent: LiveClientFrame[];
  readonly received: LiveServerFrame["type"][];
}

function watchLiveSockets(page: Page): LiveSockets {
  const sockets: LiveSockets = { open: new Set(), opened: [], received: [], sent: [] };
  page.on("websocket", (socket) => {
    if (!isLiveSocket(socket)) return;
    sockets.open.add(socket);
    sockets.opened.push(socket);
    socket.on("close", () => sockets.open.delete(socket));
    // The page sends only the contract's client frames and Dispatch only its server frames, each
    // one JSON object.
    socket.on("framesent", ({ payload }) => {
      sockets.sent.push(JSON.parse(String(payload)) as LiveClientFrame);
    });
    socket.on("framereceived", ({ payload }) => {
      sockets.received.push((JSON.parse(String(payload)) as LiveServerFrame).type);
    });
  });
  return sockets;
}

/** Waits until the page holds one open live socket and its event feed has started on it. */
async function untilFeedStarts(sockets: LiveSockets): Promise<void> {
  await expect.poll(() => sockets.open.size).toBe(1);
  await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
}

function replay(sessionID: string, extra: readonly object[] = []): object {
  return {
    frames: [
      ...Array.from({ length: 40 }, (_, index) => ({
        kind: "message",
        message: {
          at: 1_000 + index,
          id: `turn-${index}`,
          parts: [
            {
              text: `Turn ${index}: ${"The live socket keeps this text. ".repeat(4)}`,
              type: "text",
            },
          ],
          role: index % 2 === 0 ? "user" : "assistant",
          streaming: false,
        },
        seq: index + 1,
        v: 1,
      })),
      ...extra,
    ],
    session_id: sessionID,
    v: 1,
  };
}

function topTurn(page: Page): Promise<{ id: string; top: number }> {
  return page.evaluate(() => {
    const thread = document.querySelector<HTMLElement>('[data-testid="agent-thread"]');
    if (thread === null) throw new Error("no agent thread");
    const edge = thread.getBoundingClientRect().top;
    for (const turn of thread.querySelectorAll<HTMLElement>("[data-message-id]")) {
      const box = turn.getBoundingClientRect();
      if (box.bottom > edge && box.top < edge + thread.clientHeight) {
        return { id: turn.dataset.messageId ?? "", top: box.top - edge };
      }
    }
    throw new Error("no agent turn is visible");
  });
}

function trackApiRequests(page: Page): Map<Request, number> {
  const pending = new Map<Request, number>();
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/")) pending.set(request, Date.now());
  });
  const settled = (request: Request) => pending.delete(request);
  page.on("requestfailed", settled);
  page.on("requestfinished", settled);
  return pending;
}

async function waitForReads(pending: Map<Request, number>): Promise<void> {
  await expect.poll(() => pending.size).toBe(0);
}

/** Every document the page fetched for itself, in order: its first load and every reload after
 *  it. The `load` event is not that count - Playwright's Firefox fires one for the page's initial
 *  `about:blank`, Chromium none - while a reload is a navigation request of the main frame in
 *  every engine. */
function trackDocumentLoads(page: Page): string[] {
  const loaded: string[] = [];
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      loaded.push(request.url());
    }
  });
  return loaded;
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a tab holds one live socket for every live view", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Board issue" });
  await seedDeliveryFixture();
  await setLiveSessions([agent]);
  await setAgentStreamResponder(agent.session_id, true);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = watchLiveSockets(page);

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Todo" })).toBeVisible();
    await expect.poll(() => sockets.open.size).toBe(1);

    await navigateInApp(page, `/agents/${agent.session_id}/live`);
    await expect(page.getByTestId("agent-thread")).toBeVisible();
    await expect.poll(() => sockets.open.size).toBe(1);

    await navigateInApp(page, "/delivery?mode=list");
    await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    await expect.poll(() => sockets.open.size).toBe(1);

    // Each view rode the one socket the page opened at its first load: the event feed, the
    // session's agent view and the delivery timeline.
    const views = () =>
      sockets.sent.flatMap((frame) => (frame.type === "subscribe" ? [frame.view] : []));
    await expect.poll(views).toContainEqual({ kind: "events" });
    await expect.poll(views).toContainEqual({ kind: "agent", session_id: agent.session_id });
    await expect.poll(views).toContainEqual(expect.objectContaining({ kind: "timeline" }));
    expect(sockets.opened).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// Forty seconds of real idling: a page that holds a request open shows it here and nowhere else.
// It runs in every engine of this spec's projects, since what it proves - that the live socket
// replaced the long-lived request - is each engine's own connection behaviour.
test("no request to Dispatch stays open while the page idles", async ({ browser }) => {
  test.setTimeout(60_000);
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Board issue" });
  await seedDeliveryFixture();
  await setLiveSessions([agent]);
  await setAgentStreamResponder(agent.session_id, true);

  const alice = await asUser(browser, "alice");
  try {
    const board = await alice.newPage();
    const live = await alice.newPage();
    const delivery = await alice.newPage();
    const pending = [trackApiRequests(board), trackApiRequests(live), trackApiRequests(delivery)];
    const sockets = [watchLiveSockets(board), watchLiveSockets(live), watchLiveSockets(delivery)];

    await board.goto("/projects/CORE");
    await board.getByRole("button", { name: "Board" }).click();
    await expect(board.getByRole("region", { name: "Todo" })).toBeVisible();
    await live.goto(`/agents/${agent.session_id}/live`);
    await expect(live.getByTestId("agent-thread")).toBeVisible();
    await delivery.goto("/delivery?mode=list");
    await expect(delivery.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    // Each page's live views ride its socket, so the idle below holds them open; a page whose
    // socket had not opened would idle with nothing to hold.
    for (const pageSockets of sockets) await untilFeedStarts(pageSockets);
    await Promise.all(pending.map(waitForReads));

    await board.waitForTimeout(40_000);
    const old = pending.flatMap((requests) =>
      [...requests.entries()]
        .filter(([, started]) => Date.now() - started > 20_000)
        .map(([request]) => request.url())
    );
    expect(old).toEqual([]);
  } finally {
    await alice.close();
  }
});

// The page rotates before it has seen any event, so its event feed resumes from the sign-in
// check's head: the change committed while the next socket waits arrives on it, and the rotation
// costs no request beyond what that change costs a page with no rotation (the control).
test("a rotation loses no event and makes no extra request", async ({ browser }) => {
  test.setTimeout(45_000);
  await createProject({ key: "CORE", name: "Core" });
  const baselineIssue = await createIssue({ project: "CORE", title: "Comment request control" });

  const alice = await asUser(browser, "alice");
  try {
    const control = await alice.newPage();
    const controlPending = trackApiRequests(control);
    const controlSocket = control.waitForEvent("websocket", isLiveSocket);
    await control.goto(`/issues/${baselineIssue.key}/activity`);
    await expect(control.getByText("Comment request control", { exact: true })).toBeVisible();
    await controlSocket;
    await control.waitForTimeout(1_000);
    await waitForReads(controlPending);

    const controlRequests: string[] = [];
    let countControlRequests = false;
    control.on("request", (request) => {
      if (countControlRequests && new URL(request.url()).pathname.startsWith("/api/")) {
        controlRequests.push(request.url());
      }
    });
    countControlRequests = true;
    const controlComment = await createComment(
      baselineIssue.key,
      { body: "Control comment request count" },
      bob
    );
    await expect(
      control.locator(`a[href="/issues/${baselineIssue.key}/comments/${controlComment.id}"]`)
    ).toBeVisible();
    await control.waitForTimeout(5_000);
    countControlRequests = false;
    const expectedRequestCount = controlRequests.length;
    await control.close();

    const issue = await createIssue({ project: "CORE", title: "Rotation target" });
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    const sockets = watchLiveSockets(page);
    let holdReconnect = false;
    let held: WebSocketRoute | undefined;
    const reconnectReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      if (holdReconnect) {
        held = route;
        await reconnectReleased.promise;
        connect(route);
        return;
      }
      connect(route);
    });
    const first = page.waitForEvent("websocket", isLiveSocket);
    await page.goto(`/issues/${issue.key}/activity`);
    await expect(page.getByText("Rotation target", { exact: true })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await first;
    await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
    await page.waitForTimeout(1_000);
    await waitForReads(pending);

    expect(sockets.received).not.toContain("event");
    holdReconnect = true;
    await rotateLiveSockets();
    await expect.poll(() => held !== undefined).toBe(true);
    const comment = await createComment(issue.key, { body: "Committed while rotation waits" }, bob);
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);

    const apiRequests: string[] = [];
    let countRequests = true;
    page.on("request", (request) => {
      if (countRequests && new URL(request.url()).pathname.startsWith("/api/")) {
        apiRequests.push(request.url());
      }
    });
    if (held === undefined) throw new Error("the reopened live socket was not held");
    reconnectReleased.resolve();
    await expect.poll(() => sockets.received).toContain("event");
    await expect(
      page.locator(`a[href="/issues/${issue.key}/comments/${comment.id}"]`)
    ).toBeVisible();
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);
    await page.waitForTimeout(5_000);
    countRequests = false;
    expect(apiRequests).toHaveLength(expectedRequestCount);
  } finally {
    await alice.close();
  }
});

test("a change committed while the page loads is shown without a reload", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Before the page load" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    const loaded = trackDocumentLoads(page);
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Before the page load", { exact: true })).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    await waitForReads(pending);

    await patchIssue(issue.key, { title: "Committed during the page load" }, bob);
    if (held === undefined) throw new Error("the first live socket was not held");
    const opened = page.waitForEvent("websocket", isLiveSocket);
    firstSocketReleased.resolve();
    await opened;

    await expect(page.getByText("Committed during the page load", { exact: true })).toBeVisible({
      timeout: 5_000,
    });
    expect(loaded).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a first live socket opening without a change makes no API request", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Quiet page load" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    const sockets = watchLiveSockets(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });
    const requestsAfterOpen: string[] = [];
    let countRequests = false;
    page.on("request", (request) => {
      if (countRequests && new URL(request.url()).pathname.startsWith("/api/")) {
        requestsAfterOpen.push(request.url());
      }
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Quiet page load", { exact: true })).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    await waitForReads(pending);
    if (held === undefined) throw new Error("the first live socket was not held");
    countRequests = true;
    firstSocketReleased.resolve();
    // The socket opened and the feed started on it, so the quiet below is the open's own.
    await untilFeedStarts(sockets);
    await page.waitForTimeout(5_000);
    expect(requestsAfterOpen).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("an agent live view survives a rotation in place", async ({ browser }) => {
  await setLiveSessions([agent]);
  await publishAgentStreamFrame(agent.session_id, replay(agent.session_id), "replay");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = watchLiveSockets(page);
    await page.goto(`/agents/${agent.session_id}/live`);
    await expect(page.getByText("Turn 39:", { exact: false })).toBeVisible();

    const beforeFrame = {
      kind: "message",
      message: {
        at: 2_000,
        id: "before-rotation",
        parts: [{ text: "Before the rotation", type: "text" }],
        role: "assistant",
        streaming: false,
      },
      seq: 41,
      v: 1,
    };
    await publishAgentStreamFrame(agent.session_id, beforeFrame);
    await expect(page.getByText("Before the rotation", { exact: true })).toBeVisible();
    await page.getByText("Turn 12:", { exact: false }).evaluate((element) => {
      element.scrollIntoView({ block: "start" });
    });
    const before = await topTurn(page);

    await publishAgentStreamFrame(
      agent.session_id,
      replay(agent.session_id, [beforeFrame]),
      "replay"
    );
    await rotateLiveSockets();
    await expect.poll(() => sockets.open.size).toBe(1);
    await expect
      .poll(
        () =>
          sockets.sent.filter((frame) => frame.type === "subscribe" && frame.view.kind === "agent")
            .length
      )
      .toBe(2);

    await publishAgentStreamFrame(agent.session_id, {
      kind: "message",
      message: {
        at: 3_000,
        id: "after-rotation",
        parts: [{ text: "After the rotation", type: "text" }],
        role: "assistant",
        streaming: false,
      },
      seq: 42,
      v: 1,
    });
    await expect(page.getByText("After the rotation", { exact: true })).toBeVisible();
    await expect(page.getByText("Before the rotation", { exact: true })).toBeVisible();
    const after = await topTurn(page);
    expect(after.id).toBe(before.id);
    expect(Math.abs(after.top - before.top)).toBeLessThanOrEqual(1);
  } finally {
    await alice.close();
  }
});
