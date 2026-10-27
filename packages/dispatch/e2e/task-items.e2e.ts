import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { answerAsk, createComment, createIssue, createProject } from "./api";
import { conversationThreadScope, documentEditor, indexedBlockAsk } from "./editor";
import { resetDatabase } from "./seed";
import { centerOf } from "./touch";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The innermost list item in `scope` holding `text`: the task item itself, not a turn or a reply
 *  row that is a list item around it. */
function itemWith(scope: Locator, text: string): Locator {
  return scope
    .getByRole("listitem")
    .filter({ hasNot: scope.page().getByRole("listitem"), hasText: text });
}

/** The vertical extent of a task item's checkbox and of the first line of the item's own words,
 *  both read in one pass so a scroll between two reads cannot shift one against the other. The
 *  words are measured by a Range over the item's first non-blank text node, whose first client
 *  rect is the first line the text sets. */
async function boxAndFirstLine(
  item: Locator
): Promise<{ box: { top: number; bottom: number }; line: { top: number; bottom: number } }> {
  return item.evaluate((li) => {
    const box = li.querySelector('input[type="checkbox"]');
    if (box === null) {
      throw new Error("the item holds no checkbox");
    }
    const walker = document.createTreeWalker(li, NodeFilter.SHOW_TEXT, (node) =>
      (node.textContent ?? "").trim() === "" ? NodeFilter.FILTER_SKIP : NodeFilter.FILTER_ACCEPT
    );
    const text = walker.nextNode();
    if (text === null) {
      throw new Error("the item holds no text");
    }
    const range = document.createRange();
    range.selectNodeContents(text);
    const line = range.getClientRects()[0];
    if (line === undefined) {
      throw new Error("the item's text sets no line");
    }
    const boxRect = box.getBoundingClientRect();
    return {
      box: { bottom: boxRect.bottom, top: boxRect.top },
      line: { bottom: line.bottom, top: line.top },
    };
  });
}

/** Asserts the item's checkbox sits on the same line as the item's first line of text: their
 *  vertical ranges overlap. */
async function expectBoxOnTextLine(item: Locator): Promise<void> {
  const { box, line } = await boxAndFirstLine(item);
  expect(box.bottom - box.top).toBeGreaterThan(0);
  expect(box.top).toBeLessThan(line.bottom);
  expect(line.top).toBeLessThan(box.bottom);
}

/** Asserts `scope` shows exactly two task checkboxes, both read-only, the one on the item holding
 *  `tickedText` ticked and the one on the item holding `untickedText` not, each item with no
 *  bullet. Returns the two items and their boxes. */
async function expectTaskBoxes(scope: Locator, tickedText: string, untickedText: string) {
  await expect(scope.getByRole("checkbox")).toHaveCount(2);
  const ticked = itemWith(scope, tickedText);
  const unticked = itemWith(scope, untickedText);
  const tickedBox = ticked.getByRole("checkbox");
  const untickedBox = unticked.getByRole("checkbox");
  await expect(tickedBox).toBeChecked();
  await expect(tickedBox).toBeDisabled();
  await expect(untickedBox).not.toBeChecked();
  await expect(untickedBox).toBeDisabled();
  await expect(ticked).toHaveCSS("list-style-type", "none");
  await expect(unticked).toHaveCSS("list-style-type", "none");
  return { ticked, tickedBox, unticked, untickedBox };
}

/** Asserts a reader can tell the two boxes apart, however they are drawn: at least a tenth of the
 *  pixels of the two boxes' shots differ by half a colour channel's range or more. A byte compare
 *  is not enough: two boxes drawn alike still differ by a few faint antialiased edge pixels. */
async function expectBoxesLookDifferent(tickedBox: Locator, untickedBox: Locator): Promise<void> {
  const tickedShot = await tickedBox.screenshot({ animations: "disabled" });
  const untickedShot = await untickedBox.screenshot({ animations: "disabled" });
  // The page decodes the two PNGs; a Blob read by `createImageBitmap` is no fetch the page's CSP
  // could refuse.
  const { pixels, sameSize, strong } = await tickedBox.page().evaluate(
    async (shots) => {
      const [a, b] = await Promise.all(
        shots.map(async (base64) => {
          const bytes = Uint8Array.from(atob(base64), (char) => char.charCodeAt(0));
          const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/png" }));
          const context = new OffscreenCanvas(bitmap.width, bitmap.height).getContext("2d");
          if (!context) throw new Error("no 2d context to decode a box's shot");
          context.drawImage(bitmap, 0, 0);
          return context.getImageData(0, 0, bitmap.width, bitmap.height);
        })
      );
      let strong = 0;
      for (let i = 0; i < a.data.length; i += 4) {
        const channels = [0, 1, 2].map((c) => Math.abs(a.data[i + c] - b.data[i + c]));
        if (Math.max(...channels) >= 128) strong++;
      }
      return {
        pixels: a.width * a.height,
        sameSize: a.width === b.width && a.height === b.height,
        strong,
      };
    },
    [tickedShot.toString("base64"), untickedShot.toString("base64")]
  );
  expect(sameSize, "the ticked and unticked boxes are the same size").toBe(true);
  expect(strong, "pixels that tell the ticked box from the unticked one").toBeGreaterThanOrEqual(
    pixels / 10
  );
}

