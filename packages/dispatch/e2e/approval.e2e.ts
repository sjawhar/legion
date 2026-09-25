import { expect, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createIssueArtifact,
  createNamedVersion,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactText,
  getIssueEvents,
  rejectSuggestion,
  requestApproval,
} from "./api";
import { documentEditor, openSpecAndAwaitHeadingIds } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-session", origin: { session_title: "architect" } },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("a spec's approval is a human review pinned to its version: requested by the agent, answered in the Inbox, stale after an edit, changes requested from the header", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  const artifactID = issue.primary_artifact_id;

  // The agent asks for approval; the Inbox shows a fixed-option approval ask.
  const requested = await requestApproval(artifactID, session);
  expect(requested.version).toBe(1);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await expect(card).toBeVisible();
    await expect(card.getByRole("radio")).toHaveCount(2);
    const approve = card.getByRole("radio", { name: /^Approve/ });
    await expect(approve).toBeVisible();
    const requestChanges = card.getByRole("radio", { name: /^Request changes/ });
    await expect(requestChanges).toBeVisible();
    // Request changes needs a reason; the card says so instead of silently disabling Answer.
    await requestChanges.check();
    await expect(card.getByLabel("Reason (required)")).toBeFocused();
    await expect(card.getByRole("button", { name: "Answer" })).toBeDisabled();
    await expect(card.getByText("Add a reason to send Request changes")).toBeVisible();
    await approve.check();
    await expect(card.getByText("Add a reason to send Request changes")).toHaveCount(0);
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // The document header shows the approval pinned to version 1.
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);

    // The agent edits the spec: the approval goes stale.
    await editArtifact(
      artifactID,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).versions.length)
      .toBeGreaterThan(1);
    await createNamedVersion(artifactID, "revised", { login: "alice" });
    await page.reload();
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();

    // The human requests changes from the header; a reason is required.
    await page.getByRole("button", { name: "Request changes" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("button", { name: "Request changes" })).toBeDisabled();
    await dialog.getByLabel("Reason").fill("Name the rollback path.");
    await dialog.getByRole("button", { name: "Request changes" }).click();
    await expect(page.getByRole("button", { name: "Changes requested" })).toBeVisible();

    // The history lists both reviews with their versions.
    await page.getByRole("button", { name: "Changes requested" }).click();
    const reviews = page.getByRole("dialog", { name: "Reviews" });
    const afterReview = await getArtifact(artifactID, { login: "alice" });
    const pinned = afterReview.approval?.version;
    expect(afterReview.approval?.state).toBe("changes_requested");
    expect(pinned).toBeGreaterThan(1);
    await expect(reviews).toContainText(`requested changes on v${pinned}`);
    await expect(reviews).toContainText("Name the rollback path.");
    await expect(reviews).toContainText("approved v1");

    // The event contract the daemon consumes.
    const events = (await getIssueEvents(issue.key, {}, { login: "alice" })) as unknown as Array<{
      type: string;
      payload: Record<string, unknown>;
    }>;
    const approved = events.find((event) => event.type === "artifact.approved");
    const changes = events.find((event) => event.type === "artifact.changes_requested");
    expect(approved?.payload).toMatchObject({
      artifact_id: artifactID,
      version: 1,
      ask_id: requested.ask.id,
    });
    expect(changes?.payload).toMatchObject({
      artifact_id: artifactID,
      version: pinned,
      reason: "Name the rollback path.",
      ask_id: null,
    });
  } finally {
    await alice.close();
  }
});

test("reading an approved spec and moving the caret through its numbered list leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  // Numbered outcomes, as the default spec template asks for under Acceptance.
  const issue = await createIssue({
    project: "GATE",
    spec: "## Acceptance\n\n1. The migration ships.\n2. The error rate stays flat.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = await requestApproval(artifactID, session);
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // Bob reads the approved spec, and his editor gives its heading an id. Then he clicks into
    // the list and moves the caret, typing nothing: the first such move has his editor label
    // each numbered item, which changes the token again and none of the text.
    const reader = await bob.newPage();
    const opened = await openSpecAndAwaitHeadingIds(
      reader,
      issue.key,
      artifactID,
      "The error rate stays flat."
    );
    await documentEditor(reader).getByText("The migration ships.").click();
    for (const key of ["ArrowDown", "ArrowUp", "End", "Home"]) {
      await reader.keyboard.press(key);
    }
    await expect.poll(async () => (await getArtifactText(artifactID)).token).not.toBe(opened);
    // Settlement runs two seconds after the room's last update, and a version it wrote for
    // either update would leave the approval pinned to an older one.
    await reader.waitForTimeout(3000);
    const artifact = await getArtifact(artifactID, { login: "alice" });
    expect(artifact.versions).toHaveLength(1);
    expect(artifact.approval).toMatchObject({ latest_version: 1, state: "approved", version: 1 });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("an agent's comment on part of an identifier in an approved spec leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({
    project: "GATE",
    spec: "## Plan\n\nRename the user_id column.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = await requestApproval(artifactID, session);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");

    // The comment's mark starts inside `user_id`, splitting the word's text where the
    // underscore sits. That changes nothing a version stores.
    await createComment(
      issue.key,
      { anchor: { artifact: "spec", quote: "id column" }, body: "Is this indexed?" },
      session
    );
    // Settlement runs two seconds after the room's last update, and a version it wrote would
    // leave the approval pinned to an older one.
    await page.waitForTimeout(3000);
    const artifact = await getArtifact(artifactID, { login: "alice" });
    expect(artifact.versions).toHaveLength(1);
    expect(artifact.approval).toMatchObject({ latest_version: 1, state: "approved", version: 1 });
  } finally {
    await alice.close();
  }
});

