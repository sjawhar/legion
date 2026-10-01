import { expect, type Page, test } from "@playwright/test";

import { agentRow, type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createIssue,
  createProject,
  publishAgentStreamFrame,
  replyToMessageDelivery,
  setAgentStreamResponder,
} from "./api";
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

/** The phone layout runs on Chromium (the `iphone` project) and on WebKit (`webkit-iphone`). */
const PHONE_PROJECTS = ["iphone", "webkit-iphone"];

interface LiveViewLayout {
  /** The document's scrollable height and the layout viewport's height. */
  documentHeight: number;
  viewportHeight: number;
  /** The visual viewport's bottom edge, in the layout viewport's coordinates. */
  visualBottom: number;
  composerTop: number;
  composerBottom: number;
  /** The newest turn's bottom edge, and the thread's own top edge and scroll position. */
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
    const byTestId = (testId: string) => {
      const element = document.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
      if (element === null) throw new Error(`no ${testId}`);
      return element;
    };
    const main = byTestId("main-content");
    const thread = byTestId("agent-thread");
    const composer = byTestId("agent-composer").getBoundingClientRect();
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
  // The composer ends one page gutter above the visual viewport's bottom, not merely on screen:
  // a composer overflowing a collapsed thread would still be inside the window.
  expect(layout.composerBottom).toBe(layout.visualBottom - layout.mainPaddingBottom);
  await expect(page.getByRole("link", { name: "← Agents" })).toBeInViewport({ ratio: 1 });
  await expect(page.getByTestId("agent-composer")).toBeInViewport({ ratio: 1 });
  expect(layout.threadScrollHeight).toBeGreaterThan(layout.threadClientHeight);
  await page.getByTestId("agent-thread").evaluate((thread) => thread.scrollTo(0, 0));
  await expect(page.getByText("Turn 0:")).toBeInViewport();
  expect((await liveViewLayout(page)).composerBottom).toBe(layout.composerBottom);
}

/** Scrolls the thread the way a reader does, and waits for the scroll event to be delivered
 *  before going on. The thread follows its bottom only once that event has told it the reader is
 *  there, and WebKit delivers scroll events a frame later than Chromium: a keyboard raised before
 *  delivery raced the event and left the newest turn behind in about half of WebKit's runs. */
async function scrollThreadTo(page: Page, top: number | "bottom"): Promise<void> {
  await page.getByTestId("agent-thread").evaluate(
    (thread, target) =>
      new Promise<void>((resolve) => {
        const to = target === "bottom" ? thread.scrollHeight - thread.clientHeight : target;
        if (Math.abs(thread.scrollTop - to) < 1) {
          resolve();
          return;
        }
        thread.addEventListener("scroll", () => resolve(), { once: true });
        thread.scrollTo(0, to);
      }),
    top
  );
}

/** Whether <main> carries a keyboard cap, and whether that cap is at or below <main>'s own
 *  height. A cap that binds shrinks the element to itself, so this cannot tell "exactly its own
 *  height" from "smaller"; the gutter check beside each call site is what proves it moves
 *  nothing. */
function mainCap(page: Page): Promise<{ capped: boolean; fitsOwnHeight: boolean }> {
  return page.getByTestId("main-content").evaluate((main) => {
    const cap = Number.parseFloat(getComputedStyle(main).maxHeight);
    const capped = !Number.isNaN(cap);
    return {
      capped,
      fitsOwnHeight: capped && Math.abs(cap - main.getBoundingClientRect().height) < 0.5,
    };
  });
}

/** The newest turn is in view: its bottom edge is inside the thread and above the composer. */
async function expectNewestTurnAboveComposer(page: Page): Promise<void> {
  await expect
    .poll(async () => {
      const layout = await liveViewLayout(page);
      return layout.newestBottom <= layout.composerTop && layout.newestBottom > layout.threadTop;
    })
    .toBe(true);
}

/** iOS Safari's keyboard: the composer takes focus and only the visual viewport shrinks, to
 *  `height`, panned `offsetTop` px down the page (Safari pans it to bring a low input into view).
 *  `<main>` is capped in the same task as the resize event (a deferred write would leave a frame
 *  with the composer behind the keyboard), and the composer then sits one page gutter above the
 *  visual viewport's bottom edge. */
