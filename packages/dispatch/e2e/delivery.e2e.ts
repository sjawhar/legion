import { expect, type Page, test } from "@playwright/test";
import { seedFakeGithub } from "./fake-github-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { pressFinger } from "./touch";
import { asUser } from "./users";

const FIXTURE_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** Where the chart draws a run's shape: its series' name, its column on the x axis (the deploys
 *  at 0.15 and the failures at 0.85 of the Deploys/fails column, `Timeline.tsx`'s
 *  GLOBAL_DEPLOY_X/GLOBAL_FAILURE_X) and its time. Run 500's production job finished at 01:40 and
 *  run 501's integration tests failed at 12:20 (`seedDeliveryFixture`). */
const DEPLOY_500 = { series: "Deploys", x: 0.15, at: "2024-06-01T01:40:00Z" };
const FAILURE_501 = { series: "Pipeline failures", x: 0.85, at: "2024-06-01T12:20:00Z" };

/** The page position of a chart point, from ECharts' own coordinate conversion: the chart draws
 *  on a canvas, so a deploy dot or a failure triangle has no element to click. The page loads the
 *  echarts chunk once; importing the same URL here hands back that module, whose
 *  `getInstanceByDom` finds the chart the page made. `x` and `at` name the point; without them it
 *  is the series' first data point (a merge dot's x carries a per-PR jitter). Null until the
 *  chart has painted the series. */
async function shapePosition(
  page: Page,
  shape: { series: string; x?: number; at?: string }
): Promise<{ x: number; y: number } | null> {
  return page.evaluate(async ({ series, x, at }) => {
    interface Chart {
      convertToPixel: (finder: { seriesName: string }, value: number[]) => number[];
      getOption: () => { series?: { name?: string; data?: { value?: number[] }[] }[] };
    }
    const host = document.querySelector<HTMLElement>("[data-testid=delivery-chart]");
    if (host === null) return null;
    const chunks = performance
      .getEntriesByType("resource")
      .map((resource) => resource.name)
      .filter((name) => /\/assets\/index-[\w-]+\.js$/.test(name));
    let chart: Chart | undefined;
    for (const chunk of chunks) {
      // The page's own chunks, found by URL at run time: no specifier is known in advance.
      const loaded = (await import(chunk)) as {
        getInstanceByDom?: (element: HTMLElement) => Chart | undefined;
      };
      chart = loaded.getInstanceByDom?.(host);
      if (chart !== undefined) break;
    }
    if (chart === undefined) return null;
    const painted = chart.getOption().series?.find((candidate) => candidate.name === series);
    const first = painted?.data?.[0]?.value;
    if (first === undefined) return null;
    const value = x !== undefined && at !== undefined ? [x, Date.parse(at)] : first;
    const [px, py] = chart.convertToPixel({ seriesName: series }, value);
    if (px === undefined || py === undefined) return null;
    const rect = host.getBoundingClientRect();
    return { x: rect.left + px, y: rect.top + py };
  }, shape);
}

/** Opens the seeded timeline and waits for its chart to paint run 500's deploy. On a phone the
 *  facets sit above the chart, so the chart is scrolled into view before anything points at it. */
async function openTimeline(page: Page, query = ""): Promise<void> {
  await page.goto(`/delivery?${FIXTURE_WINDOW}${query}`);
  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
  await page.getByTestId("delivery-chart").scrollIntoViewIfNeeded();
  await expect.poll(() => shapePosition(page, DEPLOY_500)).not.toBeNull();
}

/** Clicks a run's shape, placed again at the moment of the click: a phone's browser can scroll
 *  the page between a hover and the click that follows it. */
async function clickShape(page: Page, shape: { series: string; x: number; at: string }) {
  const position = await shapePosition(page, shape);
  if (position === null) throw new Error(`no ${shape.series} shape painted at ${shape.at}`);
  await page.mouse.click(position.x, position.y);
}

test.beforeEach(async () => {
  await resetDatabase();
});

