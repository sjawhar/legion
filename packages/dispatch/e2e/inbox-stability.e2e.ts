import { expect, type Locator, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { answerAsk, createAsk, createComment, createIssue, createProject, patchIssue } from "./api";
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

/** Scrolls so the row's centre sits at the viewport's centre. */
async function centre(row: Locator): Promise<void> {
  await row.evaluate((node) => {
    const rect = node.getBoundingClientRect();
    window.scrollBy(0, rect.top + rect.height / 2 - window.innerHeight / 2);
  });
}

test("a row being typed into keeps its place and its draft when an agent's progress note moves it to Waiting on agents", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Six asks waiting on the reader, then four older-than-nothing asks handed to agents so the
  // agents section has rows below the one that will join it.
  const waiting = await seedAsks("Yours", 6);
  const target = waiting[2];
  if (target === undefined) throw new Error("target ask missing");
  const agents = await seedAsks("Theirs", 4, 6);
  for (const { ask, issue } of agents) {
    await createComment(issue.key, { ask_id: ask.id, body: "Clarify first." });
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(10);
    const row = page.locator(`[data-inbox-row="${target.ask.id}"]`);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    const field = row.getByLabel("Your answer");
    await field.click();
    await field.fill("Ship it once the audit lands");
    const before = await topOf(row);

    await createComment(
      target.issue.key,
      { ask_id: target.ask.id, body: "Checking the audit now.", turn: "agent" },
      session
    );

    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );
    await expect(field).toHaveValue("Ship it once the audit lands");
    await expect(field).toBeFocused();
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
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
