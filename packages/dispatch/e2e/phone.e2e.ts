import { expect, type Locator, type Page, test } from "@playwright/test";
import { setLiveSessions } from "./agents";

import {
  createArtifactAsk,
  createAsk,
  createBroadcast,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getAsk,
  patchIssue,
} from "./api";
import { actionBar, barAction, documentEditor, needsYouCards, selectEditorText } from "./editor";
import { expectConsecutiveLinksClear } from "./layout";
import { insertExternalLink, resetDatabase } from "./seed";
import { refusePosts, WRITE_REFUSED_TEXT } from "./sends";
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
      const askCard = needsYouCards(page).getByTestId(`ask-${openAsk.id}`);
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

test("a phone thread trap skips a held composer's disabled controls", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Held composer focus trap",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    await selectEditorText(page, "brown");
    await barAction(page, "Comment");
    const sheet = page.getByTestId("margin-sheet");
    const form = sheet.getByRole("form", { name: "Comment composer" });
    const body = form.getByLabel("Comment");
    const refuse = await refusePosts(page, "**/api/v1/issues/*/comments");
    await body.fill("Keep this draft");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(body).toBeDisabled();

    const focused = await sheet.evaluate((container) => {
      const controls = [
        ...container.querySelectorAll<HTMLElement>(
          'a[href], button, textarea, input, select, [tabindex]:not([tabindex="-1"])'
        ),
      ];
      const last = controls.filter((control) => !control.matches(":disabled")).at(-1);
      if (last === undefined) throw new Error("expected an enabled focus target");
      last.focus();
      return {
        focused: document.activeElement === last,
        hasDisabledControlAfter: controls
          .slice(controls.indexOf(last) + 1)
          .some((control) => control.matches(":disabled")),
      };
    });
    expect(focused).toEqual({ focused: true, hasDisabledControlAfter: true });
    await page.keyboard.press("Tab");
    expect(await activeElementInside(page, '[data-testid="margin-sheet"]')).toBe(true);
    expect(
      await sheet.evaluate(
        (container) =>
          document.activeElement instanceof HTMLElement &&
          container.contains(document.activeElement) &&
          !document.activeElement.matches(":disabled")
      )
    ).toBe(true);
    refuse();
  } finally {
    await alice.close();
  }
});

