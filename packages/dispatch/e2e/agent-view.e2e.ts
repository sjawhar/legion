import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { publishAgentStreamFrame, setAgentStreamResponder } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The live agent conversation view. The harness runs Dispatch with NATS disabled, so the frames
 * here come from the test hook rather than from a session; what the test proves is everything on
 * this side of the relay — replay, live snapshots applied last-writer-wins, a tool call finding
 * its result, and a human's message going out through the existing delivery.
 */

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  last_seen: Date.now() - 5_000,
  machine_id: "planner-host",
  roles: [],
  session_id: "01a0e090-4848-7473-acc5-fc96e6a646d3",
  title: "Planner",
};

const TOOL_CALL_ID = "call-1";

/** The session's replay: a human's prompt, and the assistant turn that answered it. */
const replay = {
  frames: [
    {
      kind: "message",
      message: {
        at: 1_000,
        id: "u1000",
        parts: [{ text: "Read the release notes", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    },
    {
      kind: "message",
      message: {
        at: 2_000,
        id: "a2000",
        parts: [
          { text: "Checking the notes.", type: "text" },
          {
            argsText: '{\n  "path": "CHANGELOG.md"\n}',
            toolCallId: TOOL_CALL_ID,
            toolName: "read",
            type: "tool-call",
          },
        ],
        role: "assistant",
        streaming: false,
      },
      seq: 2,
      v: 1,
    },
    {
      kind: "tool-result",
      result: {
        at: 2_500,
        isError: false,
        output: "## Unreleased\n- the relay",
        toolCallId: TOOL_CALL_ID,
        toolName: "read",
      },
      seq: 3,
      v: 1,
    },
  ],
  session_id: planner.session_id,
  v: 1,
};

function assistantFrame(seq: number, text: string, streaming: boolean): object {
  return {
    kind: "message",
    message: {
      at: 3_000,
      id: "a3000",
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming,
    },
    seq,
    v: 1,
  };
}

/** A replay long enough that the thread must scroll at any viewport: forty turns of prose. */
function longReplay(sessionID: string): object {
  const frames = Array.from({ length: 40 }, (_, index) => ({
    kind: "message",
    message: {
      at: 1_000 + index * 1_000,
      id: `m${index}`,
      parts: [
        {
          text: `Turn ${index}: ${"the relay carries this turn to the viewer. ".repeat(4)}`,
          type: "text",
        },
      ],
      role: index % 2 === 0 ? "user" : "assistant",
      streaming: false,
    },
    seq: index + 1,
    v: 1,
  }));
  return { frames, session_id: sessionID, v: 1 };
}

interface LiveViewLayout {
  /** The document's scrollable height and the layout viewport's height. */
  documentHeight: number;
  viewportHeight: number;
  /** The visual viewport's bottom edge, in the layout viewport's coordinates. */
  visualBottom: number;
  composerTop: number;
  composerBottom: number;
  /** The newest turn's edges, and the thread's own top edge and scroll position. */
  newestTop: number;
  newestBottom: number;
  threadTop: number;
  threadScrollTop: number;
  mainBottom: number;
  mainPaddingBottom: number;
  threadScrollHeight: number;
  threadClientHeight: number;
}

function liveViewLayout(page: Page): Promise<LiveViewLayout> {
  return page.evaluate(() => {
    const box = (testId: string) => {
      const element = document.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
      if (element === null) throw new Error(`no ${testId}`);
      return element;
    };
    const main = box("main-content");
    const thread = box("agent-thread");
    const composer = box("agent-composer").getBoundingClientRect();
    const turns = thread.querySelectorAll('[data-testid^="agent-message-"]');
    const newest = turns[turns.length - 1]?.getBoundingClientRect();
    if (newest === undefined) throw new Error("no turns");
    const viewport = window.visualViewport;
    if (viewport === null) throw new Error("no visualViewport");
    return {
      composerBottom: composer.bottom,
      composerTop: composer.top,
      documentHeight: document.documentElement.scrollHeight,
      mainBottom: main.getBoundingClientRect().bottom,
      mainPaddingBottom: Number.parseFloat(getComputedStyle(main).paddingBottom),
      newestBottom: newest.bottom,
      newestTop: newest.top,
      threadClientHeight: thread.clientHeight,
      threadScrollHeight: thread.scrollHeight,
      threadScrollTop: thread.scrollTop,
      threadTop: thread.getBoundingClientRect().top,
      viewportHeight: window.innerHeight,
      visualBottom: viewport.offsetTop + viewport.height,
    };
  });
}

/** The page never scrolls, its header and composer are on screen, and the thread is the one
 *  scroller: scrolled to its start, the first turn shows and the composer has not moved. */
async function expectViewportBound(page: Page): Promise<void> {
  await page.evaluate(() => window.scrollTo(0, 100_000));
  const layout = await liveViewLayout(page);
  expect(layout.documentHeight).toBeLessThanOrEqual(layout.viewportHeight + 1);
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  expect(layout.composerBottom).toBeLessThanOrEqual(layout.viewportHeight);
  await expect(page.getByRole("link", { name: "← Agents" })).toBeInViewport({ ratio: 1 });
  await expect(page.getByTestId("agent-composer")).toBeInViewport({ ratio: 1 });
  expect(layout.threadScrollHeight).toBeGreaterThan(layout.threadClientHeight);
  await page.getByTestId("agent-thread").evaluate((thread) => thread.scrollTo(0, 0));
  await expect(page.getByText("Turn 0:")).toBeInViewport();
  expect((await liveViewLayout(page)).composerBottom).toBe(layout.composerBottom);
}

/** iOS Safari's keyboard: the composer takes focus and only the visual viewport shrinks, to
 *  `height`. The composer then sits one page gutter above the visual viewport's bottom edge. */
async function raiseKeyboard(page: Page, height: number): Promise<void> {
  await page.getByTestId("agent-composer").locator("textarea").focus();
  await page.evaluate((visualHeight) => {
    const viewport = window.visualViewport;
    if (viewport === null) throw new Error("no visualViewport");
    Object.defineProperty(viewport, "height", { configurable: true, get: () => visualHeight });
    viewport.dispatchEvent(new Event("resize"));
  }, height);
  await expect
    .poll(async () => {
      const layout = await liveViewLayout(page);
      return layout.visualBottom - layout.mainPaddingBottom - layout.composerBottom;
    })
    .toBe(0);
  expect((await liveViewLayout(page)).visualBottom).toBe(height);
}

/** The keyboard closes: the visual viewport is the whole window again and the composer blurs,
 *  which returns it to one page gutter above the bottom of the screen. */
async function lowerKeyboard(page: Page): Promise<void> {
  await page.evaluate(() => {
    const viewport = window.visualViewport;
    if (viewport === null) throw new Error("no visualViewport");
    Reflect.deleteProperty(viewport, "height");
    viewport.dispatchEvent(new Event("resize"));
  });
  await page.getByTestId("agent-composer").locator("textarea").blur();
  await expect
    .poll(async () => {
      const layout = await liveViewLayout(page);
      return layout.viewportHeight - layout.mainPaddingBottom - layout.composerBottom;
    })
    .toBe(0);
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([planner])]);
});

