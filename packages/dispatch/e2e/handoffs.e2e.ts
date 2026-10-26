import { expect, type Page, test } from "@playwright/test";

import { createHandoff, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The session that writes both versions, as `dispatch handoff-write` does: under the agent bearer,
// naming itself, with the title its origin stamps on the write.
const implementer = {
  id: "e2e-handoff-implementer",
  kind: "session" as const,
  origin: { session_title: "Implementer (e2e)" },
};

/** An issue carrying v1 and v2 of its `implement` handoff, each body with a two-entry `proof`. */
async function seedHandoffs(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Handoffs on the issue" });
  await createHandoff(
    issue.key,
    "implement",
    {
      schemaVersion: 1,
      phase: "implement",
      issue: issue.key,
      completed: true,
      summary: "First pass",
      proof: [
        { claim: "unit tests pass", command: "bun run test" },
        { claim: "types check", command: "bun run typecheck" },
      ],
    },
    implementer
  );
  await createHandoff(
    issue.key,
    "implement",
    {
      schemaVersion: 1,
      phase: "implement",
      issue: issue.key,
      completed: true,
      summary: "Second pass, review addressed",
      files: ["web/src/a.ts", "web/src/b.ts"],
      proof: [
        { claim: "browser suite green", command: "bun run e2e" },
        { claim: "lint clean", command: "bunx biome check" },
      ],
    },
    implementer
  );
  return issue.key;
}

/** The Conversation's top-level card whose title is `title` exactly. */
function handoffCard(page: Page, title: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li[data-turn]")
    .filter({ has: page.getByText(title, { exact: true }) });
}

test.beforeEach(async () => {
  await resetDatabase();
});

// The Conversation half. Against the build of the commit before the Conversation change, this
// test fails (no handoff card) while the Activity test below passes: that is the negative control.
test("the Conversation lists each handoff version, and expanding one shows its fields and every proof entry", async ({
  browser,
}, testInfo) => {
  const key = await seedHandoffs();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}/conversation`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    const cards = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li[data-turn]");
    await expect(cards).toHaveCount(2);
    // Newest first: version 2 was written after version 1.
    await expect(cards.first()).toContainText("implement handoff, version 2");
    await expect(cards.last()).toContainText("implement handoff, version 1");

    const v1 = handoffCard(page, "implement handoff, version 1");
    const v2 = handoffCard(page, "implement handoff, version 2");
    await expect(v1).toHaveCount(1);
    await expect(v2).toHaveCount(1);
    await expect(v2).toContainText("Implementer (e2e)");
    await expect(v1).toContainText("Implementer (e2e)");

    const toggle = v2.getByRole("button", { name: "Show fields" });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await toggle.click();
    await expect(v2.getByRole("button", { name: "Hide fields" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );

    const field = (name: string) => v2.locator(`[data-handoff-field="${name}"]`);
    await expect(field("schemaVersion")).toContainText("1");
    await expect(field("phase")).toContainText("implement");
    await expect(field("issue")).toContainText(key);
    await expect(field("completed")).toContainText("true");
    await expect(field("summary")).toContainText("Second pass, review addressed");
    // An array field is indented JSON.
    await expect(field("files").locator("pre")).toContainText('"web/src/a.ts"');
    await expect(field("files").locator("pre")).toContainText('"web/src/b.ts"');
    // The proof list is one row per entry.
    const proof = field("proof");
    await expect(proof).toHaveCount(2);
    await expect(proof.nth(0)).toContainText("proof 1");
    await expect(proof.nth(0)).toContainText("browser suite green");
    await expect(proof.nth(1)).toContainText("proof 2");
    await expect(proof.nth(1)).toContainText("lint clean");
    // Version 1 stays folded: its body is not on the page.
    await expect(v1.locator("[data-handoff-field]")).toHaveCount(0);
    await expect(page.getByText("First pass", { exact: true })).toHaveCount(0);

    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`handoff-conversation-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

// The Activity half, which the commit before the Conversation change already passes.
test("the Activity list has one line per recorded handoff version", async ({ browser }) => {
  const key = await seedHandoffs();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}/activity`);
    await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    await expect(rows.filter({ hasText: "recorded the implement handoff" })).toHaveCount(2);
    for (const version of [1, 2]) {
      const line = `recorded the implement handoff, version ${version}`;
      await expect(rows.filter({ hasText: line })).toHaveCount(1);
    }
  } finally {
    await alice.close();
  }
});
