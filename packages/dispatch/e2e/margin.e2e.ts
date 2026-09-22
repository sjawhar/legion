import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getArtifact,
  getAsk,
  listComments,
} from "./api";
import {
  actionBar,
  barAction,
  deleteEditorText,
  documentEditor,
  marginCard,
  markSpan,
  selectEditorText,
} from "./editor";
import { resetDatabase, setCommentAuthorService } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-margin", origin: { tmux: "dispatch:1.4" } },
  as: "agent" as const,
};
const initialMarkdown = "The quick brown fox";

// On the phone layout the margin is a bottom sheet over the document. Acting on a selection
// opens it; close it again before selecting another range, as a person would.
async function setSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const sheet = page.getByTestId("margin-sheet");
  if ((await sheet.getAttribute("data-expanded")) !== String(open)) {
    if (open) {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    } else {
      await page.mouse.click(1, 1);
    }
  }
  await expect(sheet).toHaveAttribute("data-expanded", String(open));
}

async function expandedConversationThread(page: Page, rootId: string): Promise<Locator> {
  const turn = page.locator(`[data-turn="comment:${rootId}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  const phoneThread = page.getByRole("dialog", { name: "Thread" });
  return (await phoneThread.count()) === 0 ? turn : phoneThread;
}

async function commentWithBody(issueKey: string, artifactId: string | undefined, body: string) {
  await expect
    .poll(() =>
      listComments(issueKey, artifactId).then((items) => items.find((item) => item.body === body))
    )
    .toBeDefined();
  const comment = (await listComments(issueKey, artifactId)).find((item) => item.body === body);
  if (comment === undefined) {
    throw new Error(`Comment with body ${body} was not created.`);
  }
  return comment;
}

async function expectMark(page: Page, markId: string, quote: string): Promise<void> {
  const mark = markSpan(page, markId);
  await expect(mark).toBeVisible();
  await expect(mark).toHaveText(quote);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the selection bar comments, suggests, and asks on marks that both users see", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Review the spec",
  });
  const relatedIssue = await createIssue({ project: "CORE", title: "Related work" });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await expect(documentEditor(alicePage)).toContainText(initialMarkdown);
    await expect(documentEditor(bobPage)).toContainText(initialMarkdown);
    await expect(alicePage.getByRole("status", { name: "connected" })).toHaveText("connected");
    await expect(bobPage.getByRole("status", { name: "connected" })).toHaveText("connected");

    // Cancelling a mark composer rejects the editor action and removes the provisional mark.
    await selectEditorText(alicePage, "brown");
    await expect(actionBar(alicePage).getByRole("button")).toHaveText([
      "Comment",
      "Suggest",
      "Ask",
    ]);
    await barAction(alicePage, "Comment");
    const cancelledComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await expect(cancelledComposer).toContainText("brown");
    await alicePage.keyboard.press("Escape");
    await expect(documentEditor(alicePage).locator("span[data-proof][data-id]")).toHaveCount(0);
    await expect(documentEditor(bobPage).locator("span[data-proof][data-id]")).toHaveCount(0);

    await selectEditorText(alicePage, "brown");
    await barAction(alicePage, "Comment");
    const commentComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await commentComposer.getByLabel("Comment").press("Control+k");
    await expect(
      commentComposer.getByRole("button", { name: `${relatedIssue.key}: Related work` })
    ).toBeVisible();
    await alicePage.keyboard.press("Escape");
    await expect(alicePage.getByRole("dialog", { name: "Reference picker" })).toHaveCount(0);
    await commentComposer.getByLabel("Comment").fill("why?");
    await commentComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const comment = await commentWithBody(issue.key, artifactId, "why?");
    if (comment.anchor === null) {
      throw new Error("The selection-bar comment has no anchor.");
    }
    expect(typeof comment.anchor.mark_id).toBe("string");
    expect(comment.anchor.quote).toBe("brown");
    await Promise.all([
      expectMark(alicePage, comment.anchor.mark_id, "brown"),
      expectMark(bobPage, comment.anchor.mark_id, "brown"),
    ]);
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/conversation`),
      bobPage.goto(`/issues/${issue.key}/conversation`),
    ]);
    await Promise.all([
      expect(alicePage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText("brown"),
      expect(bobPage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText("brown"),
      expect(alicePage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText("why?"),
      expect(bobPage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText("why?"),
    ]);
    await alicePage.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const deepLinkedComment = alicePage.locator(
      `li[data-turn="comment:${comment.id}"][aria-current="true"]`
    );
    await expect(deepLinkedComment).toBeInViewport();
    await alicePage.goto(`/issues/${issue.key}/spec`);
    await bobPage.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(alicePage)).toContainText(initialMarkdown);

    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "quick");
    await barAction(alicePage, "Suggest");
    const suggestionComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await suggestionComposer.getByLabel("Replacement").fill("red");
    await suggestionComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const suggestion = await commentWithBody(issue.key, artifactId, "Suggested replacement.");
    if (suggestion.anchor === null) {
      throw new Error("The selection-bar suggestion has no anchor.");
    }
    expect(suggestion.anchor.quote).toBe("quick");
    await Promise.all([
      expectMark(alicePage, suggestion.anchor.mark_id, "quick"),
      expectMark(bobPage, suggestion.anchor.mark_id, "quick"),
    ]);

    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "fox");
    await barAction(alicePage, "Ask");
    const askComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await askComposer.getByLabel("Question").fill("Why fox?");
    await askComposer.locator('button[type="submit"]').click();
    const askCard = alicePage
      .getByRole("region", { name: "Needs you" })
      .locator("[data-margin-item]")
      .filter({ hasText: "Why fox?" });
    await expect(askCard).toBeVisible();
    const askId = await askCard.getAttribute("data-margin-item");
    if (askId === null) {
      throw new Error("The selection-bar ask has no margin id.");
    }
    const ask = await getAsk(askId);
    if (ask.ask.anchor === null) {
      throw new Error("The selection-bar ask has no anchor.");
    }
    expect(ask.ask.anchor.quote).toBe("fox");
    await Promise.all([
      expectMark(alicePage, ask.ask.anchor.mark_id, "fox"),
      expectMark(bobPage, ask.ask.anchor.mark_id, "fox"),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("an agent's quote-anchored comment and ask render as highlights in open editors", async ({
  browser,
}) => {
  await createProject({ key: "AGENT", name: "Agent marks" });
  const issue = await createIssue({
    project: "AGENT",
    spec: initialMarkdown,
    title: "Agent marks",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await expect(alicePage.getByRole("status", { name: "connected" })).toHaveText("connected");
    await expect(bobPage.getByRole("status", { name: "connected" })).toHaveText("connected");

    const comment = await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "brown" }, body: "agent comment" },
      session
    );
    const ask = await createAsk(
      issue.key,
      { anchor: { artifact: "spec", quote: "fox" }, question: "agent ask" },
      session
    );
    if (comment.anchor === null || ask.anchor === null) {
      throw new Error("The agent-created review items have no anchors.");
    }
    await Promise.all([
      expectMark(alicePage, comment.anchor.mark_id, "brown"),
      expectMark(bobPage, comment.anchor.mark_id, "brown"),
      expectMark(alicePage, ask.anchor.mark_id, "fox"),
      expectMark(bobPage, ask.anchor.mark_id, "fox"),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("highlights follow edits in the other browser and preserve their anchor state", async ({
  browser,
}) => {
  await createProject({ key: "FOLLOW", name: "Following highlights" });
  const issue = await createIssue({
    project: "FOLLOW",
    spec: initialMarkdown,
    title: "Follow edits",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const aliceEditor = documentEditor(alicePage);
    const bobEditor = documentEditor(bobPage);
    await expect(alicePage.getByRole("status", { name: "connected" })).toHaveText("connected");
    await expect(bobPage.getByRole("status", { name: "connected" })).toHaveText("connected");
    const brown = await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "brown" }, body: "brown note" },
      session
    );
    const fox = await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "fox" }, body: "fox note" },
      session
    );
    if (brown.anchor === null || fox.anchor === null) {
      throw new Error("The following-highlight comments have no anchors.");
    }
    await Promise.all([
      expectMark(alicePage, brown.anchor.mark_id, "brown"),
      expectMark(bobPage, brown.anchor.mark_id, "brown"),
      expectMark(alicePage, fox.anchor.mark_id, "fox"),
      expectMark(bobPage, fox.anchor.mark_id, "fox"),
    ]);

    await aliceEditor.click();
    await alicePage.keyboard.press("Control+Home");
    await alicePage.keyboard.type("Note: ");
    await expect(bobEditor).toContainText("Note:");
    await expectMark(bobPage, brown.anchor.mark_id, "brown");
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then((items) =>
          items.find((item) => item.id === brown.id)
        )
      )
      .toMatchObject({ anchor: { orphaned: false, quote: "brown" } });

    await deleteEditorText(alicePage, "fox");
    await expect(bobEditor).not.toContainText("fox");
    await expect
      .poll(
        () =>
          listComments(issue.key, artifactId).then((items) =>
            items.find((item) => item.id === fox.id)
          ),
        { timeout: 10_000 }
      )
      .toMatchObject({ anchor: { orphaned: true, version: 1 } });
    await bobPage.goto(`/issues/${issue.key}/comments/${fox.id}`);
    const orphanedTurn = bobPage.locator(`li[data-turn="comment:${fox.id}"][aria-current="true"]`);
    await orphanedTurn.getByRole("button", { name: "Expand thread" }).click();
    const threadDialog = bobPage.getByRole("dialog", { name: "Thread" });
    const expandedTurn = (await threadDialog.count()) === 0 ? orphanedTurn : threadDialog;
    await expect(expandedTurn).toContainText("Text changed.");
    await expandedTurn.getByRole("link", { name: "View original text" }).click();
    const versionView = bobPage.getByRole("region", { name: "Document version 1" });
    await expect(versionView.locator(`[data-id="${fox.id}"]`)).toHaveText("fox");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("accepting a suggestion changes the text in both browsers and names a version; rejecting leaves the text", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SUG", name: "Suggestions" });
  const issue = await createIssue({
    project: "SUG",
    spec: initialMarkdown,
    title: "Suggestion actions",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const aliceEditor = documentEditor(alicePage);
    const bobEditor = documentEditor(bobPage);
    await expect(alicePage.getByRole("status", { name: "connected" })).toHaveText("connected");
    await expect(bobPage.getByRole("status", { name: "connected" })).toHaveText("connected");

    await selectEditorText(bobPage, "brown");
    await barAction(bobPage, "Suggest");
    const acceptComposer = bobPage.getByRole("form", { name: "Comment composer" });
    await acceptComposer.getByLabel("Replacement").fill("red");
    await acceptComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const accepted = await commentWithBody(issue.key, artifactId, "Suggested replacement.");
    if (accepted.anchor === null) {
      throw new Error("The accepted suggestion has no anchor.");
    }

    await alicePage.goto(`/issues/${issue.key}/conversation`);
    await alicePage
      .locator(`[data-turn="comment:${accepted.id}"]`)
      .getByTestId(`margin-comment-${accepted.id}`)
      .getByRole("button", { name: "Accept suggestion" })
      .click();
    await expect(
      alicePage
        .locator(`[data-turn="comment:${accepted.id}"]`)
        .getByTestId(`margin-comment-${accepted.id}`)
    ).toHaveCount(0);
    await alicePage
      .getByRole("tabpanel", { name: "Conversation" })
      .getByRole("button", { name: "Resolved (1)" })
      .click();
    await expect(
      alicePage
        .locator(`[data-turn="comment:${accepted.id}"]`)
        .getByTestId(`margin-comment-${accepted.id}`)
        .getByRole("button", { name: "Reopen" })
    ).toHaveCount(0);
    await alicePage.goto(`/issues/${issue.key}/spec`);
    await Promise.all([
      expect(aliceEditor).toContainText("The quick red fox"),
      expect(bobEditor).toContainText("The quick red fox"),
    ]);
    await Promise.all([
      expect(markSpan(alicePage, accepted.anchor.mark_id)).toHaveCount(0),
      expect(markSpan(bobPage, accepted.anchor.mark_id)).toHaveCount(0),
    ]);
    await expect
      .poll(() =>
        getArtifact(artifactId).then(({ versions }) => versions.some((version) => version.named))
      )
      .toBe(true);

    await setSheet(bobPage, testInfo.project.name, false);
    await selectEditorText(bobPage, "quick");
    await barAction(bobPage, "Suggest");
    const rejectComposer = bobPage.getByRole("form", { name: "Comment composer" });
    await rejectComposer.getByLabel("Replacement").fill("slow");
    const submitSuggestion = rejectComposer.getByRole("button", { exact: true, name: "Send" });
    await submitSuggestion.click();
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then((items) =>
          items.find((item) => item.suggestion?.replace_with === "slow")
        )
      )
      .toBeDefined();
    const rejected = (await listComments(issue.key, artifactId)).find(
      (item) => item.suggestion?.replace_with === "slow"
    );
    if (rejected === undefined || rejected.anchor === null) {
      throw new Error("The rejected suggestion was not created with an anchor.");
    }

    await alicePage.goto(`/issues/${issue.key}/conversation`);
    const rejectButton = alicePage
      .locator(`[data-turn="comment:${rejected.id}"]`)
      .getByTestId(`margin-comment-${rejected.id}`)
      .getByRole("button", { name: "Reject suggestion" });
    const [response] = await Promise.all([
      alicePage.waitForResponse(
        (response) =>
          response.url().endsWith(`/comments/${rejected.id}/reject`) &&
          response.request().method() === "POST"
      ),
      rejectButton.click(),
    ]);
    expect(response.ok()).toBe(true);
    await alicePage.goto(`/issues/${issue.key}/spec`);
    await Promise.all([
      expect(aliceEditor).toContainText("The quick red fox"),
      expect(bobEditor).toContainText("The quick red fox"),
      expect(markSpan(alicePage, rejected.anchor.mark_id)).toHaveCount(0),
      expect(markSpan(bobPage, rejected.anchor.mark_id)).toHaveCount(0),
    ]);
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then((items) =>
          items.find((item) => item.id === rejected.id)
        )
      )
      .toMatchObject({ resolved: true, suggestion: { accepted: false } });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("anchored and unanchored comments reach Conversation for both viewers", async ({
  browser,
}) => {
  await createProject({ key: "COMMENT", name: "Issue comments" });
  const issue = await createIssue({
    project: "COMMENT",
    spec: initialMarkdown,
    title: "No selection needed",
  });
  const anchored = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "brown" }, body: "anchored first" },
    session
  );
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/conversation`),
      bobPage.goto(`/issues/${issue.key}/conversation`),
    ]);
    await Promise.all([
      expect(alicePage.locator(`[data-turn="comment:${anchored.id}"]`)).toContainText(
        "anchored first"
      ),
      expect(bobPage.locator(`[data-turn="comment:${anchored.id}"]`)).toContainText(
        "anchored first"
      ),
      // The margin keeps the anchored thread beside the document too.
      expect(
        alicePage.getByTestId("margin-sheet").locator(`[data-margin-item="${anchored.id}"]`)
      ).toContainText("anchored first"),
      expect(
        bobPage.getByTestId("margin-sheet").locator(`[data-margin-item="${anchored.id}"]`)
      ).toContainText("anchored first"),
    ]);

    const comment = await createComment(issue.key, { body: "General remark" }, session);
    expect(comment.anchor).toBeNull();
    await Promise.all([
      expect(alicePage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText(
        "General remark"
      ),
      expect(bobPage.locator(`[data-turn="comment:${comment.id}"]`)).toContainText(
        "General remark"
      ),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("a document mark opens its thread in the margin and stays on the document", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "FOCUS", name: "Focus marks" });
  const issue = await createIssue({
    project: "FOCUS",
    spec: initialMarkdown,
    title: "Focus marks",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
    const comment = await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "brown" }, body: "focus this" },
      session
    );
    if (comment.anchor === null) {
      throw new Error("The focus comment has no anchor.");
    }
    const span = markSpan(page, comment.anchor.mark_id);
    await expectMark(page, comment.anchor.mark_id, "brown");
    await span.click();
    // Proof's model: the thread opens beside the document (in the phone's Thread dialog on a
    // small viewport); the reader never leaves the document.
    const phoneThread = page.getByRole("dialog", { name: "Thread" });
    const card =
      testInfo.project.name === "iphone"
        ? phoneThread.getByTestId(`margin-comment-${comment.id}`)
        : marginCard(page, comment.id);
    await expect(card).toHaveAttribute("aria-current", "true");
    await expect(card).toContainText("focus this");
    await expect(card).toContainText("brown");
    await expect(card).toBeInViewport();
    if (testInfo.project.name === "iphone") {
      await phoneThread.getByRole("button", { name: "Back" }).click();
      await expect(phoneThread).toHaveCount(0);
    } else {
      await expect(
        page.getByTestId("margin-sheet").getByRole("tab", { name: "Comments" })
      ).toHaveAttribute("aria-selected", "true");
    }
    await expect(page).toHaveURL(`/issues/${issue.key}/spec`);
    await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
    // The same comment is still one Conversation turn; the deep link still focuses it there.
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .locator(`li[data-turn="comment:${comment.id}"][aria-current="true"]`)
    ).toContainText("focus this");
  } finally {
    await alice.close();
  }
});

test("a Conversation reply to an agent's anchored comment carries no anchor and stays in one thread", async ({
  browser,
}) => {
  await createProject({ key: "REPLY", name: "Anchored replies" });
  const issue = await createIssue({
    project: "REPLY",
    spec: initialMarkdown,
    title: "Reply threads",
  });
  const root = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "brown" }, body: "agent root" },
    session
  );
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/conversation`),
      bobPage.goto(`/issues/${issue.key}/conversation`),
    ]);
    const aliceThread = await expandedConversationThread(alicePage, root.id);
    const composer = aliceThread.getByRole("form", { name: "Comment composer" });
    await composer.getByRole("textbox", { name: "Reply" }).fill("ok");
    await composer.getByRole("button", { name: "Send" }).click();
    const reply = await commentWithBody(issue.key, undefined, "ok");
    expect(reply.reply_to).toBe(root.id);
    expect(reply.anchor).toBeNull();
    const bobThread = await expandedConversationThread(bobPage, root.id);
    await Promise.all([
      expect(aliceThread).toContainText("ok"),
      expect(bobThread).toContainText("ok"),
    ]);
    await Promise.all([
      expect(aliceThread.getByText("ok").locator("..")).toHaveCSS("margin-left", "0px"),
      expect(bobThread.getByText("ok").locator("..")).toHaveCSS("margin-left", "0px"),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("margin ask composer sends option choices that the inbox records as a selected answer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "ASK", name: "Ask options" });
  const issue = await createIssue({
    project: "ASK",
    spec: initialMarkdown,
    title: "Choose a direction",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();
    await selectEditorText(page, "brown");
    await barAction(page, "Ask");

    const composer = page.getByRole("form", { name: "Comment composer" });
    await composer.getByLabel("Question").fill("Which direction should we take?");
    await composer.getByRole("button", { name: "High" }).click();
    await composer.getByLabel("Allow multiple").check();
    await composer.getByLabel("Option 1 label").fill("Ship");
    await composer.getByLabel("Option 1 description").fill("Proceed this week");
    await composer.getByRole("button", { name: "Add option" }).click();
    await composer.getByLabel("Option 2 label").fill("Hold");
    await page.screenshot({
      path: testInfo.outputPath("ask-composer-options.png"),
      fullPage: true,
    });
    await composer.getByRole("button", { exact: true, name: "Ask" }).last().click();

    const createdCard = page
      .getByRole("region", { name: "Needs you" })
      .locator("[data-margin-item]")
      .filter({ hasText: "Which direction should we take?" });
    const askId = await createdCard.getAttribute("data-margin-item");
    if (askId === null) {
      throw new Error("The created ask has no margin id.");
    }
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({
        ask: {
          multiple: true,
          options: [{ description: "Proceed this week", label: "Ship" }, { label: "Hold" }],
          urgency: "high",
        },
      });

    await page.goto("/");
    const inboxCard = page.getByTestId(`ask-${askId}`);
    await inboxCard.getByRole("checkbox", { name: "Ship" }).check();
    await inboxCard.getByRole("button", { name: "Answer" }).click();
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({
        ask: { answer: { selected: ["Ship"], user: "alice" }, state: "answered" },
      });
  } finally {
    await alice.close();
  }
});

test("a viewer who opens the issue after an anchored ask is answered sees it in the margin", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SEES", name: "Sees answered asks" });
  const issue = await createIssue({
    project: "SEES",
    spec: initialMarkdown,
    title: "Answered ask visibility",
  });
  const ask = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "brown" },
      question: "Why brown?",
    },
    session
  );
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    await alicePage.goto(`/issues/${issue.key}`);
    await alicePage.getByRole("tab", { name: "Spec" }).click();
    await setSheet(alicePage, testInfo.project.name, true);
    const aliceCard = marginCard(alicePage, ask.id);
    await expect(aliceCard).toContainText("brown");
    await aliceCard.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    await aliceCard.getByLabel("Your answer").fill("Because it is precise.");
    await aliceCard.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect.poll(() => getAsk(ask.id)).toMatchObject({ ask: { state: "answered" } });
    await setSheet(alicePage, testInfo.project.name, false);

    // Bob's browser context has never had this issue open: no inbox history, no prior query
    // cache. He still sees the answered anchored ask in the margin.
    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}`);
    await bobPage.getByRole("tab", { name: "Spec" }).click();
    await setSheet(bobPage, testInfo.project.name, true);
    const bobCard = marginCard(bobPage, ask.id);
    await expect(bobCard).toBeVisible();
    await expect(bobCard).toContainText("brown");
    await expect(bobCard).toContainText("Why brown?");
    await expect(bobCard).toContainText("Answered by alice");
    await expect(bobCard).toContainText("Because it is precise.");
    const timestamps = bobCard.locator("time");
    await expect(timestamps).toHaveCount(2);
    for (const timestamp of await timestamps.all()) {
      await expect(timestamp).toHaveAttribute("datetime", /.+/);
    }
    await bobPage.screenshot({
      path: testInfo.outputPath("fresh-viewer-answered-anchored-ask.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
    await bob.close();
  }
});

test("long option labels and descriptions wrap inside the margin ask card", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Severity table shape",
  });
  const matrix =
    "one table per model: a row per published result, a column per selected scorer, cells the score with its confidence interval";
  const perResult =
    "one small table per published result (rows = selected scorers, columns = the models in that engagement)";
  // A single word wider than the card: a fieldset's UA `min-inline-size: min-content` would let
  // it widen the whole option list past the card unless the fieldset is `min-w-0`.
  const unbroken =
    "Supercalifragilisticexpialidocious_unbroken_identifier_that_is_very_long_indeed";
  const ask = await createAsk(
    issue.key,
    {
      options: [
        {
          description:
            "Wrong grain: a publish is per engagement and a scorer setting is per engagement, so a per-model table repeats the same selection on every row and hides which engagement chose it.",
          label: matrix,
        },
        {
          description:
            "Cheapest; every new engagement needs an engineer to add its scorers, and the page grows one table per published result.",
          label: perResult,
        },
        { label: unbroken },
      ],
      question:
        "Slice 2, decision 5 — the endpoint report's severity table shape. Recommendation: the matrix — the selection is small by construction.",
    },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await setSheet(page, testInfo.project.name, true);
    const card = page.getByRole("region", { name: "Needs you" }).getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("severity table shape");
    await expect(
      card.getByRole("button", { name: "Add a note or answer in your own words" })
    ).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`margin-long-options-${testInfo.project.name}.png`),
    });

    // The same rows the Inbox renders: radios named by label and description, each one inside
    // the card's right edge and exactly as tall as its wrapped text plus the row padding - the
    // failure this guards against is a control whose box grows beyond the words in it.
    const options = card.getByRole("group", { name: "Answer options" });
    await expect(options.getByRole("radio")).toHaveCount(4);
    const cardBox = await card.boundingBox();
    if (cardBox === null) throw new Error("the ask card has no layout box");
    const rows = await options.locator("label").all();
    expect(rows).toHaveLength(4);
    for (const row of rows) {
      const [box, textBox] = await Promise.all([
        row.boundingBox(),
        row.locator("> span").boundingBox(),
      ]);
      if (box === null || textBox === null) throw new Error("an option row has no layout box");
      expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width);
      expect(textBox.x + textBox.width).toBeLessThanOrEqual(box.x + box.width);
      // A row is its text plus 24 px of padding, never shorter than the 44 px touch minimum.
      expect(box.height).toBeLessThanOrEqual(Math.max(44, textBox.height + 24));
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);

    await options.getByRole("radio", { name: /one small table per published result/ }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect
      .poll(() => getAsk(ask.id))
      .toMatchObject({
        ask: { answer: { selected: [perResult], user: "alice" }, state: "answered" },
      });
  } finally {
    await alice.close();
  }
});

test("a comment a verified service token wrote names its service account in the margin", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SVC", name: "Service" });
  const issue = await createIssue({
    project: "SVC",
    spec: "Rotate the projected token.",
    title: "Service-token attribution",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "projected token" }, body: "Rotated it." },
    {
      actor: {
        id: "legion-worker-session",
        kind: "session",
        origin: { session_title: "Implementer" },
      },
      as: "agent",
    }
  );
  // The server never accepts a body-supplied service subject, so the fixture is the persisted
  // actor a verified token would have written.
  await setCommentAuthorService(comment.id, "system:serviceaccount:legion:legion-worker");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await setSheet(page, testInfo.project.name, true);
    // Only an expanded thread carries the author line under each comment.
    await marginCard(page, comment.id).locator('button[aria-expanded="false"]').click();
    const phoneThread = page.getByRole("dialog", { name: "Thread" });
    const thread =
      (await phoneThread.count()) === 0
        ? marginCard(page, comment.id)
        : phoneThread.getByTestId(`margin-comment-${comment.id}`);
    await expect(thread).toContainText("Implementer (as legion-worker)");
  } finally {
    await alice.close();
  }
});