test("the conversation view replays what the session held, streams its next turn, and sends an aside through the existing delivery", async ({
  browser,
}) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    // A session Dispatch has never heard from sits in a collapsed fold.
    await page.getByRole("button", { name: /No Dispatch activity/ }).click();
    await page.getByRole("link", { name: "Open" }).first().click();
    await expect(page).toHaveURL(new RegExp(`/agents/${planner.session_id}/live$`));

    // What the session replayed: the human's prompt, the reply, and the tool call with its
    // result folded away until asked for.
    await expect(page.getByTestId("agent-message-user")).toContainText("Read the release notes");
    await expect(page.getByTestId("agent-message-assistant")).toContainText("Checking the notes.");
    const tool = page.getByTestId("agent-tool-call");
    await expect(tool).toContainText("read");
    await expect(page.getByTestId("agent-tool-result")).toHaveCount(0);
    await tool.getByRole("button").click();
    await expect(page.getByTestId("agent-tool-result")).toContainText("- the relay");

    // The next turn arrives as snapshots of one message. The host delivers a message's last few
    // updates AFTER its end, so the late streaming snapshot carries the HIGHER sequence number;
    // it must not rewind the settled text or leave the turn looking like it is still running,
    // which is what disables the composer.
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "Ther", true));
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText("Ther");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(5, "There are two.", false));
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText(
      "There are two."
    );
    await publishAgentStreamFrame(planner.session_id, assistantFrame(6, "There are", true));
    await expect(page.getByTestId("agent-message-assistant")).toHaveCount(2);
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText(
      "There are two."
    );

    // Talking to the agent is the delivery the Agents page already uses, not a new write path.
    // Send being usable here is also what the late-snapshot rule above buys: a thread left
    // looking like it is still running disables the composer, and a viewer who opens a finished
    // session cannot talk to it.
    await page.getByTestId("agent-composer").locator("textarea").fill("try the other branch");
    await expect(page.getByRole("button", { name: "Send" })).toBeEnabled();
    await page.getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () => (await getSentMessages()).map((sent) => sent.message))
      .toContain("try the other branch");
  } finally {
    await context.close();
  }
});

