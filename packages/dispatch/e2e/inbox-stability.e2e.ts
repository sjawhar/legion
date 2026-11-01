import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getAsk,
  patchIssue,
} from "./api";
import {
  askCard,
  centreRow,
  hasReadingPane,
  inboxRow,
  listScrollTop,
  selectedRow,
} from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

// Issue titles sharing no terms, so the server's duplicate check never refuses a seed. There
// are enough of them to fill a list past the bottom of the viewport at 1280 px, where the rows
// are short and scroll inside the list column: a row can only be held still where the list has
// somewhere to scroll.
const TITLES = [
  "Anchor",
  "Bridge",
  "Cedar",
  "Dune",
  "Ember",
  "Fjord",
  "Glass",
  "Harbor",
  "Island",
  "Jetty",
  "Kelp",
  "Lantern",
  "Marsh",
  "Nectar",
  "Obsidian",
  "Prairie",
  "Quartz",
  "Ridge",
  "Summit",
  "Thicket",
];

/** `count` open asks, each on its own issue, oldest first; `from` picks the first title. */
async function seedAsks(prefix: string, count: number, from = 0) {
  const asks = [];
  for (let index = 0; index < count; index += 1) {
    const title = TITLES[from + index];
    if (title === undefined) throw new Error("out of issue titles");
    const issue = await createIssue({ project: "CORE", title });
    asks.push({
      ask: await createAsk(
        issue.key,
        {
          options: [{ label: "Ship" }, { label: "Hold" }],
          question: `${prefix} ask ${index + 1}`,
        },
        session
      ),
      issue,
    });
  }
  return asks;
}

async function topOf(locator: Locator): Promise<number> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("row has no box");
  return box.y;
}

async function nextFrame(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const frame = Promise.withResolvers<void>();
    requestAnimationFrame(() => frame.resolve());
    await frame.promise;
  });
}

/**
 * The row the viewport anchor's centre fallback lands on, read the way the anchor reads it
 * (`rowAtCenter`): the probe is the centre of what the list shows, which is the list column's
 * own box where the rows scroll inside one and the window's where they do not.
 */
async function rowAtCentre(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const list = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
    const root = list?.firstElementChild;
    if (list === null || !(root instanceof HTMLElement)) return null;
    const rect = root.getBoundingClientRect();
    const box = list.scrollHeight > list.clientHeight ? list.getBoundingClientRect() : null;
    const top = Math.max(rect.top, box === null ? 0 : box.top);
    const bottom = Math.min(rect.bottom, box === null ? window.innerHeight : box.bottom);
    return (
      document
        .elementFromPoint(rect.left + rect.width / 2, (top + bottom) / 2)
        ?.closest("[data-inbox-row]")
        ?.getAttribute("data-inbox-row") ?? null
    );
  });
}

/** Twelve asks waiting on the reader, then four handed to agents, so Waiting on agents has rows
 *  below the one that will join it and the list scrolls at every width; the third of the
 *  reader's is the one under test. */
const BOTH_SECTIONS_ROWS = 16;
async function seedBothSections() {
  await createProject({ key: "CORE", name: "Core" });
  const waiting = await seedAsks("Yours", 12);
  const target = waiting[2];
  if (target === undefined) throw new Error("target ask missing");
  for (const { ask, issue } of await seedAsks("Theirs", 4, 12)) {
    await createComment(issue.key, { ask_id: ask.id, body: "Clarify first." });
  }
  return target;
}

/**
 * Takes the reader's hand off the row, so the list stops holding it where they last saw it.
 *
 * With the reading column on, the hand is the selection: Escape brings the focus back from the
 * column to the row it is showing, and `j` hands the selection to the next row, leaving the
 * list to place the one let go. Without the column the hand is focus and the pointer, so the
 * pointer goes to the page corner and the focus to nothing - not onto another row, which would
 * make that row the anchor, and not by Escape, whose step back to the row would scroll a tall
 * row into view on its own.
 */
