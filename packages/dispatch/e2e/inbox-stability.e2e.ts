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

// Issue titles sharing no terms, so the server's duplicate check never refuses a seed.
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

const scrollY = (page: Page) => page.evaluate(() => window.scrollY);

/** Scrolls so the row's centre sits at the viewport's centre. */
async function centre(row: Locator): Promise<void> {
  await row.evaluate((node) => {
    const rect = node.getBoundingClientRect();
    window.scrollBy(0, rect.top + rect.height / 2 - window.innerHeight / 2);
  });
}

/** Six asks waiting on the reader, then four handed to agents, so Waiting on agents has rows
 *  below the one that will join it; the third of the reader's is the one under test. */
async function seedBothSections() {
  await createProject({ key: "CORE", name: "Core" });
  const waiting = await seedAsks("Yours", 6);
  const target = waiting[2];
  if (target === undefined) throw new Error("target ask missing");
  for (const { ask, issue } of await seedAsks("Theirs", 4, 6)) {
    await createComment(issue.key, { ask_id: ask.id, body: "Clarify first." });
  }
  return target;
}

/** Takes the reader's hand off the row: the pointer to the page corner, and focus to nothing (a
 *  click on empty page) - not onto another row, which would make that row the anchor, and not by
 *  Escape, whose step back to the row would scroll a tall row into view on its own. */
async function leaveRow(page: Page): Promise<void> {
  await page.mouse.move(0, 0);
  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.closest("[data-inbox-row]") === null))
    .toBe(true);
}

test("a row being typed into stays put, draft and all, when an agent's progress note flips its turn, and moves only once the reader leaves it", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(10);
    const row = page.locator(`[data-inbox-row="${target.ask.id}"]`);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await centre(row);
    const field = row.getByLabel("Your answer");
    await field.click();
    await field.fill("Ship it once the audit lands");
    const before = await topOf(row);
    const scrolled = await scrollY(page);

    await createComment(
      target.issue.key,
      { ask_id: target.ask.id, body: "Checking the audit now.", turn: "agent" },
      session
    );

    // The card says whose turn it is now; the row itself stays where the reader is typing.
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(field).toHaveValue("Ship it once the audit lands");
    await expect(field).toBeFocused();
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    expect(Math.abs((await scrollY(page)) - scrolled)).toBeLessThanOrEqual(2);

    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    await expect(field).toHaveValue("Ship it once the audit lands");
    expect(Math.abs((await scrollY(page)) - scrolled)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the reader's own Ask back keeps the row where it is and the view still, until they leave it", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(10);
    const row = page.locator(`[data-inbox-row="${target.ask.id}"]`);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    // Scrolled, so the centre fallback is live too: on release nothing may follow the row down.
    await centre(row);
    const field = row.getByLabel("Your answer");
    await field.click();
    await field.fill("Ship what, exactly?");
    const askBack = row.getByRole("button", { name: "Ask back" });
    // Measured with the button already in view, so the click itself scrolls nothing.
    await askBack.hover();
    const before = await topOf(row);
    const scrolled = await scrollY(page);
    expect(scrolled).toBeGreaterThan(0);

    await askBack.click();
    await expect(
      row.getByTestId(`thread-${target.ask.id}`).getByText("Ship what, exactly?")
    ).toBeVisible();
    await expect.poll(() => getAsk(target.ask.id)).toMatchObject({ ask: { waiting_on: "agent" } });
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );

    // The row is still under Waiting on you, where the reader's hand is - Ask back, disabled once
    // its text is sent, handed focus to the row - and the view has not moved after it.
    await expect(row).toBeFocused();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    const afterAskBack = await scrollY(page);
    expect(Math.abs(afterAskBack - scrolled)).toBeLessThanOrEqual(2);

    // Letting go of it: it joins Waiting on agents below; the view stays where the reader was.
    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    expect(Math.abs((await scrollY(page)) - scrolled)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the row at the viewport centre stays put when a P0 ask arrives above it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Queue", 8);
  const sixth = asks[5];
  if (sixth === undefined) throw new Error("sixth ask missing");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(8);
    const row = page.locator(`[data-inbox-row="${sixth.ask.id}"]`);
    await centre(row);
    const before = await topOf(row);
    expect(before).toBeGreaterThan(0);

    const urgent = await createIssue({ project: "CORE", title: "Meridian" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Urgent ask" }, session);

    const arrived = page.locator(`[data-inbox-row="${p0.id}"]`);
    await expect(arrived).toBeAttached();
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(9);
    expect(await topOf(arrived)).toBeLessThan(before);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the row under the pointer stays put when a row above it is answered elsewhere, and is let go once the pointer leaves the list", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Newest first in the Inbox: the ninth ask is the top row, the fifth sits four rows below it.
  const asks = await seedAsks("Queue", 9);
  const fifth = asks[4];
  const sixth = asks[5];
  const seventh = asks[6];
  const ninth = asks[8];
  if (fifth === undefined || sixth === undefined || seventh === undefined || ninth === undefined) {
    throw new Error("asks missing");
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(9);
    const row = page.locator(`[data-inbox-row="${fifth.ask.id}"]`);
    await centre(row);
    const box = await row.boundingBox();
    if (box === null) throw new Error("row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + 8);
    const before = box.y;

    await answerAsk(
      ninth.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );

    await expect(page.locator(`[data-inbox-row="${ninth.ask.id}"]`)).toHaveCount(0);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);

    // With the pointer off the list, the anchor falls back to the row nearest the centre. The
    // seventh ask sits above the fifth with the sixth between them: answering the sixth leaves
    // the centre row still and the row the pointer left rising into the gap - were the pointer
    // still counted, the fifth would hold and the centre row drop.
    await page.mouse.move(0, 0);
    const centreRow = page.locator(`[data-inbox-row="${seventh.ask.id}"]`);
    await centre(centreRow);
    const leftTop = await topOf(row);
    const centreTop = await topOf(centreRow);
    await answerAsk(
      sixth.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(page.locator(`[data-inbox-row="${sixth.ask.id}"]`)).toHaveCount(0);
    expect(Math.abs((await topOf(centreRow)) - centreTop)).toBeLessThanOrEqual(2);
    expect(leftTop - (await topOf(row))).toBeGreaterThan(2);
  } finally {
    await alice.close();
  }
});

test("a grouped header stays with its pointer-held row while a P0 ask arrives", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const grouped = await createIssue({ project: "CORE", title: "Grouped stability" });
  await createAsk(grouped.key, { question: "Grouped first" }, session);
  const held = await createAsk(grouped.key, { question: "Grouped held" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${held.id}"]`);
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

    await expect(page.locator(`[data-inbox-row="${p0.id}"]`)).toBeAttached();
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
  expect(await scrollY(page)).toBe(0);
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

test("a travelling pointer keeps its destination when a P0 row arrives", async ({ browser }) => {
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

test("a travelling pointer crosses a grouped owner without losing its destination", async ({
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
