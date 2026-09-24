import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
} from "./api";
import { barAction, heldDocumentTransport, markSpan, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const spec = "# Instruments\n\nThe astrolabe measures altitude.\n";
const secondaryName = "expert-message-v4.md";
const secondarySlug = "expert-message-v4-md";
const secondaryBody = "Secondary document: please link the astrolabe handbook here.";
/** A document whose quotation is thousands of pixels below the fold on every viewport. */
const longDocument = [
  "# Long document",
  ...Array.from(
    { length: 35 },
    (_, index) => `Paragraph ${index}: surrounding context for a long document.`
  ),
  "Unique faraway quotation.",
].join("\n\n");

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

/** The long-document fixture every landing test shares: the quotation is thousands of pixels
 *  below the fold, and the comment anchored to it is what the link names. */
async function seedLongDocument(quote = "Unique faraway quotation.") {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: longDocument,
    title: "Long document link",
  });
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote },
    body: "This route must show its quote.",
  });
  if (comment.anchor === null) {
    throw new Error("Expected anchored comment");
  }
  return { comment, issue, markId: comment.anchor.mark_id };
}

/** Open asks the reader owns, seeded one at a time: each holds its transaction across the
 *  document work that stamps its mark, and concurrent ones exhaust a small runner's pool.
 *  `onCreated` runs between them, for a test whose subject is what each arrival does. */
async function seedAsks(
  issueKey: string,
  count: number,
  onCreated?: (id: string) => Promise<void>
): Promise<string[]> {
  const ids: string[] = [];
  for (let index = 0; index < count; index += 1) {
    const ask = await createAsk(issueKey, {
      options: [{ label: "Yes" }, { label: "No" }],
      question: `Decision ${index}: does the astrolabe reading need a second observer, a longer baseline, or both before the calibration is signed off?`,
    });
    ids.push(ask.id);
    await onCreated?.(ask.id);
  }
  return ids;
}

/**
 * Waits until the landing has stopped moving and reports where it stopped. A link's scroll to
 * its quote is smooth, and the margin corrects its own scroll as it fills in, so the target is
 * in view well before either finishes; clicking something while the page is still travelling
 * never settles, and a scroll height read mid-flight is not the one the reader is left with.
 */
async function landingSettled(
  sheet: Locator
): Promise<{ scrollHeight: number; scrollTop: number; windowScrollY: number }> {
  let previous = { scrollHeight: -1, scrollTop: -1, windowScrollY: -1 };
  await expect
    .poll(
      async () => {
        const current = await sheet.evaluate((element) => ({
          scrollHeight: element.scrollHeight,
          scrollTop: element.scrollTop,
          windowScrollY: window.scrollY,
        }));
        const settled =
          current.scrollHeight === previous.scrollHeight &&
          current.scrollTop === previous.scrollTop &&
          current.windowScrollY === previous.windowScrollY;
        previous = current;
        return settled;
      },
      { intervals: [300] }
    )
    .toBe(true);
  return previous;
}

/** Polls until `inner` has settled wholly inside `outer`'s box - the margin's own scrollport,
 *  which `toBeInViewport` cannot see: it measures the window. */