test("rejecting a suggestion on part of an identifier in an approved spec leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({
    project: "GATE",
    spec: "## Plan\n\nMigrate snake_case_name first.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  // The suggestion's mark starts inside `snake_case_name`, splitting the word's text beside an
  // underscore; rejecting it removes the mark and joins the text again.
  const suggestion = await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "case_name" },
      body: "Name the column for what it holds.",
      suggestion: { replace_with: "case_label" },
    },
    session
  );
  const requested = await requestApproval(artifactID, session);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");
    const approved = (await getArtifact(artifactID, { login: "alice" })).approval;

    await rejectSuggestion(suggestion.id, { login: "bob" });
    // Settlement runs two seconds after the room's last update, and a version it wrote would
    // leave the approval pinned to an older one.
    await page.waitForTimeout(3000);
    expect((await getArtifact(artifactID, { login: "alice" })).approval).toMatchObject({
      latest_version: approved?.version,
      state: "approved",
      version: approved?.version,
    });
  } finally {
    await alice.close();
  }
});

test("an approval request for an unassigned issue's non-primary document remains actionable while the primary is draft", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue(
    { project: "GATE", spec: "Primary review target.", title: "Two approval targets" },
    session
  );
  const supporting = await createIssueArtifact(
    issue.key,
    { content: "Supporting review target.", name: "supporting-design.md" },
    session
  );
  const nonPrimary = await requestApproval(supporting.artifact.id, session);
  await expect
    .poll(
      async () => (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval?.state
    )
    .toBe("draft");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/?view=mine");

    // The shared-token issue has no assignee, so Mine retains the request in its Unassigned band
    // rather than making the human hunt in Everyone.
    const unassigned = page.locator('[data-inbox-section="unassigned"]');
    await expect(unassigned.getByTestId(`ask-${nonPrimary.ask.id}`)).toBeVisible();
    await expect(unassigned.getByTestId(`ask-${nonPrimary.ask.id}`)).toContainText(
      "Approval requested"
    );
    await expect(unassigned.getByTestId(`ask-${nonPrimary.ask.id}`).getByRole("radio")).toHaveCount(
      2
    );
    const inboxShot = testInfo.outputPath("approval-non-primary-inbox-1280.png");
    await page.screenshot({ path: inboxShot, fullPage: true });
    await testInfo.attach("non-primary approval in Inbox", {
      contentType: "image/png",
      path: inboxShot,
    });
    await page.getByRole("button", { name: "Everyone" }).click();
    await expect(page.getByTestId(`ask-${nonPrimary.ask.id}`)).toBeVisible();

    // Every open issue ask remains visible in the issue margin; the target being a secondary
    // document must not hide this decision while the primary approval stays draft.
    await page.goto(`/issues/${issue.key}/artifacts/${supporting.artifact.slug}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open review panel (1 open ask)" }).click();
    }
    const margin = page.getByRole("region", { name: "Needs you" });
    const nonPrimaryMarginCard = margin.getByTestId(`ask-${nonPrimary.ask.id}`);
    await expect(nonPrimaryMarginCard).toBeVisible();
    await expect(nonPrimaryMarginCard).toContainText("supporting-design.md");
    await nonPrimaryMarginCard
      .getByRole("radio", { name: "Approve Approve this version of the document." })
      .check();
    await expect(
      nonPrimaryMarginCard.getByRole("button", { exact: true, name: "Answer" })
    ).toBeVisible();
    const issueShot = testInfo.outputPath("approval-non-primary-issue-1280.png");
    await page.screenshot({ path: issueShot, fullPage: true });
    await testInfo.attach("non-primary approval in issue margin", {
      contentType: "image/png",
      path: issueShot,
    });
  } finally {
    await alice.close();
  }
});