async function raiseKeyboard(page: Page, height: number, offsetTop = 0): Promise<void> {
  await page.getByTestId("agent-composer").locator("textarea").focus();
  const sameTask = await page.evaluate(
    ({ visualHeight, visualTop }) => {
      const viewport = window.visualViewport;
      const main = document.querySelector<HTMLElement>('[data-testid="main-content"]');
      if (viewport === null || main === null) throw new Error("no visualViewport or main");
      Object.defineProperty(viewport, "height", { configurable: true, get: () => visualHeight });
      Object.defineProperty(viewport, "offsetTop", { configurable: true, get: () => visualTop });
      viewport.dispatchEvent(new Event("resize"));
      return {
        expected: visualTop + visualHeight - main.getBoundingClientRect().top,
        maxHeight: getComputedStyle(main).maxHeight,
      };
    },
    { visualHeight: height, visualTop: offsetTop }
  );
  expect(Number.parseFloat(sameTask.maxHeight)).toBeCloseTo(sameTask.expected, 1);
  await expect
    .poll(async () => {
      const layout = await liveViewLayout(page);
      return layout.visualBottom - layout.mainPaddingBottom - layout.composerBottom;
    })
    .toBe(0);
  expect((await liveViewLayout(page)).visualBottom).toBe(offsetTop + height);
}

/** The keyboard closes: the visual viewport is the whole window again and the composer blurs,
 *  which returns it to one page gutter above the bottom of the screen. */
async function lowerKeyboard(page: Page): Promise<void> {
  await page.evaluate(() => {
    const viewport = window.visualViewport;
    if (viewport === null) throw new Error("no visualViewport");
    Reflect.deleteProperty(viewport, "height");
    Reflect.deleteProperty(viewport, "offsetTop");
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

test("the conversation view replays what the session held, streams its next turn, and sends as Send, the terminal's Enter, through the existing delivery", async ({
  browser,
}) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    // A session Dispatch has never heard from sits in a collapsed fold, its row mounted and
    // hidden until the fold opens.
    await page.getByRole("button", { name: /No Dispatch activity/ }).click();
    await agentRow(page, planner.session_id).getByRole("link", { name: "Open" }).click();
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

    // Talking to the agent is the delivery the Agents page already uses, not a new write path,
    // and its default is Send - Enter at the session's terminal - since the session takes a steer.
    // Send being usable here is also what the late-snapshot rule above buys: a thread left
    // looking like it is still running disables the composer, and a viewer who opens a finished
    // session cannot talk to it.
    await expect(page.getByRole("combobox", { name: "Delivery mode" })).toHaveValue("steer");
    await page.getByTestId("agent-composer").locator("textarea").fill("try the other branch");
    await expect(page.getByRole("button", { name: "Send" })).toBeEnabled();
    await page.getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () =>
        (await getSentMessages()).map((sent) => ({
          message: sent.message,
          payload: JSON.parse(String(sent.payload)),
        }))
      )
      .toContainEqual({
        message: "try the other branch",
        payload: expect.objectContaining({ delivery: expect.objectContaining({ mode: "steer" }) }),
      });
  } finally {
    await context.close();
  }
});

