import { type Browser, expect, test } from "@playwright/test";

import { createComment, createIssue, createIssueArtifact, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

declare global {
  interface Window {
    /** The largest gap, in milliseconds, between two ticks of the page's main-thread heartbeat. */
    __nestedDirectiveLongestGap?: number;
    /** Starts the heartbeat's record again: no gap so far, and the last tick now. */
    __nestedDirectiveResetGap?: () => void;
  }
}

const agent = {
  actor: { id: "e2e-nested-directive-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The two bodies that froze a reader's tab for about a minute and 15 s (INFRA-581). */
const inputs = [
  {
    name: "2,000 nested unclosed openers then a 21,000-character line",
    body: ":::a\n".repeat(2000) + "x".repeat(21000),
  },
  {
    name: "1,000 nested openers then 1,000 closing lines",
    body: ":::a\n".repeat(1000) + ":::\n".repeat(1000),
  },
];

/** The longest the main thread may go without running a task while the card draws. */
const longestGapMs = 1_000;

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * Opens the issue's Conversation tab as alice, with a heartbeat installed before the page's own
 * scripts: a `setTimeout(0)` loop that records the largest gap between its ticks, which a parse
 * holding the main thread widens to the parse's length. The record starts again when the page
 * first asks for the issue's comments, before that request goes out: the bundle has been evaluated
 * and the tab drawn by then, and the card cannot draw before the answer. Answers the largest gap
 * seen from then until the comment card has drawn a `:::a` line.
 */
async function longestGapUntilTheCommentDraws(browser: Browser, issueKey: string): Promise<number> {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.addInitScript(() => {
      let last = performance.now();
      window.__nestedDirectiveLongestGap = 0;
      window.__nestedDirectiveResetGap = () => {
        last = performance.now();
        window.__nestedDirectiveLongestGap = 0;
      };
      const tick = () => {
        const now = performance.now();
        window.__nestedDirectiveLongestGap = Math.max(
          window.__nestedDirectiveLongestGap ?? 0,
          now - last
        );
        last = now;
        setTimeout(tick, 0);
      };
      setTimeout(tick, 0);
    });
    // Only the first request resets: a later one could wait out the draw, then erase its gap.
    let reset = false;
    const comments = new RegExp(`/api/v1/issues/${issueKey}/comments(\\?.*)?$`);
    await page.route(comments, async (route) => {
      if (!reset && route.request().method() === "GET") {
        reset = true;
        await page.evaluate(() => window.__nestedDirectiveResetGap?.());
      }
      await route.continue();
    });
    await page.goto(`/issues/${issueKey}/conversation`);

    const card = page.locator('[data-turn^="comment:"]');
    await expect(card).toHaveCount(1);
    await expect(card.getByText(":::a").first()).toBeVisible();
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
    if (!reset) throw new Error("the page never asked for the issue's comments");
    const gap = await page.evaluate(() => window.__nestedDirectiveLongestGap);
    if (gap === undefined) throw new Error("the main-thread heartbeat never started");
    return gap;
  } finally {
    await alice.close();
  }
}

for (const input of inputs) {
  test(`Conversation draws a comment of ${input.name} without holding the main thread for a second`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Nested directives" });
    await createComment(issue.key, { body: input.body }, agent);

    const gap = await longestGapUntilTheCommentDraws(browser, issue.key);
    // The measured gap is the run's evidence, so it is kept on the result even when it passes.
    test
      .info()
      .annotations.push({ type: "longest main-thread gap", description: `${Math.round(gap)} ms` });
    expect(
      gap,
      `the main thread ran no task for ${Math.round(gap)} ms while the card drew`
    ).toBeLessThan(longestGapMs);
  });
}

test("a document write of either nested-directive body is refused as invalid markdown in under a second", async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Nested directive documents" });

  for (const input of inputs) {
    const started = performance.now();
    const refusal = await createIssueArtifact(
      issue.key,
      { content: input.body, name: "nested.md" },
      agent
    ).then(
      () => undefined,
      (error: unknown) => error
    );
    const took = performance.now() - started;

    expect(refusal, `${input.name}: the write was accepted`).toBeInstanceOf(Error);
    const message = refusal instanceof Error ? refusal.message : "";
    expect(message, input.name).toContain(" failed: 400 ");
    expect(message, input.name).toContain('"code":"INVALID_MARKDOWN"');
    expect(took, `${input.name}: the refusal took ${Math.round(took)} ms`).toBeLessThan(1_000);
  }
});
