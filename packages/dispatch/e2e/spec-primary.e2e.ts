import { expect, test } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-spec-primary", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("issue landing keeps Spec primary and puts open asks in Needs you", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SPEC", name: "Spec primary" });
  const issue = await createIssue({
    project: "SPEC",
    spec: "Primary document content.",
    title: "Keep the document central",
  });
  await createAsk(issue.key, { question: "Approve the document-first layout?" }, session);
  await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "Primary" },
      question: "Is this opening clear?",
    },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const isPhone = testInfo.project.name === "iphone";
    await page.setViewportSize(
      isPhone ? { height: 812, width: 375 } : { height: 900, width: 1440 }
    );
    await page.goto(`/issues/${issue.key}`);

    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");

    if (isPhone) {
      const toggle = page.getByRole("button", { name: "Open review panel (2 open asks)" });
      await expect(toggle).toBeVisible();
      const toggleScreenshot = testInfo.outputPath("spec-primary-phone-toggle.png");
      await page.screenshot({ path: toggleScreenshot, fullPage: true });
      await testInfo.attach("spec primary phone toggle", {
        contentType: "image/png",
        path: toggleScreenshot,
      });
      await toggle.click();
      await expect(page.getByRole("region", { name: "Needs you" })).toContainText(
        "Approve the document-first layout?"
      );
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
      const needsYouScreenshot = testInfo.outputPath("spec-primary-phone-needs-you.png");
      await page.screenshot({ path: needsYouScreenshot, fullPage: true });
      await testInfo.attach("spec primary phone Needs you", {
        contentType: "image/png",
        path: needsYouScreenshot,
      });
    } else {
      await expect(page.getByRole("region", { name: "Needs you" })).toContainText(
        "Approve the document-first layout?"
      );
      await expect(page.getByRole("region", { name: "Needs you" })).toContainText(
        "Is this opening clear?"
      );
      const desktopScreenshot = testInfo.outputPath("spec-primary-desktop-1440.png");
      await page.screenshot({ path: desktopScreenshot, fullPage: true });
      await testInfo.attach("spec primary desktop 1440", {
        contentType: "image/png",
        path: desktopScreenshot,
      });
    }
  } finally {
    await alice.close();
  }
});
