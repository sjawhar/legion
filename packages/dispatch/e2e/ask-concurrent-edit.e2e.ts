import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  test,
} from "@playwright/test";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { createIssue, createProject, getArtifactText, getIssue } from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  indexedBlockAsk,
  placeCaret,
  selectTextIn,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// sjawhar/legion#2433: two collaborators editing one decision block at once can leave it holding what its
// content rule (`paragraph+ bullet_list?`) refuses: each adds an option list while their edits
// cross, and the merged block holds two lists. y-prosemirror could not build such a block and
// deleted it from the shared document, so every editor and the server lost the question and every
// option, and settlement retracted the ask. The block now keeps every word, reads as malformed
// until it is fixed, and its ask stays open; joining the two lists makes it a decision again.

const SPEC = 'Intro.\n\n:::ask{#a1 urgency="med" multiple="false"}\nWhich one?\n:::\n\nAfter.\n';

test.beforeEach(async () => {
  await resetDatabase();
});

function askBlock(page: Page): Locator {
  return documentEditor(page).locator('[data-dispatch-ask-block="a1"]');
}

/** The decision's content hole, the editable text under its banner: a malformed decision's banner
 * (`contenteditable="false"`) quotes the block's text, so a caret is placed by a quote found here. */
function askContent(page: Page): Locator {
  return askBlock(page).locator(":scope > [data-proof-block-content]");
}

function questionParagraph(page: Page): Locator {
  return askContent(page).locator(":scope > p").first();
}

function optionList(page: Page, index: number): Locator {
  return askContent(page).locator(":scope > ul").nth(index);
}

/** Creates the issue whose spec holds the decision a1 and waits until it is an open ask. */
async function seedDecision() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: SPEC, title: "Crossed options" });
  await indexedBlockAsk(issue.key, "a1");
  return issue;
}

/** Opens the decision in one page per option, all in `context`, then has each add its own option
 * list under the question while neither edit reaches the server or the other page, and lets both
 * edits through at once: the merged block holds two lists. The merge is between two Yjs clients,
 * which two pages are whoever holds them, so one signed-in context serves both and the second
 * editor pays no sign-in and no cold cache. Every page's timers are faked before it loads, since
 * the severed transport's reconnect waits a delay the page draws itself; a row that drives one of
 * its own (`alicePage.clock`) inherits the same install. Each page's errors are collected into
 * `errors`. */
