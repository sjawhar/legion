import { expect, test } from "@playwright/test";
import type { Artifact } from "../web/src/api/types";

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
  const requested = await requestApproval(artifactID, {}, session);
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

test("an approval request stays in one Inbox card while its document version moves", async ({
  browser,
}) => {
  await createProject({ key: "FOLLOW", name: "Approval follow" });
  const issue = await createIssue({
    project: "FOLLOW",
    spec: "The plan.",
    title: "Follow approval",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = await requestApproval(
    artifactID,
    { summary: "Caps retries at three attempts." },
    session
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await expect(
      page.locator('[data-inbox-section="human"]').getByTestId(`ask-${requested.ask.id}`)
    ).toBeVisible();

    await editArtifact(
      artifactID,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    let movedArtifact: Artifact | undefined;
    await expect
      .poll(async () => {
        movedArtifact = await getArtifact(artifactID, { login: "alice" });
        return movedArtifact.versions.length;
      })
      .toBeGreaterThan(1);
    if (movedArtifact === undefined) throw new Error("the moved artifact was not read");
    const movedVersion = Math.max(...movedArtifact.versions.map((version) => version.number));

    await page.reload();
    await expect(
      page.locator('[data-inbox-section="agent"]').getByTestId(`ask-${requested.ask.id}`)
    ).toBeVisible();
    await expect(card).toContainText(
      `Approve spec.md (version ${movedVersion})? Caps retries at three attempts.`
    );
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on agents (1)");

    const handedBack = await requestApproval(
      artifactID,
      { summary: "Adds the rollback budget." },
      session
    );
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await page.goto("/");
    await expect(
      page.locator('[data-inbox-section="human"]').getByTestId(`ask-${requested.ask.id}`)
    ).toBeVisible();
    await expect(card).toContainText(
      `Approve spec.md (version ${movedVersion})? Adds the rollback budget.`
    );
    await card.getByRole("radio", { name: /^Approve/ }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: movedVersion, ask_id: requested.ask.id });
  } finally {
    await alice.close();
  }
});

test("a human comment, revision, and hand-back return an approval card to Waiting on you", async ({
  browser,
}) => {
  await createProject({ key: "TURN", name: "Approval turn" });
  const issue = await createIssue({
    project: "TURN",
    spec: "The plan.",
    title: "Hand-back turn",
  });
  const requested = await requestApproval(
    issue.primary_artifact_id,
    { summary: "Names the initial proposal." },
    session
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  );
  await requestApproval(issue.primary_artifact_id, { summary: "Clarifies the rollout." }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(
      page.locator('[data-inbox-section="human"]').getByTestId(`ask-${requested.ask.id}`)
    ).toBeVisible();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
  } finally {
    await alice.close();
  }
});

test("an unchanged hand-back after a human comment and a progress note returns the card to Waiting on you", async ({
  browser,
}) => {
  await createProject({ key: "SAME", name: "Unchanged hand-back" });
  const issue = await createIssue({
    project: "SAME",
    spec: "The plan.",
    title: "Unchanged hand-back",
  });
  const summary = "Names the existing proposal.";
  const requested = await requestApproval(issue.primary_artifact_id, { summary }, session);
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Checking the rollout.", turn: "agent" },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(
      page.locator('[data-inbox-section="agent"]').getByTestId(`ask-${requested.ask.id}`)
    ).toBeVisible();

    // The same version, question and summary: the hand-back alone changes whose turn it is, and the
    // open Inbox moves the card on the ask.handed_back event it records, which rewords nothing.
    const handedBack = await requestApproval(issue.primary_artifact_id, { summary }, session);
    expect(handedBack.ask.id).toBe(requested.ask.id);
    const card = page
      .locator('[data-inbox-section="human"]')
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(card).toBeVisible();
    await expect(card).toContainText(`Approve spec.md (version 1)? ${summary}`);
    await expect(card).not.toContainText("Edited");
    const askEvents = (await getIssueEvents(issue.key, { limit: 200 }, { login: "alice" }))
      .filter((event) => event.type.startsWith("ask."))
      .map((event) => event.type);
    expect(askEvents).toEqual(["ask.opened", "ask.handed_back"]);
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
  } finally {
    await alice.close();
  }
});

