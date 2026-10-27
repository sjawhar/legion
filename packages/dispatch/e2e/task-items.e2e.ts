import { expect, type Locator, test } from "@playwright/test";

import { answerAsk, createComment, createIssue, createProject, getIssue } from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
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

/** The middle of an element's box, in page coordinates. */
async function centreOf(locator: Locator): Promise<{ x: number; y: number }> {
  const box = await locator.boundingBox();
  if (box === null) {
    throw new Error("the element has no box");
  }
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

for (const colorScheme of ["light", "dark"] as const) {
  test(`a comment's ticked and unticked task items show a ticked and an unticked checkbox (${colorScheme})`, async ({
    browser,
  }, testInfo) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Release checklist" });
    const comment = await createComment(
      issue.key,
      { body: "Release checklist:\n\n- [x] Tag the build\n- [ ] Publish the notes\n" },
      { as: "agent" }
    );
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.emulateMedia({ colorScheme });
      await page.goto(`/issues/${issue.key}/conversation`);
      const turn = page.locator(`[data-turn="comment:${comment.id}"]`);

      // The comment has loaded, so any failure below is about the checkboxes.
      await expect(turn.getByText("Tag the build")).toBeVisible();
      // A comment's turn shows a one-line preview until the reader expands its thread: in place
      // above the phone breakpoint, as a full-screen Thread dialog below it.
      await turn.getByRole("button", { name: "Expand thread" }).click();
      const thread =
        testInfo.project.name === "iphone" ? page.getByRole("dialog", { name: "Thread" }) : turn;
      await expect(thread.getByRole("checkbox")).toHaveCount(2);

      const ticked = itemWith(thread, "Tag the build");
      const unticked = itemWith(thread, "Publish the notes");
      const tickedBox = ticked.getByRole("checkbox");
      const untickedBox = unticked.getByRole("checkbox");
      await expect(tickedBox).toBeChecked();
      await expect(tickedBox).toBeDisabled();
      await expect(untickedBox).not.toBeChecked();
      await expect(untickedBox).toBeDisabled();

      // The two boxes look different at a glance: the ticked one is filled, the unticked one not.
      const tickedFill = await tickedBox.evaluate((box) => getComputedStyle(box).backgroundColor);
      const untickedFill = await untickedBox.evaluate(
        (box) => getComputedStyle(box).backgroundColor
      );
      expect(tickedFill).not.toBe(untickedFill);
      expect(tickedFill).not.toBe("rgba(0, 0, 0, 0)");

      // Each box stands on its item's first line, in place of the bullet.
      await ticked.scrollIntoViewIfNeeded();
      await expectBoxOnTextLine(ticked);
      await expectBoxOnTextLine(unticked);
      await expect(ticked).toHaveCSS("list-style-type", "none");
      await expect(unticked).toHaveCSS("list-style-type", "none");

      // A click on a box changes nothing: the boxes are read-only.
      const { x, y } = await centreOf(tickedBox);
      await page.mouse.click(x, y);
      await expect(tickedBox).toBeChecked();
      await expect(untickedBox).not.toBeChecked();

      await thread.screenshot({ path: testInfo.outputPath(`task-items-${colorScheme}.png`) });
    } finally {
      await alice.close();
    }
  });
}

test("a reply under a decision inside a document shows the checkboxes with no bullet", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#naming urgency="med" multiple="false"}\nWhich alias?\n\n- short\n- long\n:::\n\nAfter.\n',
    title: "Alias decision",
  });
  await expect
    .poll(async () =>
      (await getIssue(issue.key)).open_asks.some((ask) => ask.block_id === "naming")
    )
    .toBe(true);
  const blockAsk = (await getIssue(issue.key)).open_asks.find((ask) => ask.block_id === "naming");
  if (blockAsk === undefined) {
    throw new Error("the naming decision was not indexed");
  }
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

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="naming"]');
    const thread = block.getByTestId(`thread-${blockAsk.id}`);
    await expect(thread.getByText("alias added")).toBeVisible();
    await expect(thread.getByRole("checkbox")).toHaveCount(2);

    const ticked = itemWith(thread, "alias added");
    const unticked = itemWith(thread, "docs updated");
    await expect(ticked.getByRole("checkbox")).toBeChecked();
    await expect(ticked.getByRole("checkbox")).toBeDisabled();
    await expect(unticked.getByRole("checkbox")).not.toBeChecked();
    await expect(unticked.getByRole("checkbox")).toBeDisabled();
    await expect(ticked).toHaveCSS("list-style-type", "none");
    await expect(unticked).toHaveCSS("list-style-type", "none");
  } finally {
    await alice.close();
  }
});
