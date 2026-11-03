// The Legion view on the project Issues page, and its settings control (dispatch://INFRA-1077,
// acceptance A1). Both specs are tagged `@legion`, so `--grep @legion` runs this file's work and
// nothing else.
//
// The board reads only what Dispatch recorded of the project's daemon, so the fixture writes those
// rows (`legion-seed.ts`); the settings control is the one place that does call the daemon, through
// Dispatch's relay, so the second spec stands a fake daemon up and reads what reached it.
import { expect, test } from "@playwright/test";

import { LEGION_PROJECT, seedLegionFixture, startFakeLegionDaemon } from "./legion-seed";
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