test("a session that has published nothing renders as empty, not as a conversation", async ({
  browser,
}) => {
  // A session of its own: the relay's frames are process-global, so a session another test
  // seeded would carry that test's replay into this one. It answers the relay with no history,
  // which is what a live session that has not spoken yet is.
  const sessionID = "01a0e0c1-0000-7000-8000-00000000beef";
  await setAgentStreamResponder(sessionID, true);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    await expect(page.getByTestId("agent-thread-empty")).toHaveText(
      "Nothing yet. This session's next turn appears here as it happens."
    );
    await expect(page.getByText("Nothing here is stored")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a session nobody answers for is told so, and recovers when a responder appears", async ({
  browser,
}) => {
  // Nobody on the session's control subject: a plugin that predates the live view, or a session
  // that is no longer running. The relay cannot tell those apart and does not claim to.
  const sessionID = "01a0e0c1-0000-7000-8000-00000000cafe";
  await setAgentStreamResponder(sessionID, false);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    const empty = page.getByTestId("agent-thread-empty");
    await expect(empty).toHaveText(/not answering the live view/);
    await expect(empty).toContainText("no longer running");
    // Delivery never went through the stream, so the composer is live either way.
    await expect(page.getByTestId("agent-composer")).toBeVisible();

    // The session restarts onto a plugin that streams. The relay re-asks on every watch tick
    // (AGENT_STREAM_WATCH_INTERVAL_MS), so this page recovers with no reload and no reconnect.
    await publishAgentStreamFrame(
      sessionID,
      {
        frames: [
          {
            kind: "message",
            message: {
              at: 5_000,
              id: "u5000",
              parts: [{ text: "Back on a newer plugin", type: "text" }],
              role: "user",
              streaming: false,
            },
            seq: 1,
            v: 1,
          },
        ],
        session_id: sessionID,
        v: 1,
      },
      "replay"
    );
    await expect(page.getByText("Back on a newer plugin")).toBeVisible({ timeout: 20_000 });
    await expect(empty).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("on a phone the live view never scrolls the page, keeps its header and composer on screen, and follows the keyboard", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the phone layout runs on the iphone project");
  const phone: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-0000000390a4" };
  await setLiveSessions([planner, phone]);
  await publishAgentStreamFrame(phone.session_id, longReplay(phone.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/agents/${phone.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await expectViewportBound(page);

    // Chromium on Android, told `interactive-widget=resizes-content`, opens the keyboard by
    // shrinking the layout viewport.
    await page.setViewportSize({ height: 500, width: 390 });
    await expectViewportBound(page);
    await page.setViewportSize({ height: 844, width: 390 });

    // iOS Safari opens it by shrinking only the visual viewport. A reader at the newest turn
    // raises the keyboard: the composer sits one page gutter above it, and the newest turn stays
    // in view above the composer.
    const thread = page.getByTestId("agent-thread");
    await thread.evaluate((element) => element.scrollTo(0, element.scrollHeight));
    await raiseKeyboard(page, 500);
    await expect(page.getByRole("link", { name: "← Agents" })).toBeInViewport({ ratio: 1 });
    await expect
      .poll(async () => {
        const layout = await liveViewLayout(page);
        return layout.newestBottom <= layout.composerTop && layout.newestBottom > layout.threadTop;
      })
      .toBe(true);

    // Closing it takes the composer back to the bottom of the screen, still under the newest turn.
    await lowerKeyboard(page);
    await expect
      .poll(async () => {
        const layout = await liveViewLayout(page);
        return layout.newestBottom <= layout.composerTop && layout.newestBottom > layout.threadTop;
      })
      .toBe(true);

    // A reader scrolled back into the history keeps their place through a raise and a lower.
    await thread.evaluate((element) => element.scrollTo(0, 1_200));
    const reading = (await liveViewLayout(page)).threadScrollTop;
    expect(reading).toBe(1_200);
    await raiseKeyboard(page, 500);
    expect((await liveViewLayout(page)).threadScrollTop).toBe(reading);
    await lowerKeyboard(page);
    expect((await liveViewLayout(page)).threadScrollTop).toBe(reading);
  } finally {
    await context.close();
  }
});

test("at desktop width the live view fills the main column beside the navigation and the thread is its scroller", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "the desktop layout runs on the desktop project");
  const desktop: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-000000001280" };
  await setLiveSessions([planner, desktop]);
  await publishAgentStreamFrame(desktop.session_id, longReplay(desktop.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 800, width: 1280 });
    await page.goto(`/agents/${desktop.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Navigation" })).toBeVisible();
    await expectViewportBound(page);
    const layout = await liveViewLayout(page);
    expect(layout.composerBottom).toBe(layout.mainBottom - layout.mainPaddingBottom);
    expect(layout.mainBottom).toBe(layout.viewportHeight);
  } finally {
    await context.close();
  }
});
