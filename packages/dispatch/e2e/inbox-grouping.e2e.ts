import { expect, type Page, test } from "@playwright/test";

import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getInbox,
  patchIssue,
  snoozeAsk,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-inbox-grouping", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

async function seededGroups() {
  await createProject({ key: "CORE", name: "Core" });
  const leading = await createIssue({ project: "CORE", title: "Leading other issue" });
  await patchIssue(leading.key, { priority: 0 });
  const groupedIssue = await createIssue({ project: "CORE", title: "Grouped issue" });
  const trailing = await createIssue({ project: "CORE", title: "Trailing other issue" });
  await patchIssue(trailing.key, { priority: 2 });

  const leadingAsk = await createAsk(leading.key, { question: "Leading other ask" }, session);
  const first = await createAsk(groupedIssue.key, { question: "Grouped first" }, session);
  const second = await createAsk(groupedIssue.key, { question: "Grouped second" }, session);
  const third = await createAsk(groupedIssue.key, { question: "Grouped third" }, session);
  const trailingAsk = await createAsk(trailing.key, { question: "Trailing other ask" }, session);
  await createComment(
    groupedIssue.key,
    { ask_id: first.id, body: "The first grouped ask became active last." },
    session
  );

  const document = await createProjectDocument("CORE", {
    content: "# Grouped document\n",
    name: "Grouped document",
  });
  const documentFirst = await createArtifactAsk(
    document.artifact.id,
    { question: "Document first" },
    session
  );
  const documentSecond = await createArtifactAsk(
    document.artifact.id,
    { question: "Document second" },
    session
  );

  return {
    document,
    documentAsks: [documentFirst, documentSecond],
    groupedAsks: [first, second, third],
    groupedIssue,
    otherAsks: [leadingAsk, trailingAsk],
  };
}

async function inboxRows(page: Page): Promise<string[]> {
  return page.locator("[data-inbox-row]").evaluateAll((rows) =>
    rows.map((row) => row.getAttribute("data-inbox-row") ?? "")
  );
}

test("groups an issue and document's asks while rows remain the flat keyboard list", async ({
  browser,
}) => {
  const seed = await seededGroups();
  const singletonIssue = await createIssue({ project: "CORE", title: "Singleton issue" });
  const singleton = await createAsk(singletonIssue.key, { question: "Only ask" }, session);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/?view=everyone");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(8);

    const issueHeader = page.locator('[data-inbox-group-header="issue:CORE-2"]');
    await expect(issueHeader).toContainText("CORE-2");
    await expect(issueHeader).toContainText("Grouped issue");
    await expect(issueHeader).toContainText("3 asks");

    const documentHeader = page.locator('[data-inbox-group-header="document:CORE/grouped-document"]');
    await expect(documentHeader).toContainText("CORE · Grouped document");
    await expect(documentHeader).toContainText("2 asks");

    const served = await getInbox({ login: "alice" });
    const groupedIds = new Set(seed.groupedAsks.map((ask) => ask.id));
    const documentIds = new Set(seed.documentAsks.map((ask) => ask.id));
    const rowIds = await inboxRows(page);
    const ungrouped = rowIds.filter((id) => !groupedIds.has(id) && !documentIds.has(id));
    expect(ungrouped).toEqual(
      served
        .map((row) => row.id)
        .filter((id) => !groupedIds.has(id) && !documentIds.has(id))
    );

    const firstGroupId = served.find((row) => groupedIds.has(row.id))?.id;
    if (firstGroupId === undefined) throw new Error("server did not list a grouped ask");
    const firstGroupRow = page.locator(`[data-inbox-row="${firstGroupId}"]`);
    await expect(firstGroupRow).toHaveAttribute("data-inbox-owner-key", "issue:CORE-2");
    await expect(firstGroupRow.locator("xpath=preceding-sibling::*[1]")).toHaveAttribute(
      "data-inbox-group-header",
      "issue:CORE-2"
    );
    await expect(page.locator(`[data-inbox-row="${singleton.id}"]`)).not.toHaveAttribute(
      "data-inbox-group"
    );
    await expect(page.locator(`[data-inbox-group-header="issue:${singletonIssue.key}"]`)).toHaveCount(
      0
    );

    const expectedForward = await inboxRows(page);
    await page.locator("body").focus();
    const forward: string[] = [];
    for (const _ of expectedForward) {
      await page.keyboard.press("j");
      forward.push(
        await page.evaluate(
          () => document.activeElement?.getAttribute("data-inbox-row") ?? ""
        )
      );
    }
    expect(forward).toEqual(expectedForward);

    const expectedBackward = [...expectedForward].reverse();
    await page.evaluate(() => {
      const active = document.activeElement;
      if (active instanceof HTMLElement) active.blur();
    });
    const backward: string[] = [];
    for (const _ of expectedBackward) {
      await page.keyboard.press("k");
      backward.push(
        await page.evaluate(
          () => document.activeElement?.getAttribute("data-inbox-row") ?? ""
        )
      );
    }
    expect(backward).toEqual(expectedBackward);
  } finally {
    await alice.close();
  }
});

test("a grouped header crosses bands to a singleton target without making it a group", async ({
  browser,
}) => {
  const seed = await seededGroups();
  const moving = seed.groupedAsks[2];
  if (moving === undefined) throw new Error("grouped ask missing");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/?view=everyone");
    const header = page.locator('[data-inbox-group-header="issue:CORE-2"]');
    await expect(header).toContainText("3 asks");

    await createComment(
      seed.groupedIssue.key,
      { ask_id: moving.id, body: "I am looking into the grouped ask.", turn: "agent" },
      session
    );

    await expect(header).toContainText("2 asks");
    const jump = header.getByRole("button", {
      name: "Jump to CORE-2's asks Waiting on agents",
    });
    await expect(jump).toHaveText("1 more waiting on agents");
    await jump.click();

    const target = page.locator(`[data-inbox-row="${moving.id}"]`);
    await expect(target).toBeFocused();
    await expect(target).not.toHaveAttribute("data-inbox-group", "issue:CORE-2");
  } finally {
    await alice.close();
  }
});

test("a grouped header opens Later and focuses its snoozed target", async ({ browser }) => {
  const seed = await seededGroups();
  const deferred = seed.groupedAsks[2];
  if (deferred === undefined) throw new Error("grouped ask missing");
  await snoozeAsk(deferred.id, new Date(Date.now() + 60_000).toISOString(), { login: "alice" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/?view=everyone");
    const header = page.locator('[data-inbox-group-header="issue:CORE-2"]');
    const jump = header.getByRole("button", { name: "Jump to CORE-2's asks Later" });
    await expect(jump).toHaveText("1 more later");
    await jump.click();

    await expect(page.getByRole("button", { name: "Later (1)" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await expect(page.locator(`[data-inbox-row="${deferred.id}"]`)).toBeFocused();
  } finally {
    await alice.close();
  }
});
