import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { answerAsk, createAsk, createComment, createIssue, createProject, resolveAsk } from "./api";
import { recordClipboard } from "./clipboard";
import { documentEditor, indexedBlockAsk } from "./editor";
import { askCard, inboxRow } from "./inbox-pane";
import { type KeyedWrite, watchWrites } from "./keyed-writes";
import { resetDatabase } from "./seed";
import { pasteFile } from "./sends";
import { sessionIds } from "./session-ids";
import { asUser } from "./users";

/**
 * INFRA-341: the ask card's answer box and the reply box under an answered ask are one answer
 * area, with the Conversation tab's own field and `@` picker. A picked session is stored as a
 * `session:` mention on the comment Ask back and Reply post, and that session gets it; an answer
 * carries no mentions, so Answer refuses one rather than dropping it. Every send that cannot be
 * made says why in place (`RefusableButton`), and one ask's header reads the same wherever the
 * ask is drawn.
 */

/** The one live session the `@` picker offers. */
const planner: FakeSession = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: sessionIds.planner,
  title: "Planner",
};

const PLANNER_TARGET = `session:${sessionIds.planner}`;

/** The asker of every ask these rows seed. */
const asker = { actor: { id: sessionIds.planner, kind: "session" as const }, as: "agent" as const };

const MENTION_REFUSAL =
  "An answer cannot mention a session. Remove the mention, or send it with Ask back.";
const EMPTY_ANSWER_REFUSAL = "Pick an option or write an answer.";
const EMPTY_DRAFT_REFUSAL = "Write something first.";

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([planner]);
});

/** An issue with one open ask on it, asked by the live session. */
async function seedOpenAsk(question = "Which release path?") {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Answer area" });
  const ask = await createAsk(issue.key, { question }, asker);
  return { ask, issue };
}

/** Types `text` - which must end in the `@` the picker stands over - and picks `option`. */
async function pickMention(
  page: Page,
  field: Locator,
  text: string,
  option: string
): Promise<void> {
  await field.fill(text);
  await page
    .getByRole("listbox", { name: "Mention suggestions" })
    .getByRole("option", { name: option })
    .click();
  await expect(field).toHaveValue(`${text}${option}`);
}

/** The comments the page posts on `issueKey` from here on, as the bodies it sent. */
function postedComments(page: Page, issueKey: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "POST", `/api/v1/issues/${issueKey}/comments`);
}

/** One ask's whole turn in the Conversation: an answered ask's card is the record alone, and the
 *  thread under it - its replies and its Write a reply control - is the turn's, beside it. */
function conversationTurn(page: Page, askId: string): Locator {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li")
    .filter({ has: page.getByTestId(`ask-${askId}`) });
}

/** Every `data-ask-*` field the headers inside `place` hold, with the place's own anchor quote.
 *  A decision block draws two headers - the block's own urgency and state, and the hosted card's,
 *  which adds the provenance and the reference - so the fields are collected across them. A field
 *  two headers of one place disagree about is reported as both values, which fails the row with
 *  the disagreement in it. */
async function headerFacts(place: Locator): Promise<Record<string, string | undefined>> {
  return place.evaluate((node) => {
    const names = ["urgency", "asker", "at", "copy", "state"];
    const held: Record<string, string | undefined> = {};
    const headers = [...node.querySelectorAll<HTMLElement>("[data-ask-header]")];
    for (const name of names) {
      const values = [
        ...new Set(
          headers
            .map((header) => header.dataset[`ask${name[0]?.toUpperCase()}${name.slice(1)}`])
            .filter((value) => value !== undefined)
        ),
      ];
      held[name] = values.length === 0 ? undefined : values.join(" ≠ ");
    }
    const quote = node.querySelector("blockquote");
    held.anchor = quote?.textContent ?? undefined;
    return held;
  });
}

