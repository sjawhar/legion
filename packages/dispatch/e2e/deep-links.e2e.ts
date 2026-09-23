import { expect, type Page, test } from "@playwright/test";

import {
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
} from "./api";
import { markSpan } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const spec = "# Instruments\n\nThe astrolabe measures altitude.\n";
const secondaryName = "expert-message-v4.md";
const secondarySlug = "expert-message-v4-md";
const secondaryBody = "Secondary document: please link the astrolabe handbook here.";

test.setTimeout(240_000);

test.beforeEach(async () => {
  await resetDatabase();
});

async function seedIssue() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Astrolabe calibration" });
  const secondary = await createIssueArtifact(issue.key, {
    content: secondaryBody,
    name: secondaryName,
  });
  return { issue, secondary };
}

async function expectSelectedMarginItem(
  page: Page,
  id: string,
  isPhone: boolean,
  isComment: boolean
) {
  const sheet = page.getByTestId("margin-sheet");
  const item = sheet.locator(`[data-margin-item="${id}"]`);
  await expect(item).toHaveAttribute("aria-current", "true");
  if (isPhone) {
    if ((await sheet.getAttribute("data-expanded")) !== "true") {
      await sheet.getByRole("button", { name: "Open review panel" }).click();
    }
    await expect(sheet).toHaveAttribute("data-expanded", "true");
  }
  await expect(item).toBeInViewport();
  if (isPhone && isComment) {
    await expect(item).toHaveAttribute("aria-expanded", "false");
  }
}

test("emitted document item hrefs select and scroll their anchored thread", async ({
  browser,
}, testInfo) => {
  const { issue } = await seedIssue();
  const comments = await Promise.all([
    createComment(issue.key, {
      anchor: { artifact: "spec", quote: "astrolabe" },
      body: "Comment on the spec.",
    }),
    createComment(issue.key, {
      anchor: { artifact: secondarySlug, quote: "link" },
      body: "Comment on the secondary document.",
    }),
  ]);
  const asks = await Promise.all([
    createAsk(issue.key, {
      anchor: { artifact: "spec", quote: "astrolabe" },
      question: "Ask on the spec?",
    }),
    createAsk(issue.key, {
      anchor: { artifact: secondarySlug, quote: "link" },
      question: "Ask on the secondary document?",
    }),
  ]);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const targets = [
      {
        id: comments[0].id,
        isComment: true,
        path: `/issues/${issue.key}/spec?comment=${comments[0].id}`,
      },
      {
        id: comments[1].id,
        isComment: true,
        path: `/issues/${issue.key}/artifacts/${secondarySlug}?comment=${comments[1].id}`,
      },
      { id: asks[0].id, isComment: false, path: `/issues/${issue.key}/spec?ask=${asks[0].id}` },
      {
        id: asks[1].id,
        isComment: false,
        path: `/issues/${issue.key}/artifacts/${secondarySlug}?ask=${asks[1].id}`,
      },
    ];
    for (const target of targets) {
      console.log(`landing ${target.path}`);
      await page.goto(target.path);
      await expectSelectedMarginItem(
        page,
        target.id,
        testInfo.project.name === "iphone",
        target.isComment
      );
      if (testInfo.project.name === "iphone") {
        await page.getByRole("button", { name: "Close review panel" }).click();
      }
    }
  } finally {
    await context.close();
  }
});

test("an iPhone document item link brings its far-away mark above the closed review sheet", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the closed sheet is phone-only");
  await createProject({ key: "CORE", name: "Core" });
  const spec = [
    "# Long document",
    ...Array.from(
      { length: 35 },
      (_, index) => `Paragraph ${index}: surrounding context for a long document.`
    ),
    "Unique faraway quotation.",
  ].join("\n\n");
  const issue = await createIssue({ project: "CORE", spec, title: "Long document link" });
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "Unique faraway quotation." },
    body: "This route must show its quote.",
  });
  if (comment.anchor === null) {
    throw new Error("Expected anchored comment");
  }
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const mark = markSpan(page, comment.anchor.mark_id);
    await expect(mark).toHaveClass(/dispatch-mark-active/);
    await expect(mark).toBeInViewport();
    await expect(sheet).toHaveAttribute("data-expanded", "false");

    await expect
      .poll(async () => {
        const [markBottom, sheetTop] = await Promise.all([
          mark.evaluate((element) => element.getBoundingClientRect().bottom),
          sheet.evaluate((element) => element.getBoundingClientRect().top),
        ]);
        return markBottom <= sheetTop;
      })
      .toBe(true);
  } finally {
    await context.close();
  }
});

