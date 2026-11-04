import { expect, test } from "@playwright/test";

import {
  answerAsk,
  createIssue,
  createProject,
  editArtifact,
  getArtifact,
  listIssueAsks,
} from "./api";
import { blockId } from "./editor";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

test("shaped decisions remain visible on live and historical specs, with honest duplicate matching", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    title: "Passage identity",
    spec: ':::callout{#stable kind="note" title=""}\nStable typed passage.\n:::\n\nRepeated passage.\n\nUnique passage.\n\nRepeated passage.\n\nUnrelated context.\n',
  });
  const spec = await getArtifact(issue.primary_artifact_id ?? "");
  const repeated = await blockId(spec.id, "Repeated passage.");
  const unique = await blockId(spec.id, "Unique passage.");
  const shaped = `dispatch://${issue.key}/spec#b-stable@v1 dispatch://${issue.key}/spec#b-${repeated}@v1 dispatch://${issue.key}/spec#b-${unique}@v1`;
  await editArtifact(spec.id, {
    ops: [
      {
        op: "insert",
        after: "end",
        markdown: `:::decision{#storage title="Keep the original passages" shaped="${shaped}"}\nThe reason stays on the decision node.\n:::\n`,
      },
    ],
  });
  await page.goto(`/issues/${issue.key}/spec`);
  const live = page.getByRole("article", { name: "Document", exact: true });
  await expect(live.locator("[data-decision-shaped]")).toHaveCount(3);
  await expect(live.locator("[data-decision-shaped]").first()).toContainText(
    "The reason stays on the decision node."
  );
  await expect(live.getByText("One of 2 identical passages", { exact: false })).toHaveCount(0);

  // Another version leaves all three relations visible by stable block identity.
  await editArtifact(spec.id, {
    ops: [{ op: "replace", find: "Unrelated context.", with: "New unrelated context." }],
  });
  await page.reload();
  await expect(live.locator("[data-decision-shaped]")).toHaveCount(3);

  // A newer version whose shaped words changed states that fact at the passage itself.
  await editArtifact(spec.id, {
    ops: [{ op: "replace", find: "Unique passage.", with: "Unique passage updated." }],
  });
  await page.reload();
  await expect(
    live.getByText("This passage changed since the decision.", { exact: true })
  ).toBeVisible();

  // Typed IDs survive in stored Markdown. The unique untyped text matches exactly; the two
  // identical untyped candidates are both shown and labelled, never assigned invented IDs.
  await page.getByRole("combobox", { name: "Version", exact: true }).selectOption("1");
  const historical = page.getByTestId("version-view");
  await expect(historical.locator("[data-decision-shaped]")).toHaveCount(4);
  await expect(
    historical.getByText("One of 2 identical passages; the stored version records no position.", {
      exact: true,
    })
  ).toHaveCount(2);
  await expect(historical.locator('[data-block-id="stable"]')).toContainText(
    "Stable typed passage."
  );
  await expect(
    historical
      .locator("[data-decision-shaped]")
      .filter({ hasText: "Keep the original passages" })
      .first()
  ).toBeVisible();
});

test("an answered ask itself shows the person's words beside the passage it shaped", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    title: "Person's decision in context",
    spec: ':::callout{#rule kind="note" title=""}\nKeep the name.\n:::\n\n:::ask{#name}\nWhich name?\n:::\n',
  });
  await expect.poll(async () => (await listIssueAsks(issue.key)).length).toBe(1);
  await editArtifact(issue.primary_artifact_id ?? "", {
    ops: [
      {
        op: "retype",
        block: "name",
        type: "ask",
        attributes: { shaped: [`dispatch://${issue.key}/spec#b-rule`] },
      },
    ],
  });
  const [ask] = await listIssueAsks(issue.key);
  if (ask === undefined) throw new Error("The source ask was not indexed");
  const answer = "Use the name I gave it.\n\nKeep these exact words.";
  await answerAsk(ask.id, { expected_edited_at: ask.edited_at, selected: [], text: answer });
  await page.goto(`/issues/${issue.key}/spec`);
  const note = page
    .getByRole("article", { name: "Document", exact: true })
    .locator("[data-decision-shaped]");
  await expect(note).toHaveCount(1);
  await expect(note).toContainText("Shaped by this person's decision");
  await expect.poll(async () => (await note.textContent())?.includes(answer)).toBe(true);
  await expect(note.getByRole("link", { name: "Which name?", exact: true })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/spec#b-name`
  );
});
