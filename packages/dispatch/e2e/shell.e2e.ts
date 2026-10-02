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
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toHaveCount(0);

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

  await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();

  await context.close();
});

test("a page chunk that fails again after the session's one reload shows the failed download", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unreachable page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // Every download of the issue page's code fails, the way a request does when the network
  // changes under it (a VPN reconnect, a Wi-Fi switch).
  await page.route(/\/assets\/IssuePage-[^/]+\.js$/u, (route) => route.abort());
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  try {
    await page.goto(`/issues/${issue.key}`);
    // The first failure reloads the page once; the second, with that reload spent, stays.
    await expect.poll(() => loads).toBe(2);
    const box = page.getByTestId("error-boundary");
    await expect(box).toContainText("Failed to fetch dynamically imported module");
    await expect(box).not.toContainText("Cannot read properties of undefined");
    await expect(box.getByRole("button", { name: "Reload Dispatch" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the document editor renders headings and ordered lists with real typography", async ({
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

  await expect(page.getByRole("heading", { level: 1, name: "Title" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Section" })).toBeVisible();

  const sizes = await page.evaluate(() => {
    const root = document.querySelector('[role="textbox"][aria-label="Document editor"]');
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

/** Enough projects that the navigation is taller than any viewport under test. */
async function seedManyProjects(count: number): Promise<void> {
  for (let index = 0; index < count; index += 1) {
    const key = `P${String(index).padStart(2, "0")}`;
    await createProject({ key, name: `Project ${key}` });
  }
}

// The sidebar's footer holds Sign out, and the navigation above it grows with the project list.
// A column that is only as tall as the viewport at its minimum pushes the footer and the lower
// links off the bottom of the screen, where a reader reaches them only by scrolling to the end
// of the page - or, on a phone, not at all, because the drawer is fixed to the viewport.
test("the sidebar scrolls, so Sign out is reachable with a navigation taller than the viewport", async ({
  browser,
}, testInfo) => {
  await seedManyProjects(17);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const compact = testInfo.project.name !== "chromium";

  try {
    if (compact) {
      await page.setViewportSize({ height: 390, width: 844 });
    } else {
      await page.setViewportSize({ height: 720, width: 1280 });
    }
    await page.goto("/");

    if (compact) {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const signOut = page.getByRole("button", { name: "Sign out" });
    await expect(signOut).toBeAttached();
    // The rail is its own scroll container: the footer comes into view by scrolling the rail,
    // and the page itself never moves, so the footer is never left past the end of a long
    // issue, reachable only by scrolling the document to its end - or, in the drawer, not at all.
    await signOut.scrollIntoViewIfNeeded();
    await expect(signOut).toBeInViewport();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await signOut.focus();
    await expect(signOut).toBeInViewport();
    await expect(signOut).toBeFocused();
    // Every project link is reachable by scrolling that container, still not the page.
    const lastProject = page.getByRole("link", { name: /P16/ });
    await lastProject.scrollIntoViewIfNeeded();
    await expect(lastProject).toBeInViewport();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
  } finally {
    await context.close();
  }
});

// The right gutter is reserved for the collapsed-margin rail, and that rail only exists where
// the route has a margin. Reading the raw preference instead left 80px of padding on the right
// of the Inbox, beside nothing.
test("a route with no margin reserves no gutter for a margin rail", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the rails are a desktop layout");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Has a margin" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { name: "Hide margin" }).click();
    await expect(page.getByTestId("margin-rail")).toBeVisible();
    const main = page.getByTestId("main-content");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("80px");

    await page.goto("/");
    await expect(page.getByTestId("margin-rail")).toHaveCount(0);
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
    await expect(main).toHaveAttribute("data-shell-layout", "standard");

    await page.getByRole("button", { name: "Hide sidebar" }).click();
    await expect(main).toHaveAttribute("data-shell-layout", "full-width");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingLeft)).toBe("80px");
  } finally {
    await context.close();
  }
});
