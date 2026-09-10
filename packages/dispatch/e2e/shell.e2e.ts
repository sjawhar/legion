import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("a transient whoami failure shows a retry banner and keeps the app, not the sign-in page", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.route("**/auth/whoami", (route) =>
    route.fulfill({
      body: JSON.stringify({ error: "Dispatch is unavailable" }),
      contentType: "application/json",
      status: 503,
    })
  );

  await page.goto("/");

  await expect(page.getByText("Couldn't reach Dispatch.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in with GitHub" })).toHaveCount(0);

  await page.unroute("**/auth/whoami");
  await page.getByRole("button", { name: "Retry" }).click();

  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByText("Signed in as alice")).toBeVisible();

  await context.close();
});

test("signing out returns to the sign-in page", async ({ browser }, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto("/");
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByText("Signed in as alice")).toBeVisible();

  // This harness authenticates through a trusted proxy header rather than the session
  // cookie /auth/logout clears, so nothing server-side makes a later whoami actually
  // fail. Simulate the real-world post-logout state deterministically: once the logout
  // request has been seen, every subsequent whoami call is answered as unauthenticated,
  // proving the app reaches sign-in from the resetQueries-triggered refetch alone (no
  // page reload).
  let loggedOut = false;
  await page.route("**/auth/whoami", async (route) => {
    if (loggedOut) {
      await route.fulfill({
        body: JSON.stringify({ error: "unauthorized" }),
        contentType: "application/json",
        status: 401,
      });
      return;
    }
    await route.continue();
  });
  await page.route("**/auth/logout", async (route) => {
    loggedOut = true;
    await route.continue();
  });

  await page.getByRole("button", { name: "Sign out" }).click();

  await expect(page.getByRole("link", { name: "Sign in with GitHub" })).toBeVisible();

  await context.close();
});

test("the spec preview renders headings and ordered lists with real typography", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Title\n\nSome body text.\n\n## Section\n\n### Detail\n\n1. First\n2. Second\n",
    title: "Spec typography",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  // The document may now default to the rendered preview for readers; only toggle if
  // the editor is still showing source (a visible "Preview" button).
  const previewToggle = page.getByRole("button", { name: "Preview" });
  if ((await previewToggle.count()) > 0) {
    await previewToggle.click();
  }

  await expect(page.getByRole("heading", { level: 1, name: "Title" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Section" })).toBeVisible();

  const sizes = await page.evaluate(() => {
    const root = document.querySelector("article.prose");
    const h1 = root?.querySelector("h1");
    const h2 = root?.querySelector("h2");
    const paragraph = root?.querySelector("p");
    const list = root?.querySelector("ol");
    return {
      h1: h1 ? Number.parseFloat(getComputedStyle(h1).fontSize) : 0,
      h2: h2 ? Number.parseFloat(getComputedStyle(h2).fontSize) : 0,
      listStyleType: list ? getComputedStyle(list).listStyleType : "",
      paragraph: paragraph ? Number.parseFloat(getComputedStyle(paragraph).fontSize) : 0,
    };
  });

  expect(sizes.h1).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h2).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h1).toBeGreaterThan(sizes.h2);
  expect(sizes.listStyleType).not.toBe("none");

  await context.close();
});
