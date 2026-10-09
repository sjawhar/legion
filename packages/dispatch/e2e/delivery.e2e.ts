import { expect, type Page, test } from "@playwright/test";
import { seedFakeGithub } from "./fake-github-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const FIXTURE_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";
const DEPLOY_GREEN: [number, number, number] = [0x22, 0xc5, 0x5e];
const FAILURE_RED: [number, number, number] = [0xef, 0x44, 0x44];

/** The page position of the middle of the first shape of colour `rgb` the chart's canvas paints
 *  (scanning down from the top), or null. ECharts draws on a canvas, so a deploy dot or a failure
 *  triangle has no element to click; the middle of the shape is inside its hit area whatever the
 *  screen's pixel density. */
async function chartPixel(
  page: Page,
  rgb: [number, number, number]
): Promise<{ x: number; y: number } | null> {
  return page.evaluate((rgb) => {
    const canvas = document.querySelector<HTMLCanvasElement>("[data-testid=delivery-chart] canvas");
    const context = canvas?.getContext("2d");
    if (!canvas || !context) return null;
    const { width, height } = canvas;
    const data = context.getImageData(0, 0, width, height).data;
    const matches = (x: number, y: number) => {
      const i = (y * width + x) * 4;
      return (
        [0, 1, 2].every((c) => Math.abs((data[i + c] ?? 0) - rgb[c]) <= 12) &&
        (data[i + 3] ?? 0) > 200
      );
    };
    for (let y = 0; y < height; y += 1) {
      for (let x = 0; x < width; x += 1) {
        if (!matches(x, y)) continue;
        let bottom = y;
        while (bottom + 1 < height && matches(x, bottom + 1)) bottom += 1;
        const middle = Math.round((y + bottom) / 2);
        let left = x;
        let right = x;
        while (left > 0 && matches(left - 1, middle)) left -= 1;
        while (right + 1 < width && matches(right + 1, middle)) right += 1;
        const rect = canvas.getBoundingClientRect();
        return {
          x: rect.left + (((left + right) / 2) * rect.width) / width,
          y: rect.top + (middle * rect.height) / height,
        };
      }
    }
    return null;
  }, rgb);
}

/** Opens the seeded timeline and waits for its chart to paint a deploy. On a phone the facets sit
 *  above the chart, so the chart is scrolled into view before anything points at it. */
async function openTimeline(page: Page, query = ""): Promise<void> {
  await page.goto(`/delivery?${FIXTURE_WINDOW}${query}`);
  await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
  await page.getByTestId("delivery-chart").scrollIntoViewIfNeeded();
  await expect.poll(() => chartPixel(page, DEPLOY_GREEN)).not.toBeNull();
}

/** Clicks the first shape of colour `rgb`, found again at the moment of the click: a phone's
 *  browser can scroll the page between a hover and the click that follows it. The pointer first
 *  leaves the chart, since ECharts repaints a hovered shape in its emphasis colour, which the scan
 *  would not find. */
async function clickShape(page: Page, rgb: [number, number, number]): Promise<void> {
  await page.mouse.move(0, 0);
  await expect.poll(() => chartPixel(page, rgb)).not.toBeNull();
  const shape = await chartPixel(page, rgb);
  if (shape === null) throw new Error(`no shape of rgb(${rgb.join(", ")}) painted`);
  await page.mouse.click(shape.x, shape.y);
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
  const freshness = page.getByRole("status", { name: "Source freshness" });
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
  await expect(page.getByRole("button", { name: "Priority", exact: true })).toHaveText(
    "Any priority"
  );
  await page.getByRole("button", { name: "Priority", exact: true }).click();
  const options = page.getByRole("listbox", { name: "Priority options" }).getByRole("option");
  await expect(options).toHaveText([/^P0\s*1$/, /^No issue\s*1$/]);
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

  const deploy = await chartPixel(page, DEPLOY_GREEN);
  if (deploy === null) throw new Error("no deploy dot painted");
  await page.mouse.move(deploy.x, deploy.y);
  await expect(page.getByText("Run #500: 1 PR(s) shipped")).toBeVisible();
  await clickShape(page, DEPLOY_GREEN);

  const details = page.getByRole("complementary", { name: "Details" });
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

  const failure = await chartPixel(page, FAILURE_RED);
  if (failure === null) throw new Error("no failure triangle painted");
  await page.mouse.move(failure.x, failure.y);
  await expect(page.getByText("Run #501 failed: integration tests")).toBeVisible();
  await clickShape(page, FAILURE_RED);

  const details = page.getByRole("complementary", { name: "Details" });
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
  await page.getByRole("button", { name: "Repository", exact: true }).click();
  await page.getByRole("option", { name: /acme\/widgets\s*2/ }).click();
  await page.keyboard.press("Escape");
  await expect(titles).toHaveCount(2);

  await page.getByRole("cell", { name: "feat: a shipped widget" }).click();
  const details = page.getByRole("complementary", { name: "Details" });
  await expect(details.getByRole("link", { name: "Open on GitHub" })).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/pull/1"
  );
  await expect(details.getByRole("link", { name: "ACME-1 — Ship the widgets" })).toBeVisible();
  await expect(details.getByText("P0", { exact: true })).toBeVisible();

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
