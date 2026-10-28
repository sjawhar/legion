import { expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, editArtifact, getIssue } from "./api";
import { documentEditor } from "./editor";
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

test("a comment's callout shows the document's header with its kind and title", async ({
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

test("a comment's ask block shows the decision card", async ({ browser }) => {
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

// A reply under a document's decision is a text body drawn inside that decision's card. The
// decision folds its own source option list once its card's rows show the options; that fold
// must not reach the typed blocks a reply holds: a callout's bullet list is what its author
// wrote, and a malformed decision in the reply has no rows, so its list is the only place its
// options show.
test("a reply under a document's decision keeps the lists of the typed blocks it holds", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext.\n",
    title: "Reply under a decision",
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
  await expect.poll(async () => (await getIssue(issue.key)).open_asks.length).toBe(1);
  const [storage] = (await getIssue(issue.key)).open_asks;
  if (storage === undefined) {
    throw new Error("the storage decision was not indexed");
  }
  await createComment(
    issue.key,
    {
      ask_id: storage.id,
      body:
        ':::callout{kind="warning" title="Steps first"}\n- step one\n- step two\n:::\n\n' +
        ":::ask\nBroken inner?\n\n- Fine: ok\n- : blank\n:::\n",
    },
    agent
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decision = documentEditor(page).locator('[data-dispatch-ask-block="storage"]');
    const thread = decision.getByTestId(`thread-${storage.id}`);
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