async function leaveRow(page: Page): Promise<void> {
  if (hasReadingPane(page)) {
    await page.keyboard.press("Escape");
    await page.keyboard.press("j");
    return;
  }
  await page.mouse.move(0, 0);
  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.closest("[data-inbox-row]") === null))
    .toBe(true);
}

test("a row being typed into keeps its draft when an agent's progress note flips its turn, and holds its place only while the reader's hand is on it @phone", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(BOTH_SECTIONS_ROWS);
    const row = inboxRow(page, target.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await centreRow(row);
    // The question's own card, where this width draws it: in the row, or in the reading column
    // once the row is selected - which is also what holds the row there.
    const field = (await askCard(page, target.ask.id)).getByLabel("Your answer");
    await field.click();
    await field.fill("Ship it once the audit lands");
    const before = await topOf(row);
    const scrolled = await listScrollTop(page);

    await createComment(
      target.issue.key,
      { ask_id: target.ask.id, body: "Checking the audit now.", turn: "agent" },
      session
    );

    // The card says whose turn it is now, and what the reader is typing is untouched by it.
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );
    await expect(field).toHaveValue("Ship it once the audit lands");
    await expect(field).toBeFocused();
    if (hasReadingPane(page)) {
      // The question is in the column, so the reader's hand is on no row at all: the row joins
      // Waiting on agents at once, telling the truth about whose turn it is, and the column
      // keeps the question and the half-written answer where they are (INFRA-575). The view
      // does not follow the row down.
      await expect(row).toHaveAttribute("data-inbox-section", "agent");
      expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
      return;
    }
    // Without the column the card is in the row, under the reader's hand: the row stays where
    // they are typing.
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);

    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
    // The card never left the row it moved with, so what was typed into it is still there.
    await expect(field).toHaveValue("Ship it once the audit lands");
  } finally {
    await alice.close();
  }
});

