import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  listIssueAsks,
  requestApproval,
  reviewArtifact,
  setGatedSteps,
} from "./api";
import { resetDatabase } from "./seed";
import { assertPageFits, assertWhole } from "./unclipped";
import { asUser } from "./users";

/**
 * The design gate in a root issue's header: one approval line per step the project's deployment
 * publishes (`PUT /api/v1/projects/{key}/gated-steps`), each naming the document that step
 * reviews and where that document stands, and the one chip the header has always drawn
 * everywhere else - a project that publishes no list, and every issue that is not its tree's
 * root.
 *
 * Each row publishes its own project, so what one row gates says nothing about the next, and
 * reads the page by the names it draws (`Approvals`, the chip and action labels) and the
 * `data-approval-step` attributes, never by a class.
 */

const session = {
  actor: { kind: "session" as const, id: "e2e-session", origin: { session_title: "architect" } },
  as: "agent" as const,
};

/** The header's step list, by the name it carries for a reader. */
function approvalSteps(page: Page): Locator {
  return page.getByRole("list", { name: "Approvals" });
}

/** One step's line in that list. */
function stepEntry(page: Page, step: string): Locator {
  return approvalSteps(page).locator(`[data-approval-step="${step}"]`);
}

/** The header row the single chip and the step list both sit in. */
function stateActions(page: Page): Locator {
  return page.getByTestId("issue-state-actions");
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a root issue of a project gating three steps reads each step's own document: approved, approved and changed since, and awaiting", async ({
  browser,
}) => {
  await createProject({ key: "GATETHREE", name: "Three steps" });
  expect(await setGatedSteps("GATETHREE", ["decisions", "criteria", "spec"])).toEqual({
    project: "GATETHREE",
    steps: ["decisions", "criteria", "spec"],
  });
  const issue = await createIssue({
    project: "GATETHREE",
    spec: "The plan.",
    title: "Design gate",
  });

  // The decisions document is approved where it stands.
  const decisions = await createIssueArtifact(issue.key, {
    content: "# Decisions\n\nHTTP it is.\n",
    name: "decisions.md",
  });
  expect(
    (await reviewArtifact(decisions.artifact.id, { state: "approved" }, { login: "alice" })).version
  ).toBe(1);

  // The Verification document was approved and then written again, so its approval is stale.
  const verification = await createIssueArtifact(issue.key, {
    content: "# Verification\n\nOne check.\n",
    name: "verification.md",
  });
  expect(
    (await reviewArtifact(verification.artifact.id, { state: "approved" }, { login: "alice" }))
      .version
  ).toBe(1);
  await editArtifact(
    verification.artifact.id,
    { ops: [{ op: "replace", find: "One check.", with: "Two checks." }] },
    session
  );
  expect((await getArtifact(verification.artifact.id, { login: "alice" })).approval).toMatchObject({
    latest_version: 2,
    state: "stale",
    version: 1,
  });

  // The spec has an open request, so it is the step the gate waits on.
  expect((await requestApproval(issue.primary_artifact_id, {}, session)).version).toBe(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);

    // Three lines, in the order the deployment published them, and no second chip beside them.
    await expect(approvalSteps(page).locator("[data-approval-step]")).toHaveCount(3);
    expect(
      await approvalSteps(page)
        .locator("[data-approval-step]")
        .evaluateAll((entries) => entries.map((entry) => entry.getAttribute("data-approval-step")))
    ).toEqual(["decisions", "criteria", "spec"]);

    const decisionsEntry = stepEntry(page, "decisions");
    await expect(decisionsEntry).toContainText("Decisions");
    await expect(decisionsEntry.getByRole("button", { name: "Approved v1" })).toBeVisible();

    const criteriaEntry = stepEntry(page, "criteria");
    await expect(criteriaEntry).toContainText("Verification");
    await expect(
      criteriaEntry.getByRole("button", { name: "Approved v1 · changed since" })
    ).toBeVisible();

    const specEntry = stepEntry(page, "spec");
    await expect(specEntry).toContainText("Spec");
    await expect(specEntry.getByRole("button", { name: "Awaiting approval" })).toBeVisible();

    // The review actions ride the primary document's line alone: they are the header's one pair,
    // where they have always been, not one pair per step.
    await expect(specEntry.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(
      specEntry.getByRole("button", { exact: true, name: "Request changes" })
    ).toBeVisible();
    await expect(decisionsEntry.getByRole("button", { exact: true, name: "Approve" })).toHaveCount(
      0
    );
    await expect(
      decisionsEntry.getByRole("button", { exact: true, name: "Request changes" })
    ).toHaveCount(0);
    await expect(
      stateActions(page).getByRole("button", { exact: true, name: "Approve" })
    ).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a gated step whose document nobody has written reads Not requested rather than nothing", async ({
  browser,
}) => {
  await createProject({ key: "GATEGAP", name: "A missing document" });
  await setGatedSteps("GATEGAP", ["decisions", "criteria", "spec"]);
  const issue = await createIssue({
    project: "GATEGAP",
    spec: "The plan.",
    title: "No decisions document",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(approvalSteps(page).locator("[data-approval-step]")).toHaveCount(3);

    // The step is still a line of the gate: it names the document waiting to be written, and
    // says that nobody has asked for it to be reviewed.
    const decisionsEntry = stepEntry(page, "decisions");
    await expect(decisionsEntry).toContainText("Decisions");
    await expect(decisionsEntry).toContainText("Not requested");
    // Nothing to open and nothing to answer: the pill is no control.
    await expect(decisionsEntry.getByRole("button")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a project whose deployment publishes no gate keeps the header's one chip for the primary document", async ({
  browser,
}) => {
  await createProject({ key: "UNGATED", name: "Ungated" });
  const issue = await createIssue({ project: "UNGATED", spec: "The plan.", title: "No gate" });
  await requestApproval(issue.primary_artifact_id, {}, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const actions = stateActions(page);
    await expect(actions.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    await expect(actions.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(
      actions.getByRole("button", { exact: true, name: "Request changes" })
    ).toBeVisible();
    await expect(page.getByTestId("issue-approval-steps")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a child issue of a gated project keeps the header's one chip: only a root issue stands in the gate", async ({
  browser,
}) => {
  await createProject({ key: "GATECHILD", name: "A gated tree" });
  await setGatedSteps("GATECHILD", ["decisions", "criteria", "spec"]);
  const root = await createIssue({
    project: "GATECHILD",
    spec: "The root plan.",
    title: "The root",
  });
  const child = await createIssue({
    parent: root.key,
    project: "GATECHILD",
    spec: "The child plan.",
    title: "The child",
  });
  await requestApproval(child.primary_artifact_id, {}, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The root of the same tree reads step by step, so the child's one chip is the issue's own
    // place in the tree rather than a gate the project failed to publish.
    await page.goto(`/issues/${root.key}`);
    await expect(approvalSteps(page).locator("[data-approval-step]")).toHaveCount(3);

    await page.goto(`/issues/${child.key}`);
    const actions = stateActions(page);
    await expect(actions.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    await expect(actions.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(
      actions.getByRole("button", { exact: true, name: "Request changes" })
    ).toBeVisible();
    await expect(page.getByTestId("issue-approval-steps")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("one request over two documents reads the same on both steps, and one Request changes answers it once for both", async ({
  browser,
}) => {
  await createProject({ key: "GATEBOTH", name: "Two documents" });
  await setGatedSteps("GATEBOTH", ["criteria", "spec"]);
  const issue = await createIssue({
    project: "GATEBOTH",
    spec: "The plan.",
    title: "Both documents at once",
  });
  const verification = await createIssueArtifact(issue.key, {
    content: "# Verification\n\nOne check.\n",
    name: "verification.md",
  });
  const opened = await requestApproval(
    issue.primary_artifact_id,
    { include_verification: true },
    session
  );
  expect(opened.ask.question).toBe("Approve spec.md (version 1) and verification.md (version 1)?");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);

    // One request, so both lines say what is waiting: this document and the other, each at the
    // version the request names.
    const awaiting = "Awaiting approval of spec.md v1 and verification.md v1";
    await expect(stepEntry(page, "criteria").getByRole("button", { name: awaiting })).toBeVisible();
    await expect(stepEntry(page, "spec").getByRole("button", { name: awaiting })).toBeVisible();

    await stepEntry(page, "spec")
      .getByRole("button", { exact: true, name: "Request changes" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    await dialog.getByLabel("Reason").fill("Name the rollback path.");
    await dialog.getByRole("button", { exact: true, name: "Request changes" }).click();

    // The one answer is both documents' answer, on the page and on the server.
    await expect(
      stepEntry(page, "spec").getByRole("button", { name: "Changes requested" })
    ).toBeVisible();
    await expect(
      stepEntry(page, "criteria").getByRole("button", { name: "Changes requested" })
    ).toBeVisible();
    for (const id of [issue.primary_artifact_id, verification.artifact.id]) {
      await expect
        .poll(async () => (await getArtifact(id, { login: "alice" })).approval?.state)
        .toBe("changes_requested");
    }

    // And it is one ask, answered: the press answered the request rather than opening a second.
    const asks = await listIssueAsks(issue.key, { login: "alice" });
    expect(asks.map((ask) => [ask.id, ask.kind, ask.state])).toEqual([
      [opened.ask.id, "approval", "answered"],
    ]);
  } finally {
    await alice.close();
  }
});

// The phone reads the whole of a reviewer's reason: it is the one thing on this header with no
// bound on its length, and the iPhone 13's own 390 px layout is the narrowest width any project
// draws this header at before the widths below are set. @phone
test("a long changes-requested reason names its step and its reviewer whole, and fits every width @phone", async ({
  browser,
}) => {
  await createProject({ key: "GATEREASON", name: "A long reason" });
  await setGatedSteps("GATEREASON", ["decisions", "criteria", "spec"]);
  const issue = await createIssue({
    project: "GATEREASON",
    spec: "The plan.",
    title: "The reason the gate stays shut",
  });
  const reason =
    "Name the rollback path for every region before this gate opens: the spec says the live " +
    "sync replaces the nightly export, and it does not say what a region falls back to while " +
    "the sync is down, who decides to fall back, or how the export is started again once the " +
    "sync returns.";
  await reviewArtifact(
    issue.primary_artifact_id,
    { reason, state: "changes_requested" },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);

    // The chip says only that changes were requested; the line beneath the steps says which step,
    // who sent it back, at which version, and the whole of what the writer must act on.
    await expect(
      stepEntry(page, "spec").getByRole("button", { name: "Changes requested" })
    ).toBeVisible();
    const line = page.locator('[data-approval-step-reason="spec"]');
    await expect(line).toHaveText(`Spec: alice requested changes on v1 · ${reason}`);

    for (const width of [null, 320, 1440]) {
      if (width !== null) {
        await page.setViewportSize({ height: 844, width });
      }
      await assertWhole(line);
      await assertPageFits(page);
    }
  } finally {
    await alice.close();
  }
});
