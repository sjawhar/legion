import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  type TestInfo,
  test,
} from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions, setSessionSendStatus } from "./agents";
import {
  baseUrl,
  e2eAgentToken,
  listAgentMessages,
  mintAgentToken,
  publishAgentStreamFrame,
  replyToMessageDelivery,
  setAgentStreamResponder,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The live view's header shows the model and thinking level an Oh My Pi session runs now, as its
 * `settings` frame reports them, and switches either from a picker (INFRA-195). The harness runs
 * Dispatch with NATS disabled, so the frames come from the test hook rather than from a session,
 * and the session's answer to a switch is a reply posted through Dispatch, as pi-envoy posts the
 * host's printed answer. A switch is the person's own Send of `/model <id>` or `/effort <level>`,
 * so what it sends is read back from the fake Envoy and from Dispatch's stored thread.
 *
 * The frames are spelt here as the wire carries them, never imported from the SPA or the
 * contracts, so the file runs unchanged against the commit before the change and each row fails
 * there on its own assertion.
 */

/** The phone layout runs on Chromium (the `iphone` project) and on WebKit (`webkit-iphone`). */
const PHONE_PROJECTS = ["iphone", "webkit-iphone"];

const MODELS = ["anthropic/model-a", "anthropic/model-b", "openai/model-c"];
const LEVELS = ["auto", "low", "medium", "high"];

const NO_ANSWER =
  "No answer came from the session within 30 seconds. The header shows the model and thinking level the session last reported.";
const RESTART_NOTICE =
  "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.";
const UNREPORTED_NOTICE =
  "This session's Oh My Pi does not tell Dispatch its model and thinking level.";
const TERMINAL_ONLY_NOTICE =
  "This session switches its model and thinking level only at its own terminal.";
const PENDING_PLACEHOLDER = "Waiting for the session to answer the switch…";

/** A live session of the row's own: the relay's replay and responder are process-global, so a
 *  session another row seeded would carry that row's frames into this one. */
function liveSession(row: number): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/settings",
    machine_id: "settings-host",
    roles: [],
    session_id: `01a0e0c1-0195-7000-8000-${String(row).padStart(12, "0")}`,
    title: `Settings ${row}`,
  };
}

/** A reported `settings` frame: `thinking` is the level and the levels `/effort` offers, or null
 *  for a model with no adjustable thinking. */
function settingsFrame(
  seq: number,
  model: string,
  thinking: { level: string | null; levels: string[] } | null,
  models: string[] = MODELS
): object {
  return { kind: "settings", model, models, reported: true, seq, thinking, v: 1 };
}

/** The settings of a session at `model` on thinking `level`, offering every level. */
function reported(seq: number, model: string, level: string | null): object {
  return settingsFrame(seq, model, { level, levels: LEVELS });
}

/** The `commands` frame of a session that runs typed input, listing the built-ins a pick runs. */
function commandsFrame(seq: number): object {
  return {
    commands: [
      { description: "Switch the session's model", name: "model", source: "builtin" },
      { description: "Set the thinking level", name: "effort", source: "builtin" },
      { description: "Compact the session's context", name: "compact", source: "builtin" },
    ],
    kind: "commands",
    seq,
    v: 1,
  };
}

/** One settled assistant turn on `model`. */
function assistantTurn(seq: number, model: string): object {
  return {
    kind: "message",
    message: {
      at: 2_000,
      id: "a2000",
      model,
      parts: [{ text: "Done on this model.", type: "text" }],
      role: "assistant",
      streaming: false,
    },
    seq,
    v: 1,
  };
}

function replayOf(sessionID: string, frames: object[]): object {
  return { frames, session_id: sessionID, v: 1 };
}

/** What a session at `model-a` on `auto` that runs `/model` and `/effort` replays before any
 *  turn: its settings and its commands, and nothing else. */
function freshReplay(sessionID: string): object {
  return replayOf(sessionID, [reported(1, "anthropic/model-a", "auto"), commandsFrame(2)]);
}