// The wrap's other end. Shift+Tab from the sheet's first control goes to its last enabled one,
// past the held composer's disabled controls, which a trap that read the `disabled` attribute
// counted as the last and could not focus, leaving focus where it was.
test("a phone sheet trap wraps Shift+Tab from its first control past a held composer", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Held composer reverse focus trap",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    await selectEditorText(page, "brown");
    await barAction(page, "Comment");
    const sheet = page.getByTestId("margin-sheet");
    const form = sheet.getByRole("form", { name: "Comment composer" });
    const body = form.getByLabel("Comment");
    const refuse = await refusePosts(page, "**/api/v1/issues/*/comments");
    await body.fill("Keep this draft");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(body).toBeDisabled();

    // The last control a reader can reach - enabled and on screen - is marked before the key.
    const startedOnFirst = await sheet.evaluate((container) => {
      const enabled = [
        ...container.querySelectorAll<HTMLElement>(
          'a[href], button, textarea, input, select, [tabindex]:not([tabindex="-1"])'
        ),
      ].filter((control) => !control.matches(":disabled") && control.getClientRects().length > 0);
      const first = enabled[0];
      const last = enabled.at(-1);
      if (first === undefined || last === undefined || first === last)
        throw new Error("expected two enabled focus targets");
      last.dataset.e2eLastControl = "";
      first.focus();
      return document.activeElement === first;
    });
    expect(startedOnFirst).toBe(true);
    await page.keyboard.press("Shift+Tab");
    expect(
      await page.evaluate(
        () => document.activeElement?.hasAttribute("data-e2e-last-control") ?? false
      )
    ).toBe(true);
    await expect(body).toHaveValue("Keep this draft");
    refuse();
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(body).toHaveValue("Keep this draft");
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

// An anchor inside a sentence is part of the sentence. A 44px touch-target box on every anchor
// would make a reply's `view` and a `dispatch://` reference inside a body 44px-tall inline-flex
// boxes with blank space around them in the middle of a line, so the hit box grows by vertical
// padding, not an overlay: padding on an inline element grows the hit box without moving the
// line, and without covering whatever happens to sit within 44px of the link.
// The row places the turn itself and then waits for it to stop moving: the Conversation's jump
// to its newest turn lands well after a link first has a box, and the row measures what it
// placed rather than trusting the page's own placement, since a turn left under the fixed comment
// composer would have a hit test read the composer and not the link.
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
  // link, two stacked links and two links on consecutive lines can be measured. A generous hit
  // box would reach the words beside the first, the link below the stacked pair's upper one, and
  // the text of the line above the last. A question reads a single newline as a space, so the
  // last pair is split by a hard break.
  const bodyAsk = await createAsk(
    issue.key,
    {
      question: [
        "Read the [handbook for the deployment gate and its rollout rules](https://example.invalid/handbook) before changing it.",
        "",
        "[First choice](https://example.invalid/one)",
        "",
        "[Second choice](https://example.invalid/two)",
        "",
        "[Upper choice](https://example.invalid/upper-choice)\\\n[Lower choice](https://example.invalid/lower-choice)",
      ].join("\n"),
    },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );

  // A message turn is 16px prose on 24px lines, and a hard break puts two links on consecutive
  // lines of it.
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
    // The `view` link is an event line's - "<author> replied to … view" - and the event lines
    // are the Activity tab's. The line it sits in is measured as a line box rather than as its
    // paragraph's height: the row wraps the author, the description and the pin across flex
    // lines, and a paragraph two lines tall says nothing about whether the link moved one.
    await page.goto(`/issues/${issue.key}/activity`);
    await expect(page.getByRole("link", { name: "view" }).first()).toBeVisible();
    const inline = await page.evaluate(() => {
      const view = [...document.querySelectorAll("main a")].find(
        (link) => (link.textContent ?? "") === "view"
      );
      if (view === undefined) {
        throw new Error("the activity is missing its seeded view link");
      }
      const range = document.createRange();
      range.selectNodeContents(view.parentElement ?? view);
      // One rect per line box the description occupies: an inline-flex control box would be in
      // the line box, a padded inline link is not.
      const lines = [...range.getClientRects()].map((rect) => rect.height);
      if (lines.length === 0) {
        throw new Error("the activity line the view link sits in has no line box");
      }
      return {
        lineHeight: Math.max(...lines),
        viewHeight: view.getBoundingClientRect().height,
      };
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("link", { name: "Lower line" }).first()).toBeVisible();
    // The jump to the newest turn, and the unread "New" marks clearing after it, both move the
    // turn after `toBeVisible()`: put the upper link in the middle of the viewport and hold
    // there until its rect has not moved for 1.5 s, so the measurement is taken at a position
    // this row chose.
    await page.evaluate(async () => {
      // The same link the measurement below takes: outside the ask cards, where a decision
      // block renders the same body in its own card.
      const upper = [...document.querySelectorAll("main a")]
        .filter((link) => link.closest('[data-testid^="ask-"]') === null)
        .find((link) => (link.textContent ?? "") === "Upper line");
      if (upper === undefined) {
        throw new Error("the conversation is missing its seeded upper link");
      }
      upper.scrollIntoView({ block: "center" });
      // 30 samples 50 ms apart, the counter reset whenever the rect moves, bounded at 20 s.
      let stable = 0;
      let previous = upper.getBoundingClientRect().y;
      for (let sample = 0; sample < 400; sample += 1) {
        const tick = Promise.withResolvers<void>();
        setTimeout(tick.resolve, 50);
        await tick.promise;
        const y = upper.getBoundingClientRect().y;
        stable = y === previous ? stable + 1 : 0;
        previous = y;
        if (stable >= 30) {
          return;
        }
      }
      throw new Error("the seeded upper link never stopped moving");
    });
    const line = await page.evaluate(() => {
      // Outside the ask cards: a decision block renders the same body in its own card.
      const links = [...document.querySelectorAll("main a")].filter(
        (link) => link.closest('[data-testid^="ask-"]') === null
      );
      const upper = links.find((link) => (link.textContent ?? "") === "Upper line");
      const lower = links.find((link) => (link.textContent ?? "") === "Lower line");
      if (upper === undefined || lower === undefined) {
        throw new Error("the conversation is missing a seeded link");
      }
      const range = document.createRange();
      range.selectNodeContents(upper);
      const upperText = range.getBoundingClientRect();
      const x = upperText.x + upperText.width / 2;
      // One pixel below the link's own text, and one pixel above its bottom edge: both points
      // are the text's, never the anchor's padded box, which would move with the padding they
      // are there to measure.
      const probeY = upperText.bottom + 1;
      // Playwright measures a click `position` from the padding box's top-left corner, so the
      // points are returned as offsets inside it. That is what makes the lower one
      // mutation-sensitive: the point stays on the page where it is, and without the padding
      // the box shrinks above it and no longer receives it.
      const box = upper.getBoundingClientRect();
      const style = getComputedStyle(upper);
      const padTop = box.top + Number.parseFloat(style.borderTopWidth);
      const padLeft = box.left + Number.parseFloat(style.borderLeftWidth);
      // The composer is a fixed box over the foot of the page: a probe under it reads the
      // composer whatever the link does, so the probe point has to clear it. The selector is
      // the product's own (`use-follow-latest.ts`): not finding it is a failure, not clearance.
      const composer = document.querySelector('[aria-label="Comment composer"]');
      return {
        hitX: x - padLeft,
        inPaddingY: probeY - padTop,
        // How far the lower link's padding box starts below the upper link's own text.
        // Negative is padding sitting on letters a reader is trying to tap.
        lowerClearance: lower.getBoundingClientRect().y - upperText.bottom,
        onTextBottomY: upperText.bottom - 1 - padTop,
        probeClear:
          composer !== null &&
          x >= 0 &&
          x <= window.innerWidth &&
          probeY >= 0 &&
          probeY <= window.innerHeight &&
          probeY < composer.getBoundingClientRect().top,
      };
    });

    // Those points were taken on screen: inside the viewport, and above the composer's fixed
    // box, so what the clicks below hit is the turn.
    expect(
      line.probeClear,
      'the turn was left under the fixed comment composer, so the probe measured the composer - or [aria-label="Comment composer"] is gone'
    ).toBe(true);
    // On consecutive lines of one message turn - 16px prose on 24px lines - the lower link's
    // padded box starts at least two pixels below the upper link's own text, which is the
    // margin the probe at `upperText.bottom + 1` needs.
    expect(
      line.lowerClearance,
      "the lower link's padded box starts within a pixel of the upper link's own text, so the probe one pixel below that text is inside the lower link, and the padding check below would go red for the pitch rather than for the padding"
    ).toBeGreaterThanOrEqual(2);

    // The same link the measurement took: the seeded one outside the ask cards. Its hit tests
    // are Playwright's own actionability check rather than an `elementFromPoint` snapshot - a
    // trial click waits and retries until the link is attached, visible, stable and is what
    // the action point hits, and then performs no click - bounded so a refusal is prompt.
    const upperLink = page
      .locator('main a:not([data-testid^="ask-"] a)')
      .filter({ hasText: /^Upper line$/ });
    await expect(upperLink).toHaveCount(1);
    // On consecutive lines of one message turn - 16px prose on 24px lines - a tap at the bottom
    // of the upper link's own text is taken by the upper link.
    await upperLink.click({
      position: { x: line.hitX, y: line.onTextBottomY },
      timeout: 5_000,
      trial: true,
    });
    // One pixel below that text is still inside the upper link's hit box: this trial click is
    // the check that goes red when the padding of `.prose a { padding-block }` is gone.
    await upperLink.click({
      position: { x: line.hitX, y: line.inPaddingY },
      timeout: 5_000,
      trial: true,
    });

    // A real finger at the padded point, which no hit test can speak for: the click is
    // recorded in the capture phase - before anything on the way can stop it - and not
    // followed.
    await page.evaluate(() => {
      document.addEventListener(
        "click",
        (event) => {
          const target = event.target;
          document.documentElement.dataset.e2eTappedLink =
            target instanceof Element ? (target.closest("a")?.textContent ?? "not-a-link") : "none";
          event.preventDefault();
        },
        true
      );
    });
    await upperLink.tap({ position: { x: line.hitX, y: line.inPaddingY } });
    // And a finger there reaches the link, which an actionability check does not say. The
    // click Chromium synthesises from the touch sequence need not be dispatched in the task
    // that resolves the tap, so the read-back is polled.
    await expect
      .poll(() => page.evaluate(() => document.documentElement.dataset.e2eTappedLink ?? "no-click"))
      .toBe("Upper line");

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

    // The probe, the two trial clicks and the tap are asserted above, each beside what it
    // measures. What is left:
    // 1. The line the inline link sits in is a line of text: it was 44px as a control box.
    expect(inline.lineHeight).toBeLessThanOrEqual(28);
    // 2. Its own hit box clears the 24px WCAG 2.5.8 target.
    expect(inline.viewHeight).toBeGreaterThanOrEqual(24);
    // 3. A tap on the word before a wrapped link stays on the text - a claim that nothing is
    //    there, which a trial click cannot express, so it stays a hand-made hit test.
    expect(measured.onWord).toBe("not-a-link");
    // 4. Two stacked links: the lower edge of the first is the first, and it never reaches the
    //    second.
    expect(measured.onFirstLowerEdge).toBe("First choice");
    expect(measured.firstBottom).toBeLessThanOrEqual(measured.secondTop);
    // 5. Two choices a hard break puts on consecutive lines of the card's 15px text on 24.375px
    //    lines (`text-[15px] leading-relaxed`): the lower one's padded box starts at or below
    //    the upper one's own text.
    await expectConsecutiveLinksClear(card, "Upper choice", "Lower choice", {
      fontSize: 15,
      lineHeight: 24.375,
    });
  } finally {
    await context.close();
  }
});

