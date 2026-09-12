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