async function expectSettledInside(inner: Locator, outer: Locator): Promise<void> {
  await expect
    .poll(async () => {
      const [innerBounds, outerBounds] = await Promise.all([
        inner.evaluate((element) => element.getBoundingClientRect().toJSON()),
        outer.evaluate((element) => element.getBoundingClientRect().toJSON()),
      ]);
      return innerBounds.top >= outerBounds.top && innerBounds.bottom <= outerBounds.bottom;
    })
    .toBe(true);
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
  // Anchored writes are seeded one at a time. Each holds its transaction across the document
  // work that stamps the mark, and concurrent ones exhaust the server's connection pool on a
  // small-CPU runner, where the pool is four connections wide (LEGION-215 CI hang).
  const comments = [
    await createComment(issue.key, {
      anchor: { artifact: "spec", quote: "astrolabe" },
      body: "Comment on the spec.",
    }),
    await createComment(issue.key, {
      anchor: { artifact: secondarySlug, quote: "link" },
      body: "Comment on the secondary document.",
    }),
  ];
  const asks = [
    await createAsk(issue.key, {
      anchor: { artifact: "spec", quote: "astrolabe" },
      question: "Ask on the spec?",
    }),
    await createAsk(issue.key, {
      anchor: { artifact: secondarySlug, quote: "link" },
      question: "Ask on the secondary document?",
    }),
  ];
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

test("a linked card the margin's later cards push down is scrolled back into view", async ({
  browser,
}, testInfo) => {
  const { issue } = await seedIssue();
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "astrolabe" },
    body: "Comment on the spec.",
  });
  // Open asks the reader owns render in a "Needs you" group above the anchored comments, and
  // their cards are tall. The margin fills in over several renders, so on a slower machine they
  // arrive after the linked comment has already been scrolled to.
  await seedAsks(issue.key, 3);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the asks until the comment card has landed, then release them: this is the render
    // order CI produces, made deterministic.
    const asksHeld = Promise.withResolvers<void>();
    await page.route(/\/api\/v1\/(inbox|issues\/[^/]+\/asks)/, async (route) => {
      await asksHeld.promise;
      await route.continue();
    });
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);
    const isPhone = testInfo.project.name === "iphone";
    await expectSelectedMarginItem(page, comment.id, isPhone, true);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    asksHeld.resolve();
    await expect(sheet.getByRole("heading", { name: "Needs you" })).toBeVisible();
    await expect(card).toBeInViewport();
    await expectSettledInside(card, sheet);
  } finally {
    await context.close();
  }
});

test("a desktop document item link shows its far-away card and its quote", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "the phone sheet has its own landing rule");
  const { comment, issue, markId } = await seedLongDocument();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card).toHaveAttribute("aria-current", "true");
    // The card's placement follows the document's mark offsets, which arrive after the card,
    // and the quote is thousands of pixels below the fold until the link scrolls to it.
    await expect(card).toBeInViewport();
    await expect(markSpan(page, markId)).toBeInViewport();
    await expectSettledInside(card, sheet);
  } finally {
    await context.close();
  }
});

test("a composer opened from the document is in the margin's view after a comment link", async ({
  browser,
}, testInfo) => {
  const { comment, issue } = await seedLongDocument();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    if (testInfo.project.name === "iphone") {
      await sheet.getByRole("button", { name: "Open review panel" }).click();
      await expect(sheet).toHaveAttribute("data-expanded", "true");
    }
    // The margin lands held on the linked card, far down its own scroll, and the composer
    // renders at the top of that scroll: opening one from the document has to end the hold
    // and bring the form back.
    await expect(card).toBeInViewport();
    await landingSettled(sheet);

    await selectEditorText(page, "Paragraph 2: surrounding context");
    await barAction(page, "Comment");

    const composer = sheet.locator("[data-margin-composer]");
    await expect(composer).toBeVisible();
    await expect(composer).toBeInViewport();
    await expect
      .poll(async () => {
        const [composerBounds, sheetBounds] = await Promise.all([
          composer.evaluate((element) => element.getBoundingClientRect().toJSON()),
          sheet.evaluate((element) => element.getBoundingClientRect().toJSON()),
        ]);
        return composerBounds.top >= sheetBounds.top - 1 && composerBounds.top < sheetBounds.bottom;
      })
      .toBe(true);
  } finally {
    await context.close();
  }
});

