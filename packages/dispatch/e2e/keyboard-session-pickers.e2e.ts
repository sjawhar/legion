import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  type TestInfo,
  test,
} from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { listAgentMessages, publishAgentStreamFrame, replyToMessageDelivery } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The live view's header pickers switch an Oh My Pi session's model and thinking level (INFRA-195),
 * and each change of a picker is a switch the session runs, so they keep the Agents issue picker's
 * keyboard-step rule (`markKeyStep` in `AgentMessageComposer.tsx`, `keyboard-agents-picker.e2e.ts`):
 * a closed select's own keys (the arrows, `Home`/`End`, the page keys, type-ahead) only move its
 * choice and send nothing, `Enter` switches to the choice reached, and leaving the picker without
 * `Enter` puts it back on the value the session reported. While a switch is pending the pickers
 * are held (`aria-disabled`, so focus stays): their keys still step, `Enter` sends nothing, and a
 * step taken while held survives the switch's end, so `Enter` afterwards switches to it.
 *
 * The rule holds only because every engine dispatches a closed select's `change` inside the key's
 * own task, so the file runs in WebKit and Firefox as well as in Chromium. What a switch sends is
 * read back from the fake Envoy.
 *
 * The frames are spelt here as the wire carries them, and the file imports nothing the change
 * added, so it runs unchanged against the commit before the change and each row fails there on
 * its own assertion.
 */

/** The phone layout's project: the file runs in Chromium's `iphone`, never in `webkit-iphone`. */
const PHONE_PROJECTS = ["iphone"];

const MODELS = ["anthropic/model-a", "anthropic/model-b", "openai/model-c"];
const LEVELS = ["auto", "low", "medium", "high"];

/** A live session of the row's own: the relay's replay and responder are process-global, so a
 *  session another row seeded would carry that row's frames into this one. The numbers follow
 *  `agent-view-settings.e2e.ts`'s rows, which use 1 to 10. */
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

