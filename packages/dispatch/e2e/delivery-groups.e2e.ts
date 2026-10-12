import { expect, type Page, test } from "@playwright/test";
import { seedSessionGroupsFixture } from "./delivery-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The Parent agent facet's option names, as the facet labels them (`<value>, <n> PR(s)`). */
async function parentAgentOptions(page: Page): Promise<string[]> {
  await page.getByRole("button", { name: /^Parent agent: / }).click();
  const options = page.getByRole("listbox", { name: "Parent agent options" }).getByRole("option");
  await expect(options.first()).toBeVisible();
  const names = await options.evaluateAll((elements) =>
    elements.map((element) => element.getAttribute("aria-label") ?? "")
  );
  await page.keyboard.press("Escape");
  return names;
}

/** A `YYYY-MM-DD` date input's value for `daysAgo` days before now, in UTC. */
function utcDaysAgo(daysAgo: number): string {
  return new Date(Date.now() - daysAgo * 24 * 60 * 60 * 1000).toISOString().slice(0, 10);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("grouping two sessions makes them one Parent agent", async ({ browser }) => {
  await seedSessionGroupsFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto("/delivery?mode=list");
  await expect(page.getByText("3 PRs in current filter/window")).toBeVisible();
  expect(await parentAgentOptions(page)).toEqual([
    "Planner, first run, 1 PR",
    "Planner, second run, 1 PR",
    "Reviewer, 1 PR",
  ]);

  await page.getByRole("button", { name: "Edit groups" }).click();
  const editor = page.getByRole("dialog", { name: "Session groups" });

  await editor.getByRole("checkbox", { name: "Planner, first run (e2e-planner-1)" }).check();
  await editor.getByRole("checkbox", { name: "Planner, second run (e2e-planner-2)" }).check();
  await editor.getByLabel("New group name").fill("Release crew");
  await editor.getByRole("button", { name: "Create group" }).click();
  await expect(editor.getByRole("group", { name: "Release crew" })).toBeVisible();
  await editor.getByRole("button", { name: "Close session groups" }).click();

  await expect
    .poll(() => parentAgentOptions(page))
    .toEqual(["Release crew, 2 PRs", "Reviewer, 1 PR"]);
  const rows = page.getByRole("row", { name: /plan the (widgets|gadgets)/ });
  await expect(rows).toHaveCount(2);
  for (const row of await rows.all()) await expect(row).toContainText("Release crew");

  await context.close();
});

test("the window control changes what the page counts", async ({ browser }) => {
  await seedSessionGroupsFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto("/delivery?mode=list");
  await expect(page.getByText("3 PRs in current filter/window")).toBeVisible();

  await page.getByLabel("Window").selectOption("7d");
  await expect(page).toHaveURL(/[?&]window=7d/);
  await expect(page.getByText("2 PRs in current filter/window")).toBeVisible();

  await page.getByLabel("Window").selectOption("custom");
  await page.getByRole("textbox", { name: "From", exact: true }).fill(utcDaysAgo(21));
  await page.getByRole("textbox", { name: "To", exact: true }).fill(utcDaysAgo(19));
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page).toHaveURL(/[?&]from=.*&to=/);
  await expect(page.getByText("1 PRs in current filter/window")).toBeVisible();

  // 120 days ago through 19 days ago spans 102 days, past the server's 84.
  await page.getByRole("textbox", { name: "From", exact: true }).fill(utcDaysAgo(120));
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByRole("alert")).toHaveText(
    /^The window can span at most 84 days; this range spans \d+\.$/
  );
  await expect(page.getByText("1 PRs in current filter/window")).toBeVisible();

  await context.close();
});
