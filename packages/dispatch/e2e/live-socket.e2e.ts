import {
  type WebSocket as BrowserWebSocket,
  expect,
  type Page,
  type Request,
  test,
  type WebSocketRoute,
} from "@playwright/test";

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

function watchLiveSockets(page: Page): {
  readonly open: Set<BrowserWebSocket>;
  readonly subscriptions: unknown[];
} {
  const open = new Set<BrowserWebSocket>();
  const subscriptions: unknown[] = [];
  page.on("websocket", (socket) => {
    if (!isLiveSocket(socket)) return;
    open.add(socket);
    socket.on("close", () => open.delete(socket));
    socket.on("framesent", (event) => {
      const payload = typeof event.payload === "string" ? event.payload : event.payload.toString();
      try {
        subscriptions.push(JSON.parse(payload));
      } catch {
        // Only subscribe frames matter below. A malformed frame is the server's concern.
      }
    });
  });
  return { open, subscriptions };
}

function agentSubscriptions(subscriptions: readonly unknown[]): number {
  return subscriptions.filter(
    (frame) =>
      typeof frame === "object" &&
      frame !== null &&
      "type" in frame &&
      "view" in frame &&
      frame.type === "subscribe" &&
      typeof frame.view === "object" &&
      frame.view !== null &&
      "kind" in frame.view &&
      frame.view.kind === "agent"
  ).length;
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
  } finally {
    await alice.close();
  }
});

test("no request to Dispatch stays open while the page idles", async ({ browser }, testInfo) => {
  test.setTimeout(60_000);
  test.skip(testInfo.project.name !== "chromium", "the forty-second idle proof runs once");
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

    await board.goto("/projects/CORE");
    await board.getByRole("button", { name: "Board" }).click();
    await expect(board.getByRole("region", { name: "Todo" })).toBeVisible();
    await live.goto(`/agents/${agent.session_id}/live`);
    await expect(live.getByTestId("agent-thread")).toBeVisible();
    await delivery.goto("/delivery?mode=list");
    await expect(delivery.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
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

test("a rotation loses no event on a page that has seen none", async ({ browser }, testInfo) => {
  test.setTimeout(45_000);
  test.skip(testInfo.project.name !== "chromium", "the rotation proof runs once");
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
    const frames: string[] = [];
    page.on("websocket", (socket) => {
      if (!isLiveSocket(socket)) return;
      socket.on("framereceived", (event) => {
        const payload =
          typeof event.payload === "string" ? event.payload : event.payload.toString();
        const frame = JSON.parse(payload) as { readonly type?: string };
        frames.push(frame.type ?? "");
      });
    });
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
    await expect.poll(() => frames.includes("events_start")).toBe(true);
    await page.waitForTimeout(1_000);
    await waitForReads(pending);

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
    await expect.poll(() => frames).toContain("event");
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

test("a change committed while the page loads is shown without a reload", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the event-head proof runs once");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Before the page load" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    let loads = 0;
    page.on("load", () => {
      loads += 1;
    });
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
    expect(loads).toBe(1);
  } finally {
    await alice.close();
  }
});

test("a first live socket opening without a change makes no API request", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the event-head control runs once");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Quiet page load" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });
    const opened = page.waitForEvent("websocket", isLiveSocket);
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
    firstSocketReleased.resolve();
    await opened;
    countRequests = true;
    await page.waitForTimeout(5_000);
    expect(requestsAfterOpen).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("an agent live view survives a rotation in place", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the transcript position proof runs once");
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
    await expect.poll(() => agentSubscriptions(sockets.subscriptions)).toBe(2);

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