test("a hand-back is an activity line in the issue's Conversation, live and after a reload", async ({
  browser,
}) => {
  await createProject({ key: "LINE", name: "Hand-back line" });
  const issue = await createIssue({ project: "LINE", spec: "The plan.", title: "Hand-back line" });
  const requested = await requestApproval(
    issue.primary_artifact_id,
    { summary: "Names the existing proposal." },
    session
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const askTurn = page.locator(`[data-turn="ask:${requested.ask.id}"]`);
    await expect(askTurn).toHaveCount(1);
    const handBackLine = page.locator('[data-kind="activity"]', {
      hasText: `handed “${requested.ask.question}” back for approval`,
    });
    await expect(handBackLine).toHaveCount(0);

    // The human's comment left the request waiting on its agent, so this call hands it back.
    const handedBack = await requestApproval(issue.primary_artifact_id, {}, session);
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await expect(handBackLine).toBeVisible();
    // The hand-back rewords nothing: no edit line, and the ask is still one card.
    await expect(
      page.locator('[data-kind="activity"]', { hasText: "edited the question" })
    ).toHaveCount(0);
    await expect(askTurn).toHaveCount(1);

    await page.reload();
    await expect(handBackLine).toBeVisible();
    await expect(askTurn).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("an answer started before a hand-back that rewords nothing is saved, and the card gains no edit history", async ({
  browser,
}) => {
  await createProject({ key: "KEEP", name: "Kept answer" });
  const issue = await createIssue({ project: "KEEP", spec: "The plan.", title: "Kept answer" });
  const summary = "Names the existing proposal.";
  const requested = await requestApproval(issue.primary_artifact_id, { summary }, session);
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    // No live stream on this page: the card stays as the human loaded it, mid-answer.
    const answering = await alice.newPage();
    await answering.route("**/api/v1/events**", (route) => route.abort());
    await answering.goto("/");
    const shown = answering
      .locator('[data-inbox-section="agent"]')
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(shown).toBeVisible();
    await shown.getByRole("radio", { name: /^Approve/ }).check();

    await requestApproval(issue.primary_artifact_id, { summary }, session);

    // Another page sees the hand-back: the card is the human's turn, and it was never reworded.
    const watching = await alice.newPage();
    await watching.goto("/");
    const handedBack = watching
      .locator('[data-inbox-section="human"]')
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(handedBack).toBeVisible();
    await expect(handedBack).not.toContainText("Edited");

    await shown.getByRole("button", { name: "Answer" }).click();
    await expect(
      answering.getByText("Your answer was not saved, because the question changed.")
    ).toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: 1, ask_id: requested.ask.id });
  } finally {
    await alice.close();
  }
});

test("an approval ask's Inbox card shows a question carrying a long summary whole", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  // Nearly as long as the ask cap leaves a summary after "Approve spec.md (version 1)? ": 771
  // units, cut at a word so the text still reads as a sentence.
  const opening =
    "Retries a failed push at most three times, one minute apart, and then stops and names the push that failed. ";
  const ending = "Nothing else in the retry path changes.";
  const filler = opening.repeat(8).slice(0, 771 - ending.length);
  const summary = filler.slice(0, filler.lastIndexOf(" ") + 1) + ending;
  expect(summary.length).toBeGreaterThan(750);
  expect(summary.length).toBeLessThanOrEqual(771);
  const requested = await requestApproval(issue.primary_artifact_id, { summary }, session);
  expect(requested.ask.question).toBe(`Approve spec.md (version 1)? ${summary}`);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${requested.ask.id}`);
    await expect(card).toBeVisible();
    const question = card.locator("p", { hasText: ending });
    await expect(question).toHaveText(requested.ask.question);
    // Whole means nothing between the question and its card clips it: no clamp, ellipsis or box
    // shorter or narrower than the text it holds.
    const clipped = await question.evaluate((element) => {
      const clips: string[] = [];
      for (let node: Element | null = element; node !== null; node = node.parentElement) {
        const style = getComputedStyle(node);
        if (style.webkitLineClamp !== "none" || style.textOverflow === "ellipsis") {
          clips.push(`${node.tagName}: line clamp ${style.webkitLineClamp}, ${style.textOverflow}`);
        }
        if (node.scrollHeight > node.clientHeight + 1 || node.scrollWidth > node.clientWidth + 1) {
          clips.push(
            `${node.tagName}: ${node.scrollWidth}x${node.scrollHeight} in ${node.clientWidth}x${node.clientHeight}`
          );
        }
        if (node.tagName === "ARTICLE") {
          break;
        }
      }
      return clips;
    });
    expect(clipped).toEqual([]);
    await card.screenshot({ path: testInfo.outputPath("approval-long-question.png") });
  } finally {
    await alice.close();
  }
});

test("reading an approved spec and moving the caret through its numbered list leaves its approval current", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  // A spec holding a numbered list, which the caret moves through below.
  const issue = await createIssue({
    project: "GATE",
    spec: "## Acceptance\n\n1. The migration ships.\n2. The error rate stays flat.\n",
    title: "Design gate",
  });
  const artifactID = issue.primary_artifact_id;
  const requested = await requestApproval(artifactID, {}, session);
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
  const requested = await requestApproval(artifactID, {}, session);
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
  const requested = await requestApproval(artifactID, {}, session);
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
  const nonPrimary = await requestApproval(supporting.artifact.id, {}, session);
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
