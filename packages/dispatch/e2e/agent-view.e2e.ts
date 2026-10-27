import { AGENT_STREAM_LIMITS, capAgentStreamText } from "@legion/contracts";
import { expect, type Page, test } from "@playwright/test";

import { agentRow, type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createIssue,
  createProject,
  dropAgentStreamViewers,
  publishAgentStreamFrame,
  replyToMessageDelivery,
  setAgentStreamResponder,
} from "./api";
import { expectConsecutiveLinksClear, expectUncovered } from "./layout";
import { resetDatabase } from "./seed";
import { MESSAGE_REFUSED, navigateInApp, refusePosts } from "./sends";
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

function assistantFrame(seq: number, text: string, streaming: boolean, model?: string): object {
  return {
    kind: "message",
    message: {
      at: 3_000,
      id: "a3000",
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming,
      ...(model === undefined ? {} : { model }),
    },
    seq,
    v: 1,
  };
}

/** A turn the session publishes after a long replay's forty: `index` is both its message id and
 *  its place in time, past the replay's last turn, so the thread orders it newest, while `seq`
 *  orders the snapshots of one turn. `assistantFrame` reuses one id and one time, so every call
 *  to it is another snapshot of one turn, dated into the middle of a long replay. */
function liveTurn(index: number, seq: number, text: string, streaming: boolean): object {
  return {
    kind: "message",
    message: {
      at: 100_000 + index,
      id: `n${index}`,
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

/** The short `replay`'s turns, re-dated into the middle of `longReplay`'s forty: the same folded
 *  tool call, where a reader scrolled back into a long transcript meets it. The thread orders
 *  turns by their time, so where a frame sits in a replay's list says nothing about where it is
 *  read. */
const FOLD_FRAMES = replay.frames.map((frame, index) =>
  "message" in frame ? { ...frame, message: { ...frame.message, at: 20_400 + index * 100 } } : frame
);

/** The replay a session answers a reconnect with once its newest-first budget no longer reaches
 *  the whole transcript: `longReplay`'s forty turns from `from` on (0 keeps them all, for a
 *  session that needs both a transcript that scrolls and something else it published), with the
 *  older ones gone and `extra` frames behind them. The frames keep their own `at`, so the turns
 *  that survive are read where they always were, and a `seq` is last-writer-wins per message, so
 *  `extra` only orders its own snapshots. */
function rebuiltReplay(sessionID: string, from: number, extra: readonly object[] = []): object {
  const { frames } = longReplay(sessionID) as { frames: object[] };
  return { frames: [...frames.slice(from), ...extra], session_id: sessionID, v: 1 };
}

/** The phone layout runs on Chromium (the `iphone` project) and on WebKit (`webkit-iphone`). */
const PHONE_PROJECTS = ["iphone", "webkit-iphone"];

/** Reading back through a live transcript is the same in both engines, so it runs on the desktop
 *  project and the phone one; what the phone's WebKit is for is the layout row below. */
const READING_PROJECTS = ["chromium", "iphone"];

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

/** Which turn the reader is looking at, and where it sits on their screen: the topmost turn the
 *  thread shows any part of, by the key it carries, with its top edge relative to the thread's.
 *  This is what "the page left me where I was" means to a reader, and it survives the browser's
 *  own scroll anchoring moving `scrollTop` when something above them grows. */
function topmostTurn(page: Page): Promise<{ id: string; top: number }> {
  return page.evaluate(() => {
    const thread = document.querySelector<HTMLElement>('[data-testid="agent-thread"]');
    if (thread === null) throw new Error("no agent-thread");
    const edge = thread.getBoundingClientRect().top;
    for (const turn of thread.querySelectorAll<HTMLElement>("[data-message-id]")) {
      const box = turn.getBoundingClientRect();
      if (box.bottom > edge && box.top < edge + thread.clientHeight) {
        return { id: turn.getAttribute("data-message-id") ?? "", top: box.top - edge };
      }
    }
    throw new Error("no turn in view");
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

/** A rebuild of the whole transcript, the way a reader meets one. The agent subscription carries
 * no cursor, so its dropped relay viewer ends as `dropped`; the page resubscribes that view on the
 * same live socket and asks the session for its replay again. `publishAgentStreamFrame` sets what
 * that next subscription is answered with rather than pushing into the open one, so `next` is in
 * place before the viewer is dropped. */
async function rebuildThroughResubscribe(sessionID: string, next: object): Promise<void> {
  await publishAgentStreamFrame(sessionID, next, "replay");
  await dropAgentStreamViewers(sessionID);
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
  await resetDatabase();
  await setLiveSessions([planner]);
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
    // The row's live link, which `AgentName` draws beside the row's toggle.
    await agentRow(page, planner.session_id)
      .getByRole("link", { name: "Open live conversation with Planner" })
      .click();
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
    // it must not rewind the settled text or leave the finished turn rendered as still
    // streaming.
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

// A person talks to a session while it works, as at its terminal: a turn still streaming does
// not hold Send, and the message goes out at once, before the turn ends. Taking it does not
// stop the turn, and the thread grows no empty extra turn while it streams on.
test("Send stays pressable while the session is mid-turn: a click sends as Send at once, and the turn streams on with no empty extra turn", async ({
  browser,
}) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${planner.session_id}/live`);
    const assistant = page.getByTestId("agent-message-assistant");
    await expect(assistant.first()).toContainText("Checking the notes.");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "Working", true));
    await expect(assistant.last()).toContainText("Working");

    await expect(page.getByRole("combobox", { name: "Delivery mode" })).toHaveValue("steer");
    const composer = page.getByTestId("agent-composer");
    await composer.locator("textarea").fill("stop after this file");
    const send = composer.getByRole("button", { exact: true, name: "Send" });
    await expect(send).toBeEnabled();
    await send.click();
    // No frame ends the turn before the send lands.
    await expect
      .poll(async () =>
        (await getSentMessages()).map((sent) => ({
          message: sent.message,
          payload: JSON.parse(String(sent.payload)),
        }))
      )
      .toContainEqual({
        message: "stop after this file",
        payload: expect.objectContaining({ delivery: expect.objectContaining({ mode: "steer" }) }),
      });
    await expect(
      page.getByTestId("agent-message-user").filter({ hasText: "stop after this file" })
    ).toBeVisible();

    // The turn streams on: the replay's reply and this one, and no empty turn after the
    // reader's message.
    await publishAgentStreamFrame(planner.session_id, assistantFrame(5, "Working on it", true));
    await expect(assistant.last()).toContainText("Working on it");
    await expect(assistant).toHaveCount(2);
    await publishAgentStreamFrame(
      planner.session_id,
      assistantFrame(6, "Working on it. Done.", false)
    );
    await expect(assistant.last()).toContainText("Working on it. Done.");
  } finally {
    await context.close();
  }
});

// Enter is the keyboard's Send, mid-turn too, and it sends in the mode the person picked.
test("Enter sends mid-turn in the delivery mode the person picked", async ({ browser }) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${planner.session_id}/live`);
    const assistant = page.getByTestId("agent-message-assistant");
    await expect(assistant.first()).toContainText("Checking the notes.");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "Working", true));
    await expect(assistant.last()).toContainText("Working");

    await page.getByRole("combobox", { name: "Delivery mode" }).selectOption("aside");
    const field = page.getByTestId("agent-composer").locator("textarea");
    await field.fill("note the flaky test for later");
    await field.press("Enter");
    // The turn is still streaming: no frame has ended it.
    await expect
      .poll(async () =>
        (await getSentMessages()).map((sent) => ({
          message: sent.message,
          payload: JSON.parse(String(sent.payload)),
        }))
      )
      .toContainEqual({
        message: "note the flaky test for later",
        payload: expect.objectContaining({ delivery: expect.objectContaining({ mode: "aside" }) }),
      });
  } finally {
    await context.close();
  }
});

// An empty box is the one thing that refuses Send mid-turn, and Send says why: on its title, as
// its accessible description, and on a line in the composer a reader with no pointer can see.
// Neither a click nor Enter sends. With text, Send is pressable and the line says how to send.
test("mid-turn, an empty box refuses Send, says why on screen, and sends nothing", async ({
  browser,
}) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${planner.session_id}/live`);
    const assistant = page.getByTestId("agent-message-assistant");
    await expect(assistant.first()).toContainText("Checking the notes.");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "Working", true));
    await expect(assistant.last()).toContainText("Working");

    const composer = page.getByTestId("agent-composer");
    const field = composer.locator("textarea");
    const send = composer.getByRole("button", { exact: true, name: "Send" });
    const reason = composer.getByText("Type a message first.", { exact: true });
    const hint = composer.getByText("Enter to send · Shift+Enter for a new line", { exact: true });
    await expect(send).toBeDisabled();
    await expect(send).toHaveAccessibleDescription("Type a message first.");
    await expect(send).toHaveAttribute("title", "Type a message first.");
    // `toBeVisible` passes for an `sr-only` line, a 1 px box, so its height is the check.
    await expect(reason).toBeVisible();
    expect((await reason.boundingBox())?.height ?? 0).toBeGreaterThan(8);
    const sent = (await getSentMessages()).length;
    await send.click({ force: true });
    await field.press("Enter");
    await page.waitForTimeout(500);
    expect(await getSentMessages()).toHaveLength(sent);

    await field.fill("status?");
    await expect(send).toBeEnabled();
    await expect(send).not.toHaveAttribute("aria-describedby");
    await expect(reason).toHaveCount(0);
    await expect(hint).toBeVisible();
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
  // A follow-up, as `dispatch message` sends it.
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

// A session that can run typed input lists its slash commands (LEGION-394); the composer completes
// them from a leading `/`, and a pick is the text a terminal would receive, sent through the same
// delivery as any other message.
test("the composer completes the session's slash commands and sends a picked one as typed", async ({
  browser,
}) => {
  // A session of its own: the relay's replay is process-global, and the planner's is the one
  // other rows read.
  const commander: FakeSession = {
    ...planner,
    session_id: "01a0e0c1-0000-7000-8000-0000000c0de5",
    title: "Commander",
  };
  await setLiveSessions([planner, commander]);
  await publishAgentStreamFrame(
    commander.session_id,
    {
      frames: [
        {
          commands: [
            {
              description: "Start a new session",
              name: "new",
              source: "builtin",
              terminalOnly: true,
            },
            { description: "Compact the session's context", name: "compact", source: "builtin" },
            { name: "skill:dispatch", source: "skill" },
          ],
          kind: "commands",
          seq: 1,
          v: 1,
        },
      ],
      session_id: commander.session_id,
      v: 1,
    },
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${commander.session_id}/live`);
    const input = page.getByTestId("agent-composer").locator("textarea");
    await expect(input).toHaveAttribute("placeholder", /\/ for commands/);

    await input.fill("/");
    const list = page.getByRole("listbox", { name: "Slash commands" });
    await expect(list.getByRole("option")).toHaveText([
      /^\/compact/,
      /^\/skill:dispatch/,
      /^\/new\s*terminal only/,
    ]);
    await input.pressSequentially("comp");
    await expect(list.getByRole("option")).toHaveText([/^\/compact/]);
    await input.press("Enter");
    await expect(input).toHaveValue("/compact ");
    await expect(list).toHaveCount(0);

    await input.press("Enter");
    await expect
      .poll(async () =>
        (await getSentMessages()).map((sent) => ({
          message: sent.message,
          payload: JSON.parse(String(sent.payload)),
        }))
      )
      .toContainEqual({
        message: "/compact",
        payload: expect.objectContaining({ delivery: expect.objectContaining({ mode: "steer" }) }),
      });
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

// A session's reported model, capped at AGENT_STREAM_LIMITS.modelChars before it ever leaves
// pi-envoy (packages/pi-envoy/src/agent-stream.ts), is still an unbroken identifier with no
// spaces: on a 390 px phone it does not fit the header pill or the turn's own label, and without
// an ellipsis it forced the whole page to scroll sideways rather than wrapping.
test("a capped model name ends in an ellipsis on a phone, in the header and on the turn, and never forces the page to scroll sideways", async ({
  browser,
}, testInfo) => {
  test.skip(
    !PHONE_PROJECTS.includes(testInfo.project.name),
    "the phone layout runs on the phone projects"
  );
  const longModel = capAgentStreamText(
    `anthropic/${"x".repeat(300)}`,
    AGENT_STREAM_LIMITS.modelChars
  );
  await publishAgentStreamFrame(
    planner.session_id,
    {
      frames: [assistantFrame(1, "Switching models.", false, longModel)],
      session_id: planner.session_id,
      v: 1,
    },
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/agents/${planner.session_id}/live`);
    const pill = page.getByTestId("agent-session-model");
    const label = page.getByTestId("agent-message-model");
    await expect(pill).toBeVisible();
    await expect(label).toBeVisible();
    // The pill is a flex container, so its `title` sits on the inner TruncatedText span
    // (AGENTS.md's rule for a flex host); the turn label is a plain block, so it carries
    // `title` itself, the same way GitHubLink.tsx's block host does.
    await expect(pill.locator("span")).toHaveAttribute("title", longModel);
    await expect(label).toHaveAttribute("title", longModel);
    const pillBox = await pill.boundingBox();
    const labelBox = await label.boundingBox();
    expect(pillBox?.width).toBeLessThanOrEqual(390);
    expect(labelBox?.width).toBeLessThanOrEqual(390);
    const widths = await page.evaluate(() => ({
      clientWidth: document.documentElement.clientWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }));
    expect(widths.scrollWidth).toBe(widths.clientWidth);
  } finally {
    await context.close();
  }
});

// A turn's reasoning is 12px italic text on 16px lines. Two reasoning links on consecutive lines:
// the lower one's padded box has to start at or below the upper one's own text, or a tap on the
// upper line can open the lower link.
test("on a phone the live view keeps a reasoning link's padded box off the line above", async ({
  browser,
}, testInfo) => {
  test.skip(
    !PHONE_PROJECTS.includes(testInfo.project.name),
    "the padding is the phone media query's: at desktop width it is off, so the row would pass whatever the CSS"
  );
  const reasoning: FakeSession = {
    ...planner,
    session_id: "01a0e0c1-0000-7000-8000-0000000304a1",
  };
  await setLiveSessions([planner, reasoning]);
  await publishAgentStreamFrame(
    reasoning.session_id,
    {
      frames: [
        {
          kind: "message",
          message: {
            at: 1_000,
            id: "a1000",
            parts: [
              {
                text: "[Upper reasoning](https://example.invalid/upper)\n[Lower reasoning](https://example.invalid/lower)",
                type: "reasoning",
              },
              { text: "Both links are in the reasoning.", type: "text" },
            ],
            role: "assistant",
            streaming: false,
          },
          seq: 1,
          v: 1,
        },
      ],
      session_id: reasoning.session_id,
      v: 1,
    },
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/agents/${reasoning.session_id}/live`);
    const body = page.getByTestId("agent-reasoning");
    // The first paint can be the plain-text fallback, which has no links: wait for the link.
    await expect(body.getByRole("link", { name: "Lower reasoning" })).toBeVisible();
    await expectConsecutiveLinksClear(body, "Upper reasoning", "Lower reasoning", {
      fontSize: 12,
      lineHeight: 16,
    });
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

// A reader who goes back into a live session's transcript is reading it, and a turn that arrives
// while they are back there is news to come back to, not a reason to take the page away from
// them. The control is how they come back, and what it counts is what they have not seen.
test("a reader scrolled back into a live session's transcript keeps their place, and Jump to latest carries what arrived", async ({
  browser,
}, testInfo) => {
  test.skip(
    !READING_PROJECTS.includes(testInfo.project.name),
    "reading back runs on the desktop and phone Chromium projects"
  );
  const reading: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a001" };
  await setLiveSessions([planner, reading]);
  await publishAgentStreamFrame(
    reading.session_id,
    rebuiltReplay(reading.session_id, 0, FOLD_FRAMES),
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    await page.goto(`/agents/${reading.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();

    // Back in the history with nothing to catch up on: the way back is offered, and it counts
    // nothing, because nothing has arrived since the reader left the live end.
    await scrollThreadTo(page, 1_200);
    await expect(jump).toHaveText("Jump to latest");

    // A turn arrives. The reader is left on the line they were reading, and the control carries
    // the one turn they have not seen.
    await publishAgentStreamFrame(reading.session_id, liveTurn(1, 100, "The first answer.", false));
    await expect(jump).toHaveText("Jump to latest · 1 new");
    expect((await liveViewLayout(page)).threadScrollTop).toBe(1_200);

    // A turn written in front of them is one turn however many snapshots carry it, and it moves
    // them no more than a finished one does.
    await publishAgentStreamFrame(reading.session_id, liveTurn(2, 101, "Look", true));
    await publishAgentStreamFrame(reading.session_id, liveTurn(2, 102, "Looking at the", true));
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(2, 103, "Looking at the notes.", false)
    );
    await expect(jump).toHaveText("Jump to latest · 2 new");
    expect((await liveViewLayout(page)).threadScrollTop).toBe(1_200);

    // What the count is of is the transcript, not the stream: a message somebody else sends the
    // session through Dispatch is one more thing the reader has not seen, and it moves them no
    // more than the session's own turn does.
    await createAgentMessage(reading.session_id, {
      body: "Someone else asks about the notes",
      delivery: "aside",
    });
    await expect(jump).toHaveText("Jump to latest · 3 new");
    expect((await liveViewLayout(page)).threadScrollTop).toBe(1_200);

    // Going back while a turn is being written holds too: the snapshots that follow grow that
    // turn under the reader rather than pulling them down to it.
    await scrollThreadTo(page, "bottom");
    await expect(jump).toHaveCount(0);
    await publishAgentStreamFrame(reading.session_id, liveTurn(3, 104, "Starting a turn.", true));
    await expect(page.getByText("Starting a turn.")).toBeVisible();
    await scrollThreadTo(page, 900);
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(3, 105, "Starting a turn that grows", true)
    );
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(3, 106, "Starting a turn that grows on.", true)
    );
    await expect(page.getByText("Starting a turn that grows on.")).toBeVisible();
    expect((await liveViewLayout(page)).threadScrollTop).toBe(900);

    // A fold the reader opens is something they asked for, not news: it grows under the turn it
    // belongs to, and that turn stays where it is on their screen. The reader is put at the top
    // of it first, because a fold opened in the middle of the screen pushes what is above it out
    // of view by its own height whoever scrolls - the browser's own anchoring - and that is the
    // text shifting under a reader the spec leaves alone.
    const tool = page.getByTestId("agent-tool-call");
    await tool.evaluate((element) => element.scrollIntoView({ block: "start" }));
    const before = await topmostTurn(page);
    await tool.getByRole("button").click();
    await expect(page.getByTestId("agent-tool-result")).toContainText("- the relay");
    expect(await topmostTurn(page)).toEqual(before);

    // The way back: one press puts the newest turn in front of them, takes the count with it,
    // and leaves the view following the session again.
    await jump.click();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(4, 107, "And the one after.", false)
    );
    await expect(page.getByText("And the one after.")).toBeVisible();
    await expectNewestTurnAboveComposer(page);

    // Scrolling to the live end by hand is the same answer as the control: nothing left to catch
    // up on, and the next turn arrives in view.
    await scrollThreadTo(page, 1_200);
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(5, 108, "While they read back.", false)
    );
    await expect(jump).toHaveText("Jump to latest · 1 new");
    await scrollThreadTo(page, "bottom");
    await expect(jump).toHaveCount(0);
    await publishAgentStreamFrame(
      reading.session_id,
      liveTurn(6, 109, "The turn after that.", false)
    );
    await expect(page.getByText("The turn after that.")).toBeVisible();
    await expectNewestTurnAboveComposer(page);
  } finally {
    await context.close();
  }
});

// Writing to the session is joining the end of its conversation: a reader who sends from back in
// the history arrives at the live end with their own message in view. A send the server refuses
// is no arrival at all - it leaves them on the line they were reading, where the refusal is.
test("sending a message from the live view while scrolled back takes the reader to the live end, and a refused send leaves them where they are", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const sender: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a002" };
  await setLiveSessions([planner, sender]);
  await publishAgentStreamFrame(sender.session_id, longReplay(sender.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sender.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await scrollThreadTo(page, 1_200);

    // Sending is arriving: the reader's own message is at the live end, in view, and there is
    // nothing left behind them to catch up on.
    const input = page.getByTestId("agent-composer").locator("textarea");
    await input.fill("try the other branch");
    await page.getByRole("button", { name: "Send" }).click();
    // The transcript's own turn, not the held line above it, which shows while the send is out.
    await expect(
      page.getByTestId("agent-thread").getByText("try the other branch", { exact: true })
    ).toBeVisible();
    await expect(page.getByTestId("agent-send-held")).toHaveCount(0);
    await expectNewestTurnAboveComposer(page);
    await expect(page.getByTestId("jump-to-latest")).toHaveCount(0);

    // The session never took this one: the reader keeps their line, and the refusal is on screen
    // for them to read where they are.
    await scrollThreadTo(page, 1_200);
    const refuse = await refusePosts(
      page,
      `**/api/v1/agents/${sender.session_id}/messages`,
      MESSAGE_REFUSED
    );
    await input.fill("and this one is refused");
    await page.getByRole("button", { name: "Send" }).click();
    refuse();
    await expect(page.getByTestId("agent-send-error")).toBeVisible();
    // A refused message is not lost: it is held on screen, with Retry, to send again.
    await expect(page.getByTestId("agent-send-held")).toContainText(
      "Your message: “and this one is refused”"
    );
    expect((await liveViewLayout(page)).threadScrollTop).toBe(1_200);
  } finally {
    await context.close();
  }
});

// Arriving at a session is arriving at its newest text, however the reader got there: a first
// open, a switch to another session, and a reload all land at the live end with nothing to catch
// up on - a count carried over from the session before would be about turns nobody missed.
test("opening a session, switching to another, and reloading each land the reader at the newest text with no count", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const first: FakeSession = {
    ...planner,
    session_id: "01a0e0c1-0000-7000-8000-00000000a003",
    title: "First",
  };
  const second: FakeSession = {
    ...planner,
    session_id: "01a0e0c1-0000-7000-8000-00000000a004",
    title: "Second",
  };
  await setLiveSessions([planner, first, second]);
  await publishAgentStreamFrame(
    first.session_id,
    rebuiltReplay(first.session_id, 0, [liveTurn(1, 41, "The first session's last word.", false)]),
    "replay"
  );
  await publishAgentStreamFrame(
    second.session_id,
    rebuiltReplay(second.session_id, 0, [
      liveTurn(2, 41, "The second session's last word.", false),
    ]),
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    await page.goto(`/agents/${first.session_id}/live`);
    await expect(page.getByText("The first session's last word.")).toBeInViewport();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);

    // The reader goes back into the first session's history and opens another: what they arrive
    // at is the second session's own newest text.
    await scrollThreadTo(page, 1_200);
    await navigateInApp(page, `/agents/${second.session_id}/live`);
    await expect(page.getByText("The second session's last word.")).toBeInViewport();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);

    // A reload is an arrival too.
    await scrollThreadTo(page, 1_200);
    await page.reload();
    await expect(page.getByText("The second session's last word.")).toBeInViewport();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The control is something the page puts over the transcript, not a row inside it: on a phone,
// where the composer is a finger's width below the thread's foot, it covers none of the composer,
// and it takes nothing from how far the transcript scrolls.
test("on a phone the live view's Jump to latest covers none of the composer and changes how far the transcript scrolls not at all", async ({
  browser,
}) => {
  const phone: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a005" };
  await setLiveSessions([planner, phone]);
  await publishAgentStreamFrame(phone.session_id, longReplay(phone.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/agents/${phone.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    for (const viewport of [
      { height: 844, width: 390 },
      { height: 800, width: 1280 },
    ]) {
      await page.setViewportSize(viewport);
      await scrollThreadTo(page, "bottom");
      await expect(jump).toHaveCount(0);
      const reach = (await liveViewLayout(page)).threadScrollHeight;

      await scrollThreadTo(page, 1_200);
      await expect(jump).toBeVisible();
      await expectUncovered(jump, page.getByTestId("agent-composer"));
      expect((await liveViewLayout(page)).threadScrollHeight).toBe(reach);
      await expectViewportBound(page);
    }
  } finally {
    await context.close();
  }
});

// Two ways the control must stay out of the reader's way: a transcript that fits on screen has no
// live end to be away from and needs none, and where there is one it lies clear of the command
// list the composer opens over it, so the row a reader presses is the row that is picked.
test("a transcript shorter than the viewport shows no Jump to latest, and the control never swallows the composer's slash commands", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const commander: FakeSession = {
    ...planner,
    session_id: "01a0e0c1-0000-7000-8000-00000000a006",
    title: "Commander",
  };
  await setLiveSessions([planner, commander]);
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  await publishAgentStreamFrame(
    commander.session_id,
    rebuiltReplay(commander.session_id, 0, [
      {
        commands: [
          { description: "Compact the session's context", name: "compact", source: "builtin" },
        ],
        kind: "commands",
        seq: 41,
        v: 1,
      },
    ]),
    "replay"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    // A conversation that fits on screen is all of it: there is no live end to be away from.
    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-message-assistant")).toContainText("Checking the notes.");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "There are two.", false));
    await expect(page.getByText("There are two.")).toBeVisible();
    await expect(jump).toHaveCount(0);

    // The command list opens over the control, and the press lands on the row under the finger:
    // Playwright refuses the click if anything covers it.
    await page.goto(`/agents/${commander.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await scrollThreadTo(page, 1_200);
    await expect(jump).toBeVisible();
    const input = page.getByTestId("agent-composer").locator("textarea");
    await input.fill("/");
    const list = page.getByRole("listbox", { name: "Slash commands" });
    await list.getByRole("option", { name: /^\/compact/ }).click();
    await expect(input).toHaveValue("/compact ");
  } finally {
    await context.close();
  }
});

// The live stream carries no cursor, so a reconnect rebuilds the whole transcript from the
// session's replay, and the session answers newest-first inside a budget: the rebuild routinely
// drops the oldest turns, the ones a reader reading history is sitting on. What the reader keeps
// is the turn they were reading, where it was on their screen - holding the scroll offset would
// hold a number that means another turn entirely once the turns above it are gone.
test("a rebuilt transcript keeps a scrolled-back reader on the turn they were reading", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const kept: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a007" };
  await setLiveSessions([planner, kept]);
  await publishAgentStreamFrame(kept.session_id, longReplay(kept.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    await page.goto(`/agents/${kept.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();

    // The reader goes back to Turn 12 and reads from there. The control appearing is the hook
    // having taken the scroll: from here it is their place that is remembered, not the offset.
    await page
      .getByText("Turn 12:")
      .evaluate((element) => element.scrollIntoView({ block: "start" }));
    await expect(jump).toHaveText("Jump to latest");
    const before = await topmostTurn(page);
    const reach = (await liveViewLayout(page)).threadScrollHeight;

    // The connection drops and the reconnect is answered with a shorter history: the session's
    // budget no longer reaches Turn 0 through Turn 7, and two turns it wrote while this viewer
    // was away are behind the forty.
    await rebuildThroughResubscribe(
      kept.session_id,
      rebuiltReplay(kept.session_id, 8, [
        liveTurn(1, 41, "Written while they were away.", false),
        liveTurn(2, 42, "And the one after it.", false),
      ])
    );
    await expect(page.getByText("Written while they were away.")).toBeVisible();

    // It really is a shorter transcript: the eight oldest turns are not in the page at all, so
    // the reader's old offset now lands eight turns further down the conversation.
    await expect(page.getByText("Turn 7:")).toHaveCount(0);
    expect((await liveViewLayout(page)).threadScrollHeight).toBeLessThan(reach);

    // The reader is left on the turn they were reading, at the pixel it sat at, still scrolled
    // back. The count is what the rebuilt list says: two turns it carries that they have not
    // seen, and nothing for the dropped ones, which they had seen and which are simply absent.
    const after = await topmostTurn(page);
    expect(after.id).toBe(before.id);
    expect(Math.abs(after.top - before.top)).toBeLessThanOrEqual(1);
    await expect(jump).toHaveText("Jump to latest · 2 new");
  } finally {
    await context.close();
  }
});

// The same rebuild, with the reader's own turn gone from it: there is nowhere to put them back,
// and the offset they were at belongs to text they never asked for. They land at the newest text
// instead, following again, with nothing left to catch up on.
test("a rebuilt transcript that dropped the reader's turn lands them at the newest text", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const dropped: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a008" };
  await setLiveSessions([planner, dropped]);
  await publishAgentStreamFrame(dropped.session_id, longReplay(dropped.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${dropped.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await page
      .getByText("Turn 12:")
      .evaluate((element) => element.scrollIntoView({ block: "start" }));
    await expect(page.getByTestId("jump-to-latest")).toHaveText("Jump to latest");

    // This reconnect is answered with an entirely later window - Turn 20 on - which still
    // scrolls, so landing at its end is a place the view had to go to, not the only place there
    // was.
    await rebuildThroughResubscribe(dropped.session_id, rebuiltReplay(dropped.session_id, 20));
    await expect(page.getByText("Turn 12:")).toHaveCount(0);
    await expectNewestTurnAboveComposer(page);
    await expect(page.getByTestId("jump-to-latest")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The other half of a rebuild: the reader was at the live end when it landed. The replay drops
// the oldest turns and carries two written while the connection was down, so a view that held
// its `scrollTop` would sit short of the end with the newest text below the fold. Then the half
// that matters as much: what the rebuild did is over, and the reader's next scroll back is
// theirs alone - no compensation, the hook's or the browser's, may be read into it.
test("a rebuilt transcript leaves a reader who was at the live end at the live end, and still sees their next scroll back", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the desktop layout runs on the desktop project");
  const held: FakeSession = { ...planner, session_id: "01a0e0c1-0000-7000-8000-00000000a009" };
  await setLiveSessions([planner, held]);
  await publishAgentStreamFrame(held.session_id, longReplay(held.session_id), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const jump = page.getByTestId("jump-to-latest");
  try {
    await page.goto(`/agents/${held.session_id}/live`);
    await expect(page.getByText("Turn 39:")).toBeVisible();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);

    await rebuildThroughResubscribe(
      held.session_id,
      rebuiltReplay(held.session_id, 8, [
        liveTurn(1, 41, "Written while they were away.", false),
        liveTurn(2, 42, "And the one after it.", false),
      ])
    );
    await expect(page.getByText("And the one after it.")).toBeVisible();

    // Eight turns shorter at the top and two longer at the foot: the offset the reader had is a
    // place well short of the end of what came back.
    await expect(page.getByText("Turn 7:")).toHaveCount(0);
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);

    // Still following: the next turn the session writes arrives in view, with nothing to catch
    // up on.
    await publishAgentStreamFrame(held.session_id, liveTurn(3, 43, "And then this one.", false));
    await expect(page.getByText("And then this one.")).toBeVisible();
    await expectNewestTurnAboveComposer(page);
    await expect(jump).toHaveCount(0);

    // The reader goes back through the history after all that, and it is read as what it is:
    // the way back is offered, and they are at the position they scrolled to.
    await scrollThreadTo(page, 1_000);
    await expect(jump).toHaveText("Jump to latest");
    expect((await liveViewLayout(page)).threadScrollTop).toBe(1_000);
  } finally {
    await context.close();
  }
});