test("one ask header says the same of an open, an answered and a resolved ask wherever it is drawn @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nThe mirror replays the ledger on a cold start.\n",
    title: "Header everywhere",
  });
  const quote = "replays the ledger";
  const open = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Does a cold start replay the ledger?" },
    asker
  );
  const answeredAsk = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Which mirror keeps the budget?" },
    asker
  );
  await answerAsk(answeredAsk.id, {
    expected_edited_at: null,
    selected: [],
    text: "The left one.",
  });
  const resolved = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Should the mirror retry?" },
    asker
  );
  await resolveAsk(resolved.id, { kind: "resolved", reason: "Decided elsewhere." });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Inbox, which draws the open ask's card: in the reading column at desktop width, in the
    // row itself on a phone.
    await page.goto("/");
    const inboxFacts = await headerFacts(await askCard(page, open.id));
    expect(inboxFacts).toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: open.created_at,
      copy: `dispatch://${issue.key}/ask/${open.id}`,
      state: "open",
      urgency: "med",
    });

    // The Inbox drawer, over whatever page the reader is on, draws the same card.
    await page.goto(`/issues/${issue.key}`);
    await page
      .getByRole("button", { name: /^(Open inbox|Peek at the inbox)/ })
      .first()
      .click();
    const drawer = page.getByRole("dialog", { name: "Inbox" });
    await expect(drawer.getByTestId(`ask-${open.id}`)).toBeVisible();
    expect(await headerFacts(drawer.getByTestId(`ask-${open.id}`))).toMatchObject(inboxFacts);
    await page.keyboard.press("Escape");

    // The Conversation tab, where all three are drawn: the open card, the answered record and the
    // resolved record.
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.locator("#issue-conversation-panel");
    expect(await headerFacts(conversation.getByTestId(`ask-${open.id}`))).toMatchObject(inboxFacts);
    const answeredFacts = await headerFacts(conversation.getByTestId(`ask-${answeredAsk.id}`));
    expect(answeredFacts).toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: answeredAsk.created_at,
      copy: `dispatch://${issue.key}/ask/${answeredAsk.id}`,
      state: "answered",
      urgency: "med",
    });
    const resolvedFacts = await headerFacts(conversation.getByTestId(`ask-${resolved.id}`));
    expect(resolvedFacts).toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: resolved.created_at,
      copy: `dispatch://${issue.key}/ask/${resolved.id}`,
      state: "resolved",
      urgency: "med",
    });
  } finally {
    await alice.close();
  }
});

test("a decision block draws the same header fields as the ask's other places, without its anchor @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#decide urgency="high" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n',
    title: "Block header",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const inboxFacts = await headerFacts(await askCard(page, blockAsk.id));
    expect(inboxFacts).toMatchObject({
      asker: "alice",
      at: blockAsk.created_at,
      copy: `dispatch://${issue.key}/ask/${blockAsk.id}`,
      state: "open",
      urgency: "high",
    });

    // The block is its own anchor, so the block draws no quote; every other field is the same,
    // and the block's two headers hold each field once between them.
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="decide"]');
    await expect(block.locator("article[data-testid^=ask-]")).toBeVisible();
    const blockFacts = await headerFacts(block);
    expect(blockFacts).toEqual({ ...inboxFacts, anchor: undefined });
  } finally {
    await alice.close();
  }
});

test("a session picked in an Inbox ask's answer area goes out on Ask back, and no answer carries it @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    await pickMention(page, field, "Over to @", "Planner");
    await expect(card.getByRole("list", { name: "Mention targets" })).toContainText(PLANNER_TARGET);

    // An answer carries no mentions, so Answer refuses this draft: in place, with its reason on
    // screen and as its accessible description, and it sends nothing by click or by key.
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    await answer.focus();
    await expect(answer).toBeDisabled();
    await expect(answer).toHaveAttribute("aria-disabled", "true");
    await expect(answer).toHaveAttribute("title", MENTION_REFUSAL);
    await expect(answer).toHaveAccessibleDescription(MENTION_REFUSAL);
    await expect(card.getByText(MENTION_REFUSAL, { exact: true })).toBeVisible();
    await answer.click({ force: true });
    await expect(answer).toBeFocused();
    await field.press("ControlOrMeta+Enter");
    await page.waitForTimeout(500);
    expect(answers).toEqual([]);

    // Ask back posts it as a comment that names the session, and the session gets it.
    await card.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([
        { ask_id: ask.id, body: "Over to @Planner", mentions: [{ target: PLANNER_TARGET }] },
      ]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: planner.session_id }]);
    await expect(field).toHaveValue("");
  } finally {
    await alice.close();
  }
});