test("a press in the document while a comment link is still landing does not end the hold", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the phone arms the hold only once the reader opens the sheet"
  );
  const { comment, issue, markId } = await seedLongDocument();
  await seedAsks(issue.key, 3);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Both halves of the landing are held, not raced. The document's transport keeps the editor
    // from syncing, so it reports no layout and the card stays unplaced; the asks keep the
    // "Needs you" group that relays out the margin until after the press.
    const transport = await heldDocumentTransport(page);
    const asksHeld = Promise.withResolvers<void>();
    await page.route(/\/api\/v1\/(inbox|issues\/[^/]+\/asks)/, async (route) => {
      await asksHeld.promise;
      await route.continue();
    });
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    const placement = card.locator("xpath=..");
    await card.waitFor();
    // Unplaced: the open document has reported no layout, so every anchored card is stacked at
    // the top of the margin and the hold has had nothing to correct. A press now is the reader
    // arriving, not leaving, and must leave the hold armed.
    expect(await placement.evaluate((element) => (element as HTMLElement).style.top)).toBe("0px");
    await page.mouse.click(400, 400);

    await transport.release();
    await landingSettled(sheet);
    await expect(card).toBeInViewport();

    // Reading on: a wheel over the document is not taking over, and it carries the quote below
    // the fold, so the cards the asks bring re-place this one far outside the margin. Only a
    // hold that survived the press puts it back.
    await page.mouse.move(400, 400);
    await page.mouse.wheel(0, -2000);
    await expect(markSpan(page, markId)).not.toBeInViewport();
    asksHeld.resolve();
    await expect(sheet.getByRole("heading", { name: "Needs you" })).toBeVisible();

    await expect(card).toBeInViewport();
    await expectSettledInside(card, sheet);
  } finally {
    await context.close();
  }
});

test("a press while a comment link reached from another document is landing does not end the hold", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the phone arms the hold only once the reader opens the sheet"
  );
  const { comment, issue, markId } = await seedLongDocument();
  // A document the reader is already on, whose own offsets the margin has. Following a link out
  // of it is a client-side navigation: the margin provider never unmounts, so offsets left
  // behind would tell the next landing it was already over.
  const handbook = await createProjectDocument("CORE", {
    content: "The handbook explains the calibration.",
    name: "handbook.md",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const transport = await heldDocumentTransport(page, false);
    await page.goto(`/projects/CORE/documents/${handbook.artifact.slug}`);
    await expect(
      page.getByRole("textbox", { name: "Document editor" }).getByText("The handbook explains")
    ).toBeVisible();
    await landingSettled(page.getByTestId("margin-sheet"));

    // From here the next document's transport is held, so the landing the press interrupts is a
    // state this test owns: the handbook has reported its layout, the issue's spec has not.
    transport.hold();
    await page.getByRole("button", { name: /^search/i }).click();
    await page.getByRole("combobox", { name: "Search" }).fill("must show its quote");
    const hit = page.getByRole("dialog", { name: "Search" }).getByRole("option", {
      name: /^comment /,
    });
    await expect(hit).toHaveCount(1);
    await hit.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    const placement = card.locator("xpath=..");
    await card.waitFor();
    expect(await placement.evaluate((element) => (element as HTMLElement).style.top)).toBe("0px");
    await page.mouse.click(400, 400);

    await transport.release();
    // The document's half of the landing first: `landingSettled` reports stillness, and a margin
    // that has not started moving yet is still. Waiting for the released transport to project
    // the mark is waiting for the landing to have begun, not guessing that it has.
    await expect(markSpan(page, markId)).toBeInViewport();
    await landingSettled(sheet);
    await expect(card).toBeInViewport();
    await expectSettledInside(card, sheet);
  } finally {
    await context.close();
  }
});

test("the margin stops holding a landed card once the reader works the document", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the phone arms the hold only once the reader opens the sheet"
  );
  // Anchored near the top: the quote is on screen from the first frame, so the landing needs no
  // correction at all. A hold that only lets go once it has corrected something never lets go
  // here, and the asks below drag the margin thousands of pixels while the reader reads.
  const { comment, issue } = await seedLongDocument("Paragraph 1: surrounding context");
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card).toBeInViewport();
    const landed = await landingSettled(sheet);
    expect(landed.scrollTop).toBe(0);

    // The reader works the document: three presses and a selection, none of them in the margin.
    for (const offset of [0, 40, 80]) {
      await page.mouse.click(400, 300 + offset);
    }
    await selectEditorText(page, "Paragraph 4: surrounding context");

    await seedAsks(issue.key, 6, async (askId) => {
      await expect(sheet.locator(`[data-margin-item="${askId}"]`)).toBeVisible();
    });

    // Those asks push the card out of the margin's scrollport. The reader has taken over, so the
    // margin leaves its scroll exactly where they left it.
    const settled = await landingSettled(sheet);
    expect(settled.scrollTop).toBe(0);
    const [cardBounds, sheetBounds] = await Promise.all([
      card.evaluate((element) => element.getBoundingClientRect().toJSON()),
      sheet.evaluate((element) => element.getBoundingClientRect().toJSON()),
    ]);
    expect(cardBounds.top).toBeGreaterThan(sheetBounds.bottom);
  } finally {
    await context.close();
  }
});

