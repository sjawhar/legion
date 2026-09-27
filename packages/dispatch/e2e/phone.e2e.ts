import { expect, type Locator, type Page, test } from "@playwright/test";
import { setLiveSessions } from "./agents";

import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getAsk,
} from "./api";
import { actionBar, barAction, documentEditor, selectEditorText } from "./editor";
import { insertExternalLink, resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-phone" },
  as: "agent" as const,
};
const initialMarkdown =
  "The quick brown fox jumps over the lazy dog\n\n" +
  "| One | Two | Three | Four | Five | Six |\n" +
  "| --- | --- | --- | --- | --- | --- |\n" +
  "| 1 | 2 | 3 | 4 | 5 | 6 |";
const pinnedMessage = "Pin me before you forget";

function turn(page: Page, text: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li", { hasText: text });
}

async function activeElementInside(page: Page, selector: string): Promise<boolean> {
  return page.evaluate((sel) => {
    const container = document.querySelector(sel);
    return container?.contains(document.activeElement) ?? false;
  }, selector);
}

async function assertTabTrapped(page: Page, selector: string): Promise<void> {
  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.press("Tab");
    expect(await activeElementInside(page, selector)).toBe(true);
  }
  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.press("Shift+Tab");
    expect(await activeElementInside(page, selector)).toBe(true);
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the phone shell traps focus, dismisses on Escape at the right nesting level, never wraps a key, and renders a pinned event's own description", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Cache invalidation issue with a long descriptive title",
  });
  await createMessage(issue.key, { body: pinnedMessage }, session);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();

    // D13: the phone drawer moves focus in, traps Tab in both directions, and restores
    // focus to Menu on Escape. D17: the project key renders on a single line.
    if (isPhone) {
      await page.goto("/");
      const menuButton = page.getByRole("button", { name: "Open navigation" });
      await menuButton.click();
      const closeButton = page.getByRole("button", { name: "Close navigation" });
      await expect(closeButton).toBeFocused();
      await assertTabTrapped(page, '[aria-label="Navigation"]');

      const keySpan = page.getByText("CORE", { exact: true });
      const [keyBox, lineHeight] = await Promise.all([
        keySpan.boundingBox(),
        keySpan.evaluate((element) => Number.parseFloat(getComputedStyle(element).lineHeight)),
      ]);
      expect(keyBox?.height ?? Number.POSITIVE_INFINITY).toBeLessThan(lineHeight * 1.5);

      await page.keyboard.press("Escape");
      await expect(closeButton).toBeHidden();
      await expect(menuButton).toBeFocused();
    }
    const openAsk = await createAsk(
      issue.key,
      { question: "Is this landing view ready?" },
      session
    );

    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");
    if (isPhone) {
      const reviewToggle = page.getByRole("button", { name: "Open review panel (1 open ask)" });
      await expect(reviewToggle).toBeVisible();
      await reviewToggle.click();
      const askCard = page
        .getByRole("region", { name: "Needs you" })
        .getByTestId(`ask-${openAsk.id}`);
      await askCard.getByRole("button", { name: "Add a note or answer in your own words" }).click();
      await askCard.getByLabel("Your answer").fill("Yes.");
      await askCard.getByRole("button", { exact: true, name: "Answer" }).click();
      await expect
        .poll(() => getAsk(openAsk.id))
        .toMatchObject({
          ask: { answer: { text: "Yes.", user: "alice" }, state: "answered" },
        });
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }
    await expect(page.getByRole("tabpanel", { name: "Spec" }).getByRole("article")).toContainText(
      "The quick brown fox jumps over the lazy dog"
    );
    await expect(documentEditor(page).locator("table")).toBeVisible();

    // D11: choosing Comment on a selection autofocuses the composer's body field.
    await selectEditorText(page, "brown");
    if (isPhone) {
      for (const button of await actionBar(page).getByRole("button").all()) {
        const box = await button.boundingBox();
        expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
        expect(box?.width ?? 0).toBeGreaterThanOrEqual(44);
      }
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
    }
    await barAction(page, "Comment");
    const commentComposer = page.getByRole("form", { name: "Comment composer" });
    const commentBody = commentComposer.getByLabel("Comment");
    await expect(commentBody).toBeFocused();

    // D43: on phone the sheet the composer opened in also traps Tab.
    if (isPhone) {
      await assertTabTrapped(page, '[data-testid="margin-sheet"]');
      await commentBody.focus();
    }

    // D12: Ctrl+K opens a real dialog; Escape dismisses only the picker, restores focus to
    // the composer, and leaves the composer's own draft (and, on phone, the sheet) open.
    await commentBody.press("Control+k");
    const picker = page.getByRole("dialog", { name: "Reference picker" });
    await expect(picker).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(picker).toHaveCount(0);
    await expect(commentComposer).toBeVisible();
    await expect(commentBody).toBeFocused();

    // D11: Escape on a non-empty draft shows an inline "Discard draft?" affordance (not a
    // blocking confirm()); Escape again closes it. Only the composer closes — the phone
    // sheet (if any) stays open, because the composer's own Escape handler claims it first.
    await commentBody.fill("why?");
    await commentBody.press("Escape");
    await expect(page.getByText("Discard draft?")).toBeVisible();
    await expect(commentComposer).toBeVisible();
    await commentBody.press("Escape");
    await expect(commentComposer).toHaveCount(0);
    if (isPhone) {
      await expect(page.getByRole("button", { name: /Close review panel/ })).toBeVisible();
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }

    // D25: a pinned Conversation turn renders its own description, never the literal "Event <seq>".
    await page.getByRole("tab", { name: "Conversation" }).click();
    const pinnedTurn = turn(page, pinnedMessage);
    await pinnedTurn.hover();
    await pinnedTurn.getByRole("button", { name: "Pin" }).click();
    if (isPhone) {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    await page.getByRole("tab", { name: "Pinned" }).click();
    const margin = page.getByTestId("margin-sheet");
    await expect(margin.getByText(pinnedMessage)).toBeVisible();
    await expect(margin.getByText(/^Event \d+$/)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a project document has no horizontal overflow, 44px controls, and a Comments margin sheet", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: "# Design notes\\n\\nProject document body.",
    name: "Design notes",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/design-notes");
    await expect(documentEditor(page)).toContainText("Project document body.");
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).toBe(true);
    if (testInfo.project.name === "iphone") {
      const controls = await page
        .getByRole("main")
        .locator("button:visible, a:visible, select:visible")
        .all();
      for (const control of controls) {
        const box = await control.boundingBox();
        expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
      }
      await page.getByRole("button", { name: /Open review panel/ }).click();
      await expect(page.getByRole("tab", { name: "Comments" })).toBeVisible();
      await expect(page.getByRole("tab", { name: "Pinned" })).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

test("inline @ autocomplete has touch-sized controls and no overflow on iPhone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the 390 px interaction applies on iPhone");
  await setLiveSessions([
    {
      capabilities: ["aside", "btw"],
      dir: "/w/legion",
      last_seen: 1_700_000_000_000,
      roles: ["legion-planner"],
      session_id: "phone-planner",
      title: "planner",
    },
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone inline mentions" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await composer.getByLabel("Comment").fill("@");
    const picker = composer.getByRole("listbox", { name: "Mention suggestions" });
    await expect(picker).toBeVisible();
    const option = picker.getByRole("option", { exact: true, name: "planner" });
    await expect(option).toBeVisible();
    for (const control of await composer.locator("button:visible, textarea:visible").all()) {
      const box = await control.boundingBox();
      expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
    }
    await option.click();
    await composer.getByRole("button", { name: "Send" }).click();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

// An anchor inside a sentence is part of the sentence. The compact touch-target rule gave every
// anchor a 44px box, so a reply's `view` and a `dispatch://` reference inside a body became
// 44px-tall inline-flex boxes with blank space around them in the middle of a line. The fix is
// vertical padding, not an overlay: padding on an inline element grows the hit box without
// moving the line, and without covering whatever happens to sit within 44px of the link.
test("an inline link keeps its line and grows its hit box without covering its neighbours", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the 390 px interaction applies on iPhone");
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inline links" });
  const ask = await createAsk(
    issue.key,
    { question: "Which transport?" },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );
  // A session's reply to an ask is an activity line - "<author> replied to … view · <time>" -
  // whose `view` is a link in the middle of a sentence.
  await createComment(
    issue.key,
    { ask_id: ask.id, body: "Still measuring." },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );
  // An open ask's question renders in full on the Inbox, so it is where a body with a wrapped
  // link and two stacked links can be measured. A generous hit box would reach the words
  // beside the first and the link above the last.
  const bodyAsk = await createAsk(
    issue.key,
    {
      question: [
        "Read the [handbook for the deployment gate and its rollout rules](https://example.invalid/handbook) before changing it.",
        "",
        "[First choice](https://example.invalid/one)",
        "",
        "[Second choice](https://example.invalid/two)",
      ].join("\n"),
    },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );

  // A message turn is 16px prose on 24px lines - the tightest pitch a body reaches - and a
  // hard break puts two links on consecutive lines of it.
  await createMessage(
    issue.key,
    {
      body: "[Upper line](https://example.invalid/upper)\\\n[Lower line](https://example.invalid/lower)",
    },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("link", { name: "view" }).first()).toBeVisible();
    await expect(page.getByRole("link", { name: "Lower line" }).first()).toBeVisible();
    const line = await page.evaluate(() => {
      // Outside the ask cards: a decision block renders the same body in its own card.
      const links = [...document.querySelectorAll("main a")].filter(
        (link) => link.closest('[data-testid^="ask-"]') === null
      );
      const named = (text: string) => links.find((link) => (link.textContent ?? "") === text);
      const view = named("view");
      const upper = named("Upper line");
      const lower = named("Lower line");
      if (view === undefined || upper === undefined || lower === undefined) {
        throw new Error("the conversation is missing a seeded link");
      }
      const range = document.createRange();
      range.selectNodeContents(upper);
      const upperText = range.getBoundingClientRect();
      const node = document.elementFromPoint(
        upperText.x + upperText.width / 2,
        upperText.bottom - 1
      );
      return {
        lineHeight: (view.parentElement ?? view).getBoundingClientRect().height,
        // How far the lower link's padding box starts below the upper link's own text.
        // Negative is padding sitting on letters a reader is trying to tap.
        lowerClearance: lower.getBoundingClientRect().y - upperText.bottom,
        onUpperTextBottom:
          node === null ? "null" : (node.closest("a")?.textContent ?? "not-a-link"),
        viewHeight: view.getBoundingClientRect().height,
      };
    });

    await page.goto("/");
    const card = page.getByTestId(`ask-${bodyAsk.id}`);
    await expect(card.getByRole("link", { name: "Second choice" })).toBeVisible();

    const measured = await page.evaluate(() => {
      const links = [...document.querySelectorAll("main a")];
      const named = (text: string) =>
        links.find((link) => (link.textContent ?? "").startsWith(text));
      const wrapped = named("handbook for the deployment gate");
      const first = named("First choice");
      const second = named("Second choice");
      if (wrapped === undefined || first === undefined || second === undefined) {
        throw new Error("the seeded links are not all rendered");
      }
      const hitAt = (x: number, y: number) => {
        const node = document.elementFromPoint(x, y);
        return node === null ? "null" : (node.closest("a")?.textContent ?? "not-a-link");
      };
      const firstRect = first.getBoundingClientRect();
      const secondRect = second.getBoundingClientRect();
      // The word the wrapped link's own paragraph opens with.
      const lead = wrapped.parentElement?.firstChild;
      if (lead === null || lead === undefined || lead.nodeType !== Node.TEXT_NODE) {
        throw new Error("the link's paragraph does not open with text");
      }
      const range = document.createRange();
      range.setStart(lead, 0);
      range.setEnd(lead, 4);
      const wordRect = range.getBoundingClientRect();
      return {
        firstBottom: firstRect.y + firstRect.height,
        onFirstLowerEdge: hitAt(
          firstRect.x + firstRect.width / 2,
          firstRect.y + firstRect.height - 1
        ),
        onWord: hitAt(wordRect.x + wordRect.width / 2, wordRect.y + wordRect.height / 2),
        secondTop: secondRect.y,
      };
    });

    // 1. The line the inline link sits in is a line of text: it was 44px as a control box.
    expect(line.lineHeight).toBeLessThanOrEqual(28);
    // 2. Its own hit box clears the 24px WCAG 2.5.8 target.
    expect(line.viewHeight).toBeGreaterThanOrEqual(24);
    // 3. On consecutive lines of one message turn - 16px prose on 24px lines, the tightest
    //    pitch a body reaches - the lower link's padding starts below the upper link's own
    //    text, so a tap at the bottom of that text opens the upper link.
    expect(line.lowerClearance).toBeGreaterThanOrEqual(0);
    expect(line.onUpperTextBottom).toBe("Upper line");
    // 4. A tap on the word before a wrapped link stays on the text.
    expect(measured.onWord).toBe("not-a-link");
    // 5. Two stacked links: the lower edge of the first is the first, and it never reaches the
    //    second.
    expect(measured.onFirstLowerEdge).toBe("First choice");
    expect(measured.firstBottom).toBeLessThanOrEqual(measured.secondTop);
  } finally {
    await context.close();
  }
});

/** The name is cut, and whatever cuts it draws an ellipsis. `text-overflow` applies to a block
 *  container's own text, never to a flex container's, and below 1280 px every link is an
 *  inline-flex box: a `truncate` link there clipped its name mid-word with nothing to say so. */
async function expectEllipsis(link: Locator): Promise<void> {
  const state = await link.evaluate((node) => {
    const clippers = [node, ...node.querySelectorAll("*")].filter(
      (element) =>
        element.scrollWidth > element.clientWidth &&
        getComputedStyle(element).overflowX === "hidden"
    );
    const container = node.parentElement?.getBoundingClientRect();
    return {
      clipperStyles: clippers.map((element) => {
        const style = getComputedStyle(element);
        return `${style.display}/${style.textOverflow}`;
      }),
      overflowsContainer:
        container === undefined || node.getBoundingClientRect().right > container.right + 0.5,
    };
  });
  expect(state.overflowsContainer).toBe(false);
  expect(state.clipperStyles.length).toBeGreaterThan(0);
  for (const style of state.clipperStyles) {
    expect(style).toMatch(/^(block|inline-block)\/ellipsis$/);
  }
}

test("a long name ends in an ellipsis wherever a link truncates it", async ({
  browser,
}, testInfo) => {
  const longName =
    "legion-go-coordinator-stage-4b-sandbox-tree-runbook-with-every-checkpoint-and-the-evidence-each-one-left-on-the-production-cluster.md";
  const longQuestion =
    "Which checkpoint gates the sandbox tree before the daemon restarts mid-tree: the fence, the node release, or the linger close that follows both of them?";
  const longUrl = `https://docs.example.com/runbooks/legion/go-coordinator/stage-4b/${"sandbox-tree-".repeat(4)}checkpoints`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stage 4b runbook" });
  await createIssueArtifact(issue.key, { content: "# Runbook\n", name: longName });
  const document = await createProjectDocument("CORE", { content: "# Runbook\n", name: longName });
  await createArtifactAsk(
    document.artifact.id,
    { question: "Does this runbook cover it?" },
    session
  );
  await insertExternalLink(issue.key, longUrl);
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown: `:::ask{#gate urgency="high" multiple="false" state="open"}\n${longQuestion}\n\n- Fence\n- Release\n:::\n`,
          op: "insert",
        },
      ],
    },
    session
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ height: 900, width: 1280 });
  }
  try {
    // The project's Documents list: the name ends in an ellipsis, and its kind and time share
    // one line rather than stacking in a second column.
    await page.goto("/projects/CORE/documents");
    const row = page.getByRole("listitem", { name: longName });
    await expectEllipsis(row.getByRole("link", { name: longName }));
    const details = await row.evaluate((node) => {
      const time = node.querySelector("time");
      const kind = [...node.querySelectorAll("span")].find((span) => span.textContent === "doc");
      if (time === null || kind === undefined) {
        throw new Error("the row has no kind or time");
      }
      return { kind: kind.getBoundingClientRect().top, time: time.getBoundingClientRect().top };
    });
    expect(Math.abs(details.kind - details.time)).toBeLessThanOrEqual(4);

    await page.goto(`/issues/${issue.key}/artifacts`);
    await expectEllipsis(page.getByRole("link", { exact: true, name: longName }));

    await page.goto("/?view=everyone");
    await expectEllipsis(page.locator("[data-inbox-owner]", { hasText: longName }));

    await page.goto(`/issues/${issue.key}`);
    await expectEllipsis(page.getByRole("link", { name: longUrl }));
    await expectEllipsis(
      page
        .getByRole("navigation", { name: "Open decisions" })
        .getByRole("link", { name: longQuestion })
    );
  } finally {
    await context.close();
  }
});
