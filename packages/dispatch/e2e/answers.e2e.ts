import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  editArtifact,
  getArtifactText,
  getAsk,
  getIssue,
  getIssueEvents,
  listMyAnswers,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-answers", kind: "session" as const },
  as: "agent" as const,
};
const shipOrHold = [{ label: "Ship" }, { label: "Hold" }];

test.beforeEach(async () => {
  await resetDatabase();
});

function answerRows(page: Page): Locator {
  return page.locator("[data-answer-row]");
}

/** The answered ask an item route selected on its document: the margin's decided card. */
async function expectSelectedMarginAsk(page: Page, id: string, isPhone: boolean): Promise<void> {
  const sheet = page.getByTestId("margin-sheet");
  const item = sheet.locator(`[data-margin-item="${id}"]`);
  await expect(item).toHaveAttribute("aria-current", "true");
  if (isPhone && (await sheet.getAttribute("data-expanded")) !== "true") {
    await sheet.getByRole("button", { name: "Open review panel" }).click();
  }
  await expect(item).toBeInViewport();
}

/** An unanchored ask's item route lands on its Conversation turn, focused. */
async function expectFocusedAskTurn(page: Page, id: string): Promise<void> {
  const turn = page.locator(`li[data-turn="ask:${id}"][aria-current="true"]`);
  await expect(turn).toBeVisible();
  await expect(turn).toBeInViewport();
}

