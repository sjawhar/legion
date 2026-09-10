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
import { selectPreviewText } from "./preview";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-margin", origin: { tmux: "dispatch:1.4" } },
  as: "agent" as const,
};
const initialMarkdown = "The quick brown fox";

// On the phone layout the margin is a bottom sheet over the rendered document. Acting on a
// selection opens it; the test closes it again before returning to the document, as a user would.
async function setSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const toggle = page.getByRole("button", {
    name: open ? /Open review panel/ : /Close review panel/,
  });
  if ((await toggle.count()) > 0) {
    await toggle.click();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("margin creates and preserves anchored review items", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Review the spec",
  });
  const relatedIssue = await createIssue({ project: "CORE", title: "Related work" });
  const artifactId = issue.primary_artifact_id;
  const foxComment = await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "fox" },
      body: "fox note",
    },
    session
  );
  const ask = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "brown" },
      question: "Why brown?",
    },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();
    const article = page.getByRole("tabpanel", { name: "Spec" }).getByRole("article");
    await expect(article).toContainText(initialMarkdown);
    await expect(page.locator('[aria-label="Selection actions"]')).toHaveCount(0);

    await selectPreviewText(page, "brown");
    await page.getByRole("button", { exact: true, name: "Comment" }).click();
    const commentComposer = page.getByRole("form", { name: "Comment composer" });
    await commentComposer.getByLabel("Comment").press("Control+k");
    await expect(
      commentComposer.getByRole("button", { name: `${relatedIssue.key}: Related work` })
    ).toBeVisible();
    // The picker is a real dismissible dialog now; close it before continuing the draft.
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Reference picker" })).toHaveCount(0);
    await commentComposer.getByLabel("Comment").fill("why?");
    await commentComposer.getByRole("button", { exact: true, name: "Comment" }).click();
    await expect
      .poll(async () =>
        (await listComments(issue.key, artifactId)).find(({ body }) => body === "why?")
      )
      .toBeDefined();
    const comment = (await listComments(issue.key, artifactId)).find(({ body }) => body === "why?");
    if (comment === undefined) {
      throw new Error("The anchored comment was not created.");
    }
    if (comment.anchor === null) {
      throw new Error("The anchored comment has no anchor.");
    }
    expect(typeof comment.anchor.mark_id).toBe("string");
    await expect(page.getByTestId(`margin-comment-${comment.id}`)).toContainText("brown");
    await expect(page.getByTestId(`margin-comment-${foxComment.id}`)).toContainText("fox");
    await page.screenshot({
      path: testInfo.outputPath("rendered-document-margin.png"),
      fullPage: true,
    });
    expect(comment.anchor).toMatchObject({ quote: "brown", version: 1 });
    await setSheet(page, testInfo.project.name, false);
    await article.scrollIntoViewIfNeeded();

    await selectPreviewText(page, "brown");
    await page.getByRole("button", { exact: true, name: "Suggest" }).click();
    const suggestionComposer = page.getByRole("form", { name: "Suggest composer" });
    await suggestionComposer.getByLabel("Replacement").fill("red");
    await suggestionComposer.getByRole("button", { exact: true, name: "Suggest" }).click();
    await expect
      .poll(async () =>
        (await listComments(issue.key, artifactId)).find(
          ({ suggestion }) => suggestion?.replace_with === "red"
        )
      )
      .toBeDefined();
    const suggestion = (await listComments(issue.key, artifactId)).find(
      ({ suggestion: value }) => value?.replace_with === "red"
    );
    if (suggestion === undefined) {
      throw new Error("The anchored suggestion was not created.");
    }
    const versionsBeforeAccept = (await getArtifact(artifactId)).versions.length;
    await page
      .getByTestId(`margin-comment-${suggestion.id}`)
      .getByRole("button", { name: "Accept" })
      .click();
    await expect(article).toContainText("The quick red fox");
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then((items) =>
          items.find(({ id }) => id === suggestion.id)
        )
      )
      .toMatchObject({ resolved: true, suggestion: { accepted: true } });
    await expect
      .poll(() => getArtifact(artifactId).then(({ versions }) => versions.length))
      .toBeGreaterThan(versionsBeforeAccept);
    // A long version summary must never widen the layout viewport (it breaks every tap on a phone).
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
    await setSheet(page, testInfo.project.name, false);

    await page.goto(`/issues/${issue.key}`);
    await setSheet(page, testInfo.project.name, true);
    const askCard = page.getByRole("region", { name: "Needs you" }).getByTestId(`ask-${ask.id}`);
    await expect(askCard).toContainText("brown");
    await askCard.getByLabel("Your answer").fill("Because it is precise.");
    await askCard.getByRole("button", { name: "Submit answer" }).click();
    await expect.poll(() => getAsk(ask.id)).toMatchObject({ ask: { state: "answered" } });
    await page.goto("/");
    await expect(page.getByTestId(`ask-${ask.id}`)).toHaveCount(0);

    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const margin = page.getByTestId("margin-sheet");
    if (testInfo.project.name === "iphone") {
      await expect(page.getByRole("button", { name: /Close review panel/ })).toBeVisible();
      await expect(page.getByTestId(`margin-comment-${comment.id}`)).toBeVisible();
      await page.getByRole("button", { name: /Close review panel/ }).click();
      await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();
    } else {
      await expect.poll(() => margin.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    }
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByRole("tabpanel", { name: "Spec" }).getByRole("article")).toContainText(
      "The quick red fox"
    );
    await page.goto(`/issues/${issue.key}/artifact/spec`);
    await expect(page.getByRole("tabpanel", { name: "Spec" }).getByRole("article")).toContainText(
      "The quick red fox"
    );
    await expect(page.getByTestId(`margin-comment-${comment.id}`)).toContainText("why?");
  } finally {
    await alice.close();
  }
});

