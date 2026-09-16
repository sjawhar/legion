import { expect, type Locator, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
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
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
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

test("the row under the pointer stays put when a row above it is answered elsewhere", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Queue", 8);
  const second = asks[1];
  const fifth = asks[4];
  if (second === undefined || fifth === undefined) throw new Error("asks missing");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(8);
    const row = page.locator(`[data-inbox-row="${fifth.ask.id}"]`);
    await centre(row);
    const box = await row.boundingBox();
    if (box === null) throw new Error("row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + 8);
    const before = box.y;

    await answerAsk(
      second.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );

    await expect(page.locator(`[data-inbox-row="${second.ask.id}"]`)).toHaveCount(0);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});
