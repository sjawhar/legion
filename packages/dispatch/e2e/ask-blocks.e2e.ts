import { expect, test } from "@playwright/test";

import { createIssue, createProject, editArtifact } from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("agentWrittenAskBlockAppearsAndAnswersInPlace", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Ask block" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    await expect(ask).toContainText("Should we ship?");
    await expect(page.getByText("1 open decisions")).toBeVisible();
    const form = ask.locator("form");
    await expect(form).toBeVisible({ timeout: 10_000 });
    await form.locator('textarea[name="answer"]').fill("Yes.");
    await form.getByRole("button", { name: "Answer" }).click();
    await expect(ask).toContainText("Answered by alice");
    await expect(ask).toContainText("Yes.");
  } finally {
    await alice.close();
  }
});