/** The name is cut, and whatever cuts it draws an ellipsis. `text-overflow` applies to a block
 *  container's own text, never to a flex container's, and below 1280 px every link is an
 *  inline-flex box: a `truncate` link there clipped its name mid-word with nothing to say so.
 *  An inline box clips nothing (`overflow` does not apply to it), so it is not counted: from
 *  1280 px a link is a block and the `TruncatedText` span inside it stays inline, and Firefox
 *  reports that span's `scrollWidth` as its text's width beside a `clientWidth` of 0, where
 *  Chromium and WebKit report 0 for both. A `MarkdownPreview` clips with a line clamp instead
 *  (`-webkit-line-clamp`, which draws its own ellipsis at the last line and overflows
 *  vertically rather than horizontally); Chromium reports the clamped box's display as
 *  `flow-root`, so the clamp is read from its own property. */
async function expectEllipsis(link: Locator): Promise<void> {
  const state = await link.evaluate((node) => {
    const clippers = [node, ...node.querySelectorAll("*")].filter((element) => {
      const style = getComputedStyle(element);
      const clamped = style.webkitLineClamp !== "none";
      return (
        style.display !== "inline" &&
        (clamped
          ? element.scrollHeight > element.clientHeight
          : element.scrollWidth > element.clientWidth) &&
        (clamped ? style.overflow === "hidden" : style.overflowX === "hidden")
      );
    });
    const container = node.parentElement?.getBoundingClientRect();
    return {
      clipperStyles: clippers.map((element) => {
        const style = getComputedStyle(element);
        return style.webkitLineClamp === "none"
          ? `${style.display}/${style.textOverflow}`
          : `clamp/${style.webkitLineClamp}`;
      }),
      overflowsContainer:
        container === undefined || node.getBoundingClientRect().right > container.right + 0.5,
    };
  });
  expect.soft(state.overflowsContainer).toBe(false);
  expect.soft(state.clipperStyles.length).toBeGreaterThan(0);
  for (const style of state.clipperStyles) {
    expect.soft(style).toMatch(/^((block|inline-block)\/ellipsis|clamp\/\d+)$/);
  }
}