test("a decision block's answer area picks a session the same way and asks back with it @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#decide urgency="med" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n',
    title: "Block answer area",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="decide"]');
    const card = block.locator("article[data-testid^=ask-]");
    await expect(card).toBeVisible();
    await card.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    const field = card.getByLabel("Your answer");
    await pickMention(page, field, "Ask @", "Planner");

    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    await expect(answer).toBeDisabled();
    await expect(answer).toHaveAccessibleDescription(MENTION_REFUSAL);
    await card.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([
        { ask_id: blockAsk.id, body: "Ask @Planner", mentions: [{ target: PLANNER_TARGET }] },
      ]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: planner.session_id }]);
  } finally {
    await alice.close();
  }
});

test("the Conversation tab's copy of an ask, and the reply box under an answered ask, send a picked session @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.locator("#issue-conversation-panel");

    // The open ask's own card in the timeline: the compact variant, whose answer area is behind
    // its note disclosure.
    const openCard = conversation.getByTestId(`ask-${ask.id}`);
    await openCard.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    await pickMention(page, openCard.getByLabel("Your answer"), "Ask @", "Planner");
    await openCard.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: ask.id, body: "Ask @Planner", mentions: [{ target: PLANNER_TARGET }] }]);

    // The reply box under the answered ask, behind its own Write a reply control. An answered
    // ask's card is its record, and the thread with that control is the turn's, beside it.
    const answeredTurn = conversationTurn(page, decided.id);
    await answeredTurn.getByRole("button", { name: "Write a reply" }).click();
    await pickMention(page, answeredTurn.getByLabel("Reply"), "Over to @", "Planner");
    await answeredTurn.getByRole("button", { exact: true, name: "Reply" }).click();
    await expect.poll(() => comments.map((write) => write.input)).toHaveLength(2);
    expect(comments[1]?.input).toEqual({
      ask_id: decided.id,
      body: "Over to @Planner",
      mentions: [{ target: PLANNER_TARGET }],
    });
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([
        { target_session: planner.session_id },
        { target_session: planner.session_id },
      ]);
  } finally {
    await alice.close();
  }
});