test("the reader's own Ask back leaves the view still and holds the row only where their hand is @phone", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(BOTH_SECTIONS_ROWS);
    const row = inboxRow(page, target.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    // Scrolled, so the centre fallback is live too: on release nothing may follow the row down.
    await centreRow(row);
    const card = await askCard(page, target.ask.id);
    const field = card.getByLabel("Your answer");
    await field.click();
    await field.fill("Ship what, exactly?");
    const askBack = card.getByRole("button", { name: "Ask back" });
    // Measured with the button already in view, so the click itself scrolls nothing.
    await askBack.hover();
    const before = await topOf(row);
    const scrolled = await listScrollTop(page);
    expect(scrolled).toBeGreaterThan(0);

    await askBack.click();
    await expect(
      card.getByTestId(`thread-${target.ask.id}`).getByText("Ship what, exactly?")
    ).toBeVisible();
    await expect.poll(() => getAsk(target.ask.id)).toMatchObject({ ask: { waiting_on: "agent" } });
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );

    if (hasReadingPane(page)) {
      // The question is in the column, which stays on it: the selection is the reader's and
      // only they move it. The row beside it joins Waiting on agents at once - their hand is in
      // the column, on no row (INFRA-575) - and the view does not follow it.
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", target.ask.id);
      await expect(row).toHaveAttribute("data-inbox-section", "agent");
      expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
      return;
    }
    // Without the column the row is still under Waiting on you, where the reader's hand is -
    // the focus Ask back handed back to the row as it disabled itself - and the view has not
    // moved after it.
    await expect(row).toBeFocused();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    const afterAskBack = await listScrollTop(page);
    expect(Math.abs(afterAskBack - scrolled)).toBeLessThanOrEqual(2);

    // Letting go of it: it joins Waiting on agents below; the view stays where the reader was.
    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the row at the viewport centre stays put when a P0 ask arrives above it @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Queue", 16);
  const sixth = asks[5];
  if (sixth === undefined) throw new Error("sixth ask missing");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(16);
    const row = inboxRow(page, sixth.ask.id);
    await centreRow(row);
    const before = await topOf(row);
    expect(before).toBeGreaterThan(0);

    const urgent = await createIssue({ project: "CORE", title: "Meridian" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Urgent ask" }, session);

    const arrived = inboxRow(page, p0.id);
    await expect(arrived).toBeAttached();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(17);
    expect(await topOf(arrived)).toBeLessThan(before);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the row under the pointer stays put when a row above it is answered elsewhere, and is let go once the pointer leaves the list @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Long enough to scroll in whichever box holds it, with rows above and below each of the four
  // below, so every one of them can be brought to the centre of what is visible.
  const asks = await seedAsks("Queue", 20);
  /** Newest first in the Inbox, so a row's place is counted back from the last ask seeded. */
  const atRow = (fromTop: number) => {
    const ask = asks[asks.length - 1 - fromTop];
    if (ask === undefined) throw new Error("ask missing");
    return ask;
  };
  const above = atRow(4);
  const centreAsk = atRow(9);
  const between = atRow(10);
  const held = atRow(11);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(20);
    const row = inboxRow(page, held.ask.id);
    await centreRow(row);
    const box = await row.boundingBox();
    if (box === null) throw new Error("row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + 8);
    const before = box.y;

    await answerAsk(
      above.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );

    await expect(inboxRow(page, above.ask.id)).toHaveCount(0);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);

    // With the pointer off the list, the anchor falls back to the row nearest the centre. That
    // row sits above the one the pointer left with one row between them: answering the row
    // between leaves the centre row still and the row the pointer left rising into the gap -
    // were the pointer still counted, it would hold and the centre row drop.
    await page.mouse.move(0, 0);
    const centred = inboxRow(page, centreAsk.ask.id);
    await centreRow(centred);
    await expect.poll(() => rowAtCentre(page)).toBe(centreAsk.ask.id);
    const leftTop = await topOf(row);
    const centreTop = await topOf(centred);
    await answerAsk(
      between.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(inboxRow(page, between.ask.id)).toHaveCount(0);
    expect(Math.abs((await topOf(centred)) - centreTop)).toBeLessThanOrEqual(2);
    expect(leftTop - (await topOf(row))).toBeGreaterThan(2);
  } finally {
    await alice.close();
  }
});

test("a grouped header stays with its pointer-held row while a P0 ask arrives", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Older rows under the pair, so the list runs past the bottom of the viewport and has
  // somewhere to scroll when the arrival pushes the pair down.
  await seedAsks("Below", 14);
  const grouped = await createIssue({ project: "CORE", title: "Grouped stability" });
  await createAsk(grouped.key, { question: "Grouped first" }, session);
  const held = await createAsk(grouped.key, { question: "Grouped held" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = inboxRow(page, held.id);
    const header = page.locator(`[data-inbox-group-header="issue:${grouped.key}"]`);
    await expect(row).toBeVisible();
    await expect(header).toBeVisible();
    const box = await row.boundingBox();
    if (box === null) throw new Error("held row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    const rowBefore = await topOf(row);
    const headerBefore = await topOf(header);

    const urgent = await createIssue({ project: "CORE", title: "Ungrouped P0 arrival" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Urgent arrival" }, session);

    await expect(inboxRow(page, p0.id)).toBeAttached();
    expect(Math.abs((await topOf(row)) - rowBefore)).toBeLessThanOrEqual(2);
    expect(Math.abs((await topOf(header)) - headerBefore)).toBeLessThanOrEqual(2);

    await leaveRow(page);
  } finally {
    await alice.close();
  }
});

async function seedTravellingPointerGroup() {
  await createProject({ key: "CORE", name: "Core" });
  const grouped = await createIssue({ project: "CORE", title: "Grouped travelling pointer asks" });
  await patchIssue(grouped.key, { priority: 1 });
  await createAsk(grouped.key, { question: "Travelling pointer grouped first" }, session);
  await createAsk(grouped.key, { question: "Travelling pointer grouped second" }, session);
  await seedAsks("Travelling pointer", 6);
}

async function travelPointerAcrossIncomingRow(page: Page): Promise<void> {
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(8);
  expect(await listScrollTop(page)).toBe(0);
  const sixth = rows.nth(5);
  const initialSixthBox = await sixth.boundingBox();
  const viewport = page.viewportSize();
  if (initialSixthBox === null || viewport === null) throw new Error("sixth row has no box");
  // Keep the destination on screen while leaving rows below it, so the anchor can still scroll.
  await page.setViewportSize({
    height: Math.ceil(initialSixthBox.y + initialSixthBox.height + 40),
    width: viewport.width,
  });
  const fourth = await rows.nth(3).boundingBox();
  const fifth = await rows.nth(4).boundingBox();
  const sixthBox = await sixth.boundingBox();
  if (fourth === null || fifth === null || sixthBox === null)
    throw new Error("Inbox row has no box");
  const sixthID = await sixth.getAttribute("data-inbox-row");
  if (sixthID === null) throw new Error("sixth row has no id");
  const destination = page.locator(`[data-inbox-row="${sixthID}"]`);
  const sixthBefore = await topOf(destination);
  const x = sixthBox.x + sixthBox.width / 2;
  const gap = (fourth.y + fourth.height + fifth.y) / 2;
  const requested = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const delivered = Promise.withResolvers<void>();
  let hold = true;
  await page.route("**/api/v1/inbox", async (route) => {
    const response = await route.fetch();
    const listed = (await response.json()) as Array<{ question?: string }>;
    if (!hold || !listed.some((row) => row.question === "Travelling pointer arrival")) {
      await route.fulfill({ json: listed, response });
      return;
    }
    hold = false;
    requested.resolve();
    await release.promise;
    await route.fulfill({ json: listed, response });
    delivered.resolve();
  });

  try {
    for (const y of [fourth.y - 20, fourth.y + fourth.height / 2, gap]) {
      await page.mouse.move(x, y);
      await nextFrame(page);
    }

    const urgent = await createIssue({ project: "CORE", title: "Travelling pointer P0" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Travelling pointer arrival" }, session);
    await requested.promise;
    // Seeding can outlast the hold; this last move puts the refetch inside the pointer's travel.
    await page.mouse.move(x, gap + 1);
    await nextFrame(page);
    release.resolve();
    await delivered.promise;
    // Keep travelling inside the gap while the released response renders. Each move is still pointer
    // motion, but none rests on a row the viewport anchor could hold.
    for (let step = 0; step < 10; step += 1) {
      await page.mouse.move(x, gap + (step % 2));
      await nextFrame(page);
      await page.waitForTimeout(25);
    }

    const targetY = sixthBox.y + sixthBox.height / 2;
    for (let step = 1; step <= 5; step += 1) {
      await page.mouse.move(x, gap + ((targetY - gap) * step) / 5);
      await nextFrame(page);
    }

    await expect(page.locator(`[data-inbox-row="${p0.id}"]`)).toBeAttached();
    await expect(rows).toHaveCount(9);
    const underPointer = await page.evaluate(
      ({ x, y }) =>
        document
          .elementFromPoint(x, y)
          ?.closest("[data-inbox-row]")
          ?.getAttribute("data-inbox-row"),
      { x, y: targetY }
    );
    expect(underPointer).toBe(sixthID);
    expect(Math.abs((await topOf(destination)) - sixthBefore)).toBeLessThanOrEqual(2);
  } finally {
    release.resolve();
    await page.unroute("**/api/v1/inbox");
  }
}

test("a travelling pointer keeps its destination when a P0 row arrives @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedAsks("Travelling pointer", 8);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await travelPointerAcrossIncomingRow(page);
  } finally {
    await alice.close();
  }
});

test("a travelling pointer crosses a grouped owner without losing its destination @phone", async ({
  browser,
}) => {
  await seedTravellingPointerGroup();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await travelPointerAcrossIncomingRow(page);
  } finally {
    await alice.close();
  }
});
