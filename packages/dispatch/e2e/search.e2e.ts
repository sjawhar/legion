import { expect, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  e2eAgentToken,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const searchTerm = "astrolabe";
const searchSpec = "# Instruments\n\nThe astrolabe measures altitude. Every astrolabe is brass.\n";
const searchSession = {
  actor: { kind: "session" as const, id: "e2e-search" },
  as: "agent" as const,
};

interface SearchFixture {
  readonly commentID: string;
  readonly commentIssueKey: string;
  readonly firstIssueKey: string;
}

async function seedSearchFixture(): Promise<SearchFixture> {
  await createProject({ key: "CORE", name: "Core" });
  const firstIssue = await createIssue({
    project: "CORE",
    spec: searchSpec,
    title: "Navigation instruments",
  });
  const secondIssue = await createIssue({ project: "CORE", title: "Other work" });
  const comment = await createComment(secondIssue.key, { body: "Move the astrolabe diagram." });
  const thirdIssue = await createIssue({ project: "CORE", title: "Unrelated" });
  await createMessage(thirdIssue.key, { body: "No instruments here." }, searchSession);

  return {
    commentID: comment.id,
    commentIssueKey: secondIssue.key,
    firstIssueKey: firstIssue.key,
  };
}

async function openSearchFromRail(page: Page, isPhone: boolean): Promise<void> {
  await page.goto("/");
  if (isPhone) {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await page.getByRole("button", { name: /search/i }).click();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("Ctrl+K and Cmd+K open the palette, grouped results navigate a document to its highlighted term", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
  const fixture = await seedSearchFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("button", { name: /search/i })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });
    await expect(dialog).toBeVisible();
    await expect(search).toBeFocused();
    await search.fill(searchTerm);

    const options = page.getByRole("option");
    await expect(options).toHaveCount(2);
    const documentResult = page.getByRole("option", { name: /^doc spec\.md / });
    await expect(documentResult).toBeVisible();
    await expect(documentResult.locator("mark")).toHaveText([searchTerm, searchTerm]);
    await expect(page.locator('[role="presentation"]')).toHaveCount(2);
    await expect(
      page.locator('[role="presentation"][aria-label="CORE-1: Navigation instruments"]')
    ).toBeVisible();
    await expect(
      page.locator('[role="presentation"][aria-label="CORE-2: Other work"]')
    ).toBeVisible();

    const documentID = await documentResult.getAttribute("id");
    expect(documentID).not.toBeNull();
    await search.press("ArrowDown");
    await expect(search).toHaveAttribute("aria-activedescendant", /search-option-comment-/);
    await search.press("ArrowUp");
    await expect(search).toHaveAttribute("aria-activedescendant", documentID as string);
    await search.press("Enter");

    await expect(page).toHaveURL(
      new RegExp(`/issues/${fixture.firstIssueKey}/spec\\?q=${searchTerm}$`)
    );
    const editor = documentEditor(page);
    await expect(editor).toContainText(searchTerm);
    await expect(editor).toBeInViewport();
    await expect
      .poll(() =>
        page.evaluate(() => {
          // Playwright's TypeScript context lacks CSS Highlight API declarations.
          const css = CSS as unknown as { highlights?: Map<string, Highlight> };
          const highlights = css.highlights?.get("dispatch-search");
          return (
            highlights !== undefined &&
            [...highlights].some((range) => range.toString() === "astrolabe")
          );
        })
      )
      .toBe(true);
    await expect(dialog).toHaveCount(0);

    await page.keyboard.press("Meta+k");
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("a search term lists marked document, comment, ask, and issue-title results in issue groups", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "result-kind coverage is independent of viewport");
  await seedSearchFixture();
  const titleIssue = await createIssue({ project: "CORE", title: "Astrolabe calibration" });
  await createAsk(titleIssue.key, { question: "Who calibrates the astrolabe?" }, searchSession);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, false);
    const search = page.getByRole("combobox", { name: "Search" });
    await search.fill(searchTerm);

    const options = page.getByRole("option");
    await expect(options).toHaveCount(4);
    for (const name of [/^issue /, /^doc /, /^comment /, /^ask /]) {
      const option = page.getByRole("option", { name });
      await expect(option).toHaveCount(1);
      await expect(option.locator("mark").first()).toHaveText(/astrolabe/i);
    }
    await expect(page.locator('[role="presentation"]')).toHaveCount(3);
    await expect(
      page.locator('[role="presentation"][aria-label="CORE-4: Astrolabe calibration"]')
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the rail Search control opens the palette and Escape returns focus to it", async ({
  browser,
}, testInfo) => {
  await seedSearchFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const isPhone = testInfo.project.name === "iphone";
    await openSearchFromRail(page, isPhone);

    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });
    await expect(dialog).toBeVisible();
    await expect(search).toBeFocused();
    await search.fill(searchTerm);
    await expect(page.getByRole("option")).toHaveCount(2);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`search-palette-${testInfo.project.name}.png`),
    });

    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    if (!isPhone) {
      await expect(page.getByRole("button", { name: /search/i })).toBeFocused();
    }
  } finally {
    await context.close();
  }
});

test("phone search rows are at least 44px, do not overflow, and a comment result opens Conversation", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "phone row geometry is covered by the iphone project"
  );
  const fixture = await seedSearchFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, true);
    await page.getByRole("combobox", { name: "Search" }).fill(searchTerm);

    const options = page.getByRole("option");
    await expect(options).toHaveCount(2);
    for (let index = 0; index < (await options.count()); index += 1) {
      const box = await options.nth(index).boundingBox();
      expect(box, `result option ${index} has a layout box`).not.toBeNull();
      expect(box?.height ?? 0, `result option ${index} is a touch target`).toBeGreaterThanOrEqual(
        44
      );
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);

    await page.getByRole("option", { name: /^comment / }).click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${fixture.commentIssueKey}/comments/${fixture.commentID}$`)
    );
    await expect(
      page.getByRole("region", { name: "Conversation" }).getByText("Move the astrolabe diagram.")
    ).toBeVisible();
    await expect(page.getByTestId("margin-sheet")).not.toContainText("Move the astrolabe diagram.");
  } finally {
    await context.close();
  }
});

test("no results and short queries are stated, not silent", async ({ browser }, testInfo) => {
  await seedSearchFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openSearchFromRail(page, testInfo.project.name === "iphone");
    const search = page.getByRole("combobox", { name: "Search" });

    await search.fill("a");
    await expect(page.getByText("Type at least 2 characters")).toBeVisible();
    await search.fill("zzqqxx");
    await expect(page.getByText('No results for "zzqqxx"')).toBeVisible();
  } finally {
    await context.close();
  }
});
test("the agent issue API returns POSSIBLE_DUPLICATE and force creates the issue", async ({
  request,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Calibrate astrolabe navigation instruments" });

  const duplicate = await request.post("/api/v1/issues", {
    data: {
      actor: searchSession.actor,
      project: "CORE",
      title: "Astrolabe navigation instruments rollout",
    },
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  expect(duplicate.status()).toBe(409);
  await expect(duplicate.json()).resolves.toMatchObject({
    candidates: [
      {
        key: "CORE-1",
        title: "Calibrate astrolabe navigation instruments",
      },
    ],
    code: "POSSIBLE_DUPLICATE",
  });

  const forced = await request.post("/api/v1/issues", {
    data: {
      actor: searchSession.actor,
      force: true,
      project: "CORE",
      title: "Astrolabe navigation instruments rollout",
    },
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  expect(forced.status()).toBe(201);
  await expect(forced.json()).resolves.toMatchObject({ key: "CORE-2" });
});