test("the answers page lists a person's answers and replies, opens each ask, and changes an answer", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const anchoredIssue = await createIssue({
    project: "CORE",
    spec: "Ship the release tonight.\n",
    title: "Release train",
  });
  const changedIssue = await createIssue({ project: "CORE", title: "Billing freeze" });
  const repliedIssue = await createIssue({ project: "CORE", title: "Build question" });
  const anchored = await createAsk(
    anchoredIssue.key,
    { anchor: { artifact: "spec", quote: "release" }, options: shipOrHold, question: "Ship it?" },
    session
  );
  const changed = await createAsk(
    changedIssue.key,
    { options: shipOrHold, question: "Freeze billing?" },
    session
  );
  const replied = await createAsk(
    repliedIssue.key,
    { options: shipOrHold, question: "Which build?" },
    session
  );

  // Oldest to newest: alice's reply, then her two answers.
  await createComment(repliedIssue.key, { ask_id: replied.id, body: "Does nightly count?" });
  await answerAsk(anchored.id, { expected_edited_at: null, selected: ["Ship"] });
  await answerAsk(changed.id, {
    expected_edited_at: null,
    selected: ["Ship"],
    text: "Freeze after the run.",
  });

  // The server names each issue ask by its item route, which is what Open follows.
  const listed = await listMyAnswers();
  expect(listed.total).toBe(3);
  expect(listed.rows.map((row) => [row.kind, row.ask_id, row.ref])).toEqual([
    ["answer", changed.id, `/issues/${changedIssue.key}/asks/${changed.id}`],
    ["answer", anchored.id, `/issues/${anchoredIssue.key}/asks/${anchored.id}`],
    ["reply", replied.id, `/issues/${repliedIssue.key}/asks/${replied.id}`],
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await page.getByRole("link", { name: "Answered by you" }).click();
    await expect(page).toHaveURL("/answers");
    await expect(page.getByRole("heading", { name: "Answered by you" })).toBeVisible();
    await expect(answerRows(page)).toHaveCount(3);
    await expect
      .poll(() => answerRows(page).evaluateAll((rows) => rows.map((row) => row.dataset.answerRow)))
      .toEqual([changed.id, anchored.id, replied.id]);

    const rowOf = (id: string) => page.locator(`[data-answer-row="${id}"]`).first();
    for (const [id, issue, question] of [
      [changed.id, changedIssue, "Freeze billing?"],
      [anchored.id, anchoredIssue, "Ship it?"],
      [replied.id, repliedIssue, "Which build?"],
    ] as const) {
      const row = rowOf(id);
      await expect(row.locator("time")).toBeVisible();
      await expect(row.getByRole("link", { name: new RegExp(issue.key) })).toHaveAttribute(
        "href",
        `/issues/${issue.key}`
      );
      await expect(row).toContainText(issue.title);
      await expect(row).toContainText(question);
    }
    await expect(rowOf(changed.id)).toContainText("Ship");
    await expect(rowOf(changed.id)).toContainText("Freeze after the run.");
    await expect(rowOf(anchored.id)).toContainText("Ship");
    await expect(rowOf(replied.id)).toContainText("Replied:");
    await expect(rowOf(replied.id)).toContainText("Does nightly count?");
    await expect(rowOf(replied.id).getByRole("button", { name: "Change answer" })).toHaveCount(0);

    // Open lands on the ask: an anchored one selected on its document, an unanchored one on its
    // Conversation turn.
    await rowOf(anchored.id).getByRole("link", { exact: true, name: "Open" }).click();
    await expect(page).toHaveURL(`/issues/${anchoredIssue.key}/spec?ask=${anchored.id}`);
    await expectSelectedMarginAsk(page, anchored.id, isPhone);
    for (const [id, issue] of [
      [changed.id, changedIssue],
      [replied.id, repliedIssue],
    ] as const) {
      await page.goto("/answers");
      await rowOf(id).getByRole("link", { exact: true, name: "Open" }).click();
      await expect(page).toHaveURL(`/issues/${issue.key}/asks/${id}`);
      await expectFocusedAskTurn(page, id);
    }

    // Change answer on the top row opens the form seeded with the current answer, not a
    // second completion card.
    await page.goto("/answers");
    await expect(answerRows(page)).toHaveCount(3);
    await rowOf(changed.id).getByRole("button", { name: "Change answer" }).click();
    const card = page.locator(`[data-answer-card="${changed.id}"]`);
    await expect(card.getByRole("radio", { name: "Ship" })).toBeChecked();
    await expect(card.getByLabel("Your answer")).toHaveValue("Freeze after the run.");
    await expect(card).not.toContainText("Answered by");
    await card.getByRole("radio", { name: "Hold" }).check();
    await card.getByLabel("Your answer").fill("");
    await card.getByRole("button", { name: "Save answer" }).click();

    // The new answer heads the list, the replaced one reads as history, and the card stays open
    // under the new row with both answers in order.
    await expect(answerRows(page)).toHaveCount(4);
    await expect
      .poll(() => answerRows(page).evaluateAll((rows) => rows.map((row) => row.dataset.answerRow)))
      .toEqual([changed.id, changed.id, anchored.id, replied.id]);
    const top = answerRows(page).nth(0);
    const older = answerRows(page).nth(1);
    await expect(top).toContainText("Hold");
    await expect(top).not.toContainText("Changed since");
    await expect(older).toContainText("Changed since");
    await expect(older.getByRole("button", { name: "Change answer" })).toHaveCount(0);
    await expect(card).toContainText("Answered by alice");
    await expect(card.locator('ul[aria-label="Options"] li[data-selected="true"]')).toContainText(
      "Hold"
    );
    await card.getByText("Show 1 earlier answer").click();
    await expect(card).toContainText("Freeze after the run.");

    const read = await getAsk(changed.id);
    expect(read.answers.map((answer) => [answer.user, answer.selected])).toEqual([
      ["alice", ["Ship"]],
      ["alice", ["Hold"]],
    ]);
    const answered = (await getIssueEvents(changedIssue.key)).flatMap((event) =>
      event.type === "ask.answered" ? [event.payload] : []
    );
    expect(answered).toHaveLength(2);
    expect(answered[0]?.previous_answer).toBeUndefined();
    expect(answered[1]?.previous_answer?.selected).toEqual(["Ship"]);
    expect(answered[1]?.answer?.selected).toEqual(["Hold"]);
  } finally {
    await alice.close();
  }

  // Another person's page holds none of alice's answers.
  expect((await listMyAnswers({}, { login: "bob" })).rows).toEqual([]);
  const bob = await asUser(browser, "bob");
  try {
    const page = await bob.newPage();
    await page.goto("/answers");
    await expect(page.getByText("You have not answered or replied on an ask yet.")).toBeVisible();
    await expect(answerRows(page)).toHaveCount(0);
  } finally {
    await bob.close();
  }
});