test("an iPhone secondary-document link scrolls its selected card after opening the review sheet", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the bottom-sheet viewport is phone-only");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Primary\n\nnavmatrix primary quote\n\n# primary-decision",
    title: "Secondary document link",
  });
  const secondary = await createIssueArtifact(issue.key, {
    content: "# Secondary\n\nnavmatrix secondary quote\n\n# secondary-decision",
    name: secondaryName,
  });
  const ask = { options: [{ label: "Yes" }], question: "navmatrix choose?" };
  await Promise.all([
    createAsk(issue.key, { ...ask, anchor: { artifact: "spec", quote: "primary quote" } }),
    createAsk(issue.key, {
      ...ask,
      anchor: { artifact: secondary.artifact.slug, quote: "secondary quote" },
    }),
    createAsk(issue.key, { ...ask, anchor: { artifact: "spec", quote: "primary quote" } }),
    createComment(issue.key, {
      anchor: { artifact: "spec", quote: "primary quote" },
      body: "Primary comment.",
    }),
    createComment(issue.key, { body: "Unanchored comment." }),
  ]);
  const comment = await createComment(issue.key, {
    anchor: { artifact: secondary.artifact.slug, quote: "secondary quote" },
    body: "Secondary comment.",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(
      `/issues/${issue.key}/artifacts/${secondary.artifact.slug}?comment=${comment.id}`
    );

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card).toHaveAttribute("aria-current", "true");
    await expect(sheet).toHaveAttribute("data-expanded", "false");
    await sheet.getByRole("button", { name: "Open review panel" }).click();
    await expect(sheet).toHaveAttribute("data-expanded", "true");
    await expect(card).toBeInViewport();

    await expect
      .poll(async () => {
        const [cardBounds, sheetBounds] = await Promise.all([
          card.evaluate((element) => element.getBoundingClientRect().toJSON()),
          sheet.evaluate((element) => element.getBoundingClientRect().toJSON()),
        ]);
        return cardBounds.top >= sheetBounds.top && cardBounds.bottom <= sheetBounds.bottom;
      })
      .toBe(true);
  } finally {
    await context.close();
  }
});

test("an unanchored project-document comment is selected in its only discussion surface", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const document = await createProjectDocument("CORE", {
    content: "The astrolabe handbook is a project document.",
    name: "handbook.md",
  });
  const comment = await createArtifactComment(document.artifact.id, {
    body: "Document-level note without a quote.",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}?comment=${comment.id}`);
    await expect(page).toHaveURL(
      `/projects/CORE/documents/${document.artifact.slug}?comment=${comment.id}`
    );
    await expectSelectedMarginItem(page, comment.id, testInfo.project.name === "iphone", true);
    await expect(
      page.getByTestId("margin-sheet").locator(`[data-margin-item="${comment.id}"]`)
    ).toContainText("Document-level note without a quote.");
  } finally {
    await context.close();
  }
});

test("an unanchored comment deep link still focuses its Conversation turn", async ({ browser }) => {
  const { issue } = await seedIssue();
  const comment = await createComment(issue.key, { body: "No quote, just a note." });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    await expect(page).toHaveURL(`/issues/${issue.key}/comments/${comment.id}`);
    const turn = page.locator(`li[data-turn="comment:${comment.id}"][aria-current="true"]`);
    await expect(turn).toContainText("No quote, just a note.");
    await expect(turn).toBeInViewport();
  } finally {
    await context.close();
  }
});

test("an item link the SPA cannot resolve names what is missing, inside the shell", async ({
  browser,
}, testInfo) => {
  const { issue } = await seedIssue();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    for (const [segment, heading] of [
      ["comments", "Comment not found"],
      ["asks", "Ask not found"],
      ["messages", "Message not found"],
    ] as const) {
      for (const id of ["00000000-0000-4000-8000-000000000000", "not-a-uuid"]) {
        await page.goto(`/issues/${issue.key}/${segment}/${id}`);
        await expect(page.getByRole("heading", { name: heading })).toBeVisible();
        await expect(page.getByTestId("app-shell")).toBeVisible();
        if (testInfo.project.name !== "iphone") {
          await expect(page.getByRole("link", { exact: true, name: "Inbox" })).toBeVisible();
        }
      }
    }
  } finally {
    await context.close();
  }
});
