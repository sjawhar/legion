import { expect, type Page, test } from "@playwright/test";

import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  patchIssue,
} from "./api";
import { inboxList, selectedRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * How the Inbox bands its one list (INFRA-450). The owner groups that once sat inside the turn
 * bands, and the jump a grouped header offered across them, are gone: the bands are a choice of
 * **Group by**, priority by default and the turn bands under **Waiting on**, and whichever is
 * chosen the rows stay one flat list the keyboard walks top to bottom.
 */

const session = {
  actor: { id: "e2e-inbox-grouping", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * Rows across three priority bands and both turn bands: a P0 issue's ask, three on a P2 issue
 * of which one has been handed back to its session, and a document's ask, which has no issue
 * and so no priority at all.
 */
async function seededBands() {
  await createProject({ key: "CORE", name: "Core" });
  const urgent = await createIssue({ project: "CORE", title: "Urgent issue" });
  await patchIssue(urgent.key, { priority: 0 });
  const steady = await createIssue({ project: "CORE", title: "Steady issue" });
  await patchIssue(steady.key, { priority: 2 });

  const urgentAsk = await createAsk(urgent.key, { question: "Urgent ask" }, session);
  const steadyFirst = await createAsk(steady.key, { question: "Steady first" }, session);
  const steadySecond = await createAsk(steady.key, { question: "Steady second" }, session);
  const handed = await createAsk(steady.key, { question: "Handed back" }, session);
  // Alice asks for clarification instead of answering, which hands this one to its session:
  // the row the **Waiting on** grouping puts in a band of its own.
  await createComment(steady.key, { ask_id: handed.id, body: "Which widths matter?" });

  const document = await createProjectDocument("CORE", {
    content: "# Banded document\n",
    name: "Banded document",
  });
  const documentAsk = await createArtifactAsk(
    document.artifact.id,
    { question: "Document ask" },
    session
  );

  return {
    handed,
    rows: [urgentAsk, steadyFirst, steadySecond, handed, documentAsk],
    unbanded: documentAsk,
  };
}

/** Every band heading the list is drawing, top to bottom, with its count. */
async function bandHeadings(page: Page): Promise<string[]> {
  return inboxList(page)
    .locator("h2 button")
    .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim()));
}

/** The ids the list is drawing, top to bottom, bands and all. */
async function inboxRows(page: Page): Promise<string[]> {
  return page
    .locator("[data-inbox-row]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-inbox-row") ?? ""));
}

test("the list bands by priority and by whose turn it is, and its rows stay one flat keyboard list", async ({
  browser,
}) => {
  const seed = await seededBands();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    // The cleared filter, so the document's ask - which nobody holds - is listed beside the
    // issues' and every band is read over every row the server returned.
    await page.goto("/?assignee=everyone");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(seed.rows.length);

    // The default banding is the priority one: one band per priority the rows hold, highest
    // first, with the rows that have none last under their own heading.
    expect(await bandHeadings(page)).toEqual(["P0 (1)", "P2 (3)", "No priority (1)"]);

    // The turn bands are a choice of their own now, not the default, and they band the same
    // rows: the one handed back to its session stands apart from the four waiting on a person.
    await page.goto("/?assignee=everyone&group=waiting-on");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(seed.rows.length);
    expect(await bandHeadings(page)).toEqual(["Waiting on a person (4)", "Waiting on agents (1)"]);
    await expect(page.locator(`[data-inbox-row="${seed.handed.id}"]`)).toHaveAttribute(
      "data-inbox-section",
      "agent"
    );
    await expect(page.locator(`[data-inbox-row="${seed.unbanded.id}"]`)).toHaveAttribute(
      "data-inbox-section",
      "human"
    );

    // Whatever the banding, the rows are one list: the Inbox selects its first row, `j` walks
    // down it and `k` walks back up, across every band heading on the way.
    const forward = await inboxRows(page);
    await page.locator("body").focus();
    for (const [index, id] of forward.entries()) {
      if (index > 0) await page.keyboard.press("j");
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", id);
    }
    for (const [index, id] of [...forward].reverse().entries()) {
      if (index > 0) await page.keyboard.press("k");
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", id);
    }
  } finally {
    await alice.close();
  }
});
