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

/** CSS pixels of background a box's shot takes in on every side of the box. */
const BOX_SHOT_PADDING = 4;
/** Device pixels at the outer edge of a box's shot read as the background around the box. */
const BACKGROUND_RING = 2;
/** How far a pixel's largest colour channel must stand from the background to count as ink. */
const INK_CONTRAST = 96;

/** How many shots of a box are taken, at most, waiting for two in a row to match. */
const STEADY_SHOT_ATTEMPTS = 10;

type Rect = { height: number; width: number; x: number; y: number };

/** A shot of `box` padded by `BOX_SHOT_PADDING` on every side, with the box's rect and the clip
 *  shot, both in CSS pixels of the viewport. The clip is widened to whole CSS pixels, so its shot
 *  maps back to it exactly at any device pixel ratio. The shot is taken once two in a row match
 *  with the box in the same place: the page can shift for a frame between reading the box's rect
 *  and the shot, which would then show whatever passed under the box. */
async function steadyShotOf(box: Locator): Promise<{ clip: Rect; rect: Rect; shot: Buffer }> {
  let previous: { rect: Rect; shot: Buffer } | undefined;
  for (let attempt = 0; attempt < STEADY_SHOT_ATTEMPTS; attempt++) {
    await box.scrollIntoViewIfNeeded();
    const rect = await box.boundingBox();
    if (rect === null) throw new Error("the checkbox is not on the page");
    const x = Math.floor(rect.x - BOX_SHOT_PADDING);
    const y = Math.floor(rect.y - BOX_SHOT_PADDING);
    const clip = {
      height: Math.ceil(rect.y + rect.height + BOX_SHOT_PADDING) - y,
      width: Math.ceil(rect.x + rect.width + BOX_SHOT_PADDING) - x,
      x,
      y,
    };
    const shot = await box.page().screenshot({ animations: "disabled", clip });
    if (
      previous?.shot.equals(shot) &&
      previous.rect.x === rect.x &&
      previous.rect.y === rect.y &&
      previous.rect.width === rect.width &&
      previous.rect.height === rect.height
    ) {
      return { clip, rect, shot };
    }
    previous = { rect, shot };
  }
  throw new Error(`the checkbox never held still for two shots in ${STEADY_SHOT_ATTEMPTS}`);
}

/** The share of the middle of `box` (its rect inset by a quarter on every side) that is ink: pixels
 *  whose largest channel differs from the background by `INK_CONTRAST` or more. The background is
 *  the per-channel median of the outermost `BACKGROUND_RING` device pixels of the box's shot. */
async function inkedShareOfMiddle(box: Locator): Promise<number> {
  const { clip, rect, shot } = await steadyShotOf(box);
  // The page decodes the PNG; a Blob read by `createImageBitmap` is no fetch the page's CSP could
  // refuse.
  return box.page().evaluate(
    async ({ base64, clip, contrast, rect, ring }) => {
      const bytes = Uint8Array.from(atob(base64), (char) => char.charCodeAt(0));
      const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/png" }));
      const { height, width } = bitmap;
      const context = new OffscreenCanvas(width, height).getContext("2d");
      if (!context) throw new Error("no 2d context to decode a box's shot");
      context.drawImage(bitmap, 0, 0);
      const { data } = context.getImageData(0, 0, width, height);
      const scale = width / clip.width;
      if (height !== Math.round(clip.height * scale)) {
        throw new Error(`a ${clip.width}x${clip.height} clip shot as ${width}x${height}`);
      }
      const ringChannels: number[][] = [[], [], []];
      for (let py = 0; py < height; py++) {
        for (let px = 0; px < width; px++) {
          if (px >= ring && py >= ring && px < width - ring && py < height - ring) continue;
          for (let c = 0; c < 3; c++) ringChannels[c].push(data[(py * width + px) * 4 + c]);
        }
      }
      const background = ringChannels.map(
        (values) => values.sort((a, b) => a - b)[values.length >> 1]
      );
      const left = Math.ceil((rect.x - clip.x + rect.width / 4) * scale);
      const right = Math.floor((rect.x - clip.x + (rect.width * 3) / 4) * scale);
      const top = Math.ceil((rect.y - clip.y + rect.height / 4) * scale);
      const bottom = Math.floor((rect.y - clip.y + (rect.height * 3) / 4) * scale);
      let inked = 0;
      for (let py = top; py < bottom; py++) {
        for (let px = left; px < right; px++) {
          const at = (py * width + px) * 4;
          const strongest = Math.max(
            ...background.map((value, c) => Math.abs(data[at + c] - value))
          );
          if (strongest >= contrast) inked++;
        }
      }
      return inked / ((right - left) * (bottom - top));
    },
    {
      base64: shot.toString("base64"),
      clip,
      contrast: INK_CONTRAST,
      rect,
      ring: BACKGROUND_RING,
    }
  );
}

/** Asserts a reader sees the ticked box as ticked and the unticked one as empty, however they are
 *  drawn: at least a quarter of the ticked box's middle is ink, and at most a twentieth of the
 *  unticked box's (drawn as styles.css draws them, half or more of a ticked box's middle is ink,
 *  the tick the rest, and none of an unticked box's). Each box is measured against its own
 *  surroundings, never pixel by pixel against the other box: the boxes are a fractional size at
 *  fractional positions, so shots of two identical boxes can be cut a device pixel apart, or a
 *  pixel different in size, and their borders alone would differ. */
async function expectTickedFilledUntickedEmpty(
  tickedBox: Locator,
  untickedBox: Locator
): Promise<void> {
  const ticked = await inkedShareOfMiddle(tickedBox);
  const unticked = await inkedShareOfMiddle(untickedBox);
  expect(ticked, "share of the ticked box's middle that is ink").toBeGreaterThanOrEqual(0.25);
  expect(unticked, "share of the unticked box's middle that is ink").toBeLessThanOrEqual(0.05);
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
  // A comment's turn shows a one-line preview until the reader expands its thread.
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
      await expectTickedFilledUntickedEmpty(tickedBox, untickedBox);

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
      const { tickedBox, untickedBox } = await expectTaskBoxes(
        thread,
        "Tag the build",
        "Publish the notes"
      );

      await expectTickedFilledUntickedEmpty(tickedBox, untickedBox);

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