test("an ask draft keeps its picked mention when its box closes and opens again @phone", async ({
  browser,
}) => {
  const { issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    const open = turn.getByRole("button", { name: "Write a reply" });
    await open.click();
    await pickMention(page, turn.getByLabel("Reply"), "Over to @", "Planner");

    // The box goes with its disclosure and comes back with the draft the ask kept, mentions and
    // all, so the reply still reaches the session that was picked before it closed.
    await open.click();
    await expect(turn.getByLabel("Reply")).toHaveCount(0);
    await open.click();
    await expect(turn.getByLabel("Reply")).toHaveValue("Over to @Planner");
    await expect(turn.getByRole("list", { name: "Mention targets" })).toContainText(PLANNER_TARGET);
    await turn.getByRole("button", { exact: true, name: "Reply" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([
        { ask_id: decided.id, body: "Over to @Planner", mentions: [{ target: PLANNER_TARGET }] },
      ]);
  } finally {
    await alice.close();
  }
});

test("Answer, Ask back and Reply refuse an empty ask box in place, sending nothing @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    const askBack = card.getByRole("button", { exact: true, name: "Ask back" });
    for (const [button, reason] of [
      [answer, EMPTY_ANSWER_REFUSAL],
      [askBack, EMPTY_DRAFT_REFUSAL],
    ] as const) {
      // A refused button keeps its place in the tab order, so the reader can reach and hold it;
      // pressing it anyway does nothing. (A tap on a phone leaves the focus where the browser
      // puts it, so the reach is read before the press, as the other refusal rows read it.)
      await button.focus();
      await expect(button).toBeFocused();
      await expect(button).toBeDisabled();
      await expect(button).toHaveAttribute("title", reason);
      await expect(button).toHaveAccessibleDescription(reason);
      await expect(card.getByText(reason, { exact: true })).toBeVisible();
      await button.click({ force: true });
    }
    await page.waitForTimeout(500);
    expect(answers).toEqual([]);
    expect(comments).toEqual([]);

    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    await turn.getByRole("button", { name: "Write a reply" }).click();
    const reply = turn.getByRole("button", { exact: true, name: "Reply" });
    await reply.focus();
    await expect(reply).toBeFocused();
    await expect(reply).toBeDisabled();
    await expect(reply).toHaveAttribute("title", EMPTY_DRAFT_REFUSAL);
    await expect(reply).toHaveAccessibleDescription(EMPTY_DRAFT_REFUSAL);
    await expect(turn.getByText(EMPTY_DRAFT_REFUSAL, { exact: true })).toBeVisible();
    await reply.click({ force: true });
    await turn.getByLabel("Reply").press("ControlOrMeta+Enter");
    await page.waitForTimeout(500);
    expect(comments).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a file pasted into an ask's answer box and into its reply box adds its line, and Ctrl+Enter sends", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    await field.fill("See");
    await pasteFile(field, "notes.md", "the notes");
    // A file that is no picture joins the draft as its plain reference (`uploadedFileText`).
    const line = `dispatch://${issue.key}/artifact/notes-md`;
    await expect(field).toHaveValue(`See ${line}`);
    await field.press("ControlOrMeta+Enter");
    await expect
      .poll(() => answers.map((write) => write.input))
      .toEqual([{ expected_edited_at: null, selected: [], text: `See ${line}` }]);

    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    await turn.getByRole("button", { name: "Write a reply" }).click();
    const replyField = turn.getByLabel("Reply");
    await replyField.fill("And");
    await pasteFile(replyField, "notes.md", "the notes");
    const second = `dispatch://${issue.key}/artifact/notes-md`;
    await expect(replyField).toHaveValue(`And ${second}`);
    await replyField.press("ControlOrMeta+Enter");
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: decided.id, body: `And ${second}` }]);
  } finally {
    await alice.close();
  }
});

test("the open picker owns the arrows and Enter inside an Inbox ask card @phone", async ({
  browser,
}) => {
  const { ask } = await seedOpenAsk();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    await field.fill("@");
    const option = page
      .getByRole("listbox", { name: "Mention suggestions" })
      .getByRole("option", { name: "Planner" });
    await expect(option).toBeVisible();
    // Nothing is active until a key moves it; the arrows are the picker's, not the Inbox's own
    // j/k twins, and the row the reader is on keeps the focus context either way.
    await expect(option).toHaveAttribute("aria-selected", "false");
    await page.keyboard.press("ArrowDown");
    await expect(option).toHaveAttribute("aria-selected", "true");
    await page.keyboard.press("ArrowUp");
    await expect(option).toHaveAttribute("aria-selected", "true");
    await expect(field).toBeFocused();
    // Enter picks the option rather than moving the Inbox's own focus into the answer box.
    await page.keyboard.press("Enter");
    await expect(field).toHaveValue("@Planner");
    await expect(page.getByRole("listbox", { name: "Mention suggestions" })).toHaveCount(0);
    await expect(field).toBeFocused();
    await expect(inboxRow(page, ask.id)).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("an ask reply in the Inbox copies its own dispatch:// reference @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const posted = await createComment(
    issue.key,
    { ask_id: ask.id, body: "Which environment?" },
    asker
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const copied = await recordClipboard(page);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const reply = card.getByTestId(`thread-${ask.id}`).locator("li[data-reply]").first();
    await expect(reply).toContainText("Which environment?");
    await reply.getByRole("button", { name: /^Copy reference/ }).click();
    expect(await copied()).toEqual([`dispatch://${issue.key}/comment/${posted.id}`]);
  } finally {
    await alice.close();
  }
});
