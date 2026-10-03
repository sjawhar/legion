import { expect, type Locator, type Page, type Route, test } from "@playwright/test";

import {
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createNamedVersion,
  createProject,
  createProjectDocument,
  editArtifact,
} from "./api";
import { barAction, documentTransport, markSpan, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const spec = "# Instruments\n\nThe astrolabe measures altitude.\n";
const agentSession = {
  actor: { kind: "session" as const, id: "e2e-deep-links" },
  as: "agent" as const,
};
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
 * How many frames without a scroll or a change of the margin's scroll height count as the
 * landing having stopped. A smooth scroll emits a scroll event every frame it runs, and the
 * margin's own corrections follow the render that moved its cards, so the thing being waited
 * for is measured in frames; a machine under load stretches them and stretches this with them,
 * which a wall-clock window cannot do. Ten covers the gap between the document reporting its
 * mark offsets and the margin re-placing the cards those offsets moved.
 */
const STILL_FRAMES = 10;

/**
 * Waits until the landing has stopped moving and reports where it stopped. A link's scroll to
 * its quote is smooth, and the margin corrects its own scroll as it fills in, so the target is
 * in view well before either finishes; clicking something while the page is still travelling
 * never settles, and a scroll height read mid-flight is not the one the reader is left with.
 */
async function landingSettled(
  sheet: Locator
): Promise<{ scrollHeight: number; scrollTop: number; windowScrollY: number }> {
  return await sheet.evaluate(
    (element, stillFrames) =>
      new Promise<{ scrollHeight: number; scrollTop: number; windowScrollY: number }>((resolve) => {
        let still = 0;
        let height = element.scrollHeight;
        const moved = () => {
          still = 0;
        };
        element.addEventListener("scroll", moved, { passive: true });
        window.addEventListener("scroll", moved, { passive: true });
        const frame = () => {
          if (element.scrollHeight !== height) {
            height = element.scrollHeight;
            still = 0;
          }
          still += 1;
          if (still < stillFrames) {
            requestAnimationFrame(frame);
            return;
          }
          element.removeEventListener("scroll", moved);
          window.removeEventListener("scroll", moved);
          resolve({
            scrollHeight: element.scrollHeight,
            scrollTop: element.scrollTop,
            windowScrollY: window.scrollY,
          });
        };
        requestAnimationFrame(frame);
      }),
    STILL_FRAMES
  );
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

interface HeldAnimationFrames {
  cancelAnimationFrame: typeof cancelAnimationFrame;
  frames: Map<number, FrameRequestCallback>;
  requestAnimationFrame: typeof requestAnimationFrame;
}

const heldAnimationFramesKey = "__dispatchHeldAnimationFrames";

/** Runs `during` with the page's animation frames held, then runs every frame it requested, however
 *  `during` ends: a frame-scheduled step, such as the editor's layout report, waits until it is over. */
async function withAnimationFramesHeld(page: Page, during: () => Promise<void>): Promise<void> {
  await page.evaluate((key) => {
    const target = window as typeof window & { [key: string]: HeldAnimationFrames | undefined };
    const frames = new Map<number, FrameRequestCallback>();
    let nextID = 0;
    target[key] = {
      cancelAnimationFrame: window.cancelAnimationFrame,
      frames,
      requestAnimationFrame: window.requestAnimationFrame,
    };
    window.requestAnimationFrame = ((callback: FrameRequestCallback) => {
      nextID += 1;
      frames.set(nextID, callback);
      return nextID;
    }) as typeof requestAnimationFrame;
    window.cancelAnimationFrame = ((frame: number) => {
      frames.delete(frame);
    }) as typeof cancelAnimationFrame;
  }, heldAnimationFramesKey);
  try {
    await during();
  } finally {
    await page.evaluate((key) => {
      const target = window as typeof window & { [key: string]: HeldAnimationFrames | undefined };
      const held = target[key];
      if (held === undefined) {
        return;
      }
      window.requestAnimationFrame = held.requestAnimationFrame;
      window.cancelAnimationFrame = held.cancelAnimationFrame;
      for (const callback of held.frames.values()) {
        held.requestAnimationFrame.call(window, callback);
      }
      delete target[key];
    }, heldAnimationFramesKey);
  }
}

/** Searches the issue tracker for `query` from the shell's search dialog and returns its one
 *  comment hit. */
async function searchHit(page: Page, query: string): Promise<Locator> {
  await page.getByRole("button", { name: /^search/i }).click();
  await page.getByRole("combobox", { name: "Search" }).fill(query);
  const hit = page.getByRole("dialog", { name: "Search" }).getByRole("option", {
    name: /^comment /,
  });
  await expect(hit).toHaveCount(1);
  return hit;
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
  // work that stamps the mark, so concurrent ones queue on the server's connection pool
  // instead of overlapping (LEGION-215).
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

test("a document link followed while the page before it is still downloading its document opens the link", async ({
  browser,
}, testInfo) => {
  const { issue } = await seedIssue();
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
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // The first page's document transport is still downloading when the reader follows the next
    // link. WebKit cancels that download as the navigation starts (Firefox cancels a real one too,
    // though not one Playwright holds), and the page must not answer the failed chunk by
    // reloading itself over the navigation.
    const transport = /\/assets\/yjs-[^/]+\.js$/u;
    const held: Route[] = [];
    await page.route(transport, (route) => {
      held.push(route);
    });
    await page.goto(`/issues/${issue.key}/spec?comment=${comments[0].id}`);
    await expect.poll(() => held.length).toBeGreaterThan(0);
    await page.unroute(transport);

    const link = `/issues/${issue.key}/artifacts/${secondarySlug}?comment=${comments[1].id}`;
    await page.goto(link);
    await expectSelectedMarginItem(page, comments[1].id, testInfo.project.name === "iphone", true);
    const opened = new URL(page.url());
    expect(`${opened.pathname}${opened.search}`).toBe(link);
    await Promise.all(held.map((route) => route.abort().catch(() => undefined)));
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
  const { comment, issue, markId } = await seedLongDocument();
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
    // and bring the form back. Both halves of the landing come first: the card, and the quote
    // the document scrolls to once the editor has projected its mark, which on a loaded machine
    // is later than the card. A selection made before that scroll is carried off screen by it,
    // and so is the fixed action bar placed beside the selection.
    await expect(card).toBeInViewport();
    await expect(markSpan(page, markId)).toBeInViewport();
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
    const transport = await documentTransport(page, { holding: true });
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

const crossDocumentLandingStates = [
  {
    holdIssuePageCode: false,
    name: "the issue page is shown with the document transport held",
  },
  { holdIssuePageCode: true, name: "the issue page code is held" },
] as const;

for (const state of crossDocumentLandingStates) {
  test(`a press while a comment link reached from another document is landing and ${state.name} does not end the hold`, async ({
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
    const pageCodeHeld = Promise.withResolvers<void>();

    try {
      const page = await context.newPage();
      const transport = await documentTransport(page);
      await page.goto(`/projects/CORE/documents/${handbook.artifact.slug}`);
      await expect(
        page.getByRole("textbox", { name: "Document editor" }).getByText("The handbook explains")
      ).toBeVisible();
      await landingSettled(page.getByTestId("margin-sheet"));

      // The document transport stays held in both states, so the issue editor has not reported
      // layout. The loop pins whether its route has rendered yet, rather than racing the press
      // against the Suspense reveal.
      transport.hold();
      if (state.holdIssuePageCode) {
        await page.route(/\/assets\/IssuePage-[^/]+\.js$/, async (route) => {
          await pageCodeHeld.promise;
          await route.continue();
        });
      }
      await (await searchHit(page, "must show its quote")).click();
      await expect(page).toHaveURL(`/issues/${issue.key}/spec?comment=${comment.id}`);
      if (!state.holdIssuePageCode) {
        await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute(
          "aria-selected",
          "true"
        );
      }

      const sheet = page.getByTestId("margin-sheet");
      const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
      const placement = card.locator("xpath=..");
      await card.waitFor();
      expect(await placement.evaluate((element) => (element as HTMLElement).style.top)).toBe("0px");
      await page.mouse.click(400, 400);

      pageCodeHeld.resolve();
      await transport.release();
      // The document's half of the landing first: `landingSettled` reports stillness, and a
      // margin that has not started moving yet is still. Waiting for the released transport to
      // project the mark is waiting for the landing to have begun, not guessing that it has.
      await expect(markSpan(page, markId)).toBeInViewport();
      await landingSettled(sheet);
      await expect(card).toBeInViewport();
      await expectSettledInside(card, sheet);
    } finally {
      pageCodeHeld.resolve();
      await context.close();
    }
  });
}

/** Two views of the issue that keep the live editor mounted and registered but hidden. `hide` leaves
 *  the editor of the issue `issueKey` hidden, opened at `link`, and returns the way back to it. */
const hiddenEditorJourneys = [
  {
    name: "a hidden spec editor withdraws its report before View in document returns to it",
    namedVersion: false,
    async hide(page: Page): Promise<() => Promise<void>> {
      await page.getByRole("tab", { name: "Conversation" }).click();
      const conversation = page.getByRole("tabpanel", { name: "Conversation" });
      await expect(conversation).toBeVisible();
      return async () => {
        await conversation.getByRole("link", { name: "View in document" }).click();
        await expect(page.getByRole("tabpanel", { name: "Spec" })).toBeVisible();
      };
    },
  },
  {
    name: "a hidden live editor withdraws its report before returning from a historical version",
    namedVersion: true,
    async hide(page: Page, issueKey: string, link: string): Promise<() => Promise<void>> {
      await page.getByRole("combobox", { name: "Version" }).selectOption("1");
      await expect(page.getByTestId("version-view")).toBeVisible();
      await expect(page).toHaveURL(`/issues/${issueKey}/artifacts/spec?v=1`);
      const hit = await searchHit(page, "must show its quote");
      return async () => {
        await hit.click();
        await expect(page).toHaveURL(link);
      };
    },
  },
] as const;

for (const journey of hiddenEditorJourneys) {
  test(journey.name, async ({ browser }, testInfo) => {
    test.skip(
      testInfo.project.name === "iphone",
      "the phone arms the hold only once the reader opens the sheet"
    );
    const { comment, issue, markId } = await seedLongDocument();
    if (journey.namedVersion) {
      await createNamedVersion(issue.primary_artifact_id, "Initial long document");
    }
    const context = await asUser(browser, "alice");

    try {
      const page = await context.newPage();
      const link = `/issues/${issue.key}/spec?comment=${comment.id}`;
      await page.goto(link);
      const sheet = page.getByTestId("margin-sheet");
      const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
      const placement = card.locator("xpath=..");
      await expect(markSpan(page, markId)).toBeInViewport();
      await landingSettled(sheet);

      const returnToDocument = await journey.hide(page, issue.key, link);
      await expect
        .poll(() => placement.evaluate((element) => (element as HTMLElement).style.top))
        .toBe("0px");

      // The press lands before the shown editor reports its layout, which waits for a frame.
      await withAnimationFramesHeld(page, async () => {
        await returnToDocument();
        await expect(page.getByRole("article", { name: "Document" })).toBeVisible();
        await page.mouse.click(400, 400);
      });
      await expect(markSpan(page, markId)).toBeInViewport();
      await landingSettled(sheet);
      await expect(card).toBeInViewport();
      await expectSettledInside(card, sheet);
    } finally {
      await context.close();
    }
  });
}

test("a resolving reference above a linked card does not end the hold", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the phone arms the hold only once the reader opens the sheet"
  );
  const { comment, issue } = await seedLongDocument();
  const target = await createIssue({
    project: "CORE",
    title: "The calibration needs a much longer title after the reference resolves",
  });
  for (const index of [1, 2]) {
    await createAsk(issue.key, {
      options: [{ label: "Yes" }, { label: "No" }],
      question: `Does dispatch://${target.key} need observer ${index} before this document can ship?`,
    });
  }
  const context = await asUser(browser, "alice");
  const referenceHeld = Promise.withResolvers<void>();

  try {
    const page = await context.newPage();
    await page.route(new RegExp(`/api/v1/issues/${target.key}$`), async (route) => {
      await referenceHeld.promise;
      await route.continue();
    });
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card).toBeInViewport();
    await expect(sheet.getByText(target.key, { exact: true }).first()).toBeVisible();
    await expect(sheet.getByText(target.title, { exact: true })).toHaveCount(0);
    await landingSettled(sheet);

    referenceHeld.resolve();
    await expect(sheet.getByText(target.title, { exact: true })).toHaveCount(2);
    const before = "Paragraph 34: surrounding context for a long document.";
    const inserted = Array.from(
      { length: 30 },
      (_, index) => `Inserted paragraph ${index}: the linked quote moves farther down.`
    );
    await editArtifact(
      issue.primary_artifact_id,
      { ops: [{ after: before, markdown: inserted.join("\n\n"), op: "insert" }] },
      agentSession
    );
    await expect(page.getByRole("textbox", { name: "Document editor" })).toContainText(
      "Inserted paragraph 29"
    );
    await landingSettled(sheet);
    await expect(card).toBeInViewport();
    await expectSettledInside(card, sheet);
  } finally {
    referenceHeld.resolve();
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
  const { comment, issue, markId } = await seedLongDocument("Paragraph 1: surrounding context");
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec?comment=${comment.id}`);

    const sheet = page.getByTestId("margin-sheet");
    const card = sheet.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card).toBeInViewport();
    // The presses below must land in the document: its editor mounts, and the landing its mark
    // starts, only after the card, and a press before that works nothing.
    await expect(markSpan(page, markId)).toBeInViewport();
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
    await expect(mark.locator(".dispatch-mark-active")).toHaveCount(1);
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
