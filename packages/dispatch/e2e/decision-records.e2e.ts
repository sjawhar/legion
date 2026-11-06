import { expect, test } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue/page-words";
import { createIssue, createIssueArtifact, createProject, editArtifact, getArtifact } from "./api";
import { blockId } from "./editor";
import { gotoIssue, nodeRows } from "./issue-page";
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

  // The Decisions preset: the decision is a node of the issue's tree, the preset opens on it
  // because it waits for the reader's review, and its body holds Keep and Change. Until
  // 2026-11-06 this read the Decisions tab's four sections of cards
  // (`features/issue/DecisionsPanel.tsx`, `region "Waiting for your review"`), which
  // dispatch://INFRA-1196 replaced with the tree: the sections are gone, so what waits for the
  // reader is the decision whose node carries Keep, and a shaped link that went out of date is
  // a word on that node's own chip instead of a line in a card.
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  const decision = nodeRows(page).filter({ hasText: DECISION }).first();
  await expect(decision).toBeVisible();
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(0);

  // Change opens the composer on the decision's own text and posts a comment anchored to it.
  await decision.focus();
  await decision.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }).click();
  const composer = page.getByRole("form", { name: `Change ${DECISION}` });
  await expect(composer.getByTestId("decision-quote")).toContainText(REASON);
  await composer.getByRole("textbox").fill("Measure the file store first.");
  await composer.getByRole("button", { name: CONTROL_LABELS.send_change }).click();
  await expect(composer).toBeHidden();

  // The agent changes the decision's text: both blocks it links are out of date, on the
  // Decisions preset and as a mark on the Spec tab.
  await editArtifact(decisions.artifact.id, {
    ops: [{ find: REASON, op: "replace", with: `${REASON} Measured against the file store.` }],
    summary: "measured",
  });
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(2);
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(2);

  // The agent changes one of those spec blocks: its mark goes and the other stays.
  await editArtifact(spec.id, {
    ops: [{ find: "Store each notice.", op: "replace", with: "Store each notice in a table." }],
    summary: "stored",
  });
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(1);
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(1);

  // Keep records the reader's agreement: the decision stops waiting for the reader's review,
  // so the control that takes it there is gone and the decision is still a node of the tree.
  const waiting = nodeRows(page).filter({ hasText: DECISION }).first();
  await waiting.focus();
  await waiting.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision }).click();
  await expect(
    page.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
  ).toHaveCount(0);
  await expect(nodeRows(page).filter({ hasText: DECISION }).first()).toBeVisible();
});