// A session answers a human's direct message through Dispatch, which the relayed stream shows
// only as a tool call. The replies are news the human did not go looking for, so they are
// counted unread in the navigation, and the live view shows them - which is reading them.
test("a session's replies to a direct message are unread until the live view shows them", async ({
  browser,
}) => {
  const asked = await createAgentMessage(planner.session_id, {
    body: "Where is the dashboard?",
    delivery: "aside",
  });
  const session = { id: planner.session_id, kind: "session" as const };
  await replyToMessageDelivery(asked.id, { attempt: 1, body: "Switching to it now." }, session);
  // A follow-up, as dispatch_message sends it.
  await replyToMessageDelivery(asked.id, { attempt: 1, body: "Done: it is at /dash." }, session, {
    followUp: true,
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    // A shown badge: a folded row's text is in the page too, hidden.
    await expect(page.getByText("New replies 2").filter({ visible: true }).first()).toBeVisible();

    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-thread")).toContainText("Where is the dashboard?");
    const replies = page.getByTestId("agent-dispatch-reply");
    await expect(replies).toHaveCount(2);
    await expect(replies.first()).toContainText("Switching to it now.");
    await expect(replies.last()).toContainText("Done: it is at /dash.");
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
    await page.reload();
    await expect(page.getByTestId("agent-dispatch-reply")).toHaveCount(2);
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The unread count is over every direct message the human sent, while the conversation list is a
// window of the fifty that moved last. A reply outside that window is counted, and the mark this
// view writes marks it read, so the live view has to show it or it is read without being seen.
test("the live view shows an unread reply from outside the fifty most active conversations", async ({
  browser,
}) => {
  const session = { id: planner.session_id, kind: "session" as const };
  const asked = await createAgentMessage(planner.session_id, {
    body: "Did the migration land?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "No - it rolled back, here is why." },
    session
  );
  // Fifty conversations that moved after it, each answered, so the one above is the fifty-first.
  for (let index = 0; index < 50; index += 1) {
    const busier = await createAgentMessage(planner.session_id, {
      body: `Busier question ${index}`,
      delivery: "aside",
    });
    await replyToMessageDelivery(
      busier.id,
      { attempt: 1, body: `Busier answer ${index}` },
      session
    );
  }
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(page.getByText("New replies 51").filter({ visible: true }).first()).toBeVisible();

    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-thread")).toContainText("Did the migration land?");
    await expect(page.getByText("No - it rolled back, here is why.")).toBeVisible();
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
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
    await expect(page.getByText(/relayed while this page is open/)).toBeVisible();
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
  test.skip(
    !PHONE_PROJECTS.includes(testInfo.project.name),
    "the phone layout runs on the phone projects"
  );
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
    // shrinking the layout viewport, which already fits the shell above it. With the composer
    // unfocused <main> carries no cap; focused, the cap equals <main>'s own height, so it moves
    // nothing, and the composer sits one gutter above the keyboard either way.
    const composerInput = page.getByTestId("agent-composer").locator("textarea");
    await page.setViewportSize({ height: 500, width: 390 });
    expect(await mainCap(page)).toEqual({ capped: false, fitsOwnHeight: false });
    await expectViewportBound(page);
    await composerInput.focus();
    // The pair discriminates jointly: mainCap says a cap is present and not larger than <main>,
    // and expectViewportBound's gutter check is the one that proves the cap moves nothing, so it
    // is not redundant beside it.
    await expect.poll(() => mainCap(page)).toEqual({ capped: true, fitsOwnHeight: true });
    await expectViewportBound(page);
    await composerInput.blur();
    await page.setViewportSize({ height: 844, width: 390 });

    // iOS Safari opens it by shrinking only the visual viewport. A reader at the newest turn
    // raises the keyboard: the composer sits one page gutter above it, and the newest turn stays
    // in view above the composer.
    await scrollThreadTo(page, "bottom");
    await raiseKeyboard(page, 500);
    await expect(page.getByRole("link", { name: "← Agents" })).toBeInViewport({ ratio: 1 });
    await expectNewestTurnAboveComposer(page);

    // Closing it takes the composer back to the bottom of the screen, still under the newest turn.
    await lowerKeyboard(page);
    await expectNewestTurnAboveComposer(page);

    // A reader scrolled back into the history keeps their place through a raise and a lower.
    await scrollThreadTo(page, 1_200);
    const reading = (await liveViewLayout(page)).threadScrollTop;
    expect(reading).toBe(1_200);
    await raiseKeyboard(page, 500);
    expect((await liveViewLayout(page)).threadScrollTop).toBe(reading);
    await lowerKeyboard(page);
    expect((await liveViewLayout(page)).threadScrollTop).toBe(reading);

    // Safari also pans the visual viewport down the page to bring a low input into view: the cap
    // and the gutter follow the visual viewport's bottom in page coordinates, offsetTop included.
    await raiseKeyboard(page, 500, 120);
    await lowerKeyboard(page);
  } finally {
    await context.close();
  }
});

test("at desktop width the live view fills the main column beside the navigation and the thread is its scroller", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
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

test("a document-scrolling route is never capped for the keyboard, at mount or after focus and resize", async ({
  browser,
}, testInfo) => {
  test.skip(
    !PHONE_PROJECTS.includes(testInfo.project.name),
    "the phone layout runs on the phone projects"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Scrolls the document.",
    title: "Doc route",
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // Inspect the properties, not the layout: a stray cap or custom property on <main> here has no
  // layout signature on a document-scrolling page, only a style recalc on every keyboard frame.
  const mainKeyboardStyle = () =>
    page.getByTestId("main-content").evaluate((main) => ({
      computedMaxHeight: getComputedStyle(main).maxHeight,
      customProperties: Array.from(main.style).filter((name) => name.startsWith("--")),
      inlineMaxHeight: main.style.maxHeight,
    }));
  const uncapped = { computedMaxHeight: "none", customProperties: [], inlineMaxHeight: "" };
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    // The keyboard is already up when the route mounts.
    await page.addInitScript(() => {
      const viewport = window.visualViewport;
      if (viewport !== null) {
        Object.defineProperty(viewport, "height", { configurable: true, get: () => 500 });
      }
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByTestId("main-content").locator("textarea").first();
    await expect(composer).toBeVisible();
    expect(await mainKeyboardStyle()).toEqual(uncapped);

    await composer.focus();
    await page.evaluate(() => window.visualViewport?.dispatchEvent(new Event("resize")));
    expect(await mainKeyboardStyle()).toEqual(uncapped);
  } finally {
    await context.close();
  }
});
