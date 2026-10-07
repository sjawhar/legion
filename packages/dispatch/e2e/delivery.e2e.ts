import { expect, test } from "@playwright/test";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** Seeds one delivery_settings row plus two population pull requests (one shipped by a deploy
 * run, one still waiting) and the deploy run itself, directly in Postgres -- the delivery
 * timeline's GET route reads only stored facts, so this is the same shape intake/reconcile would
 * have written, without needing a live GitHub App for this page-level e2e. acme/widgets-shaped
 * placeholders throughout, per AGENTS.md ("This repository is public"). */
async function seedDeliveryFixture(): Promise<void> {
  await sql(
    `INSERT INTO delivery_settings (
       singleton, deploy_repo, deploy_workflow_path, production_job_name,
       pr_checks_workflow_path, population_authors, excluded_repos, updated_by
     ) VALUES (
       true, 'acme/widgets', '.github/workflows/deploy.yml', 'widgets-release / widgets-release',
       '.github/workflows/pr-checks.yml', ARRAY['octocat'], ARRAY[]::text[], '{"kind":"system","id":"e2e-seed"}'
     )`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, sessions, partial
     ) VALUES
       ('acme/widgets', 1, 'feat: a shipped widget', 'https://github.com/acme/widgets/pull/1', 'octocat',
        '2024-06-01T00:00:00Z', '2024-06-01T01:00:00Z', 12, 3, false, ARRAY[]::text[], false),
       ('acme/widgets', 2, 'feat: a waiting widget', 'https://github.com/acme/widgets/pull/2', 'octocat',
        '2024-06-01T02:00:00Z', '2024-06-01T03:00:00Z', 4, 1, false, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url
     ) VALUES (
       'acme/widgets', 500, 'deploy', 'deadbeef', '2024-06-01T01:00:00Z',
       '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success',
       'https://github.com/acme/widgets/actions/runs/500'
     )`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
     VALUES ('acme/widgets', 500, 'widgets-release / widgets-release', '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success')`
  );
}

test("the Delivery sidebar nav entry opens the delivery timeline", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  // Start from another page (the Inbox, at "/") rather than navigating to /delivery directly,
  // so this exercises the Sidebar's own "Delivery" rail link (`features/sidebar/Sidebar.tsx`)
  // rather than only the route itself. Below `xl` (a phone viewport) the sidebar is a sheet
  // behind its own "Menu" control (app.tsx) -- that button's accessible name is its
  // `aria-label` ("Open navigation"), not its visible "Menu" text, since an aria-label
  // overrides text content for accessible-name computation. A one-shot `isVisible()` check here
  // races the SPA's first render under load (CI's iphone project flaked on exactly this): wait
  // for either the Menu button or the link itself to actually appear before deciding, rather
  // than reading visibility once before the page has necessarily finished mounting.
  await page.goto("/");
  const menuButton = page.getByRole("button", { name: "Open navigation" });
  const deliveryLink = page.getByRole("link", { name: "Delivery" });
  // Promise.race never cancels its losing branch: on a desktop-viewport run the mobile-only
  // "Open navigation" button never renders, so that waitFor keeps polling toward its own default
  // timeout after the race settles, risking an unhandled rejection ("Target closed") once
  // context.close() below tears the page down while it is still pending. Each branch catches its
  // own eventual rejection so a loser that never resolves never surfaces one.
  await Promise.race([
    menuButton.waitFor({ state: "visible" }).catch(() => {}),
    deliveryLink.waitFor({ state: "visible" }).catch(() => {}),
  ]);
  if (await menuButton.isVisible()) {
    await menuButton.click();
  }
  await expect(deliveryLink).toBeVisible();
  await deliveryLink.click();

  await expect(page).toHaveURL(/\/delivery$/);
  await expect(page.getByRole("heading", { name: "Delivery" })).toBeVisible();

  await context.close();
});

