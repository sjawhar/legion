import { expect, test } from "@playwright/test";

import {
  agentRow,
  openAgents,
  plannerSession,
  refusePosts,
  seedAgents,
  setLiveSessions,
} from "./agents";
import { createAgentMessage } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// TEMPORARY, deleted by the commit that fixes it. It passes only while the review's finding stands:
// pick CORE-1, reply to a direct exchange, cancel the reply mid-send, take a 503, and Retry posts
// the refused text to CORE-1 as a comment with no mention - one the agent never hears about.
test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

test("CONFIRM: a reply cancelled mid-send and refused retries on the issue without its mention", async ({
  browser,
}) => {
  const issueKey = await seedAgents();
  await createAgentMessage(plannerSession.session_id, { body: "Are you free?", delivery: "btw" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openAgents(page);
    const row = agentRow(page, plannerSession.session_id);
    const field = row.getByRole("textbox", { name: "Comment" });
    const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");

    await page.keyboard.press("j");
    await page.keyboard.press("i");
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Enter");
    await expect(field).toHaveValue("@Planner");
    await row.getByRole("button", { name: "Reply" }).first().click();
    await field.fill("Yes");
    await field.press("Control+Enter");
    await expect(field).toBeDisabled();
    await row.getByRole("button", { name: "Cancel reply" }).click();
    // The channel went back to the issue under the send, and the live draft took its seed.
    await expect(field).toHaveValue("@Planner Yes");
    refuse();

    await expect(row.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Yes");
    const retried = page.waitForRequest(
      (request) =>
        request.method() === "POST" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issueKey}/comments`
    );
    await row.getByRole("button", { name: "Retry" }).click();
    expect((await retried).postDataJSON()).toEqual({ body: "Yes" });
  } finally {
    await context.close();
  }
});