test("an issue page's decided ask offers Change answer to its answerer alone, and lists the earlier answer after a change", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release train" });
  const ask = await createAsk(issue.key, { options: shipOrHold, question: "Ship it?" }, session);
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="ask:${ask.id}"]`);
    await expect(turn).toContainText("Answered by alice");
    await expect(turn).not.toContainText("earlier answer");
    await turn.getByRole("button", { name: "Change answer" }).click();
    await expect(turn.getByRole("radio", { name: "Ship" })).toBeChecked();
    await turn.getByRole("radio", { name: "Hold" }).check();
    await turn.getByRole("button", { name: "Save answer" }).click();
    await expect(turn.locator('ul[aria-label="Options"] li[data-selected="true"]')).toContainText(
      "Hold"
    );
    await expect(turn.getByText("Show 1 earlier answer")).toBeVisible();
    await expect(turn.getByRole("button", { name: "Change answer" })).toBeVisible();
  } finally {
    await alice.close();
  }

  const bob = await asUser(browser, "bob");
  try {
    const page = await bob.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="ask:${ask.id}"]`);
    await expect(turn).toContainText("Answered by alice");
    await expect(turn.getByText("Show 1 earlier answer")).toBeVisible();
    await expect(turn.getByRole("button", { name: "Change answer" })).toHaveCount(0);
  } finally {
    await bob.close();
  }
});

test("changing a decision block's answer rewrites the block in the document", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext\n",
    title: "Ask block",
  });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  await expect
    .poll(async () =>
      (await getIssue(issue.key)).open_asks.some((ask) => ask.block_id === "ship-decision")
    )
    .toBe(true);
  const documentText = async () => (await getArtifactText(issue.primary_artifact_id)).markdown;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    const hosted = block.locator("article[data-testid^=ask-]");
    await expect(hosted).toBeVisible();
    await hosted.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    await hosted.getByLabel("Your answer").fill("Release tonight.");
    await hosted.getByRole("radio", { name: "Ship Release it" }).check();
    await hosted.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(block).toContainText("Answered by alice");

    await expect.poll(documentText).toContain('selected="[&#x22;Ship&#x22;]"');
    const first = await documentText();
    expect(first).toContain('answer="Release tonight."');
    const firstAnsweredAt = /answered_at="([^"]+)"/.exec(first)?.[1];
    expect(firstAnsweredAt).toBeDefined();

    // The compact card shows the note the change seeds, so it is cleared on purpose, not sent
    // unseen.
    await hosted.getByRole("button", { name: "Change answer" }).click();
    await expect(hosted.getByLabel("Your answer")).toHaveValue("Release tonight.");
    await hosted.getByLabel("Your answer").fill("");
    await hosted.getByRole("radio", { name: "Hold Wait for review" }).check();
    await hosted.getByRole("button", { name: "Save answer" }).click();
    await expect(block.locator('ul[aria-label="Options"] li[data-selected="true"]')).toContainText(
      "Hold"
    );
    await expect(block.getByText("Show 1 earlier answer")).toBeVisible();

    await expect.poll(documentText).toContain('selected="[&#x22;Hold&#x22;]"');
    const second = await documentText();
    expect(second).not.toMatch(/\sanswer="/);
    const secondAnsweredAt = /answered_at="([^"]+)"/.exec(second)?.[1];
    expect(secondAnsweredAt).toBeDefined();
    expect(Date.parse(secondAnsweredAt ?? "")).toBeGreaterThan(Date.parse(firstAnsweredAt ?? ""));
  } finally {
    await alice.close();
  }
});