/** A browser context signed in as `login`, and a page at the row's width: a 390 px phone, or a
 *  1280 × 800 desktop. */
async function viewer(
  browser: Browser,
  testInfo: TestInfo,
  login: string
): Promise<{ context: BrowserContext; page: Page }> {
  const context = await asUser(browser, login);
  const page = await context.newPage();
  await page.setViewportSize(
    PHONE_PROJECTS.includes(testInfo.project.name)
      ? { height: 844, width: 390 }
      : { height: 800, width: 1280 }
  );
  return { context, page };
}

function livePath(sessionID: string): string {
  return `/agents/${sessionID}/live`;
}

/** On a phone, the header's settings never push the page sideways: the document is no wider than
 *  the 390 px screen, and each rendered piece of the header's settings lies inside it. */
async function expectNoSidewaysScroll(page: Page, testInfo: TestInfo): Promise<void> {
  if (!PHONE_PROJECTS.includes(testInfo.project.name)) return;
  const widths = await page.evaluate(() => ({
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(widths.clientWidth).toBe(390);
  expect(widths.scrollWidth).toBe(widths.clientWidth);
  for (const selector of [
    '[data-testid="agent-model-picker"]',
    '[data-testid="agent-thinking-picker"]',
    'span[data-testid="agent-session-model"]',
    'span[data-testid="agent-session-thinking"]',
    '[data-testid="agent-switch-status"]',
    '[data-testid="agent-settings-notice"]',
  ]) {
    const element = page.locator(selector);
    if ((await element.count()) === 0) continue;
    const box = await element.boundingBox();
    expect(box, selector).not.toBeNull();
    expect(box?.x ?? -1, selector).toBeGreaterThanOrEqual(0);
    expect((box?.x ?? 0) + (box?.width ?? 0), selector).toBeLessThanOrEqual(widths.clientWidth);
  }
}

/** The header's pickers and the shown values, as a reader finds them. */
function header(page: Page) {
  return {
    model: page.getByRole("combobox", { name: "Model" }),
    notice: page.getByTestId("agent-settings-notice"),
    shownModel: page.getByTestId("agent-session-model"),
    shownThinking: page.getByTestId("agent-session-thinking"),
    status: page.getByRole("status").and(page.getByTestId("agent-switch-status")),
    thinking: page.getByRole("combobox", { name: "Thinking level" }),
  };
}

/** The header shows `model` and `level` in its pickers, each a rendered box, with the selected
 *  option carrying the shown value. */
async function expectPicked(page: Page, model: string, level: string): Promise<void> {
  const { model: modelPicker, shownModel, shownThinking, thinking } = header(page);
  await expect(modelPicker).toBeVisible();
  await expect(modelPicker).toHaveValue(model);
  await expect(shownModel).toHaveText(model);
  await expect(thinking).toBeVisible();
  await expect(thinking).toHaveValue(level);
  await expect(shownThinking).toHaveText(level);
}

/** The stored id of the switch whose body is `body`, once Dispatch has stored it, with its author. */
async function storedSwitch(
  sessionID: string,
  body: string
): Promise<{ id: string; author: { kind: string; id: string } }> {
  let found: { id: string; author: { kind: string; id: string } } | undefined;
  await expect
    .poll(async () => {
      const read = (await listAgentMessages(sessionID)).find((row) => row.message.body === body);
      found = read === undefined ? undefined : read.message;
      return found !== undefined;
    })
    .toBe(true);
  if (found === undefined) throw new Error(`no stored ${body}`);
  return { author: found.author, id: found.id };
}

/** What the fake Envoy was asked to send, with each delivery frame decoded. */
async function sentSwitches(): Promise<
  {
    message: unknown;
    target: unknown;
    mode: unknown;
    actor: unknown;
  }[]
> {
  return (await getSentMessages()).map((sent) => {
    const payload = JSON.parse(String(sent.payload)) as {
      delivery: { mode: string };
      event: { actor: { kind: string; id: string } };
    };
    return {
      actor: payload.event.actor,
      message: sent.message,
      mode: payload.delivery.mode,
      target: sent.target_session,
    };
  });
}

/** The session answers a switch the way pi-envoy posts the host's printed answer. */
function answer(sessionID: string, messageID: string, body: string): Promise<unknown> {
  return replyToMessageDelivery(
    messageID,
    { attempt: 1, body },
    { id: sessionID, kind: "session" }
  );
}

test.beforeEach(async () => {
  await resetDatabase();
});

// A1: a session on this change's pi-envoy reports its settings in its replay, before any turn has
// run, and every later report replaces them, with no turn run.
test("the header shows the session's reported model and thinking level before any turn, and follows its next settings frame", async ({
  browser,
}, testInfo) => {
  const session = liveSession(1);
  await setLiveSessions([session]);
  await publishAgentStreamFrame(session.session_id, freshReplay(session.session_id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(session.session_id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, notice, thinking } = header(page);
    // Every model the session lists, grouped by provider, and every level its `/effort` offers.
    await expect(model.locator("optgroup")).toHaveCount(2);
    await expect(model.locator("optgroup").first()).toHaveAttribute("label", "anthropic");
    await expect(model.locator("optgroup").last()).toHaveAttribute("label", "openai");
    await expect(model.locator("option")).toHaveText(MODELS);
    await expect(thinking.locator("option")).toHaveText(LEVELS);
    // No turn ran: the values came from the session's report, not from a turn's label.
    await expect(page.getByTestId("agent-thread-empty")).toBeVisible();
    await expect(page.getByTestId("agent-message-assistant")).toHaveCount(0);
    await expect(notice).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);

    // The model and thinking level change at the session's terminal: the next frame carries both.
    await publishAgentStreamFrame(session.session_id, reported(3, "anthropic/model-b", "high"));
    await expectPicked(page, "anthropic/model-b", "high");
    await expect(page.getByTestId("agent-message-assistant")).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A1: a session on an older pi-envoy replays turns and no settings frame. Its header is the last
// turn's model, as before, and it says what a restart would bring; a session not answering the
// live view at all says nothing of the kind, since the empty transcript already says why.
test("an older session's header shows its last turn's model with no picker and asks for a restart, and a session not answering the live view shows no such notice", async ({
  browser,
}, testInfo) => {
  const session = liveSession(2);
  await setLiveSessions([session]);
  await publishAgentStreamFrame(
    session.session_id,
    replayOf(session.session_id, [assistantTurn(1, "anthropic/old-model")]),
    "replay"
  );
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(session.session_id));
    const { model, notice, shownModel, thinking } = header(page);
    await expect(notice).toBeVisible();
    await expect(notice).toHaveText(RESTART_NOTICE);
    await expect(shownModel).toBeVisible();
    await expect(shownModel).toHaveText("anthropic/old-model");
    await expect(model).toHaveCount(0);
    await expect(thinking).toHaveCount(0);
    await expect(page.getByTestId("agent-session-thinking")).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);

    await setAgentStreamResponder(session.session_id, false);
    await page.reload();
    await expect(page.getByTestId("agent-thread-empty")).toHaveText(/not answering the live view/);
    await expect(notice).toHaveCount(0);
    await expect(model).toHaveCount(0);
    await expect(thinking).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A1, decision 7's other rows: what the header shows when the session reports less than a model
// and a level it can switch from Dispatch, and why.
test("the header says why a session shows less: an Oh My Pi that does not report, no switch from Dispatch, no adjustable thinking, and a model-default level", async ({
  browser,
}, testInfo) => {
  const session = liveSession(3);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(
    id,
    replayOf(id, [
      assistantTurn(1, "anthropic/old-model"),
      { kind: "settings", reported: false, seq: 2, v: 1 },
    ]),
    "replay"
  );
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    const { model, notice, shownModel, shownThinking, thinking } = header(page);

    // A current pi-envoy on an Oh My Pi that cannot tell it: the last turn's model, no picker.
    await page.goto(livePath(id));
    await expect(notice).toBeVisible();
    await expect(notice).toHaveText(UNREPORTED_NOTICE);
    await expect(shownModel).toBeVisible();
    await expect(shownModel).toHaveText("anthropic/old-model");
    await expect(model).toHaveCount(0);
    await expect(thinking).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);

    // Reported settings from a session whose host cannot run typed input (no `commands` frame):
    // both values, as text, and no picker.
    await publishAgentStreamFrame(
      id,
      replayOf(id, [reported(1, "anthropic/model-a", "auto")]),
      "replay"
    );
    await page.reload();
    await expect(notice).toBeVisible();
    await expect(notice).toHaveText(TERMINAL_ONLY_NOTICE);
    await expect(shownModel).toBeVisible();
    await expect(shownModel).toHaveText("anthropic/model-a");
    await expect(shownThinking).toBeVisible();
    await expect(shownThinking).toHaveText("auto");
    await expect(model).toHaveCount(0);
    await expect(thinking).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);

    // A model with no adjustable thinking: the model picker, and words in place of a level.
    await publishAgentStreamFrame(
      id,
      replayOf(id, [settingsFrame(1, "anthropic/model-a", null), commandsFrame(2)]),
      "replay"
    );
    await page.reload();
    await expect(model).toBeVisible();
    await expect(model).toHaveValue("anthropic/model-a");
    await expect(shownThinking).toBeVisible();
    await expect(shownThinking).toHaveText("no adjustable thinking");
    await expect(thinking).toHaveCount(0);
    await expect(notice).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);

    // No level configured: the thinking picker shows the model's default, which is no pick.
    await publishAgentStreamFrame(
      id,
      replayOf(id, [reported(1, "anthropic/model-a", null), commandsFrame(2)]),
      "replay"
    );
    await page.reload();
    await expect(thinking).toBeVisible();
    await expect(thinking).toHaveValue("");
    await expect(shownThinking).toHaveText("model default");
    await expect(shownThinking).toBeDisabled();
    await expect(model).toHaveValue("anthropic/model-a");
    await expect(notice).toHaveCount(0);
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A2: a pick is the person's own Send of `/model <id>`, which Dispatch stores under their name and
// the thread shows as theirs; Send waits for the session's answer, and the header moves only when
// the session reports the switch. A pick is a steer whatever the composer's mode, since a BTW is a
// side question the session answers rather than input it runs.
test("a model picked in the header is the viewer's own Send of /model, holds Send until the session answers, and goes as a steer whatever Send as says", async ({
  browser,
}, testInfo) => {
  const session = liveSession(4);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status, thinking } = header(page);
    const input = page.getByTestId("agent-composer").locator("textarea");
    const send = page.getByRole("button", { name: "Send" });
    await input.fill("now review the diff");
    await expect(send).toBeEnabled();

    await model.selectOption("anthropic/model-b");
    await expect(status).toBeVisible();
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    await expect(send).toBeDisabled();
    await expect(input).toHaveAttribute("placeholder", PENDING_PLACEHOLDER);
    await expect(model).toBeDisabled();
    await expect(thinking).toBeDisabled();
    // Nothing the session has not reported: the picker stays on the model it runs.
    await expect(model).toHaveValue("anthropic/model-a");

    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    expect(await sentSwitches()).toEqual([
      {
        actor: { id: "alice", kind: "user" },
        message: "/model anthropic/model-b",
        mode: "steer",
        target: id,
      },
    ]);
    const stored = await storedSwitch(id, "/model anthropic/model-b");
    expect(stored.author).toEqual(expect.objectContaining({ id: "alice", kind: "user" }));
    const own = page
      .getByTestId("agent-message-user")
      .filter({ hasText: "/model anthropic/model-b" });
    await expect(own).toBeVisible();

    await answer(id, stored.id, "Model set to anthropic/model-b.");
    await publishAgentStreamFrame(id, reported(3, "anthropic/model-b", "auto"));
    await expect(status).toHaveText("The session answered: Model set to anthropic/model-b.");
    await expectPicked(page, "anthropic/model-b", "auto");
    await expect(
      page
        .getByTestId("agent-dispatch-reply")
        .filter({ hasText: "Model set to anthropic/model-b." })
    ).toBeVisible();
    await expect(send).toBeEnabled();
    await expect(model).toBeEnabled();
    await expectNoSidewaysScroll(page, testInfo);

    // The composer set to BTW: the pick still goes as a steer, and the composer keeps its mode.
    const mode = page.getByRole("combobox", { name: "Delivery mode" });
    await mode.selectOption("btw");
    await model.selectOption("anthropic/model-a");
    await expect(status).toHaveText("Switching model to anthropic/model-a…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(2);
    expect((await sentSwitches())[1]).toEqual({
      actor: { id: "alice", kind: "user" },
      message: "/model anthropic/model-a",
      mode: "steer",
      target: id,
    });
    await expect(mode).toHaveValue("btw");
  } finally {
    await context.close();
  }
});

// A2: a thinking level picked from the levels the session's `/effort` offers.
test("a thinking level picked in the header sends /effort, and the session's answer and next frame update the header", async ({
  browser,
}, testInfo) => {
  const session = liveSession(5);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { status, thinking } = header(page);

    await thinking.selectOption("high");
    await expect(status).toBeVisible();
    await expect(status).toHaveText("Switching thinking level to high…");
    await expect(thinking).toHaveValue("auto");
    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    expect(await sentSwitches()).toEqual([
      { actor: { id: "alice", kind: "user" }, message: "/effort high", mode: "steer", target: id },
    ]);
    const stored = await storedSwitch(id, "/effort high");
    await expect(
      page.getByTestId("agent-message-user").filter({ hasText: "/effort high" })
    ).toBeVisible();

    await answer(id, stored.id, "Thinking set to high.");
    await publishAgentStreamFrame(id, reported(3, "anthropic/model-a", "high"));
    await expect(status).toHaveText("The session answered: Thinking set to high.");
    await expectPicked(page, "anthropic/model-a", "high");
    await expect(thinking).toBeEnabled();
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A2: a switch the session refuses (here, a model whose key it cannot read) changes nothing, and
// the page shows the session's own reason, quoted.
test("a switch the session refuses shows its reason and leaves the header on the values it last reported", async ({
  browser,
}, testInfo) => {
  const session = liveSession(6);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status } = header(page);

    await model.selectOption("openai/model-c");
    await expect(status).toHaveText("Switching model to openai/model-c…");
    const stored = await storedSwitch(id, "/model openai/model-c");
    await answer(id, stored.id, "Failed to set model: No API key for openai/model-c");
    await expect(status).toBeVisible();
    await expect(status).toHaveText(
      "The session answered: Failed to set model: No API key for openai/model-c"
    );
    await expectPicked(page, "anthropic/model-a", "auto");
    await expect(model).toBeEnabled();
    await expect(page.getByRole("button", { name: "Send" })).toBeVisible();
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A2 and R2: a switch is stored under the person who made it. Bob switches from his own browser;
// Alice, reading the same session, sees his name on the switch in the thread.
test("a switch from the header is stored under the person who made it and names them to everyone else", async ({
  browser,
}, testInfo) => {
  const session = liveSession(7);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const alice = await viewer(browser, testInfo, "alice");
  const bob = await viewer(browser, testInfo, "bob");
  try {
    await alice.page.goto(livePath(id));
    await expectPicked(alice.page, "anthropic/model-a", "auto");
    await bob.page.goto(livePath(id));
    await expectPicked(bob.page, "anthropic/model-a", "auto");

    await header(bob.page).model.selectOption("anthropic/model-b");
    await expect(header(bob.page).status).toHaveText("Switching model to anthropic/model-b…");
    const stored = await storedSwitch(id, "/model anthropic/model-b");
    expect(stored.author).toEqual(expect.objectContaining({ id: "bob", kind: "user" }));
    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    expect((await sentSwitches())[0]).toEqual(
      expect.objectContaining({ actor: { id: "bob", kind: "user" }, mode: "steer" })
    );
    // Bob's own switch reads as his; to Alice it names him.
    await expect(
      bob.page.getByTestId("agent-message-user").filter({ hasText: "/model anthropic/model-b" })
    ).toBeVisible();
    const seenByAlice = alice.page
      .getByTestId("agent-message-other")
      .filter({ hasText: "/model anthropic/model-b" });
    await expect(seenByAlice).toBeVisible();
    await expect(seenByAlice).toContainText("bob");
    await expectNoSidewaysScroll(alice.page, testInfo);
    await expectNoSidewaysScroll(bob.page, testInfo);
  } finally {
    await alice.context.close();
    await bob.context.close();
  }
});

// A3, a regression guard: the route a switch goes through refuses an agent's bearer, the shared
// agent token and a person's own agent token alike, so no agent can switch a session from
// Dispatch. The route refused a bearer before this change, so this row passes at the commit
// before it too; it is here so that a later change to the route cannot open the switch.
test("an agent's bearer cannot switch a session: the messages route answers 403 HUMAN_ONLY and nothing is sent", async ({
  browser,
}, testInfo) => {
  const session = liveSession(8);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const personal = await mintAgentToken("t", "alice");
  expect(await getSentMessages()).toEqual([]);
  for (const [bearer, actor] of [
    [e2eAgentToken, { id, kind: "session" }],
    [personal, undefined],
  ] as const) {
    const response = await fetch(
      new URL(`/api/v1/agents/${encodeURIComponent(id)}/messages`, baseUrl),
      {
        body: JSON.stringify({
          body: "/model anthropic/model-b",
          delivery: "steer",
          ...(actor === undefined ? {} : { actor }),
        }),
        headers: { Authorization: `Bearer ${bearer}`, "Content-Type": "application/json" },
        method: "POST",
      }
    );
    expect(response.status).toBe(403);
    expect(await response.json()).toEqual(expect.objectContaining({ code: "HUMAN_ONLY" }));
  }
  expect(await getSentMessages()).toEqual([]);
  expect(await listAgentMessages(id)).toEqual([]);

  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expect(page.getByTestId("agent-thread-empty")).toBeVisible();
    await expect(page.getByText("/model anthropic/model-b")).toHaveCount(0);
    expect(await getSentMessages()).toEqual([]);
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// A4: a session that stopped running never answers. Thirty seconds after Dispatch took the
// switch the page says no answer came, keeps the values the session last reported, and gives the
// pickers back; an answer that still comes replaces the line. A switch whose send already failed
// (no live session took it) says so as soon as Dispatch answers with the failed attempt, without
// waiting for that deadline.
test("a switch the session never answers says so after 30 seconds and keeps the header, a late answer replaces that, and a send that failed says so without waiting for the deadline", async ({
  browser,
}, testInfo) => {
  // Room for the failed send's 12 s listener take-over wait at the end of the row.
  test.setTimeout(60_000);
  const session = liveSession(9);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.clock.install();
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status, thinking } = header(page);
    const send = page.getByRole("button", { name: "Send" });
    await page.getByTestId("agent-composer").locator("textarea").fill("carry on");

    await model.selectOption("anthropic/model-b");
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    const stored = await storedSwitch(id, "/model anthropic/model-b");
    // The page refetches its thread once Dispatch has answered the send, which is when the
    // deadline starts.
    await expect(
      page.getByTestId("agent-message-user").filter({ hasText: "/model anthropic/model-b" })
    ).toBeVisible();
    await page.clock.fastForward(25_000);
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    await expect(model).toBeDisabled();
    await page.clock.fastForward(5_000);
    await expect(status).toBeVisible();
    await expect(status).toHaveText(NO_ANSWER);
    await expectPicked(page, "anthropic/model-a", "auto");
    await expect(model).toBeEnabled();
    await expect(thinking).toBeEnabled();
    await expect(send).toBeEnabled();
    await expectNoSidewaysScroll(page, testInfo);

    await answer(id, stored.id, "Model set to anthropic/model-b.");
    await expect(status).toHaveText("The session answered: Model set to anthropic/model-b.");

    // The send itself fails: Dispatch answers with a failed attempt, and the page says so with no
    // wait for the deadline. Dispatch answers only once the listener take-over window has passed
    // (listenerTakeoverWindow, 12 s, in packages/envoy/internal/dispatch/api/envoy_resolve.go),
    // in real time the page's installed clock does not hold back, so this wait gets more than the
    // default 15 s expect timeout.
    await setSessionSendStatus(id, 404);
    await model.selectOption("openai/model-c");
    await expect(status).toHaveText(/^No answer came from the session: \S/, { timeout: 30_000 });
    await expect(status).not.toHaveText(NO_ANSWER);
    await expect(model).toBeEnabled();
    await expect(send).toBeEnabled();
    await expectPicked(page, "anthropic/model-a", "auto");
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});

// Decision 8: the header's values come from the session's reports alone and the line from the
// latest switch's answer alone, so neither depends on which arrives first, and an earlier switch's
// late answer never speaks for a newer switch.
test("a switch's answer and the session's frame land in either order, and an earlier switch's late answer never changes a newer one's status", async ({
  browser,
}, testInfo) => {
  const session = liveSession(10);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.clock.install();
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status, thinking } = header(page);

    // The answer before the frame: the line answers, and the header waits for the report.
    await model.selectOption("anthropic/model-b");
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    const toModelB = await storedSwitch(id, "/model anthropic/model-b");
    await answer(id, toModelB.id, "Model set to anthropic/model-b.");
    await expect(status).toHaveText("The session answered: Model set to anthropic/model-b.");
    await expect(model).toBeEnabled();
    await expectPicked(page, "anthropic/model-a", "auto");
    await publishAgentStreamFrame(id, reported(3, "anthropic/model-b", "auto"));
    await expectPicked(page, "anthropic/model-b", "auto");
    await expect(status).toHaveText("The session answered: Model set to anthropic/model-b.");

    // The frame before the answer: the header moves, and the switch still waits for its answer.
    await thinking.selectOption("high");
    await expect(status).toHaveText("Switching thinking level to high…");
    const toHigh = await storedSwitch(id, "/effort high");
    await publishAgentStreamFrame(id, reported(4, "anthropic/model-b", "high"));
    await expectPicked(page, "anthropic/model-b", "high");
    await expect(status).toHaveText("Switching thinking level to high…");
    await expect(thinking).toBeDisabled();
    await answer(id, toHigh.id, "Thinking set to high.");
    await expect(status).toHaveText("The session answered: Thinking set to high.");
    await expect(thinking).toBeEnabled();

    // An earlier switch goes unanswered past its deadline, a newer one starts, and only then does
    // the earlier one's answer come: it shows in the thread and leaves the newer line alone.
    await model.selectOption("anthropic/model-a");
    await expect(status).toHaveText("Switching model to anthropic/model-a…");
    const toModelA = await storedSwitch(id, "/model anthropic/model-a");
    await expect(
      page.getByTestId("agent-message-user").filter({ hasText: "/model anthropic/model-a" })
    ).toBeVisible();
    await page.clock.fastForward(30_000);
    await expect(status).toHaveText(NO_ANSWER);
    await thinking.selectOption("low");
    await expect(status).toHaveText("Switching thinking level to low…");
    const toLow = await storedSwitch(id, "/effort low");
    await answer(id, toModelA.id, "Model set to anthropic/model-a.");
    await expect(
      page
        .getByTestId("agent-dispatch-reply")
        .filter({ hasText: "Model set to anthropic/model-a." })
    ).toBeVisible();
    await expect(status).toHaveText("Switching thinking level to low…");
    await expect(thinking).toBeDisabled();

    await publishAgentStreamFrame(id, reported(5, "anthropic/model-a", "low"));
    await answer(id, toLow.id, "Thinking set to low.");
    await expect(status).toHaveText("The session answered: Thinking set to low.");
    await expectPicked(page, "anthropic/model-a", "low");
    await expect(model).toBeEnabled();
    await expectNoSidewaysScroll(page, testInfo);
  } finally {
    await context.close();
  }
});