/** The settings of a session at `model` on thinking `level`, offering every model and level. */
function reported(seq: number, model: string, level: string): object {
  return {
    kind: "settings",
    model,
    models: MODELS,
    reported: true,
    seq,
    thinking: { level, levels: LEVELS },
    v: 1,
  };
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

/** What a session at `model-a` on `auto` that runs `/model` and `/effort` replays before any
 *  turn: its settings and its commands, and nothing else. */
function freshReplay(sessionID: string): object {
  return {
    frames: [reported(1, "anthropic/model-a", "auto"), commandsFrame(2)],
    session_id: sessionID,
    v: 1,
  };
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

/** The header's pickers and the shown values, as a reader finds them. */
function header(page: Page) {
  return {
    model: page.getByRole("combobox", { name: "Model" }),
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

// A2: a picker whose change switches a live session follows the rule the issue picker already
// keeps (`markKeyStep` in `AgentMessageComposer.tsx`, `keyboard-agents-picker.e2e.ts`): the arrows
// choose and Enter commits, so a keyboard reader can pass the next model to reach the one after
// it. An engine that commits a closed select's arrow step would switch the session to every model
// it passes on the way.
test("the arrows move a header picker's choice without switching the session, and Enter switches it once @phone", async ({
  browser,
}, testInfo) => {
  const session = liveSession(11);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status, thinking } = header(page);

    await model.focus();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    // Two steps past model-a reach openai/model-c; neither is a switch.
    await expect(model).toBeEnabled();
    await expect(model).toBeFocused();
    await expect(status).toHaveCount(0);
    expect(await sentSwitches()).toEqual([]);
    await page.keyboard.press("Enter");
    await expect(status).toHaveText("Switching model to openai/model-c…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    expect((await sentSwitches()).map((sent) => sent.message)).toEqual(["/model openai/model-c"]);

    const stored = await storedSwitch(id, "/model openai/model-c");
    await answer(id, stored.id, "Model set to openai/model-c.");
    await publishAgentStreamFrame(id, reported(3, "openai/model-c", "auto"));
    await expectPicked(page, "openai/model-c", "auto");

    await thinking.focus();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(thinking).toBeEnabled();
    expect((await sentSwitches()).map((sent) => sent.message)).toEqual(["/model openai/model-c"]);
    await page.keyboard.press("Enter");
    await expect(status).toHaveText("Switching thinking level to medium…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(2);
    expect((await sentSwitches()).map((sent) => sent.message)).toEqual([
      "/model openai/model-c",
      "/effort medium",
    ]);
  } finally {
    await context.close();
  }
});

// Decision 6a: leaving a picker without Enter drops its step, whichever way focus leaves it, and
// the picker shows the reported value again. The next switch, made with Enter, is then the only
// one the session was sent, so the dropped step never went out.
for (const [row, way, leave] of [
  [12, "Tab", (page: Page) => page.keyboard.press("Tab")],
  [
    13,
    "a click elsewhere on the page",
    (page: Page) => page.getByTestId("agent-thread-empty").click(),
  ],
] as const) {
  test(`a header picker's step left by ${way} sends nothing and puts the picker back on the reported model @phone`, async ({
    browser,
  }, testInfo) => {
    const session = liveSession(row);
    const id = session.session_id;
    await setLiveSessions([session]);
    await publishAgentStreamFrame(id, freshReplay(id), "replay");
    const { context, page } = await viewer(browser, testInfo, "alice");
    try {
      await page.goto(livePath(id));
      await expectPicked(page, "anthropic/model-a", "auto");
      const { model, shownModel, status } = header(page);

      await model.focus();
      await page.keyboard.press("ArrowDown");
      await expect(model).toHaveValue("anthropic/model-b");
      await expect(model).toBeFocused();
      await leave(page);
      await expect(model).not.toBeFocused();
      await expect(model).toHaveValue("anthropic/model-a");
      await expect(shownModel).toHaveText("anthropic/model-a");
      await expect(status).toHaveCount(0);
      expect(await sentSwitches()).toEqual([]);

      await model.focus();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(status).toHaveText("Switching model to openai/model-c…");
      await expect.poll(async () => (await sentSwitches()).length).toBe(1);
      expect(await sentSwitches()).toEqual([
        {
          actor: { id: "alice", kind: "user" },
          message: "/model openai/model-c",
          mode: "steer",
          target: id,
        },
      ]);
    } finally {
      await context.close();
    }
  });
}

// Decision 6a: type-ahead is a step as the arrows are. A letter moves the choice to the next
// option it starts, here `o` to openai/model-c past both anthropic models, and sends nothing;
// Enter switches to it.
test("type-ahead on a header picker moves its choice without switching the session, and Enter switches it once @phone", async ({
  browser,
}, testInfo) => {
  const session = liveSession(14);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, status } = header(page);

    await model.focus();
    await page.keyboard.press("o");
    await expect(model).toHaveValue("openai/model-c");
    await expect(model).toBeFocused();
    await expect(status).toHaveCount(0);
    expect(await sentSwitches()).toEqual([]);

    await page.keyboard.press("Enter");
    await expect(status).toHaveText("Switching model to openai/model-c…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    expect(await sentSwitches()).toEqual([
      {
        actor: { id: "alice", kind: "user" },
        message: "/model openai/model-c",
        mode: "steer",
        target: id,
      },
    ]);
  } finally {
    await context.close();
  }
});

// Decision 8: while a switch is pending the pickers are held with `aria-disabled`, not `disabled`,
// so a held picker keeps focus and its keys still step its choice, but Enter sends nothing. The
// step survives the switch's end, since the session reported no new thinking level meanwhile, so
// Enter afterwards switches to it.
test("a held header picker keeps focus and steps its choice but Enter sends nothing until the switch ends, and Enter then switches to that step @phone", async ({
  browser,
}, testInfo) => {
  const session = liveSession(15);
  const id = session.session_id;
  await setLiveSessions([session]);
  await publishAgentStreamFrame(id, freshReplay(id), "replay");
  const { context, page } = await viewer(browser, testInfo, "alice");
  try {
    await page.goto(livePath(id));
    await expectPicked(page, "anthropic/model-a", "auto");
    const { model, shownModel, status, thinking } = header(page);

    await model.selectOption("anthropic/model-b");
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(1);
    const stored = await storedSwitch(id, "/model anthropic/model-b");

    await thinking.focus();
    await expect(thinking).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("ArrowDown");
    await expect(thinking).toHaveValue("low");
    await page.keyboard.press("Enter");
    await expect(thinking).toBeFocused();
    await expect(thinking).toHaveAttribute("aria-disabled", "true");
    await expect(thinking).toHaveValue("low");
    await expect(status).toHaveText("Switching model to anthropic/model-b…");
    expect((await sentSwitches()).map((sent) => sent.message)).toEqual([
      "/model anthropic/model-b",
    ]);

    await answer(id, stored.id, "Model set to anthropic/model-b.");
    await publishAgentStreamFrame(id, reported(3, "anthropic/model-b", "auto"));
    await expect(status).toHaveText("The session answered: Model set to anthropic/model-b.");
    await expect(model).toHaveValue("anthropic/model-b");
    await expect(shownModel).toHaveText("anthropic/model-b");
    await expect(thinking).not.toHaveAttribute("aria-disabled", "true");
    await expect(thinking).toBeFocused();
    await expect(thinking).toHaveValue("low");

    await page.keyboard.press("Enter");
    await expect(status).toHaveText("Switching thinking level to low…");
    await expect.poll(async () => (await sentSwitches()).length).toBe(2);
    expect(await sentSwitches()).toEqual([
      {
        actor: { id: "alice", kind: "user" },
        message: "/model anthropic/model-b",
        mode: "steer",
        target: id,
      },
      { actor: { id: "alice", kind: "user" }, message: "/effort low", mode: "steer", target: id },
    ]);
  } finally {
    await context.close();
  }
});
