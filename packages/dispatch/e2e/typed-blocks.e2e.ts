import { expect, test } from "@playwright/test";

import { createProject, createProjectDocument } from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("typedBlockDirectiveRoundTrip", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n',
    name: "Typed blocks",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/typed-blocks");

    const callout = documentEditor(page).locator('[data-proof-block-type="callout"]');
    await expect(callout).toContainText("Body text.");
    await expect(callout).toHaveAttribute("data-block-id", "callout-1");
    await expect(callout.locator("[data-proof-block-name]")).toHaveText("callout");
    await expect(callout.locator('[data-proof-block-attribute="kind"]')).toHaveText("warning");
    await expect(callout.locator('[data-proof-block-attribute="title"]')).toHaveText("Read this");
  } finally {
    await alice.close();
  }
});

// A header listing every node attribute would show a reader `callout / kind / title / blockId` as
// bare lines - the internal block id included - above the block's own words.
test("a typed block's header shows the author's attributes and none of the internals", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n',
    name: "Typed blocks",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/typed-blocks");
    const callout = documentEditor(page).locator('[data-proof-block-type="callout"]');
    const header = callout.locator("[data-proof-block-summary]");
    await expect(header).toBeVisible();

    // The author's own attributes, each in its own role.
    await expect(header.locator("[data-proof-block-name]")).toHaveText("callout");
    await expect(header.locator("[data-proof-block-badge]")).toHaveText("warning");
    await expect(header.locator("[data-proof-block-title]")).toHaveText("Read this");

    // Nothing the author did not write: no block id, anywhere in the header.
    await expect(header).not.toContainText("callout-1");
    await expect(header).not.toContainText("blockId");
    await expect(header.locator('[data-proof-block-attribute="blockId"]')).toHaveCount(0);

    // The header reads as a label rather than a stack of prose lines: one row.
    const box = await header.boundingBox();
    expect(box?.height ?? 0).toBeLessThanOrEqual(40);
  } finally {
    await alice.close();
  }
});