/** Seeds an issue with an agent comment holding a ticked and an unticked task item. */
async function seedChecklist(): Promise<{ commentId: string; issueKey: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release checklist" });
  const comment = await createComment(
    issue.key,
    { body: "Release checklist:\n\n- [x] Tag the build\n- [ ] Publish the notes\n" },
    { as: "agent" }
  );
  return { commentId: comment.id, issueKey: issue.key };
}

/** Opens the issue's Conversation, expands the comment's thread and returns where it shows. */
async function openChecklistThread(
  page: Page,
  project: string,
  { commentId, issueKey }: { commentId: string; issueKey: string }
): Promise<Locator> {
  await page.goto(`/issues/${issueKey}/conversation`);
  const turn = page.locator(`[data-turn="comment:${commentId}"]`);
  // The comment has loaded, so any failure after this is about the checkboxes.
  await expect(turn.getByText("Tag the build")).toBeVisible();
  // A comment's turn shows a one-line preview until the reader expands its thread: in place
  // above the phone breakpoint, as a full-screen Thread dialog below it.
  await turn.getByRole("button", { name: "Expand thread" }).click();
  return conversationThreadScope(page, project, turn);
}

async function withAlice(browser: Browser, run: (page: Page) => Promise<void>): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await run(await alice.newPage());
  } finally {
    await alice.close();
  }
}

for (const colorScheme of ["light", "dark"] as const) {
  test(`a comment's ticked and unticked task items show a ticked and an unticked checkbox (${colorScheme})`, async ({
    browser,
  }, testInfo) => {
    const checklist = await seedChecklist();
    await withAlice(browser, async (page) => {
      await page.emulateMedia({ colorScheme });
      const thread = await openChecklistThread(page, testInfo.project.name, checklist);
      const { ticked, tickedBox, unticked, untickedBox } = await expectTaskBoxes(
        thread,
        "Tag the build",
        "Publish the notes"
      );

      // Each box stands on its item's first line, in place of the bullet.
      await ticked.scrollIntoViewIfNeeded();
      await expectBoxOnTextLine(ticked);
      await expectBoxOnTextLine(unticked);
      await expectBoxesLookDifferent(tickedBox, untickedBox);

      // A click on a box changes nothing: the boxes are read-only.
      const { x, y } = await centerOf(tickedBox);
      await page.mouse.click(x, y);
      await expect(tickedBox).toBeChecked();
      await expect(untickedBox).not.toBeChecked();

      await thread.screenshot({ path: testInfo.outputPath(`task-items-${colorScheme}.png`) });
    });
  });

  // Windows High Contrast forces every fill and border to the system colours, so a box drawn by
  // the page alone could lose what tells a ticked one from an unticked one.
  test(`under forced colors a comment's ticked and unticked task items still look different (${colorScheme})`, async ({
    browser,
  }, testInfo) => {
    const checklist = await seedChecklist();
    await withAlice(browser, async (page) => {
      await page.emulateMedia({ colorScheme, forcedColors: "active" });
      const thread = await openChecklistThread(page, testInfo.project.name, checklist);
      const { ticked, tickedBox, untickedBox } = await expectTaskBoxes(
        thread,
        "Tag the build",
        "Publish the notes"
      );

      await ticked.scrollIntoViewIfNeeded();
      await expectBoxesLookDifferent(tickedBox, untickedBox);

      await thread.screenshot({
        path: testInfo.outputPath(`task-items-forced-${colorScheme}.png`),
      });
    });
  });
}

test("a reply under a decision inside a document shows the checkboxes with no bullet", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#naming urgency="med" multiple="false"}\nWhich alias?\n\n- short\n- long\n:::\n\nAfter.\n',
    title: "Alias decision",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "naming");
  await answerAsk(
    blockAsk.id,
    { expected_edited_at: null, selected: ["short"], text: "Short it is." },
    { login: "bob" }
  );
  await createComment(
    issue.key,
    { ask_id: blockAsk.id, body: "- [x] alias added\n- [ ] docs updated" },
    { as: "agent" }
  );

  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="naming"]');
    const thread = block.getByTestId(`thread-${blockAsk.id}`);
    await expect(thread.getByText("alias added")).toBeVisible();
    await expectTaskBoxes(thread, "alias added", "docs updated");

    await thread.screenshot({ path: testInfo.outputPath("task-items-ask-footer.png") });
  });
});
