import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { claimIssue, createIssue, createProject, mintAgentToken } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

// A session title is free text of any length, and the claim chip renders it on the issue
// header, so a long holder name must not widen the page. The long-payload shape is
// labels.e2e.ts's; the width assertion is the one above.
test("a long holder name never widens the issue page", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed by a long name" });
  const token = await mintAgentToken("claim-e2e", "alice");
  const holder = "Implementer whose session title runs on and on and on".padEnd(120, "!");
  await setLiveSessions([{ session_id: "claim-e2e-session", title: holder }]);
  await claimIssue(issue.key, {
    actor: { id: "claim-e2e-session", kind: "session", origin: { session_title: holder } },
    as: "agent",
    token,
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const { width, height } of [
      { height: 844, width: 390 },
      { height: 900, width: 1280 },
    ]) {
      await page.setViewportSize({ height, width });
      await page.goto(`/issues/${issue.key}`);
      const chip = page.getByTestId("issue-claim");
      await expect(chip).toBeVisible();
      await expect(chip).toHaveAttribute("title", /Claimed by Implementer whose session/);
      // The holder ellipsizes inside the chip; the page itself never grows a scrollbar.
      const pageWidths = await page.evaluate(() => ({
        clientWidth: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(pageWidths.scrollWidth).toBe(pageWidths.clientWidth);
      expect(pageWidths.clientWidth).toBeLessThanOrEqual(width);
      expect(await chip.evaluate((element) => getComputedStyle(element).flexShrink)).toBe("1");
      const shot = testInfo.outputPath(`claim-long-holder-${width}.png`);
      await page.screenshot({ path: shot });
      await testInfo.attach(`long holder name (${width}px)`, {
        contentType: "image/png",
        path: shot,
      });
    }
  } finally {
    await context.close();
  }
});
