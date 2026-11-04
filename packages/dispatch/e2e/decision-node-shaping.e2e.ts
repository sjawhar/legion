import { expect, test } from "@playwright/test";

import { createIssue, createProject, editArtifact, getArtifact } from "./api";
import { blockId } from "./editor";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

test("shaped decisions remain visible on live and historical specs, with honest duplicate matching", async ({ page }) => {
  const issue = await createIssue({
    project: "CORE",
    title: "Passage identity",
    spec: ":::callout{#stable kind=\"note\" title=\"\"}\nStable typed passage.\n:::\n\nRepeated passage.\n\nUnique passage.\n\nRepeated passage.\n\nUnrelated context.\n",
  });
  const spec = await getArtifact(issue.primary_artifact_id ?? "");
  const repeated = await blockId(spec.id, "Repeated passage.");
  const unique = await blockId(spec.id, "Unique passage.");
  const shaped = `dispatch://${issue.key}/spec#b-stable dispatch://${issue.key}/spec#b-${repeated} dispatch://${issue.key}/spec#b-${unique}`;
  await editArtifact(spec.id, {
    ops: [{ op: "insert", after: "end", markdown: `:::decision{#storage title="Keep the original passages" shaped="${shaped}"}\nThe reason stays on the decision node.\n:::\n` }],
  });
  await page.goto(`/issues/${issue.key}/spec`);
  const live = page.getByRole("article", { name: "Document", exact: true });
  await expect(live.locator("[data-decision-shaped]")).toHaveCount(3);
  await expect(live.locator("[data-decision-shaped]").first()).toContainText("The reason stays on the decision node.");
  await expect(live.getByText("One of 2 identical passages", { exact: false })).toHaveCount(0);

  // Another version leaves all three relations visible by stable block identity.
  await editArtifact(spec.id, { ops: [{ op: "replace", find: "Unrelated context.", with: "New unrelated context." }] });
  await page.reload();
  await expect(live.locator("[data-decision-shaped]")).toHaveCount(3);

  // A newer version whose shaped words changed states that fact at the passage itself.
  await editArtifact(spec.id, { ops: [{ op: "replace", find: "Unique passage.", with: "Unique passage updated." }] });
  await page.reload();
  await expect(live.getByText("This passage changed since the decision.", { exact: true })).toBeVisible();

  // Typed IDs survive in stored Markdown. The unique untyped text matches exactly; the two
  // identical untyped candidates are both shown and labelled, never assigned invented IDs.
  await page.goto(`/issues/${issue.key}/spec?v=1`);
  const historical = page.getByTestId("version-view");
  await expect(historical.locator("[data-decision-shaped]")).toHaveCount(4);
  await expect(historical.getByText("One of 2 identical passages; the stored version records no position.", { exact: true })).toHaveCount(2);
  await expect(historical.locator('[data-block-id="stable"]')).toContainText("Stable typed passage.");
  await expect(historical.locator("[data-decision-shaped]").filter({ hasText: "Keep the original passages" }).first()).toBeVisible();
});
