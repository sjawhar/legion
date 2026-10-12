import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  listMyReplies,
  replyToMessageDelivery,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A row names its replier the way every surface does (`actorLabel`): by the title the session
// stamped on its write.
const session = {
  actor: {
    id: "e2e-replies-agent",
    kind: "session" as const,
    origin: { session_title: "Release bot" },
  },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** A thread alice wrote in, and how its session replies in it. */
interface SeededThread {
  readonly id: string;
  readonly reply: (body: string) => Promise<unknown>;
}

/** The three kinds of thread the Replies page lists, each seeded with alice's own write. */
const threadKinds: Record<string, (issue: string) => Promise<SeededThread>> = {
  "a comment thread": async (issue) => {
    const root = await createComment(issue, { body: "Please fix the flaky test." });
    return {
      id: root.id,
      reply: (body) => createComment(issue, { body, reply_to: root.id }, session),
    };
  },
  "an answered ask": async (issue) => {
    const ask = await createAsk(
      issue,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship tonight?" },
      session
    );
    await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });
    return { id: ask.id, reply: (body) => createComment(issue, { ask_id: ask.id, body }, session) };
  },
  "an issue message thread": async (issue) => {
    const root = await createMessage(issue, { body: "Where is the build?" });
    return {
      id: root.id,
      reply: (body) => createMessage(issue, { body, in_reply_to: root.id }, session),
    };
  },
};

/**
 * The Replies count where this width shows it: from 1280 px the sidebar's Replies row, which is
 * listed at 0 too and carries the count above it; below, the compact header's `Replies N` link,
 * absent at 0.
 */
async function expectRepliesCount(page: Page, isPhone: boolean, count: number): Promise<void> {
  if (isPhone) {
    const link = page.getByRole("link", { name: /^Replies \d+$/ });
    if (count === 0) {
      await expect(link).toHaveCount(0);
      return;
    }
    await expect(link).toHaveText(`Replies ${count}`);
    await expect(link).toHaveAttribute("href", "/replies");
    return;
  }
  const row = page
    .getByRole("navigation", { name: "Navigation" })
    .getByRole("link", { name: /^Replies/ });
  await expect(row).toHaveAttribute("href", "/replies");
  await expect(row).toHaveText(count === 0 ? "Replies" : new RegExp(`^Replies\\s*${count}$`));
}

for (const [kind, seed] of Object.entries(threadKinds)) {
  test(`replies: ${kind} a session replied in is listed, clears once read, and returns on the next reply`, async ({
    browser,
  }, testInfo) => {
    const isPhone = testInfo.project.name === "iphone";
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Release train" });
    const thread = await seed(issue.key);
    await thread.reply("Done: the build is green.");
    // The fixture puts exactly this thread on alice's page, and nothing on bob's.
    expect((await listMyReplies()).rows.map((row) => row.thread_id)).toEqual([thread.id]);
    expect((await listMyReplies({}, { login: "bob" })).total).toBe(0);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.goto("/replies");
      const row = page.locator(`[data-replies-row="${thread.id}"]`);
      await expect(row).toContainText("Done: the build is green.");
      await expect(row).toContainText("Release bot");
      await expectRepliesCount(page, isPhone, 1);
      await page.screenshot({
        fullPage: true,
        path: testInfo.outputPath(`replies-row-${testInfo.project.name}.png`),
      });

      // Opening the thread shows its reply, and reading it clears the row and the count.
      await row.getByRole("link", { name: "Open" }).click();
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/`));
      await expect(
        page.getByText("Done: the build is green.").filter({ visible: true }).first()
      ).toBeInViewport();
      await expectRepliesCount(page, isPhone, 0);
      await expect.poll(async () => (await listMyReplies()).total).toBe(0);
      await page.screenshot({
        fullPage: true,
        path: testInfo.outputPath(`replies-read-${testInfo.project.name}.png`),
      });

      await page.goto("/replies");
      await expect(page.getByText("No new replies in threads you wrote in.")).toBeVisible();

      // A later reply brings the row and the count back, live.
      await thread.reply("One more thing: the deploy is out.");
      await expect(row).toContainText("One more thing: the deploy is out.");
      await expectRepliesCount(page, isPhone, 1);
    } finally {
      await alice.close();
    }
  });
}

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  roles: [],
  session_id: "01a0e090-4848-7473-acc5-fc96e6a646d3",
  title: "Planner",
};

// A phone's compact header carries the title, every count that is up and Menu on one bar. With
// all three counts up (an open ask, an agent's reply, a thread's reply), the title stays whole,
// every badge reads on one line, Menu stays on screen, and the page never scrolls sideways.
test("replies: the phone header keeps the title and every count whole when all three are up", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the compact header renders below 1280 px");
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release train" });
  await createAsk(issue.key, { question: "Ship tonight?" }, session);
  const asked = await createAgentMessage(planner.session_id, {
    body: "Status?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Green." },
    { id: planner.session_id, kind: "session" }
  );
  const root = await createComment(issue.key, { body: "Please fix the flaky test." });
  await createComment(issue.key, { body: "Done.", reply_to: root.id }, session);
  expect((await listMyReplies()).total).toBe(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    for (const width of [390, 360, 320]) {
      await page.setViewportSize({ height: 844, width });
      await page.goto("/replies");
      const header = page.locator("header").first();
      const badges = [
        header.getByRole("button", { name: "Open inbox, needs you 1" }),
        header.getByRole("link", { name: "New reply 1" }),
        header.getByRole("link", { name: "Replies 1" }),
      ];
      for (const badge of badges) await expect(badge).toBeVisible();
      const title = header.getByRole("link", { name: "Dispatch", exact: true });
      // A clipped title reads `Dispatc`: its text is wider than its box.
      expect(await title.evaluate((node) => node.scrollWidth <= node.clientWidth), `${width}`).toBe(
        true
      );
      // A badge wrapped onto two lines is taller than the one-line Menu button beside it.
      const menu = header.getByRole("button", { name: "Open navigation" });
      const lineHeight = (await menu.boundingBox())?.height ?? 0;
      for (const badge of badges) {
        expect((await badge.boundingBox())?.height, `${width}`).toBeLessThanOrEqual(lineHeight);
      }
      await expect(menu).toBeInViewport({ ratio: 1 });
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),
        `${width}`
      ).toBe(true);
      await header.screenshot({ path: testInfo.outputPath(`replies-header-${width}.png`) });
    }
  } finally {
    await alice.close();
  }
});