async function crossOptionLists(
  issueKey: string,
  context: BrowserContext,
  options: readonly string[],
  errors: string[]
): Promise<Page[]> {
  const opened = [];
  for (const option of options) {
    const page = await context.newPage();
    page.on("pageerror", (error) => errors.push(error.message));
    await page.clock.install();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issueKey}`);
    opened.push({ option, page, transport });
  }
  for (const { page } of opened) {
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(askBlock(page)).toContainText("Which one?");
  }
  for (const { transport } of opened) {
    transport.hold();
    await transport.sever();
  }
  // Each page's edit is local while its transport is severed, so neither waits on the other.
  await Promise.all(
    opened.map(async ({ option, page }) => {
      // The option first, then the `- ` input rule before it: inserted into an empty item, the
      // text lands in the question, since the block hides an empty option while it reads as
      // malformed. Only the `- ` needs real keys, which is what the rule reads.
      await placeCaret(page, "after", "Which one?");
      await page.keyboard.press("Enter");
      await page.keyboard.insertText(option);
      await page.keyboard.press("Home");
      await page.keyboard.type("- ");
      await expect(askBlock(page).getByRole("listitem")).toHaveText(option);
    })
  );
  await Promise.all(opened.map(({ transport }) => transport.release()));
  // The provider draws its reconnect delay from [0, RECONNECT_SPREAD_MS)
  // (`web/src/features/doc/connection.ts`), so the crossing would otherwise wait out a random
  // second of it; the pages' own clocks spend it instead.
  await Promise.all(opened.map(({ page }) => page.clock.runFor(RECONNECT_SPREAD_MS)));
  return opened.map(({ page }) => page);
}

/** Waits until every page's decision block and the stored markdown hold each of `words`. */
async function expectWordsKept(
  pages: readonly Page[],
  artifactId: string,
  words: readonly string[]
): Promise<void> {
  for (const page of pages) {
    for (const word of words) {
      await expect(askBlock(page)).toContainText(word);
    }
  }
  await expect
    .poll(async () => {
      const { markdown } = await getArtifactText(artifactId);
      return words.filter((word) => !markdown.includes(word));
    })
    .toEqual([]);
}

// SLOW(>=2s): the crossing itself - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block
// (400 INVALID_ASK_BLOCK on every write path).
test("two editors that each add an option list to an ask while their edits cross keep the question and both options", async ({
  browser,
}) => {
  const issue = await seedDecision();
  const alice = await asUser(browser, "alice");
  const errors: string[] = [];
  try {
    const pages = await crossOptionLists(issue.key, alice, ["Option A", "Option B"], errors);
    await expectWordsKept(pages, issue.primary_artifact_id, ["Which one?", "Option A", "Option B"]);

    for (const page of pages) {
      await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
    }
    expect((await getIssue(issue.key)).open_asks.map((ask) => ask.block_id)).toContain("a1");

    // One list again: Backspace at the start of the second list joins it to the first, and the
    // block parses, so the ask takes both options. Which list comes first is the merge's order of
    // the two editors' Yjs client ids, so the second list is read from the page; Backspace at the
    // start of the first list would lift its item into the question instead.
    const [alicePage] = pages;
    const secondList = (await optionList(alicePage, 1).textContent()) ?? "";
    await placeCaret(
      alicePage,
      "before",
      secondList.includes("Option B") ? "Option B" : "Option A",
      askContent(alicePage)
    );
    await alicePage.keyboard.press("Backspace");
    for (const page of pages) {
      await expect(askBlock(page)).not.toHaveAttribute("data-dispatch-ask-malformed");
    }
    await expect
      .poll(async () =>
        (await getIssue(issue.key)).open_asks
          .find((ask) => ask.block_id === "a1")
          ?.options.map(({ label }) => label)
          .sort()
      )
      .toEqual(["Option A", "Option B"]);
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** Opens the decision a1 in two of alice's pages and crosses their option lists, so the block
 * holds two lists, then runs `act` on the first page with the two lists' options in document
 * order (the merge orders the lists by the two editors' Yjs client ids). `act` answers the words
 * that must survive it; every page and the stored markdown must keep them, the ask must stay
 * open, and no page error may escape. Page errors are checked first as well, so a keystroke that
 * throws fails on its error rather than on the words it lost. */
async function withTwoLists(
  browser: Browser,
  act: (input: {
    alicePage: Page;
    artifactId: string;
    first: string;
    pages: Page[];
    second: string;
  }) => Promise<readonly string[]>
): Promise<void> {
  const issue = await seedDecision();
  const alice = await asUser(browser, "alice");
  const errors: string[] = [];
  try {
    const pages = await crossOptionLists(issue.key, alice, ["Option A", "Option B"], errors);
    for (const page of pages) {
      await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
    }
    const [alicePage] = pages;
    await expect(askContent(alicePage).locator(":scope > ul")).toHaveCount(2);
    // A list's text also holds the other editor's cursor label when its caret is in it, so it is
    // matched, not compared.
    const firstList = (await optionList(alicePage, 0).textContent()) ?? "";
    const [first, second] = firstList.includes("Option A")
      ? ["Option A", "Option B"]
      : ["Option B", "Option A"];
    await expect(optionList(alicePage, 1)).toContainText(second);

    const words = await act({
      alicePage,
      artifactId: issue.primary_artifact_id,
      first,
      pages,
      second,
    });
    expect(errors).toEqual([]);
    await expectWordsKept(pages, issue.primary_artifact_id, words);
    for (const page of pages) {
      await expect(askBlock(page)).toBeVisible();
    }
    expect((await getIssue(issue.key)).open_asks.map((ask) => ask.block_id)).toContain("a1");
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
}

/** Gives the first list a second item, `Option C`, after `first`: Enter at the end of a list's
 * item opens the next item, and the item's text needs no real keys. */
async function addOptionToFirstList(page: Page, first: string): Promise<void> {
  await placeCaret(page, "after", first, optionList(page, 0));
  await page.keyboard.press("Enter");
  await page.keyboard.insertText("Option C");
  await expect(optionList(page, 0).getByRole("listitem")).toContainText([first, "Option C"]);
}

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("Enter in the question of an ask holding two lists keeps every word", async ({ browser }) => {
  await withTwoLists(browser, async ({ alicePage, artifactId, pages }) => {
    await placeCaret(alicePage, "after", "Which one?", questionParagraph(alicePage));
    await alicePage.keyboard.press("Enter");
    await alicePage.keyboard.type("More");
    for (const page of pages) {
      await expect(questionParagraph(page)).toContainText("More");
      await expect(optionList(page, 0)).not.toContainText("More");
      await expect(optionList(page, 1)).not.toContainText("More");
    }
    await expect
      .poll(async () =>
        (await getArtifactText(artifactId)).markdown
          .split("\n")
          .filter((line) => line.includes("More"))
      )
      .toEqual(["Which one?More"]);
    return ["Which one?", "More", "Option A", "Option B"];
  });
});

// Each input below, on a block off its content rule, either throws inside ProseMirror (a key
// command; the edit a typed letter, a cut, a composition's start or a drop builds) or is an edit
// the browser makes itself, which ProseMirror reads back with a replace that drops the second
// list. The editor's input guards decline the input, or apply ProseMirror's own edit in place of
// the browser's, so no page error escapes and no word outside the input's range is lost.

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("Enter twice at the end of the second list of an ask holding two lists keeps every word", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, second }) => {
    await placeCaret(alicePage, "after", second, optionList(alicePage, 1));
    await alicePage.keyboard.press("Enter");
    await alicePage.keyboard.press("Enter");
    return ["Which one?", "Option A", "Option B"];
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("Backspace at the start of the first list of an ask holding two lists keeps every word", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first }) => {
    await addOptionToFirstList(alicePage, first);
    await placeCaret(alicePage, "before", first, optionList(alicePage, 0));
    await alicePage.keyboard.press("Backspace");
    return ["Which one?", "Option A", "Option B", "Option C"];
  });
});

/** Gives the first list a second item, so the block would still hold two lists after a deletion
 * of the range, then selects from after "Which " in the question to after "Opt" in the first
 * list's first item. Answers the words outside the selection. */
async function selectQuestionIntoFirstList(
  page: Page,
  first: string,
  second: string
): Promise<string[]> {
  await addOptionToFirstList(page, first);
  await selectTextIn(page, askContent(page), "one?Opt");
  return ["Which ", first.slice("Opt".length), "Option C", second];
}

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("Backspace over a selection from the question into the first list of an ask holding two lists keeps the words outside it", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    const words = await selectQuestionIntoFirstList(alicePage, first, second);
    await alicePage.keyboard.press("Backspace");
    return words;
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("typing over a selection from the question into the first list of an ask holding two lists keeps the words outside it", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    const words = await selectQuestionIntoFirstList(alicePage, first, second);
    await alicePage.keyboard.type("x");
    return words;
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("cutting a selection from the question into the first list of an ask holding two lists keeps the words outside it", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    const words = await selectQuestionIntoFirstList(alicePage, first, second);
    await alicePage.keyboard.press("ControlOrMeta+X");
    return words;
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("text input without a keypress over a selection from the question into the first list of an ask holding two lists keeps the words outside it", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    const words = await selectQuestionIntoFirstList(alicePage, first, second);
    // An input method's commit or dictation: a beforeinput and the browser's own edit, no keypress.
    await alicePage.keyboard.insertText("x");
    return words;
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("an input method composing over a selection from the question into the first list of an ask holding two lists keeps the words outside it", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    const words = await selectQuestionIntoFirstList(alicePage, first, second);
    const cdp = await alicePage.context().newCDPSession(alicePage);
    await cdp.send("Input.imeSetComposition", { selectionEnd: 1, selectionStart: 1, text: "x" });
    await cdp.send("Input.insertText", { text: "x" });
    return words;
  });
});

// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("dragging a selection from the question into the first list of an ask holding two lists to its second list keeps every word", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, first, second }) => {
    await selectQuestionIntoFirstList(alicePage, first, second);
    const from = await alicePage.evaluate(() => {
      const box = window.getSelection()?.getRangeAt(0).getBoundingClientRect();
      return box && { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    });
    const to = await optionList(alicePage, 1).getByText(second).boundingBox();
    if (!from || !to) throw new Error("the selection or the second list's option has no box");
    await alicePage.mouse.move(from.x, from.y);
    await alicePage.mouse.down();
    await alicePage.mouse.move(from.x + 5, from.y + 5, { steps: 3 });
    await alicePage.mouse.move(to.x + to.width - 2, to.y + to.height / 2, { steps: 10 });
    await alicePage.mouse.up();
    // The other page draws the acting caret after "Opt", inside the first option.
    return ["Which one?", first.slice("Opt".length), "Option C", second];
  });
});

// A grammar or writing extension replaces selected text with document.execCommand, which fires no
// beforeinput: the browser edits the DOM itself and ProseMirror reads the edit back. Over a
// selection across blocks of a decision two editors broke, that read-back must not drop the
// option list after the selection, which the person did not select.
for (const [command, value] of [
  ["insertText", "x"],
  ["delete", undefined],
] as const) {
  // SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
  // y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
  test(`document.execCommand("${command}") over a selection from the question into the first list of an ask holding two lists keeps the words outside it`, async ({
    browser,
  }) => {
    await withTwoLists(browser, async ({ alicePage, first, second }) => {
      const words = await selectQuestionIntoFirstList(alicePage, first, second);
      await alicePage.evaluate(([name, text]) => document.execCommand(name, false, text), [
        command,
        value,
      ] as const);
      return words;
    });
  });
}

// The read-back must not drop the second list when another block holds its text: here the first
// list's second option reads the second list's option followed by " later".
// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test(`document.execCommand("insertText") over a selection from the question into the first list of an ask holding two lists keeps a second list whose text another option holds`, async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, artifactId, first, pages, second }) => {
    const later = `${second} later`;
    await placeCaret(alicePage, "after", first, optionList(alicePage, 0));
    await alicePage.keyboard.press("Enter");
    await alicePage.keyboard.insertText(later);
    await expect(optionList(alicePage, 0).getByRole("listitem")).toContainText([first, later]);
    await selectTextIn(alicePage, askContent(alicePage), "one?Opt");
    await alicePage.evaluate(() => document.execCommand("insertText", false, "x"));
    for (const page of pages) {
      // The other editor's cursor label can sit in the second list, so its item is matched.
      const items = optionList(page, 1).getByRole("listitem");
      await expect(items).toHaveCount(1);
      await expect(items).toContainText(second);
      await expect(items).not.toContainText("later");
      await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
    }
    // The serializer marks the second of two adjacent lists with `*`, so the two do not merge.
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown.split("\n"))
      .toContain(`* ${second}`);
    return ["Which ", first.slice("Opt".length), later, second];
  });
});

/** The second list's text on `page` without a collaborator's cursor label, which sits inside it
 * (`contenteditable="false"`) wherever that collaborator's caret is. */
function secondListText(page: Page): Promise<string> {
  return optionList(page, 1).evaluate((list) => {
    const copy = list.cloneNode(true) as Element;
    for (const label of copy.querySelectorAll('[contenteditable="false"]')) label.remove();
    return copy.textContent ?? "";
  });
}

// Undo and redo revert or redo the person's own edit wherever the caret now is: a letter typed in
// the second list is undone, and redone, with the caret in the question, and no other word moves.
// SLOW(>=2s): the two-list fixture below - two editor mounts, a severed transport each and the
// y-prosemirror merge - which no write path can seed, since Dispatch refuses such a block.
test("Ctrl+Z and Ctrl+Shift+Z with the caret in the question undo and redo a letter typed in the second list of an ask holding two lists", async ({
  browser,
}) => {
  await withTwoLists(browser, async ({ alicePage, artifactId, first, pages, second }) => {
    const typed = `${second.slice(0, -1)}x${second.slice(-1)}`;
    const storedLines = async () => (await getArtifactText(artifactId)).markdown.split("\n");
    const expectSecondList = async (text: string) => {
      for (const page of pages) await expect.poll(() => secondListText(page)).toBe(text);
      await expect.poll(storedLines).toContain(`* ${text}`);
    };
    // Past prosemirror-history's 500 ms group delay, which it reads off `Date.now()`, so the
    // letter is an undo step of its own: the page's clock is advanced past it rather than waited
    // out, after the crossed edit above has landed, so nothing of it joins the letter's group.
    await alicePage.clock.fastForward(600);
    await placeCaret(alicePage, "after", second, optionList(alicePage, 1));
    await alicePage.keyboard.press("ArrowLeft");
    await alicePage.keyboard.type("x");
    await expectSecondList(typed);

    await placeCaret(alicePage, "after", "Which", questionParagraph(alicePage));
    await alicePage.keyboard.press("ControlOrMeta+Z");
    await expectSecondList(second);

    // The undo put the caret back where the letter was; it goes to the question again.
    await placeCaret(alicePage, "after", "Which", questionParagraph(alicePage));
    await alicePage.keyboard.press("ControlOrMeta+Shift+Z");
    await expectSecondList(typed);
    return ["Which one?", first];
  });
});
