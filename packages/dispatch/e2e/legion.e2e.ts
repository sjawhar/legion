// The Legion view on the project Issues page, its settings control, the Settings page's own
// Legion daemon field, the live push, `/pipeline` and the Delivery page's chart controls
// (dispatch://INFRA-1077, acceptance A1). Every spec is tagged `@legion`, so `--grep @legion`
// runs this file's work and nothing else.
//
// The board reads only what Dispatch recorded of the project's daemon, so the fixture writes those
// rows (`legion-seed.ts`); the settings control is the one place that does call the daemon, through
// Dispatch's relay, so that spec stands a fake daemon up and reads what reached it.
import { expect, test } from "@playwright/test";

import { baseUrl, userHeaders } from "./api";
import { PIPELINE_SECTION_URL, seedPipelineFixture } from "./delivery-seed";
import {
  LEGION_PROJECT,
  pushLegionObservation,
  seedLegionFixture,
  seedLegionMetricsCorpus,
  seedLegionProject,
  startFakeLegionDaemon,
} from "./legion-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The ten columns, in the order the server sends them and the view draws them. */
const COLUMNS = [
  "queue",
  "design",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "merging",
  "awaiting_person",
  "held",
];

test("the Legion view shows the daemon's columns and its cards @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();

    const view = page.getByRole("region", { name: "Legion" });
    await expect(view).toBeVisible();
    // Every column, in the server's order, each with the label the server gave it.
    await expect(view.locator("[data-legion-column]")).toHaveCount(COLUMNS.length);
    expect(
      await view
        .locator("[data-legion-column]")
        .evaluateAll((columns) =>
          columns.map((column) => column.getAttribute("data-legion-column"))
        )
    ).toEqual(COLUMNS);
    await expect(page.getByRole("region", { name: "Waiting on a person" })).toBeVisible();

    // The header: two of the four slots held, and the daemon's waiting line.
    await expect(view.locator("[data-legion-slots]")).toHaveText("2 of 4 slots in use");
    await expect(view.locator("[data-legion-queue-length]")).toHaveText("3 waiting in the queue");

    // The implementing card, with every fact the spec names on it.
    const implementing = page.getByRole("region", { name: "Implementing" });
    const card = implementing.locator('[data-legion-card="ACME-1"]');
    await expect(card).toBeVisible();
    await expect(card).toContainText("Ship the widgets");
    await expect(card).toContainText("P0");
    await expect(card.locator('[data-legion-fact="phase-time"]')).toContainText("2 h");
    await expect(card.locator('[data-legion-fact="slot"]')).toContainText("0");
    await expect(card.locator('[data-legion-fact="roles"]')).toContainText("implementer");
    await expect(card.locator('[data-legion-fact="checks"]')).toContainText("failing");

    // The queue keeps the daemon's own order: ACME-4 before ACME-3, then the blocked root, which
    // names what holds it. Neither key order nor priority order would give this.
    const queue = page.getByRole("region", { name: "Queue" });
    expect(
      await queue
        .locator("[data-legion-card]")
        .evaluateAll((cards) => cards.map((card) => card.getAttribute("data-legion-card")))
    ).toEqual(["ACME-4", "ACME-3", "ACME-5"]);
    await expect(
      queue.locator('[data-legion-card="ACME-5"] [data-legion-fact="blocked-by"]')
    ).toContainText("Blocked by ACME-1");

    // What a root waits on from a person, on its own card.
    await expect(
      page
        .getByRole("region", { name: "Waiting on a person" })
        .locator('[data-legion-card="ACME-2"] [data-legion-fact="waiting-on"]')
    ).toContainText("Waiting on ask");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("the settings control changes the admission cap of a daemon @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({ admission_cap: { applied: "at-once" } });
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();

    // Each setting with its value and where it came from, as the daemon reports them.
    const cap = page.locator('[data-legion-setting="admission_cap"]');
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("4");
    await expect(cap.locator('[data-legion-setting-source="file"]')).toContainText(
      "configuration file"
    );
    await expect(
      page.locator('[data-legion-setting="review_round_cap"] [data-legion-setting-source="route"]')
    ).toContainText("Changed here by bob");

    await page.getByLabel("New value for admission_cap").fill("6");
    await page.getByRole("button", { name: "Change admission_cap" }).click();

    // What the daemon said about the key, and the new value with the route as its source and the
    // person who changed it.
    await expect(cap.locator("[data-legion-setting-result]")).toHaveText("Applied at once");
    await expect(cap.locator("[data-legion-setting-value]")).toHaveText("6");
    await expect(cap.locator('[data-legion-setting-source="route"]')).toContainText(
      "Changed here by alice"
    );

    // Dispatch relayed the change itself, naming the person who asked for it.
    expect(daemon.changes).toEqual([{ by: "alice", changes: { admission_cap: 6 } }]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "a daemon URL set on the Settings page that brings the view", and A4's "A project with no
// Legion setting shows no Legion view". The project is seeded with its issues and its daemon's
// observations but no `legion_settings` row, so the only thing that can bring the view is the
// person naming the daemon on the Settings page, beside the Delivery settings, as the Design's
// "Where the data comes from" says.
test("a project with no Legion setting has no Legion view until the Settings page names its daemon @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionProject();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Legion" })).toHaveCount(0);

    // The Settings page, beside the Delivery settings.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Legion" })).toBeVisible();
    await page.getByLabel("Legion daemon state URL").fill(daemon.url);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    expect((await saved).status()).toBe(200);

    // The view the setting brought.
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    await expect(page.getByRole("region", { name: "Legion" })).toBeVisible();
    await expect(
      page.getByRole("region", { name: "Implementing" }).locator('[data-legion-card="ACME-1"]')
    ).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The other half of the Settings control: the URL it refuses, and the Remove that takes the
// view away again. A state URL that is not an absolute http(s) one with a host and no path is
// refused, and the page's own wording must be the server's own, since the page carries a second
// copy of the rule (`legionStateUrl` in LegionDaemonsSection.tsx) and only the server's copy
// decides what is stored.
test("the Settings page refuses a state URL that is not a daemon's, and its Remove takes the view away @legion", async ({
  browser,
}) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const bad = `${daemon.url}/legion/v1/state`;

  try {
    // What the server says about this URL, so the page can be held to its wording.
    const refusal = await fetch(new URL(`/api/v1/settings/legion/${LEGION_PROJECT}`, baseUrl), {
      body: JSON.stringify({ state_url: bad }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(refusal.status).toBe(400);
    const refused = (await refusal.json()) as { code: string; error: string };
    expect(refused.code).toBe("LEGION_SETTING_INPUT");

    await page.goto("/settings");
    const section = page.locator("[data-legion-daemons]");
    await expect(section).toContainText(daemon.url);

    // The page refuses the same URL, in the server's own words, and sends no write at all.
    let wrote = false;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
      ) {
        wrote = true;
      }
    });
    await page.getByLabel("Legion daemon state URL").fill(bad);
    await page
      .getByRole("combobox", { exact: true, name: "Legion project" })
      .selectOption(LEGION_PROJECT);
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    await expect(page.getByRole("alert")).toHaveText(refused.error);
    expect(wrote).toBe(false);
    // The refusal changed nothing: the project still reads the daemon it did.
    await expect(section).toContainText(daemon.url);

    // Remove stops the reading, and the Issues page loses its Legion view.
    const removed = page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        response.url().endsWith(`/api/v1/settings/legion/${LEGION_PROJECT}`)
    );
    await page
      .getByRole("button", { name: `Remove the Legion daemon of ${LEGION_PROJECT}` })
      .click();
    expect((await removed).status()).toBeLessThan(300);

    await page.goto(`/projects/${LEGION_PROJECT}`);
    await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Legion" })).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "a `/ws/live` push that moves a card with no reload". A read of the daemon writes the
// observations it found and announces itself on `legion.StateChannel`; the open board recomputes
// and the card moves with no navigation of any kind.
test("a live push moves a card between columns with no reload @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/projects/${LEGION_PROJECT}`);
    await page.getByRole("button", { name: "Legion" }).click();
    const implementing = page.getByRole("region", { name: "Implementing" });
    const testing = page.getByRole("region", { name: "Testing" });
    await expect(implementing.locator('[data-legion-card="ACME-1"]')).toBeVisible();
    await expect(testing.locator('[data-legion-card="ACME-1"]')).toHaveCount(0);

    // What a read of the daemon a minute later would have left, and the announcement it makes.
    await pushLegionObservation("ACME-1", "phase", '"testing"');

    // No reload: the card moves under the open page.
    await expect(testing.locator('[data-legion-card="ACME-1"]')).toBeVisible({ timeout: 15000 });
    await expect(implementing.locator('[data-legion-card="ACME-1"]')).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// A1: "`/pipeline` reaching the Pipeline section".
test("/pipeline reaches the Delivery page's Pipeline section @legion", async ({ browser }) => {
  await seedPipelineFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/pipeline");
    await expect(page).toHaveURL(/\/delivery#pipeline$/);
    const pipeline = page.locator("#pipeline");
    await expect(pipeline).toBeVisible();
    await expect(page.getByRole("heading", { name: /Pipeline/ })).toBeVisible();
  } finally {
    await context.close();
  }
});

// A1: "every chart on the Delivery page switching to its table, back to its graph, and to a
// compare range". Every chart the page draws is read off the page itself, so a chart added later
// is covered without this spec being edited. The Legion section is seeded here too, so its four
// metrics' charts are among them.
test("every chart on the Delivery page shows its table, its graph again and a compare range @legion", async ({
  browser,
}) => {
  // Four more charts than the Pipeline section's own: the Legion section's metrics are on this
  // page too, and each one is toggled, compared and toggled again here.
  test.setTimeout(120_000);
  const daemon = await startFakeLegionDaemon({});
  await seedPipelineFixture();
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(PIPELINE_SECTION_URL);
    await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    const labels = await page
      .locator("[data-chart]")
      .evaluateAll((charts) => charts.map((chart) => chart.getAttribute("data-chart") ?? ""));
    expect(labels.length).toBeGreaterThan(0);

    for (const label of labels) {
      const chart = page.locator(`[data-chart="${label}"]`);
      const view = chart.getByRole("group", { name: `${label} view` });
      await view.getByRole("button", { name: "Table" }).click();
      await expect(chart.locator(`[data-chart-table="${label}"]`)).toBeVisible();
      await view.getByRole("button", { name: "Graph" }).click();
      await expect(chart.locator(`[data-chart-table="${label}"]`)).toHaveCount(0);
      await expect(chart.locator('[data-chart-range="window"]').first()).toBeVisible();

      // The compare control's default second range: the range just before the window.
      await chart.locator(`select[data-chart-compare="${label}"]`).selectOption("preceding");
      await expect(chart.locator('[data-chart-range="compare"]').first()).toBeVisible();
      await view.getByRole("button", { name: "Table" }).click();
      // The compare column's heading names whatever a row of this chart is: a UTC day for the
      // delivery and pipeline charts, an issue, a worker type or a UTC week for the Legion ones.
      await expect(
        chart.locator(`[data-chart-table="${label}"] th`, { hasText: /^Compare / }).first()
      ).toBeVisible();
      await view.getByRole("button", { name: "Graph" }).click();
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// Spec R3/R4 and the Design's "The Delivery page", section 1: the Legion section draws the four
// metrics — work time to merge, phase time by worker type, rework, and splitting and
// follow-ups — each through the one chart component, under a project picker and a toggle for
// all issues, roots or children. The fixture's tree is one root and one child that finished in
// the window (`seedLegionMetricsCorpus`).
test("the Delivery page's Legion section draws the four metrics @legion", async ({ browser }) => {
  const daemon = await startFakeLegionDaemon({});
  await seedLegionFixture(daemon.url);
  await seedLegionMetricsCorpus();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/delivery");
    const section = page.getByRole("region", { name: "Legion: issue to merge" });
    await expect(section).toBeVisible();

    // One card per metric, each drawing its numbers through `Chart`, so each has the
    // graph-or-table toggle and the compare control every chart on this page has.
    for (const [metric, label] of [
      ["work_time", "Work time to merge"],
      ["phase_time", "Phase time by worker type"],
      ["rework", "Rework"],
      ["splitting", "Splitting and follow-ups"],
    ]) {
      const card = section.locator(`[data-legion-metric="${metric}"]`);
      await expect(card).toBeVisible();
      await expect(card.locator(`[data-chart="${label}"]`)).toBeVisible();
      await expect(card.getByRole("group", { name: `${label} view` })).toBeVisible();
      await expect(card.locator(`select[data-chart-compare="${label}"]`)).toBeVisible();
    }
    await expect(section.locator("[data-legion-metric]")).toHaveCount(4);

    // The headline numbers of the two issues that finished: both are counted, the child's
    // review round and send-back are on the rework card, and the child the architect filed
    // while designing the root is on the splitting card.
    const figure = (metric: string, label: string) =>
      section.locator(`[data-legion-metric="${metric}"] [data-legion-figure="${label}"]`);
    await expect(figure("work_time", "Issues finished")).toHaveText("2");
    // Two issues finished: the root with no review round, the child with one.
    await expect(figure("rework", "Median review rounds")).toHaveText("0.5");
    await expect(figure("rework", "Sent back at least once")).toHaveText("50.0%");
    await expect(figure("splitting", "Issues filed")).toHaveText("1");
    await expect(figure("splitting", "Roots finished")).toHaveText("1");

    // The worker types the child's work was shared out over, on the phase-time card's table.
    const phaseTime = section.locator('[data-legion-metric="phase_time"]');
    await phaseTime.getByRole("button", { name: "Table" }).click();
    const table = phaseTime.locator('[data-chart-table="Phase time by worker type"]');
    await expect(table.locator("td", { hasText: "implementer" }).first()).toBeVisible();
    await expect(table.locator("th", { hasText: "Worker type" }).first()).toBeVisible();

    // The toggle reads the metrics again for the roots alone: one of the two finished.
    await section.getByRole("button", { name: "Roots" }).click();
    await expect(figure("work_time", "Issues finished")).toHaveText("1");
    await expect(section.locator("[data-legion-metrics-scope]")).toHaveAttribute(
      "data-legion-metrics-scope",
      "roots"
    );

    // The project picker names the one project with a daemon.
    await expect(section.locator("[data-legion-metrics-project]")).toHaveValue(LEGION_PROJECT);
  } finally {
    await context.close();
    await daemon.close();
  }
});
