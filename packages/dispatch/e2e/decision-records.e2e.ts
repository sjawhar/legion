import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject, editArtifact, getArtifact } from "./api";
import { blockId } from "./editor";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

// The decision records end to end, against the real server: an agent writes `decisions.md` with
// one decision that links two spec blocks, and a person reads it on the Spec tab beside those
// blocks and on the Decisions tab, answers it with Keep and Change, and sees each block the
// decision has changed since marked out of date until that block changes too.
//
// Every block id comes from `GET /api/v1/artifacts/<id>/blocks` (`blockId`), never from a guess,
// and every seed goes through the API the agent uses.

const STALE_TITLE = "A decision that shaped this block changed after it.";
const DECISION = "Store notices in a table";
const REASON = "Chosen for the simplest storage.";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

test("a decision links the spec blocks it shaped, goes out of date with them, and takes Keep and Change", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    spec: "Notices are lost today.\n\nStore each notice.\n\nShow the newest first.\n",
    title: "Decision records",
  });
  const spec = await getArtifact(issue.primary_artifact_id ?? "");
  const stored = await blockId(spec.id, "Store each notice.");
  const newest = await blockId(spec.id, "Show the newest first.");
  const specVersions = spec.versions.length;

  const reference = `dispatch://${issue.key}/spec`;
  const decisions = await createIssueArtifact(issue.key, {
    content: `## ${DECISION}\n\n${REASON} Shapes ${reference}#b-${stored} and ${reference}#b-${newest}.\n`,
    name: "decisions.md",
  });
  expect(decisions.artifact.slug).toBe("decisions-md");

  // Writing decisions.md writes the spec no version of its own.
  expect((await getArtifact(spec.id)).versions).toHaveLength(specVersions);

  // The Spec tab: the decision is a margin card, and no block is out of date yet.
  await page.goto(`/issues/${issue.key}/spec`);
  const cards = page.getByRole("article", { name: `Decision: ${DECISION}` });
  await expect(cards).toHaveCount(2);
  await expect(page.locator(`[data-margin-item$=":${stored}"]`)).toHaveCount(1);
  await expect(page.locator(`[data-margin-item$=":${newest}"]`)).toHaveCount(1);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(0);

  // The Decisions tab lists it under the reader's review, with both blocks and neither out of
  // date.
  await page.goto(`/issues/${issue.key}/decisions`);
  const waiting = page.getByRole("region", { name: "Waiting for your review" });
  await expect(waiting.getByRole("heading", { name: DECISION })).toBeVisible();
  await expect(waiting.getByText("out of date")).toHaveCount(0);

  // Change opens the composer on the decision's own text and posts a comment anchored to it.
  await waiting.getByRole("button", { name: "Change" }).click();
  const composer = page.getByRole("form", { name: `Change ${DECISION}` });
  await expect(composer.getByTestId("decision-quote")).toContainText(REASON);
  await composer.getByRole("textbox").fill("Measure the file store first.");
  await composer.getByRole("button", { name: "Send" }).click();
  await expect(composer).toBeHidden();

  // The agent changes the decision's text: both blocks it links are out of date, on the Decisions
  // tab and as a mark on the Spec tab.
  await editArtifact(decisions.artifact.id, {
    ops: [{ find: REASON, op: "replace", with: `${REASON} Measured against the file store.` }],
    summary: "measured",
  });
  await page.goto(`/issues/${issue.key}/decisions`);
  await expect(page.getByText("out of date")).toHaveCount(2);
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(2);

  // The agent changes one of those spec blocks: its mark goes and the other stays.
  await editArtifact(spec.id, {
    ops: [{ find: "Store each notice.", op: "replace", with: "Store each notice in a table." }],
    summary: "stored",
  });
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(1);
  await page.goto(`/issues/${issue.key}/decisions`);
  await expect(page.getByText("out of date")).toHaveCount(1);

  // Keep records the reader's agreement: the decision leaves the section that waits for it and
  // stands under the decisions that are theirs.
  await page
    .getByRole("region", { name: "Waiting for your review" })
    .getByRole("button", { name: "Keep" })
    .click();
  await expect(
    page.getByRole("region", { name: "Waiting for your review" }).getByRole("heading", {
      name: DECISION,
    })
  ).toHaveCount(0);
  await expect(
    page.getByRole("region", { name: "Your decisions" }).getByRole("heading", { name: DECISION })
  ).toBeVisible();
});
