import { expect, test } from "@playwright/test";

import { answerAsk, createIssue, createProject, editArtifact, listIssueAsks } from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

const QUESTION = "Which name do we use?";
const ANSWER = "Use the name I gave it.\n\nDo not replace my words with an agent's summary.";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

test("Decisions is first and reads a person's answer from the spec's own ask", async ({ page }) => {
  const issue = await createIssue({
    project: "CORE",
    spec: `The name stays.\n\n:::ask{#name}\n${QUESTION}\n\n- Keep: Keep the name.\n- Change: Pick another.\n:::\n`,
    title: "Decision nodes",
  });
  await expect.poll(async () => (await listIssueAsks(issue.key)).length).toBe(1);
  const [ask] = await listIssueAsks(issue.key);
  if (ask === undefined) throw new Error("The spec's ask was not indexed");
  await answerAsk(ask.id, { expected_edited_at: ask.edited_at, selected: ["Keep"], text: ANSWER });
  await page.goto(`/issues/${issue.key}/decisions`);
  await expect(page.getByRole("tablist", { name: "Issue detail" }).getByRole("tab").first()).toHaveText("Decisions");
  const decisions = page.getByRole("region", { name: "Your decisions" });
  await expect(decisions.getByText(QUESTION, { exact: true })).toBeVisible();
  await expect.poll(async () => (await decisions.getByTestId("decision-answer").textContent())?.endsWith(ANSWER)).toBe(true);
  await expect(decisions.getByTestId("decision-answer")).toContainText("Selected: Keep");

  // An unrelated spec version does not hide the decision or rewrite the person's answer.
  await editArtifact(issue.primary_artifact_id ?? "", {
    ops: [{ op: "replace", find: "The name stays.", with: "The name still stays." }],
  });
  await page.reload();
  await expect.poll(async () => (await decisions.getByTestId("decision-answer").textContent())?.endsWith(ANSWER)).toBe(true);
});
