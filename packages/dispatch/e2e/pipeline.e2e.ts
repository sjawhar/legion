import { expect, test } from "@playwright/test";
import { mintAgentToken } from "./api";
import { PIPELINE_AT, seedPipelineFixture } from "./delivery-seed";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("the Pipeline rail entry opens the page", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  // From another page, so the sidebar's own Pipeline link is what opens it. Below `xl` the sidebar
  // is a sheet behind "Open navigation"; each branch of the race catches its own rejection, as
  // delivery.e2e.ts's rail test explains.
  await page.goto("/");
  const menuButton = page.getByRole("button", { name: "Open navigation" });
  const pipelineLink = page.getByRole("link", { name: /^Pipeline/ });
  await Promise.race([
    menuButton.waitFor({ state: "visible" }).catch(() => {}),
    pipelineLink.waitFor({ state: "visible" }).catch(() => {}),
  ]);
  if (await menuButton.isVisible()) {
    await menuButton.click();
  }
  await expect(pipelineLink).toBeVisible();
  await pipelineLink.click();

  await expect(page).toHaveURL(/\/pipeline$/);
  await expect(page.getByRole("heading", { level: 1, name: "Pipeline" })).toBeVisible();
  await expect(page).toHaveTitle("Pipeline · Dispatch");

  await context.close();
});

test("the Pipeline page shows each stage's timing and the failing jobs", async ({
  browser,
}, testInfo) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/pipeline?at=${PIPELINE_AT}`);

  await expect(page.getByText("Last 7 days, Sep 26 to Oct 2 (UTC)")).toBeVisible();
  // Today's two timed PR-checks runs; the merge_group run is not one of them.
  await expect(page.locator('[data-metric="pr_ci"]')).toContainText("p90 30m · 2 runs");
  const e2e = page.locator('[data-metric="staging_e2e"]');
  await expect(e2e).toContainText("staging-e2e / run");
  await expect(e2e).toContainText("55m"); // today's median of 40 and 70
  await expect(e2e).toContainText("50%");
  await expect(page.locator('[data-metric="staging_apply"]')).toContainText("p90 8.8m · 3 runs");
  // The feature/x run's successful production job is not counted, so the week has one result.
  const production = page.locator('[data-metric="production_apply"]');
  await expect(production).toContainText("100%");
  await expect(production).toContainText("p90 9.0m · 1 run");
  await expect(
    page.locator('[data-metric="merge_to_production"] [data-target-met]')
  ).toHaveAttribute("data-target-met", "false");
  await expect(page.locator('[data-metric="opened_to_merge"] [data-target-met]')).toHaveAttribute(
    "data-target-met",
    "true"
  );

  const slowest = page.locator('[data-section="slowest-jobs"]');
  await expect(slowest.locator('[data-job="build / unit"]')).toBeVisible();
  const failing = page.locator('[data-section="failing-jobs"]');
  const unit = failing.locator('[data-failing-job="build / unit"]');
  await expect(unit.locator("td").nth(1)).toHaveText("2");
  await expect(unit.locator("td").nth(2)).toHaveText("3");
  await expect(unit.getByRole("link")).toHaveAttribute(
    "href",
    "https://github.com/acme/widgets/actions/runs/2"
  );
  // The roll-up job is left out; a job that only failed before the 24 hours is not listed.
  await expect(failing.locator('[data-failing-job="checks-complete"]')).toHaveCount(0);
  await expect(failing.locator('[data-failing-job="typecheck"]')).toHaveCount(0);

  // On a phone the six cards stack in one column: the page is narrower than the 46rem the
  // second column needs.
  const viewport = page.viewportSize();
  if (viewport !== null && viewport.width < 768) {
    const first = await page.locator('[data-metric="pr_ci"]').boundingBox();
    const second = await page.locator('[data-metric="staging_e2e"]').boundingBox();
    expect(first).not.toBeNull();
    expect(second).not.toBeNull();
    if (first !== null && second !== null) {
      expect(second.y).toBeGreaterThanOrEqual(first.y + first.height);
      expect(Math.abs(second.x - first.x)).toBeLessThan(2);
    }
  }

  await page.screenshot({ path: testInfo.outputPath("pipeline.png"), fullPage: true });
  await context.close();
});

test("a pipeline with no staging end-to-end gate omits its card", async ({ browser }) => {
  await seedPipelineFixture();
  await sql("UPDATE delivery_settings SET staging_e2e_job_name = null WHERE singleton");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/pipeline?at=${PIPELINE_AT}`);

  await expect(page.locator('[data-metric="pr_ci"]')).toBeVisible();
  await expect(page.locator('[data-metric="staging_e2e"]')).toHaveCount(0);
  await expect(page.locator('[data-metric="staging_apply"]')).toBeVisible();
  await context.close();
});

test("the pipeline API gives an agent the same numbers", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const token = await mintAgentToken("pipeline-e2e", "alice");
  const response = await page.request.get(`/api/v1/delivery/pipeline?at=${PIPELINE_AT}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(response.status()).toBe(200);
  const body = (await response.json()) as {
    days: string[];
    staging_e2e: { days: { n: number; median_minutes: number | null }[] } | null;
    pr_checks: { failing_jobs: { name: string; failures: number; results: number }[] };
    production_apply: { total: { n: number } };
  };
  expect(body.days.at(-1)).toBe("2026-10-02");
  expect(body.staging_e2e?.days[6]?.n).toBe(2);
  expect(body.staging_e2e?.days[6]?.median_minutes).toBe(55);
  expect(body.pr_checks.failing_jobs[0]).toEqual(
    expect.objectContaining({ name: "build / unit", failures: 2, results: 3 })
  );
  expect(body.production_apply.total.n).toBe(1);
  await context.close();
});