/** Seeds one delivery_settings row, two population pull requests (#1 shipped by deploy run 500
 * and attributed to ACME-1, a P0 issue; #2 waiting, a rework, with no issue), the deploy run with
 * its jobs, and a failed run (501) -- directly in Postgres: the delivery timeline's GET routes
 * read only stored facts, so this is the same shape intake/reconcile would have written, without
 * needing a live GitHub App for this page-level e2e. acme/widgets-shaped placeholders throughout,
 * per AGENTS.md ("This repository is public"). */
async function seedDeliveryFixture(): Promise<void> {
  await sql(
    `INSERT INTO delivery_settings (
       singleton, deploy_repo, deploy_workflow_path, production_job_name,
       pr_checks_workflow_path, population_authors, excluded_repos, updated_by
     ) VALUES (
       true, 'acme/widgets', '.github/workflows/deploy.yml', 'widgets-release / widgets-release',
       '.github/workflows/pr-checks.yml', ARRAY['octocat'], ARRAY[]::text[], '{"kind":"system","id":"e2e-seed"}'
     )`,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('ACME-1', 'ACME', 1, 'Ship the widgets', 'todo', 0, '{"kind":"system","id":"e2e-seed"}', 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 1, 'feat: a shipped widget', 'https://github.com/acme/widgets/pull/1', 'octocat',
        '2024-06-01T00:00:00Z', '2024-06-01T01:00:00Z', 12, 3, false, 'ACME-1', ARRAY[]::text[], false),
       ('acme/widgets', 2, 'fix: a waiting widget', 'https://github.com/acme/widgets/pull/2', 'octocat',
        '2024-06-01T02:00:00Z', '2024-06-01T03:00:00Z', 4, 1, true, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, completed_at, conclusion, url
     ) VALUES
       ('acme/widgets', 500, 'deploy', 'deadbeef', '2024-06-01T01:00:00Z',
        '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/500'),
       ('acme/widgets', 501, 'deploy', 'cafef00d', '2024-06-01T03:00:00Z',
        '2024-06-01T12:00:00Z', '2024-06-01T12:20:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/501')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ('acme/widgets', 500, 'build', '2024-06-01T01:30:00Z', '2024-06-01T01:32:00Z', 'success'),
       ('acme/widgets', 500, 'widgets-release / widgets-release', '2024-06-01T01:32:00Z', '2024-06-01T01:40:00Z', 'success'),
       ('acme/widgets', 501, 'build', '2024-06-01T12:00:00Z', '2024-06-01T12:05:00Z', 'success'),
       ('acme/widgets', 501, 'integration tests', '2024-06-01T12:05:00Z', '2024-06-01T12:20:00Z', 'failure'),
       ('acme/widgets', 501, 'widgets-release / widgets-release', null, null, 'skipped')`
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
  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();

  await context.close();
});

test("the header names the window and the freshness row names the prototype's six sources", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  await expect(page.getByText(/Generated .* · window/)).toBeVisible();
  const freshness = page.getByRole("region", { name: "Source freshness" });
  for (const source of [
    "PRs never checked",
    "Deploy runs never checked",
    "PR CI never checked",
    "Events never received",
  ]) {
    await expect(freshness.getByText(source)).toBeVisible();
  }
  await expect(freshness.getByText(/^Dispatch \d+ s ago$/)).toBeVisible();
  await expect(freshness.getByText(/^Agents \d+ s ago$/)).toBeVisible();
  await context.close();
});

test("a facet lists each value with its count and narrows the PRs, kept in the URL", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  await expect(page.getByText("2 PRs in current filter/window")).toBeVisible();
  const priority = page.getByRole("button", { name: /^Priority:/ });
  await expect(priority).toHaveText("Any priority");
  await priority.click();
  const options = page.getByRole("listbox", { name: "Priority options" }).getByRole("option");
  await expect(options).toHaveText([/^P0\s*1$/, /^No issue\s*1$/]);
  await expect(options.first()).toHaveAccessibleName("P0, 1 PR");
  await options.filter({ hasText: "P0" }).click();
  await page.keyboard.press("Escape");

  await expect(page.getByText("1 PRs in current filter/window")).toBeVisible();
  await expect(page).toHaveURL(/[?&]priority=P0(&|$)/);
  await context.close();
});

test("colour by priority with swimlanes splits the merges into one column per priority", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const lanes = page.getByRole("switch", { name: "Swimlanes" });
  await expect(lanes).toHaveAttribute("aria-checked", "false");
  await expect(page.getByTitle("Merges (2)")).toBeVisible();
  await page.getByLabel("Color merges by").selectOption("priority");
  await lanes.click();

  await expect(page).toHaveURL(/colorBy=priority/);
  await expect(page).toHaveURL(/lanes=1/);
  await expect(page.getByTitle("P0 (1)")).toBeVisible();
  await expect(page.getByTitle("No issue (1)")).toBeVisible();
  await context.close();
});

test("hovering a deploy names its run, and clicking it lists what it shipped and every job", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const deploy = await shapePosition(page, DEPLOY_500);
  if (deploy === null) throw new Error("no deploy dot painted");
  await page.mouse.move(deploy.x, deploy.y);
  await expect(page.getByText("Run #500: 1 PR(s) shipped")).toBeVisible();
  await clickShape(page, DEPLOY_500);

  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Deploy: run #500" })).toBeVisible();
  await expect(details.getByText("acme/widgets#1 — feat: a shipped widget")).toBeVisible();
  await expect(details.getByText("widgets-release / widgets-release")).toBeVisible();
  await expect(details.getByText("success (480s)")).toBeVisible();
  await details.getByRole("button", { name: "Close details" }).click();
  await expect(details).toHaveCount(0);
  await context.close();
});

test("clicking a pipeline failure shows its failed job in red and every job's result", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const failure = await shapePosition(page, FAILURE_501);
  if (failure === null) throw new Error("no failure triangle painted");
  await page.mouse.move(failure.x, failure.y);
  await expect(page.getByText("Run #501 failed: integration tests")).toBeVisible();
  await clickShape(page, FAILURE_501);

  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Pipeline failure: run #501" })).toBeVisible();
  await expect(details.getByText(/^integration tests at /)).toBeVisible();
  await expect(details.getByText("skipped")).toBeVisible();
  await context.close();
});

test("dragging along the time axis sets a brush window, which the header clears", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const chart = page.getByTestId("delivery-chart");
  const box = await chart.boundingBox();
  if (box === null) throw new Error("no chart");
  const x = box.x + box.width * 0.6;
  await page.mouse.move(x, box.y + box.height * 0.2);
  await page.mouse.down();
  await page.mouse.move(x, box.y + box.height * 0.4, { steps: 6 });
  await expect(page.getByTestId("delivery-brush-band")).toBeVisible();
  await page.mouse.move(x, box.y + box.height * 0.6, { steps: 6 });
  await page.mouse.up();

  await expect(page).toHaveURL(/[?&]ws=.*[?&]we=/);
  const clear = page.getByRole("button", { name: "clear brush window" });
  await expect(clear).toBeVisible();
  await clear.click();
  await expect(page).not.toHaveURL(/[?&]ws=/);
  await expect(clear).toHaveCount(0);
  await context.close();
});

test("dragging the zoom slider zooms without setting a brush window", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  // The vertical slider is the chart's rightmost 16 px (Timeline.tsx: `right: 4, width: 12`),
  // outside the plot grid, on the same canvas the brush listens to.
  const chart = page.getByTestId("delivery-chart");
  const box = await chart.boundingBox();
  if (box === null) throw new Error("no chart");
  const x = box.x + box.width - 10;
  await page.mouse.move(x, box.y + box.height * 0.4);
  await page.mouse.down();
  await page.mouse.move(x, box.y + box.height * 0.5, { steps: 6 });
  await page.mouse.move(x, box.y + box.height * 0.6, { steps: 6 });
  await page.mouse.up();

  await expect(page.getByTestId("delivery-brush-band")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "clear brush window" })).toHaveCount(0);
  await expect(page).not.toHaveURL(/[?&](ws|we)=/);
  await expect(page.getByText("2 PRs in current filter/window")).toBeVisible();
  await context.close();
});

test("on a phone a finger dragged along the chart sets a brush window and moves nothing else", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "a finger drag exercises the phone project");
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  // A real touch through Chromium's Input.dispatchTouchEvent (touch.ts), not page.mouse, which
  // the other rows use on this project too. The finger lands on #1's merge dot, so its first
  // contact shows that dot's tooltip, and drags from there along the time axis.
  const box = await page.getByTestId("delivery-chart").boundingBox();
  if (box === null) throw new Error("no chart");
  const dot = await shapePosition(page, { series: "PR merges" });
  if (dot === null) throw new Error("no merge dot painted");
  const layout = () =>
    page.evaluate(() => ({
      scrollY: Math.round(window.scrollY),
      width: document.documentElement.scrollWidth,
    }));
  const before = await layout();
  const finger = await pressFinger(page, dot);
  await expect(page.getByText("acme/widgets#1: feat: a shipped widget")).toBeVisible();
  // The tooltip stays inside the chart: ECharts paints it inside the chart's host, and before the
  // host's wrapper clipped it, its first frame stood past the viewport's right edge, widening the
  // page to 462 px and jumping it upward under the finger.
  expect(await layout()).toEqual(before);
  // Up the time axis (later times) from the dot, which sits near the chart's foot.
  await finger.moveTo({ x: dot.x, y: Math.max(box.y + 20, dot.y - box.height * 0.4) }, 8);
  await expect(page.getByTestId("delivery-brush-band")).toBeVisible();
  // The chart claims the finger (touch-action: none): the page does not scroll under it.
  expect(await layout()).toEqual(before);
  await finger.lift();

  await expect(page).toHaveURL(/[?&]ws=.*[?&]we=/);
  await expect(page.getByRole("button", { name: "clear brush window" })).toBeVisible();
  await context.close();
});

test("the list sorts by its headers and a row opens the PR's details", async ({
  browser,
}, testInfo) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  await page.getByRole("button", { name: "List", exact: true }).click();
  await expect(page).toHaveURL(/mode=list/);
  const titles = page.locator("tbody tr td:nth-child(4)");
  await expect(titles).toHaveText(["fix: a waiting widget", "feat: a shipped widget"]);
  await page.getByRole("button", { name: /^Merged/ }).click();
  await expect(titles).toHaveText(["feat: a shipped widget", "fix: a waiting widget"]);
  await expect(page.getByText("ACME-1 · P0")).toBeVisible();

  // The Repository facet narrows the list server-side; acme/widgets is the only repository
  // seeded, so selecting it is a no-op on the result but proves the picker and the facet
  // round-trip to the server run. Each option carries its count.
  await page.getByRole("button", { name: /^Repository:/ }).click();
  await page.getByRole("option", { name: "acme/widgets, 2 PRs" }).click();
  await page.keyboard.press("Escape");
  await expect(titles).toHaveCount(2);

  await page.getByRole("cell", { name: "feat: a shipped widget" }).click();
  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("link", { name: "Open on GitHub" })).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/pull/1"
  );
  await expect(details.getByRole("link", { name: "ACME-1 — Ship the widgets" })).toBeVisible();
  await expect(details.getByText("P0", { exact: true })).toBeVisible();

  await page.screenshot({ path: testInfo.outputPath("delivery-drilldown.png"), fullPage: true });
  await context.close();
});

test("the list reaches a deploy's drill-down by keyboard, and Escape hands focus back", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/delivery?${FIXTURE_WINDOW}&mode=list`);

  // The rows are one tab stop: the first row takes focus, the arrow key moves to the next.
  const rows = page.locator("tbody tr[aria-rowindex]");
  await expect(rows).toHaveCount(2);
  await rows.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("Home");
  await expect(rows.first()).toBeFocused();

  // #1 shipped in run 500: its "deployed" opens that run's drill-down.
  const deployed = page.getByRole("button", { name: "Open deploy run #500" });
  await deployed.focus();
  await page.keyboard.press("Enter");
  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Deploy: run #500" })).toBeVisible();
  await expect(details.getByRole("button", { name: "Close details" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(details).toHaveCount(0);
  await expect(deployed).toBeFocused();
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

  await page.goto(`/delivery?${FIXTURE_WINDOW}`);

  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
  await expect(
    page.getByText(/^PRs \d+ h ago: the installation lacks Actions: read on acme\/widgets$/)
  ).toBeVisible();
  // Never the never-checked wording once a pass has succeeded.
  await expect(page.getByText("PRs never checked")).toHaveCount(0);

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

  await page.goto(`/delivery?${FIXTURE_WINDOW}`);

  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
  await expect(
    page.getByText(/1 pull request can no longer be fetched from GitHub/i)
  ).toBeVisible();

  await context.close();
});

test("an unconfigured Delivery page sets itself up from its form and then shows the timeline", async ({
  browser,
}, testInfo) => {
  // The save proves the GitHub App can read the deploy repository (the installation with
  // Contents: read, a minted token, then the repository read under it); the fake answers all
  // three for a seeded repository and 404s an unseeded one.
  await seedFakeGithub({ "acme/widgets": { contents: "read", installation_id: 301 } });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/delivery?${FIXTURE_WINDOW}`);
    await expect(page.getByRole("heading", { name: "Set up the delivery timeline" })).toBeVisible();
    await expect(page.getByLabel("Deploy repository")).toHaveValue("");
    await expect(page.getByLabel("Deploy repository")).toHaveAttribute("placeholder", "owner/repo");

    // A repository the App cannot read is refused with the server's own reason, and nothing is
    // stored: the page still offers the setup form.
    await page.getByLabel("Deploy repository").fill("acme/unseen");
    await page.getByLabel("Deploy workflow").fill(".github/workflows/deploy.yml");
    await page.getByLabel("Production job").fill("widgets-release");
    await page.getByLabel("PR checks workflow").fill(".github/workflows/pr-checks.yml");
    await page.getByLabel("Population authors").fill("octocat\noctocat-agent[bot]");
    await page.getByLabel("Excluded repositories").fill("acme/dojo");
    await page.getByRole("button", { name: "Save delivery settings" }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "the GitHub App is not installed on acme/unseen" })
    ).toBeVisible();
    await expect(page.getByRole("heading", { name: "Set up the delivery timeline" })).toBeVisible();
    expect(await sql("SELECT count(*) FROM delivery_settings")).toBe("0");
    await page.screenshot({
      path: testInfo.outputPath("delivery-setup-refused.png"),
      fullPage: true,
    });

    await page.getByLabel("Deploy repository").fill("acme/widgets");
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/settings/delivery") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Save delivery settings" }).click();
    await saved;

    // The timeline replaces the form: nothing has been reconciled yet, so the freshness row
    // reads never-happened and the list view is empty.
    await expect(page.getByRole("heading", { name: "Set up the delivery timeline" })).toHaveCount(
      0
    );
    await expect(page.getByText("PRs never checked")).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("delivery-after-save.png"),
      fullPage: true,
    });
    await page.getByRole("button", { name: "List", exact: true }).click();
    await expect(page.getByText("No PRs in the current filter/window.")).toBeVisible();
    expect(
      await sql(
        `SELECT deploy_repo || ' ' || array_to_string(population_authors, ',') || ' ' || array_to_string(excluded_repos, ',') FROM delivery_settings`
      )
    ).toBe("acme/widgets octocat,octocat-agent[bot] acme/dojo");

    // Settings shows the same record, to change it later.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    await expect(page.getByLabel("Deploy repository")).toHaveValue("acme/widgets");
    await expect(page.getByLabel("Population authors")).toHaveValue("octocat\noctocat-agent[bot]");
    await page.screenshot({
      path: testInfo.outputPath("delivery-settings.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
