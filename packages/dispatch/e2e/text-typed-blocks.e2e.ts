import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  test,
} from "@playwright/test";
import type { IssueDetails } from "../web/src/api/types";

import {
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifactText,
  getIssue,
} from "./api";
import { documentEditor, openWithCaret } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A typed block written in a text body (a comment, an ask, a message) is drawn as a document
// draws it: a callout carries its header, the block's name with its kind badge and its title on
// one row, and a decision (`:::ask`) block is the decision card, its urgency pill and "Decision"
// in the header and its options as the card's rows, with the source list folded away.

const agent = {
  actor: { id: "e2e-text-blocks-agent", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** Opens the comment's thread, where its body is drawn whole (a collapsed turn shows a two-line
 * preview), and returns what holds it: the turn itself, or on a phone the thread's own dialog. */
async function openCommentThread(page: Page, commentId: string): Promise<Locator> {
  const turn = page
    .getByRole("list", { name: "Conversation turns" })
    .locator(`:scope > li[data-turn="comment:${commentId}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  const phoneThread = page.getByRole("dialog", { name: "Thread" });
  await expect(phoneThread.or(turn.getByRole("button", { name: "Collapse thread" }))).toBeVisible();
  return (await phoneThread.count()) === 0 ? turn : phoneThread;
}

test("a comment's callout shows the document's header with its kind and title @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Callout in a comment" });
  const comment = await createComment(
    issue.key,
    { body: ':::callout{kind="warning" title="Read this"}\nBody text.\n:::\n' },
    agent
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, comment.id);
    const callout = thread.locator('section[data-proof-block-type="callout"]');
    await expect(
      callout.locator("[data-proof-block-content]").getByText("Body text.", { exact: true })
    ).toBeVisible();

    const header = callout.locator("[data-proof-block-summary]");
    await expect(header).toBeVisible();
    await expect(callout.locator("[data-proof-block-name]")).toHaveText("callout");

    // The kind reads as a badge: a filled pill, not bare text.
    const badge = callout.locator("[data-proof-block-badge]");
    await expect(badge).toBeVisible();
    await expect(badge).toHaveText("warning");
    expect(await badge.evaluate((element) => getComputedStyle(element).backgroundColor)).not.toBe(
      "rgba(0, 0, 0, 0)"
    );

    const title = callout.locator("[data-proof-block-title]");
    await expect(title).toHaveText("Read this");
    await expect(title).toBeVisible();
    expect((await title.boundingBox())?.width).toBeGreaterThan(0);

    // The header reads as a label rather than a stack of prose lines: one row.
    expect((await header.boundingBox())?.height).toBeLessThanOrEqual(40);
  } finally {
    await alice.close();
  }
});

test("a comment's ask block shows the decision card @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Decision in a comment" });
  const comment = await createComment(
    issue.key,
    { body: ':::ask{urgency="high"}\nWhich one?\n\n- A: first\n- B: second\n:::\n' },
    agent
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, comment.id);
    const decision = thread.locator('section[data-proof-block-type="ask"]');
    await expect(decision.getByText("Which one?", { exact: true })).toBeVisible();

    // The header names the block a decision and its urgency.
    const header = decision.locator("[data-dispatch-ask-header]");
    await expect(header).toBeVisible();
    await expect(header).toContainText("Decision");
    await expect(header.getByText("Decision", { exact: true })).toBeVisible();
    const pill = decision.locator('[data-dispatch-ask-pill="high"]');
    await expect(pill).toBeVisible();
    await expect(pill).toHaveText("High");

    // The options are the card's rows, label and description each.
    const record = decision.locator("[data-dispatch-ask-record]");
    await expect(record).toBeVisible();
    const rows = record.locator('ul[aria-label="Options"] > li');
    await expect(rows).toHaveCount(2);
    for (const [index, label, description] of [
      [0, "A", "first"],
      [1, "B", "second"],
    ] as const) {
      const row = rows.nth(index);
      await expect(row).toBeVisible();
      await expect(row).toContainText(label);
      await expect(row).toContainText(description);
    }

    // The source "- Option: description" list is there, folded behind the rows.
    const source = decision.locator("[data-proof-block-content] > ul");
    await expect(source).toHaveCount(1);
    await expect(source).toBeHidden();

    // The card's urgency accent: a 4px left border.
    await expect(decision).toHaveCSS("border-left-width", "4px");
  } finally {
    await alice.close();
  }
});

/** Files an issue titled `title` whose document holds an open decision, replies `body` under that
 * decision, and opens the issue as alice. Returns alice's context, for the caller to close, and
 * the reply's thread inside the document's decision card. */
async function replyUnderDocumentDecision(
  browser: Browser,
  title: string,
  body: string
): Promise<{ alice: BrowserContext; thread: Locator }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext.\n",
    title,
  });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#storage urgency="high" multiple="false" state="open"}\nWhich engine?\n\n- Postgres: reuse\n- SQLite: one file\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  let storage: IssueDetails["open_asks"][number] | undefined;
  await expect
    .poll(async () => {
      [storage] = (await getIssue(issue.key)).open_asks;
      return storage;
    })
    .toBeDefined();
  if (storage === undefined) {
    throw new Error("the storage decision was not indexed");
  }
  await createComment(issue.key, { ask_id: storage.id, body }, agent);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decision = documentEditor(page).locator('[data-dispatch-ask-block="storage"]');
    return { alice, thread: decision.getByTestId(`thread-${storage.id}`) };
  } catch (error) {
    await alice.close();
    throw error;
  }
}

// A reply under a document's decision is a text body drawn inside that decision's card. The
// decision folds its own source option list once its card's rows show the options; that fold
// must not reach the typed blocks a reply holds: a callout's bullet list is what its author
// wrote, and a malformed decision in the reply has no rows, so its list is the only place its
// options show.
test("a reply under a document's decision keeps the lists of the typed blocks it holds", async ({
  browser,
}) => {
  const { alice, thread } = await replyUnderDocumentDecision(
    browser,
    "Reply under a decision",
    ':::callout{kind="warning" title="Steps first"}\n- step one\n- step two\n:::\n\n' +
      ":::ask\nBroken inner?\n\n- Fine: ok\n- : blank\n:::\n"
  );

  try {
    await expect(thread.getByText("Broken inner?", { exact: true })).toBeVisible();

    // The callout's own list shows.
    await expect(thread.getByText("step one", { exact: true })).toBeVisible();
    await expect(thread.getByText("step two", { exact: true })).toBeVisible();
    // The malformed decision's source list shows: it has no rows to stand in for it.
    await expect(thread.getByText("Fine: ok", { exact: true })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// A decision drawn inside a reply sits in a card of its own, so its question and its last line
// sit flush with the card's edges as a document's decision does. The reply sits in the footer of
// the document's decision, whose `prose-sm` spacing for the reply's paragraphs and lists must not
// reach the nested card's first and last lines.
// SLOW(>=2s): an issue document with a decision card, its reply and the nested card whose first
// and last lines are measured; the time is the editor's mount.
test("a decision in a reply under a document's decision sits flush in its card", async ({
  browser,
}) => {
  const { alice, thread } = await replyUnderDocumentDecision(
    browser,
    "Nested decision in a reply",
    ":::ask\nNested question?\n\n- A: a\n- B: b\n:::\n\n" +
      ":::ask\nBroken nested?\n\n- Fine: ok\n- : blank\n:::\n"
  );

  try {
    const nested = thread.locator('section[data-proof-block-type="ask"]');

    // The well-formed nested decision's question sits flush under its card's header.
    const wellFormedQuestion = nested
      .filter({ hasText: "Nested question?" })
      .locator(":scope > [data-proof-block-content] > :first-child");
    await expect(wellFormedQuestion).toHaveText("Nested question?");
    await expect(wellFormedQuestion).toBeVisible();
    await expect(wellFormedQuestion).toHaveCSS("margin-top", "0px");

    // The malformed one shows its source list; question and list sit flush in the card.
    const brokenHole = nested
      .filter({ hasText: "Broken nested?" })
      .locator(":scope > [data-proof-block-content]");
    const brokenQuestion = brokenHole.locator(":scope > :first-child");
    await expect(brokenQuestion).toHaveText("Broken nested?");
    await expect(brokenQuestion).toBeVisible();
    await expect(brokenQuestion).toHaveCSS("margin-top", "0px");
    const brokenList = brokenHole.locator(":scope > :last-child");
    await expect(brokenList).toHaveJSProperty("tagName", "UL");
    await expect(brokenList).toBeVisible();
    await expect(brokenList).toHaveCSS("margin-bottom", "0px");
  } finally {
    await alice.close();
  }
});

// A malformed decision shows its source list, the only place its options show. A blank item in
// that list is no option at all, so it shows no bullet where nothing is edited: a text body,
// whose tight list items carry no paragraph, hides it, and so does a read-only render of a
// document. In an editable document the blank item is where a human types the option, so it shows.
test("a malformed decision's blank option shows no bullet in a body or a read-only document", async ({
  browser,
}) => {
  const source = ':::ask{#blank urgency="high"}\nWhich one?\n\n- A: first\n-\n:::\n';
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext.\n",
    title: "Blank option",
  });
  // Edits to the issue's own document refuse a malformed decision, so it is filed as one of the
  // issue's documents.
  const created = await createIssueArtifact(
    issue.key,
    { content: source, name: "blank.md" },
    { as: "agent" }
  );
  const comment = await createComment(issue.key, { body: source }, agent);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, comment.id);
    const bodyDecision = thread.locator('section[data-proof-block-type="ask"]');
    await expect(bodyDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    await expect(
      bodyDecision.locator("[data-proof-block-content] li").filter({ hasText: "A: first" })
    ).toBeVisible();
    const bodyItems = bodyDecision.locator("[data-proof-block-content] > ul > li");
    await expect(bodyItems).toHaveCount(2);
    await expect(bodyItems.nth(1)).toHaveCSS("display", "none");

    await page.goto(`/issues/${issue.key}/artifacts/${created.artifact.slug}`);
    const docDecision = documentEditor(page).locator('[data-dispatch-ask-block="blank"]');
    await expect(docDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    const docItems = docDecision.locator("[data-proof-block-content] > ul > li");
    await expect(docItems).toHaveCount(2);
    await expect(docItems.filter({ hasText: "A: first" })).toBeVisible();
    const docBlank = docItems.nth(1);
    await expect(docBlank).toHaveText("");
    await expect(docBlank).toHaveCSS("display", "list-item");

    await page.goto(`/issues/${issue.key}/artifacts/${created.artifact.slug}?v=1`);
    const versionView = page.getByTestId("version-view");
    await expect(versionView.locator('.dispatch-doc[data-read-only="true"]')).toBeVisible();
    const readOnlyDecision = versionView.locator('[data-dispatch-ask-block="blank"]');
    await expect(readOnlyDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    const readOnlyItems = readOnlyDecision.locator("[data-proof-block-content] > ul > li");
    await expect(readOnlyItems).toHaveCount(2);
    await expect(readOnlyItems.filter({ hasText: "A: first" })).toBeVisible();
    await expect(readOnlyItems.nth(1)).toHaveText("");
    await expect(readOnlyItems.nth(1)).toHaveCSS("display", "none");
  } finally {
    await alice.close();
  }
});

// Typing a decision's first option goes through a blank item: Enter after the question, then
// `- `, leaves an option without a label, so the decision is malformed until its label is typed.
// The blank item holds the caret, so it shows, and the label typed next lands in it, not in the
// question.
test("typing a new option into a decision with no options stores it as a typed option", async ({
  browser,
}) => {
  const { alice, issue, page } = await openWithCaret(
    browser,
    "Typed option",
    "Intro.\n\n:::ask{#a1}\nWhich one?\n:::\n\nAfter.\n",
    "Which one?",
    "end"
  );
  try {
    await page.keyboard.press("Enter");
    await page.keyboard.type("- ");
    await page.keyboard.type("Option A");

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toMatch(/\nWhich one\?\n\n- Option A\n:::\n/);
  } finally {
    await alice.close();
  }
});