test("margin anchors a whole-paragraph selection made in the rendered preview", async ({
  browser,
}) => {
  await createProject({ key: "PREV", name: "Preview" });
  const issue = await createIssue({
    project: "PREV",
    spec: initialMarkdown,
    title: "Preview selection",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();
    const paragraph = page.getByRole("article").locator("p").first();
    await expect(paragraph).toContainText(initialMarkdown);

    // A drag that overshoots the first or last glyph of a block - or a native "Select All" -
    // resolves its Range boundaries onto the block's parent element (an offset into its
    // children) rather than into the text node itself. Native multi-click timing is unreliable
    // across Chromium builds, so drive that exact boundary shape directly and let the app's own
    // mouseup handler read it, the same way a real overshoot selection would arrive.
    await page.evaluate(() => {
      const article = document.querySelector("article");
      if (article === null) {
        throw new Error("Expected an article element in the rendered document.");
      }
      const range = document.createRange();
      range.selectNodeContents(article);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      article.dispatchEvent(new MouseEvent("mouseup", { bubbles: true }));
    });

    await page.getByRole("button", { exact: true, name: "Comment" }).click();
    const commentComposer = page.getByRole("form", { name: "Comment composer" });
    await commentComposer.getByLabel("Comment").fill("anchors the whole paragraph");
    await commentComposer.getByRole("button", { exact: true, name: "Comment" }).click();
    await expect
      .poll(async () =>
        (await listComments(issue.key, artifactId)).find(
          ({ body }) => body === "anchors the whole paragraph"
        )
      )
      .toBeDefined();
    const comment = (await listComments(issue.key, artifactId)).find(
      ({ body }) => body === "anchors the whole paragraph"
    );
    if (comment === undefined) {
      throw new Error("The preview-mode anchored comment was not created.");
    }
    expect(comment.anchor).toMatchObject({ quote: initialMarkdown, version: 1 });
  } finally {
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
    await selectPreviewText(page, "brown");
    await page.getByRole("button", { exact: true, name: "Ask" }).click();

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
      .locator("[data-testid^=ask-]")
      .filter({ hasText: "Which direction should we take?" });
    const askTestId = await createdCard.getAttribute("data-testid");
    if (askTestId === null) {
      throw new Error("The created ask has no test id.");
    }
    const askId = askTestId.slice("ask-".length);
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
    const inboxCard = page.getByTestId(askTestId);
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
    const aliceCard = alicePage
      .getByRole("region", { name: "Margin review items" })
      .getByTestId(`ask-${ask.id}`);
    await expect(aliceCard).toContainText("brown");
    await aliceCard.getByLabel("Your answer").fill("Because it is precise.");
    await aliceCard.getByRole("button", { name: "Submit answer" }).click();
    await expect.poll(() => getAsk(ask.id)).toMatchObject({ ask: { state: "answered" } });
    await setSheet(alicePage, testInfo.project.name, false);

    // Bob's browser context has never had this issue open: no inbox history, no prior
    // query cache. He still sees the answered anchored ask in the margin.
    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}`);
    await bobPage.getByRole("tab", { name: "Spec" }).click();
    await setSheet(bobPage, testInfo.project.name, true);
    const bobCard = bobPage
      .getByRole("region", { name: "Margin review items" })
      .getByTestId(`ask-${ask.id}`);
    await expect(bobCard).toBeVisible();
    await expect(bobCard).toContainText("brown");
    await expect(bobCard).toContainText("Why brown?");
    await expect(bobCard).toContainText("alice answered");
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
