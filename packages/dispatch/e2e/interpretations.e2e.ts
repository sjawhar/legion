import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";
import { answerAsk, createIssue, createProject, editArtifact, listIssueAsks } from "./api";
import { runDispatch } from "./dispatch-cli";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test("agent interpretations appear under the original person's decision, with trusted authors", async ({
  context,
  page,
}) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title: "Interpretations",
    spec: ":::ask{#choice}\nWhich name?\n:::\n",
  });
  await expect.poll(async () => (await listIssueAsks(issue.key)).length).toBe(1);
  const [ask] = await listIssueAsks(issue.key);
  if (!ask) throw new Error("missing ask");
  await answerAsk(ask.id, {
    expected_edited_at: ask.edited_at,
    selected: [],
    text: "My exact answer.",
  });
  const home = mkdtempSync(join(tmpdir(), "dispatch-interpret-"));
  try {
    const added = await runDispatch(
      [
        "interpret",
        "--ref",
        `dispatch://${issue.key}/spec`,
        "--decision",
        "choice",
        "--body",
        "I understand this to mean we keep the original name.",
      ],
      home
    );
    expect(added.code, added.stdout + added.stderr).toBe(0);
    expect(added.stdout).toContain("Interpretation by e2e-dispatch-cli:");
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
  await page.goto(`/issues/${issue.key}/decisions`);
  const decision = page.locator('[data-decision="choice"]');
  await expect(decision.getByTestId("decision-answer")).toHaveText("My exact answer.");
  await expect(decision.getByTestId("decision-interpretation")).toContainText("e2e-dispatch-cli");
  await expect(decision.getByTestId("decision-interpretation")).toContainText(
    "I understand this to mean"
  );
  await expect(page.locator("[data-decision]")).toHaveCount(1);
  // A hand-made block is visible too, but no caller attribute can claim its author.
  await editArtifact(issue.primary_artifact_id ?? "", {
    ops: [
      {
        op: "insert",
        after: "end",
        markdown: `:::interpretation{#handmade interprets="dispatch://${issue.key}/spec#b-choice"}\nA second reading, with no author recorded.\n:::\n`,
      },
    ],
  });
  await expect(decision.getByTestId("decision-interpretation")).toHaveCount(2);
  const handmade = decision.getByTestId("decision-interpretation").filter({ hasText: "A second reading" });
  await expect(handmade).toContainText("A second reading");
  await expect(handmade.locator("p").first()).toHaveText("Interpretation");
  await expect(decision.getByTestId("decision-answer")).toHaveText("My exact answer.");
  await expect(page.locator("[data-decision]")).toHaveCount(1);
});
