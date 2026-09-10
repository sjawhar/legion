import { expect, type Page, test } from "@playwright/test";

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
import { resetDatabase } from "./seed";
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
  await expect(mark).toBeVisible({ timeout: 1000 });
  await expect(mark).toHaveText(quote, { timeout: 1000 });
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the selection bar comments, suggests, and asks on marks that both users see within a second", async ({
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
    await expect(
      alicePage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
    await expect(
      bobPage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");

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
    await expect(documentEditor(bobPage).locator("span[data-proof][data-id]")).toHaveCount(0, {
      timeout: 1000,
    });

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
    await commentComposer.getByRole("button", { exact: true, name: "Comment" }).click();
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
    await setSheet(bobPage, testInfo.project.name, true);
    await Promise.all([
      expect(marginCard(alicePage, comment.id)).toContainText("brown"),
      expect(marginCard(bobPage, comment.id)).toContainText("brown"),
    ]);
    await alicePage.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const deepLinkedComment = marginCard(alicePage, comment.id);
    if (testInfo.project.name === "iphone") {
      await expect(alicePage.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
      await expect(deepLinkedComment).toBeVisible();
    } else {
      await expect(deepLinkedComment).toBeInViewport();
    }
    await alicePage.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(alicePage)).toContainText(initialMarkdown);

    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "quick");
    await barAction(alicePage, "Suggest");
    const suggestionComposer = alicePage.getByRole("form", { name: "Suggest composer" });
    await suggestionComposer.getByLabel("Replacement").fill("red");
    await suggestionComposer.getByRole("button", { exact: true, name: "Suggest" }).click();
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
    const askComposer = alicePage.getByRole("form", { name: "Ask composer" });
    await askComposer.getByLabel("Question").fill("Why fox?");
    await askComposer.getByRole("button", { exact: true, name: "Ask" }).click();
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
    await expect(
      alicePage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
    await expect(
      bobPage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");

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

test("highlights follow edits in the other browser and orphan to their original version when the text is deleted", async ({
  browser,
}, testInfo) => {
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
    await expect(
      alicePage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
    await expect(
      bobPage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
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
    await expect(bobEditor).toContainText("Note:", { timeout: 1000 });
    await expectMark(bobPage, brown.anchor.mark_id, "brown");
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then((items) =>
          items.find((item) => item.id === brown.id)
        )
      )
      .toMatchObject({ anchor: { orphaned: false, quote: "brown" } });

    await deleteEditorText(alicePage, "fox");
    await expect(bobEditor).not.toContainText("fox", { timeout: 1000 });
    await expect
      .poll(
        () =>
          listComments(issue.key, artifactId).then((items) =>
            items.find((item) => item.id === fox.id)
          ),
        { timeout: 10_000 }
      )
      .toMatchObject({ anchor: { orphaned: true, version: 1 } });

    await setSheet(bobPage, testInfo.project.name, true);
    const orphanedCard = marginCard(bobPage, fox.id);
    await expect(orphanedCard).toContainText("Text changed");
    await orphanedCard.getByRole("link", { name: "View original text" }).click();
    const versionView = bobPage.getByRole("region", { name: "Document version 1" });
    await expect(versionView.locator(`[data-id="${fox.id}"]`)).toHaveText("fox");
    await expect(versionView.locator(".dispatch-mark-pulse")).toHaveCount(1);
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
    await expect(
      alicePage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
    await expect(
      bobPage.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");

    await selectEditorText(bobPage, "brown");
    await barAction(bobPage, "Suggest");
    const acceptComposer = bobPage.getByRole("form", { name: "Suggest composer" });
    await acceptComposer.getByLabel("Replacement").fill("red");
    await acceptComposer.getByRole("button", { exact: true, name: "Suggest" }).click();
    const accepted = await commentWithBody(issue.key, artifactId, "Suggested replacement.");
    if (accepted.anchor === null) {
      throw new Error("The accepted suggestion has no anchor.");
    }

    await setSheet(alicePage, testInfo.project.name, true);
    await marginCard(alicePage, accepted.id).getByRole("button", { name: "Accept" }).click();
    await Promise.all([
      expect(aliceEditor).toContainText("The quick red fox", { timeout: 1000 }),
      expect(bobEditor).toContainText("The quick red fox", { timeout: 1000 }),
    ]);
    await Promise.all([
      expect(markSpan(alicePage, accepted.anchor.mark_id)).toHaveCount(0, { timeout: 1000 }),
      expect(markSpan(bobPage, accepted.anchor.mark_id)).toHaveCount(0, { timeout: 1000 }),
    ]);
    await expect
      .poll(() =>
        getArtifact(artifactId).then(({ versions }) => versions.some((version) => version.named))
      )
      .toBe(true);

    await setSheet(bobPage, testInfo.project.name, false);
    await selectEditorText(bobPage, "quick");
    await barAction(bobPage, "Suggest");
    const rejectComposer = bobPage.getByRole("form", { name: "Suggest composer" });
    await rejectComposer.getByLabel("Replacement").fill("slow");
    await rejectComposer.getByRole("button", { exact: true, name: "Suggest" }).click();
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

    await setSheet(alicePage, testInfo.project.name, true);
    await marginCard(alicePage, rejected.id).getByRole("button", { name: "Reject" }).click();
    await Promise.all([
      expect(aliceEditor).toContainText("The quick red fox", { timeout: 1000 }),
      expect(bobEditor).toContainText("The quick red fox", { timeout: 1000 }),
      expect(markSpan(alicePage, rejected.anchor.mark_id)).toHaveCount(0, { timeout: 1000 }),
      expect(markSpan(bobPage, rejected.anchor.mark_id)).toHaveCount(0, { timeout: 1000 }),
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

test("a comment from the Log with nothing selected reaches the Log and margin of both users", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "LOG", name: "Log comments" });
  const issue = await createIssue({
    project: "LOG",
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
      alicePage.goto(`/issues/${issue.key}/log`),
      bobPage.goto(`/issues/${issue.key}/log`),
    ]);

    const composer = alicePage.getByRole("form", { name: "Comment composer" });
    await expect(composer).toBeVisible();
    await composer.getByLabel("Comment").fill("General remark");
    await composer.getByRole("button", { exact: true, name: "Comment" }).click();
    await setSheet(alicePage, testInfo.project.name, true);
    await setSheet(bobPage, testInfo.project.name, true);
    await expect(marginCard(alicePage, anchored.id)).toContainText("anchored first");
    await expect(marginCard(bobPage, anchored.id)).toContainText("anchored first");
    const comment = await commentWithBody(issue.key, undefined, "General remark");
    expect(comment.anchor).toBeNull();

    await Promise.all([
      expect(
        alicePage.locator("article[data-event-seq]").getByText("General remark", { exact: true })
      ).toBeVisible(),
      expect(
        bobPage.locator("article[data-event-seq]").getByText("General remark", { exact: true })
      ).toBeVisible({ timeout: 1000 }),
      expect(marginCard(alicePage, comment.id)).toBeVisible(),
      expect(marginCard(bobPage, comment.id)).toBeVisible({ timeout: 1000 }),
    ]);
    await expect(marginCard(bobPage, comment.id).locator("blockquote")).toHaveCount(0);
    const marginOrder = await bobPage
      .getByLabel("Margin review items")
      .locator("[data-margin-item]")
      .evaluateAll((items) => items.map((item) => item.getAttribute("data-margin-item")));
    expect(marginOrder.indexOf(anchored.id)).toBeLessThan(marginOrder.indexOf(comment.id));
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("margin cards and document highlights focus each other", async ({ browser }, testInfo) => {
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
    await expect(
      page.getByRole("region", { name: "Document editor" }).getByRole("status")
    ).toHaveText("connected");
    const comment = await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "brown" }, body: "focus this" },
      session
    );
    if (comment.anchor === null) {
      throw new Error("The focus comment has no anchor.");
    }
    const span = markSpan(page, comment.anchor.mark_id);
    const card = marginCard(page, comment.id);
    await expectMark(page, comment.anchor.mark_id, "brown");

    if (testInfo.project.name === "chromium") {
      await span.hover();
      await expect(card).toHaveAttribute("aria-current", "true");
      await card.hover();
      await expect(span).toHaveClass(/dispatch-mark-active/);
    }

    await span.click();
    if (testInfo.project.name === "iphone") {
      await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    }
    await expect(card).toHaveAttribute("aria-current", "true");
    await card.click();
    await expect(span).toHaveClass(/dispatch-mark-pulse/, { timeout: 300 });
  } finally {
    await alice.close();
  }
});

test("a reply to an agent's anchored comment carries no anchor and lands in the same thread", async ({
  browser,
}, testInfo) => {
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
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await setSheet(alicePage, testInfo.project.name, true);
    await setSheet(bobPage, testInfo.project.name, true);

    await marginCard(alicePage, root.id).getByRole("button", { name: "Reply" }).click();
    const composer = alicePage.getByRole("form", { name: "Comment composer" });
    await expect(composer.locator("blockquote")).toHaveCount(0);
    await composer.getByLabel("Comment").fill("ok");
    await composer.getByRole("button", { exact: true, name: "Comment" }).click();
    const reply = await commentWithBody(issue.key, undefined, "ok");
    expect(reply.reply_to).toBe(root.id);
    expect(reply.anchor).toBeNull();
    const aliceReply = marginCard(alicePage, reply.id);
    const bobReply = marginCard(bobPage, reply.id);
    await Promise.all([
      expect(aliceReply).toContainText("ok"),
      expect(bobReply).toContainText("ok", { timeout: 1000 }),
    ]);
    await Promise.all([
      expect(aliceReply).toHaveCSS("margin-left", "12px"),
      expect(bobReply).toHaveCSS("margin-left", "12px"),
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

    const composer = page.getByRole("form", { name: "Ask composer" });
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
    await composer.getByRole("button", { exact: true, name: "Ask" }).click();

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
    await inboxCard.getByRole("button", { name: "Submit answer" }).click();
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
    await aliceCard.getByLabel("Your answer").fill("Because it is precise.");
    await aliceCard.getByRole("button", { name: "Submit answer" }).click();
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
    await expect(bobCard).toContainText("alice answered");
    await expect(bobCard).toContainText("Because it is precise.");
    await expect(bobCard.locator("time")).toHaveAttribute("datetime", /.+/);
    await bobPage.screenshot({
      path: testInfo.outputPath("fresh-viewer-answered-anchored-ask.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
    await bob.close();
  }
});