test("the delivery timeline shows seeded merges and deploys, filters by facet, and drills into a PR", async ({
  browser,
}, testInfo) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto(`/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z`);

  await expect(page.getByRole("heading", { name: "Delivery" })).toBeVisible();
  // The source-freshness row renders once the window's data has loaded: reconcile/events never
  // ran in this seeded-only fixture, so lib/freshness.ts's never-happened wording appears.
  await expect(page.getByText(/reconcile never ran/i)).toBeVisible();
  await expect(page.getByText(/events never received/i)).toBeVisible();

  // The default view is the timeline chart (ECharts, canvas renderer): assert it has actually
  // mounted and painted before switching to the list, so a regression of the chart's async-init
  // race (it used to stay blank whenever the data effect ran before `echarts.init` resolved) is
  // caught here instead of going unnoticed, since every other assertion below runs after the
  // list view replaces it.
  const chartCanvas = page.locator("canvas").first();
  await expect(chartCanvas).toBeVisible();
  const chartBox = await chartCanvas.boundingBox();
  expect(chartBox?.width).toBeGreaterThan(0);
  expect(chartBox?.height).toBeGreaterThan(0);

  // Switch to the list view, where both seeded PRs show as rows.
  await page.getByRole("button", { name: "Show list" }).click();
  await expect(page.getByText("feat: a shipped widget")).toBeVisible();
  await expect(page.getByText("feat: a waiting widget")).toBeVisible();

  // The Repository facet narrows the list server-side; acme/widgets is the only repository
  // seeded, so selecting it is a no-op on the result but proves the picker and the facet
  // round-trip to the server run. MultiSelect's trigger is a button named for the facet (plus an
  // optional " · N" selection count); its popover exposes each option with role "option".
  await page.getByRole("button", { name: /^Repository( · \d+)?$/ }).click();
  await page.getByRole("option", { exact: true, name: "acme/widgets" }).click();
  await page.keyboard.press("Escape");
  await expect(page.getByText("feat: a shipped widget")).toBeVisible();
  await expect(page.getByText("feat: a waiting widget")).toBeVisible();

  // A row click opens the drill-down with the GitHub link.
  await page.getByRole("cell", { name: "feat: a shipped widget" }).click();
  await expect(page.getByRole("link", { name: "acme/widgets#1 on GitHub" })).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/pull/1"
  );
  await expect(page.getByText(/deployed · deploy run 500/i)).toBeVisible();

  await page.screenshot({ path: testInfo.outputPath("delivery-drilldown.png"), fullPage: true });
  await context.close();
});

test("the freshness row shows a named reconcile failure, distinct from mere staleness", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  // A reconcile pass that failed (a missing GitHub App permission here) still advances nothing
  // happy: last_reconcile_at is set (a prior pass did succeed once) but last_error now names the
  // most recent failure, exactly as reconcile.go's RecordReconcileError leaves it -- the
  // freshness row must show this failure by name, not the generic "Reconcile X ago"/"never ran"
  // wording a merely-stale-but-healthy row gets (lib/freshness.ts's reconcileRow).
  await sql(
    `UPDATE delivery_settings
       SET last_reconcile_at = '2024-06-01T01:00:00Z',
           last_error = 'the installation lacks Actions: read on acme/widgets'
       WHERE singleton`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto(`/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z`);

  await expect(page.getByRole("heading", { name: "Delivery" })).toBeVisible();
  await expect(
    page.getByText(/reconcile failing: the installation lacks actions: read on acme\/widgets/i)
  ).toBeVisible();
  // Never the generic staleness wording once a named failure is present.
  await expect(page.getByText(/reconcile never ran/i)).toHaveCount(0);

  await context.close();
});

test("an unfetchable pull request shows on the freshness row by count", async ({ browser }) => {
  await seedDeliveryFixture();
  // S3: a population pull request whose completing fetch answered a permanent 404/410 from
  // GitHub is marked unfetchable (store.go's MarkPullRequestUnfetchable) rather than retried
  // every reconcile pass forever -- the freshness row surfaces it by count.
  await sql(
    `UPDATE delivery_pull_requests
       SET unfetchable_at = '2024-06-01T05:00:00Z', unfetchable_reason = 'pull request not found or gone (status 404)'
       WHERE repo = 'acme/widgets' AND number = 2`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto(`/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z`);

  await expect(page.getByRole("heading", { name: "Delivery" })).toBeVisible();
  await expect(
    page.getByText(/1 pull request can no longer be fetched from GitHub/i)
  ).toBeVisible();

  await context.close();
});