/** Lets another anchored comment arrive above the linked one - which re-measures the whole
 *  column - and reports where the linked card landed, with the document's own scroll so the
 *  caller can hold that constant: a card follows its mark when the document moves. */
async function placementAfterArrival(
  page: Page,
  sheet: Locator,
  card: Locator,
  issueKey: string,
  paragraph: number
): Promise<{ top: string; windowScrollY: number }> {
  const arrival = await createComment(issueKey, {
    anchor: {
      artifact: "spec",
      quote: `Paragraph ${paragraph}: surrounding context for a long document.`,
    },
    body: `Another anchored thread ${paragraph}.`,
  });
  await expect(sheet.locator(`[data-margin-item="${arrival.id}"]`)).toBeVisible();
  await landingSettled(sheet);
  return {
    top: await card.evaluate((element) => (element.parentElement as HTMLElement).style.top),
    windowScrollY: await page.evaluate(() => window.scrollY),
  };
}

test("a linked card's placement ignores the margin's own scroll", async ({ browser }, testInfo) => {
  // The compact sheet stacks its cards from the top of its column rather than tracking marks -
  // measured here, the linked card sits at 102 px and an arrival above it moves it to 204 px,
  // the arriving card's own height - so a placement is not a mark's offset there and holding one
  // equal across arrivals is not the sheet's contract. The sheet's landing and hold are covered
  // by the phone tests above.
  test.skip(testInfo.project.name === "iphone", "only the desktop margin places cards by mark");
  const { comment, issue, markId } = await seedLongDocument();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);

    // Both halves of the landing: the card in the margin and the quote on screen. The mark
    // renders once the editor has projected it, which on a loaded machine is later than the
    // card, and a measurement taken before that scroll is of a different document position.
    await expect(card).toBeInViewport();
    await expect(markSpan(page, markId)).toBeInViewport();
    await landingSettled(sheet);

    // Each arrival re-measures the column while the margin sits wherever the hold left it, and
    // the document stands still throughout - so the linked card's placement, its mark's offset
    // in the document's own layout, is the same measurement both times. Measured against the
    // margin's scrolled region instead it carries that scroll and moves with it: 5,566 px then
    // 5,358 px, for a mark 277 px down the column, each correction feeding the next placement.
    const first = await placementAfterArrival(page, sheet, card, issue.key, 7);
    const second = await placementAfterArrival(page, sheet, card, issue.key, 11);

    expect(second.windowScrollY).toBe(first.windowScrollY);
    expect(second.top).toBe(first.top);
  } finally {
    await context.close();
  }
});

test("an iPhone document item link brings its far-away mark above the closed review sheet", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the closed sheet is phone-only");
  const { comment, issue, markId } = await seedLongDocument();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const mark = markSpan(page, markId);
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
  await createAsk(issue.key, { ...ask, anchor: { artifact: "spec", quote: "primary quote" } });
  await createAsk(issue.key, {
    ...ask,
    anchor: { artifact: secondary.artifact.slug, quote: "secondary quote" },
  });
  await createAsk(issue.key, { ...ask, anchor: { artifact: "spec", quote: "primary quote" } });
  await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "primary quote" },
    body: "Primary comment.",
  });
  await createComment(issue.key, { body: "Unanchored comment." });
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

    await expectSettledInside(card, sheet);
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