/** A control whose lines are meant to stack: each child starts at the same left edge, below the
 *  one before it. Below 1280 px `styles.css` makes every link and button an inline-flex row,
 *  which puts a `block` control's lines side by side. */
async function expectStacked(control: Locator): Promise<void> {
  const rows = await control.evaluate((node) =>
    [...node.children].map((child) => {
      const box = child.getBoundingClientRect();
      return { bottom: box.bottom, left: box.left, top: box.top };
    })
  );
  expect.soft(rows.length).toBeGreaterThan(1);
  for (const [index, row] of rows.entries()) {
    if (index === 0) continue;
    expect.soft(Math.abs(row.left - rows[0].left)).toBeLessThanOrEqual(1);
    expect.soft(row.top).toBeGreaterThanOrEqual(rows[index - 1].bottom - 1);
  }
}

test("a long name ends in an ellipsis wherever a link or control truncates it", async ({
  browser,
}, testInfo) => {
  test.setTimeout(90_000);
  const longName =
    "legion-go-coordinator-stage-4b-sandbox-tree-runbook-with-every-checkpoint-and-the-evidence-each-one-left-on-the-production-cluster.md";
  const longQuestion =
    "Which checkpoint gates the sandbox tree before the daemon restarts mid-tree: the fence, the node release, or the linger close that follows both of them?";
  const longUrl = `https://docs.example.com/runbooks/legion/go-coordinator/stage-4b/${"sandbox-tree-".repeat(4)}checkpoints`;
  const longRoute = "role:merge-queue-controller-for-legion";
  // A label's 40-character cap is still wider than the label picker's 256 px popover.
  const longLabel = "sandbox-tree-checkpoint-evidence-runbook";
  const longBroadcast =
    "Please rebase every open pull request onto main before the stage 4b sandbox tree run, and post the new head in this thread once its checks have gone green again";
  const longTitle =
    "LEGION-67 implementer: long names end in an ellipsis wherever a link or a control truncates them, on a 390 px phone and in a 280 px margin as much as on a 1280 px desktop";
  const longTitledSession = {
    actor: {
      kind: "session" as const,
      id: "e2e-long-title",
      origin: { session_title: longTitle, tmux: "legion:1.2" },
    },
    as: "agent" as const,
  };
  await setLiveSessions([
    {
      capabilities: ["aside", "btw"],
      dir: "/w/legion",
      last_seen: Date.now(),
      roles: ["legion-implementer"],
      session_id: "e2e-long-title",
      title: longTitle,
    },
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stage 4b runbook" });
  await patchIssue(issue.key, { route: longRoute });
  const labelled = await createIssue({ project: "CORE", title: "Labelled" });
  await patchIssue(labelled.key, { labels: [longLabel] });
  await createIssueArtifact(issue.key, { content: "# Runbook\n", name: longName });
  const document = await createProjectDocument("CORE", { content: "# Runbook\n", name: longName });
  await createArtifactAsk(
    document.artifact.id,
    { question: "Does this runbook cover it?" },
    longTitledSession
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
  await createBroadcast({ body: longBroadcast, delivery: "btw", session_ids: ["e2e-long-title"] });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ height: 900, width: 1280 });
  }
  try {
    // The project's Documents list: the name ends in an ellipsis, and its kind (with the latest
    // version's size after it) and time share one line rather than stacking in a second column.
    await page.goto("/projects/CORE/documents");
    const row = page.getByRole("listitem", { name: longName });
    await expectEllipsis(row.getByRole("link", { name: longName }));
    const details = await row.evaluate((node) => {
      const time = node.querySelector("time");
      const kind = [...node.querySelectorAll("span")].find((span) =>
        /^doc · \d/.test(span.textContent ?? "")
      );
      if (time === null || kind === undefined) {
        throw new Error("the row has no kind and size, or no time");
      }
      return { kind: kind.getBoundingClientRect().top, time: time.getBoundingClientRect().top };
    });
    expect.soft(Math.abs(details.kind - details.time)).toBeLessThanOrEqual(4);
    // The cut name is still readable: the full name is the truncated text's title.
    await expect
      .soft(row.getByRole("link", { name: longName }).locator("[title]"))
      .toHaveAttribute("title", longName);

    await page.goto(`/issues/${issue.key}/artifacts`);
    const artifactLink = page.getByRole("link", { exact: true, name: longName });
    await expectEllipsis(artifactLink);
    await expect.soft(artifactLink.locator("[title]")).toHaveAttribute("title", longName);

    // The long name belongs to a project document, whose ask nobody holds: the cleared filter
    // is where the Inbox lists it.
    await page.goto("/?assignee=everyone");
    const inboxOwner = page.locator("[data-inbox-owner]", { hasText: longName });
    await expectEllipsis(inboxOwner);
    // The row already opens a RefPreview hover card (referenceTriggerProps); a native title
    // would draw its own tooltip over that card, so this row carries none.
    await expect.soft(inboxOwner.locator("[title]")).toHaveCount(0);
    // The ask's author chip is a flex button that opens the session's handles.
    const authorChip = page.getByRole("button", { exact: true, name: longTitle });
    await expectEllipsis(authorChip);
    await expect
      .soft(authorChip.getByText(longTitle, { exact: true }))
      .toHaveAttribute("title", `${longTitle} · e2e-long-title`);

    await page.goto(`/issues/${issue.key}`);
    await expectEllipsis(page.getByRole("link", { name: longUrl }));
    await expectEllipsis(
      page
        .getByRole("navigation", { name: "Open decisions" })
        .getByRole("link", { name: longQuestion })
    );
    // A button that is inline-flex by its own classes is the same case as a link below 1280 px.
    await expectEllipsis(page.locator(`button[title="${longRoute}"]`));

    // The @ picker's detail line: a role's detail names the live session that holds it.
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await composer.getByLabel("Comment").fill("@");
    const roleOption = composer
      .getByRole("listbox", { name: "Mention suggestions" })
      .getByRole("option", { exact: true, name: "legion-implementer" });
    await expectEllipsis(roleOption);
    await expectStacked(roleOption);
    const detail = roleOption.getByText(new RegExp(`^${longTitle} · /w/legion · `));
    await expect.soft(detail).toHaveAttribute("title", (await detail.textContent()) ?? "");

    // A label picker row.
    await page.goto(`/issues/${labelled.key}`);
    await page.getByRole("button", { name: "Edit labels" }).click();
    const labelOption = page.getByRole("option", { exact: true, name: longLabel });
    await expectEllipsis(labelOption);
    await expect
      .soft(labelOption.getByText(longLabel, { exact: true }))
      .toHaveAttribute("title", longLabel);

    // A sent broadcast's row: the metadata line, then the message's first line under it, cut
    // by a one-line clamp. Its hover title is the message as plain words (the preview renders
    // the Markdown, so the source's syntax is never what the reader hovers over).
    await page.goto("/agents/broadcasts");
    const broadcastRow = page
      .getByRole("region", { name: "Broadcasts" })
      .getByRole("listitem")
      .getByRole("link");
    await expectEllipsis(broadcastRow);
    await expectStacked(broadcastRow);
    await expect
      .soft(broadcastRow.locator("[data-markdown-preview]"))
      .toHaveAttribute("title", longBroadcast);
  } finally {
    await context.close();
    await setLiveSessions([]);
  }
});
