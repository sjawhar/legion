import { setTimeout as sleep } from "node:timers/promises";

import type { WriteAdvice } from "@legion/contracts";
import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, type Locator, type Page, test } from "@playwright/test";
import type { Artifact } from "../web/src/api/types";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createNamedVersion,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssueEvents,
  listIssueAsks,
  rejectSuggestion,
  requestApproval,
  reviewArtifact,
  withOpenedAsk,
} from "./api";
import { bodyOf } from "./bodies";
import { documentEditor, needsYouCards, openSpecAndAwaitHeadingIds } from "./editor";
import { askControls, hasReadingPane, inboxRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { assertWhole } from "./unclipped";
import { asUser } from "./users";

function drawer(page: Page) {
  return page.getByRole("dialog", { name: "Inbox" });
}

/**
 * An approval request's own card, as the Inbox draws one at this width.
 *
 * Below `READING_PANE_WIDTH` that is the card in the question's row on the Inbox page. From
 * there the page shows an approval as the document under review in its reading column
 * (INFRA-575), which carries no card, and the drawer is the Inbox that still keeps one
 * (`pane={false}`): it opens over whatever page is showing, so this navigates to one.
 */
async function approvalCard(page: Page, askId: string): Promise<Locator> {
  if (!hasReadingPane(page)) {
    await page.goto("/");
    return page.getByTestId(`ask-${askId}`);
  }
  await page.goto("/agents");
  await page.locator("body").focus();
  await page.keyboard.press("i");
  await expect(drawer(page)).toBeVisible();
  return drawer(page).getByTestId(`ask-${askId}`);
}

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

  // The agent asks for approval; the Inbox shows the request and the two reviews it offers.
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  expect(requested.version).toBe(1);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // From 1280 px an approval's own surface in the Inbox is the reading column (INFRA-575):
    // the document at the version under review, with Approve and Request changes in the
    // column's head. That head is where the reader answers it.
    const head = await askControls(page, requested.ask.id);
    const approve = head.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve).toBeVisible();
    const requestChanges = head.getByRole("button", { exact: true, name: "Request changes" });
    await expect(requestChanges).toBeVisible();
    // Request changes needs a reason, and says so by holding its own button rather than
    // refusing the review silently.
    await requestChanges.click();
    const reason = page.getByRole("dialog", { name: "Request changes" });
    await expect(reason.getByRole("button", { name: "Request changes" })).toBeDisabled();
    await reason.getByRole("button", { name: "Cancel" }).click();
    await expect(reason).toHaveCount(0);
    await approve.click();
    await expect(inboxRow(page, requested.ask.id)).toHaveCount(0);
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
      .poll(async () => ((await getArtifact(artifactID, { login: "alice" })).versions ?? []).length)
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

test("a reason longer than the server takes shows the server's own refusal in the Request changes dialog", async ({
  browser,
}) => {
  await createProject({ key: "LONGREASON", name: "Long reason" });
  const issue = await createIssue({
    project: "LONGREASON",
    spec: "The plan.",
    title: "Reason past the bound",
  });
  const artifactID = issue.primary_artifact_id;
  await requestApproval(artifactID, {}, session);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { exact: true, name: "Request changes" }).click();
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    const field = dialog.getByLabel("Reason");
    await expect(field).toBeVisible();

    // One UTF-16 unit past the bound the server holds a review's reason to. `fill` puts the
    // whole text in through the page's own editing pipeline, the path a paste takes, and the
    // last character is not whitespace, so the server's trim leaves the length as written.
    const reason = `${bodyOf(DISPATCH_EVENT_BODY_MAX)}x`;
    await field.fill(reason);
    await expect.poll(async () => (await field.inputValue()).length).toBe(reason.length);

    await dialog.getByRole("button", { exact: true, name: "Request changes" }).click();

    // What the reader is shown is the server's refusal, which names the field and the bound,
    // in place of the sentence the dialog says for a failure it cannot read.
    await expect(dialog.getByRole("alert")).toHaveText(
      `reason is 1 characters over the ${DISPATCH_EVENT_BODY_MAX}-character limit ` +
        `(${reason.length}/${DISPATCH_EVENT_BODY_MAX})`
    );
    await expect(dialog.getByText("Could not request changes. Try again.")).toHaveCount(0);

    // The draft stays in the field, and no review was written.
    await expect(field).toHaveValue(reason);
    expect((await getArtifact(artifactID, { login: "alice" })).approval?.state).toBe("awaiting");
  } finally {
    await alice.close();
  }
});

/** A document write's response as the restore tests read it: the advice beside its result. */
interface AdvisedWrite {
  readonly advice?: WriteAdvice;
}

/** The decision block the restore tests hold open in a spec. */
const waveAsk =
  ':::ask{#wave urgency="med" multiple="false"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n';

/**
 * Creates a spec holding `spec`, waits for each of its decision blocks to be an ask, has alice
 * approve version 1 from its header, then has the agent change "The plan." by accident. The edit
 * stales the approval and its advice says so. Returns the spec's issue.
 */
async function approveThenChange(
  spec: string,
  blocks: number
): Promise<{ key: string; id: string }> {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec, title: "Design gate" });
  const id = issue.primary_artifact_id;
  await expect
    .poll(async () => (await listIssueAsks(issue.key)).filter((ask) => ask.block_artifact).length)
    .toBe(blocks);
  expect((await reviewArtifact(id, { state: "approved" }, { login: "alice" })).version).toBe(1);
  const edited = (await editArtifact(
    id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  )) as AdvisedWrite;
  expect(edited.advice?.superseded_approval).toEqual({
    approved_by: "alice",
    approved_version: 1,
    version: 2,
  });
  expect((await getArtifact(id, { login: "alice" })).approval).toMatchObject({
    latest_version: 2,
    state: "stale",
    version: 1,
  });
  return { id, key: issue.key };
}

test("the agent's undo of an accidental edit to an approved spec makes that approval current at the undo's version, so no approval request is needed", async ({
  browser,
}) => {
  const issue = await approveThenChange("The plan.", 0);

  // The undo's text is version 1's again: version 3 carries alice's approval of version 1, and
  // the undo superseded no approval.
  const undone = (await editArtifact(
    issue.id,
    { ops: [{ op: "replace", find: "The revised plan.", with: "The plan." }] },
    session
  )) as AdvisedWrite;
  expect(undone.advice?.superseded_approval).toBeUndefined();
  expect((await getArtifact(issue.id, { login: "alice" })).approval).toMatchObject({
    by: { id: "alice", kind: "user" },
    latest_version: 3,
    state: "approved",
    version: 3,
  });
  expect((await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown
  );
  const events = (await getIssueEvents(issue.key, {}, { login: "alice" })) as unknown as Array<{
    type: string;
    payload: Record<string, unknown>;
  }>;
  expect(
    events.find((event) => event.type === "artifact.approved" && event.payload.version === 3)
      ?.payload
  ).toMatchObject({
    artifact_id: issue.id,
    reason:
      "Carried forward from the approval of version 1: this version's text is that version's, unchanged.",
  });

  // A request for approval finds the approval at the latest version and opens nothing.
  expect((await requestApproval(issue.id, {}, session)).ask).toBeNull();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v3" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an undo one character away from the approved spec leaves its approval stale", async ({
  browser,
}) => {
  const issue = await approveThenChange("The plan.", 0);

  await editArtifact(
    issue.id,
    { ops: [{ op: "replace", find: "The revised plan.", with: "The plan!" }] },
    session
  );
  expect((await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown.replace("plan.", "plan!")
  );
  expect((await getArtifact(issue.id, { login: "alice" })).approval).toMatchObject({
    latest_version: 3,
    state: "stale",
    version: 1,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve v3" })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// Today's rule for a version whose decision blocks changed is the answer carry: it carries an
// approval only when every block it changed is the approver's own answer, given since. A version
// holding the approved text around a block the agent reworded holds no answer of hers, so its
// approval stays stale, as it did before restored text carried an approval.
test("an undo that restores the approved text around a decision block the agent reworded leaves the approval stale", async ({
  browser,
}) => {
  const issue = await approveThenChange(`The plan.\n\n${waveAsk}`, 1);

  await editArtifact(
    issue.id,
    {
      ops: [
        { op: "replace", find: "The revised plan.", with: "The plan." },
        {
          op: "replace",
          find: "How many regions take the first wave?",
          with: "How many regions go in the first wave?",
        },
      ],
    },
    session
  );
  const restored = (await getArtifactVersion(issue.id, 3, { login: "alice" })).markdown;
  expect(restored).toBe(
    (await getArtifactVersion(issue.id, 1, { login: "alice" })).markdown.replace(
      "take the first wave?",
      "go in the first wave?"
    )
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Settlement runs two seconds after the room's last update; a version it wrote would be
    // judged by the same rule, so the approval is read once it has had the chance.
    await page.waitForTimeout(3000);
    expect((await getArtifact(issue.id, { login: "alice" })).approval).toMatchObject({
      latest_version: 3,
      state: "stale",
      version: 1,
    });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Approved v1 · changed since" })).toBeVisible();
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
  const requested = withOpenedAsk(
    await requestApproval(artifactID, { summary: "Caps retries at three attempts." }, session)
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // One request, one item: the row it is, in the band whose turn it is. From 1280 px the
    // card the Inbox drew for it is the reading column's document (INFRA-575), so the row
    // carries the question's own words and the column's head carries Approve.
    const row = inboxRow(page, requested.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");

    await editArtifact(
      artifactID,
      { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
      session
    );
    let movedArtifact: Artifact | undefined;
    await expect
      .poll(async () => {
        movedArtifact = await getArtifact(artifactID, { login: "alice" });
        return (movedArtifact.versions ?? []).length;
      })
      .toBeGreaterThan(1);
    if (movedArtifact === undefined) throw new Error("the moved artifact was not read");
    const movedVersion = Math.max(
      ...(movedArtifact.versions ?? []).map((version) => version.number)
    );

    await page.reload();
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    await expect(row).toContainText(
      `Approve spec.md (version ${movedVersion})? Caps retries at three attempts.`
    );
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on agents (1)");

    const handedBack = withOpenedAsk(
      await requestApproval(artifactID, { summary: "Adds the rollback budget." }, session)
    );
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await page.goto("/");
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(row).toContainText(
      `Approve spec.md (version ${movedVersion})? Adds the rollback budget.`
    );
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect(row).toHaveCount(0);
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
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: "Names the initial proposal." },
      session
    )
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
    // The band the question sits in is the row's, at every width: from 1280 px an approval's
    // card in the Inbox is the reading column's document (INFRA-575), not a card in the list.
    await expect(inboxRow(page, requested.ask.id)).toHaveAttribute("data-inbox-section", "human");
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
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
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
    const row = inboxRow(page, requested.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");

    // The same version, question and summary: the hand-back alone changes whose turn it is,
    // and the open Inbox moves on the ask.handed_back event it records, which rewords nothing
    // - the question waits on the reader again, live. The row the reading column is showing
    // is held in the band they saw it in until they look away (INFRA-575, `heldRow`), so its
    // band is read on their next look at the list.
    const handedBack = withOpenedAsk(
      await requestApproval(issue.primary_artifact_id, { summary }, session)
    );
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await expect(page.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await page.reload();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(row).toContainText(`Approve spec.md (version 1)? ${summary}`);
    // Nothing the Inbox draws of it - the row's own words, and the column beside them - says
    // the question was reworded.
    await expect(page.locator("main")).not.toContainText("Edited");
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

test("a hand-back is an Activity line, live and after a reload, and leaves one ask card", async ({
  browser,
}) => {
  await createProject({ key: "LINE", name: "Hand-back line" });
  const issue = await createIssue({ project: "LINE", spec: "The plan.", title: "Hand-back line" });
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: "Names the existing proposal." },
      session
    )
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The first events subscription resumes from the sign-in check's head. Once its socket opens,
    // the hand-back below reaches this tab.
    const streaming = page.waitForEvent(
      "websocket",
      (socket) => new URL(socket.url()).pathname === "/ws/live"
    );
    await page.goto(`/issues/${issue.key}/activity`);
    await streaming;
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    const handBackLine = rows.filter({
      hasText: `handed “${requested.ask.question}” back for approval`,
    });
    await expect(handBackLine).toHaveCount(0);

    // The human's comment left the request waiting on its agent, so this call hands it back.
    const handedBack = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
    expect(handedBack.ask.id).toBe(requested.ask.id);
    await expect(handBackLine).toHaveCount(1);
    // The hand-back rewords nothing: no edit line anywhere.
    await expect(rows.filter({ hasText: "edited the question" })).toHaveCount(0);

    await page.reload();
    await expect(handBackLine).toHaveCount(1);

    // The Conversation draws no line for any of it: the ask is still exactly one card.
    await page.getByRole("tab", { name: "Conversation" }).click();
    const askTurn = page.locator(`[data-turn="ask:${requested.ask.id}"]`);
    await expect(askTurn).toHaveCount(1);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .getByText("back for approval", { exact: false })
    ).toHaveCount(0);
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
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
  await createComment(
    issue.key,
    { ask_id: requested.ask.id, body: "Please clarify the rollout." },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    // No live socket on this page: the card stays as the human loaded it, mid-answer. An
    // answer can be half-made only where the Inbox draws the question's own card, which for an
    // approval from 1280 px is the drawer rather than the page's reading column (INFRA-575):
    // the column shows the document under review, whose Approve is one press with no draft to
    // hold. The drawer keeps today's card at every width.
    const answering = await alice.newPage();
    await answering.routeWebSocket(/\/ws\/live$/u, (route) =>
      route.close({ code: 1011, reason: "test network drop" })
    );
    await answering.goto("/agents");
    await answering.locator("body").focus();
    await answering.keyboard.press("i");
    const shown = drawer(answering)
      .locator('[data-inbox-section="agent"]')
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(shown).toBeVisible();
    await shown.getByRole("radio", { name: /^Approve/ }).check();

    await requestApproval(issue.primary_artifact_id, { summary }, session);

    // Another page sees the hand-back: the question is the human's turn again, and the card the
    // drawer draws for it was never reworded.
    const watching = await alice.newPage();
    await watching.goto("/");
    await expect(inboxRow(watching, requested.ask.id)).toHaveAttribute(
      "data-inbox-section",
      "human"
    );
    await watching.goto("/agents");
    await watching.locator("body").focus();
    await watching.keyboard.press("i");
    const handedBack = drawer(watching).getByTestId(`ask-${requested.ask.id}`);
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

test("an approval ask's Inbox card shows a question carrying a long summary whole @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  // Nearly as long as the ask bound leaves a summary, budgeted as the server budgets it
  // (`approvalQuestion` in `internal/dispatch/api/reviews.go`): the bound on an ask's question
  // and options together, less the question at the longest version a request can reach, less
  // the two fixed options it carries, less the space before the summary. Cut at a word, so the
  // text still reads as a sentence.
  const options = [
    "Approve",
    "Approve this version of the document.",
    "Request changes",
    "Say what must change before it can be approved.",
  ].reduce((units, text) => units + text.length, 0);
  const room =
    DISPATCH_EVENT_BODY_MAX - "Approve spec.md (version 9999999999)?".length - options - 1;
  const opening =
    "Retries a failed push at most three times, one minute apart, and then stops and names the push that failed. ";
  const ending = "Nothing else in the retry path changes.";
  const filler = opening.repeat(Math.ceil(room / opening.length)).slice(0, room - ending.length);
  const summary = filler.slice(0, filler.lastIndexOf(" ") + 1) + ending;
  expect(summary.length).toBeGreaterThan(room - 15);
  expect(summary.length).toBeLessThanOrEqual(room);
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary }, session)
  );
  expect(requested.ask.question).toBe(`Approve spec.md (version 1)? ${summary}`);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The question's own card is where the Inbox shows the question itself: the row's card
    // below 1280 px, and the drawer's from there up, since the Inbox page's reading column
    // shows an approval as the document under review rather than a card (INFRA-575) and its
    // short row gives the question one line on purpose.
    const card = await approvalCard(page, requested.ask.id);
    await expect(card).toBeVisible();
    const question = card.locator("p", { hasText: ending });
    await expect(question).toHaveText(requested.ask.question);
    // Whole is the card's promise, so the window is made tall enough to hold the whole card:
    // what is left to flag is a clip, not a surface scrolling as it is meant to. A summary at
    // this bound is thousands of lines, past any height a person's window has, so the height
    // is the one the card itself reports rather than a number written here.
    const viewport = page.viewportSize();
    if (viewport !== null && hasReadingPane(page)) {
      const needed = await question.evaluate((element) => {
        let height = element.scrollHeight;
        for (let node: Element | null = element; node !== null; node = node.parentElement) {
          height = Math.max(height, node.scrollHeight);
        }
        return height;
      });
      await page.setViewportSize({ height: needed + 100, width: viewport.width });
      await expect(question).toHaveText(requested.ask.question);
    }
    // Whole means nothing between the question and its card clips it: no clamp, ellipsis or box
    // shorter or narrower than the text it holds.
    await assertWhole(question);
    await card.screenshot({ path: testInfo.outputPath("approval-long-question.png") });
  } finally {
    await alice.close();
  }
});

// A settlement that writes no version is proved by outlasting the window it would run in: the
// harness holds it to 250 ms (`DISPATCH_TEST_SETTLE_DELAY`, `e2e/run-server.sh`), so a second of
// real time covers four of them. It is the server's own timer, which no page clock reaches.
const SETTLED_MS = 1_000;

// SLOW(>=2s): Two users, two pages, and a settle window that must be outlasted to prove no
// version was written.
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
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // The reading column's head is where the Inbox offers the review from 1280 px (INFRA-575).
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
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
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote for either update would leave the approval
    // pinned to an older one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
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
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
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
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote would leave the approval pinned to an older
    // one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
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
  const requested = withOpenedAsk(await requestApproval(artifactID, {}, session));
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await (await askControls(page, requested.ask.id))
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect
      .poll(async () => (await getArtifact(artifactID, { login: "alice" })).approval?.state)
      .toBe("approved");
    const approved = (await getArtifact(artifactID, { login: "alice" })).approval;

    await rejectSuggestion(suggestion.id, { login: "bob" });
    // Settlement runs 250 ms after the room's last update (`DISPATCH_TEST_SETTLE_DELAY`,
    // `e2e/run-server.sh`), and a version it wrote would leave the approval pinned to an older
    // one. A second of real time is four of those windows.
    await sleep(SETTLED_MS);
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
  const nonPrimary = withOpenedAsk(await requestApproval(supporting.artifact.id, {}, session));
  await expect
    .poll(
      async () => (await getArtifact(issue.primary_artifact_id, { login: "alice" })).approval?.state
    )
    .toBe("draft");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Nobody holds the shared-token issue, so its request is nobody's own work: the reader's
    // default list does not hold it. An unassigned issue's request reaches a person through the
    // Unassigned filter, where it is as answerable as anywhere, or through the issue itself
    // (the margin below).
    await page.goto("/");
    await expect(page.getByTestId(`ask-${nonPrimary.ask.id}`)).toHaveCount(0);
    await expect(page.getByText("Nothing needs you")).toBeVisible();

    await page.goto("/?assignee=unassigned");
    await expect(inboxRow(page, nonPrimary.ask.id)).toBeVisible();
    // Actionable means the Inbox offers the review where this width draws it: the reading
    // column's head over the document under review from 1280 px (INFRA-575), and the
    // question's own card, which names itself an approval, below that.
    const controls = await askControls(page, nonPrimary.ask.id);
    if (hasReadingPane(page)) {
      await expect(controls.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
      await expect(
        controls.getByRole("button", { exact: true, name: "Request changes" })
      ).toBeVisible();
    } else {
      await expect(controls).toContainText("Approval requested");
      await expect(controls.getByRole("radio")).toHaveCount(2);
    }
    const inboxShot = testInfo.outputPath("approval-non-primary-inbox-1280.png");
    await page.screenshot({ path: inboxShot, fullPage: true });
    await testInfo.attach("non-primary approval under the Unassigned filter", {
      contentType: "image/png",
      path: inboxShot,
    });

    // Every open issue ask remains visible in the issue margin; the target being a secondary
    // document must not hide this decision while the primary approval stays draft.
    await page.goto(`/issues/${issue.key}/artifacts/${supporting.artifact.slug}`);
    const margin = needsYouCards(page);
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

test("an approval ask's Inbox row, drawer link and bare ask route land on the spec with the approve control in view, while an anchored ask and an unanchored question still land where they do today", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  // Alice's own issue, so the request is in her Inbox and her drawer, which both open on the
  // reader's own work; the agent still requests the approval.
  const issue = await createIssue({ project: "GATE", spec: "The plan.", title: "Design gate" });
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
  const specPath = `/issues/${issue.key}/spec?ask=${requested.ask.id}`;

  const anchoredAsk = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "The plan" }, question: "Which plan?" },
    session
  );
  const unanchoredAsk = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it?" },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The Inbox row's own link for the request lands on the spec, approve control in view — not
    // the Conversation turn `useItemLanding` fell through to before this fix. From 1280 px the
    // reading column's head carries the selected question's owner link too (INFRA-575), and it
    // names the same address; following the row's link is the row's click, not the list's
    // selection, so the issue it opens stays open.
    await page.goto("/");
    const head = (await askControls(page, requested.ask.id)).locator("[data-inbox-owner]");
    const owner = inboxRow(page, requested.ask.id).locator("[data-inbox-owner]");
    await expect(owner).toHaveAttribute("href", (await head.getAttribute("href")) ?? "");
    await owner.click();
    await expect(page).toHaveURL(specPath);
    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();

    // The drawer's card names the document and version it asks about, as a link; following it
    // opens the spec and closes the drawer. The drawer rides the shell the reader is already in,
    // so this leg needs no cold route of its own.
    await navigateInApp(page, "/agents");
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    const drawerCard = drawer(page).getByTestId(`ask-${requested.ask.id}`);
    await expect(drawerCard).toBeVisible();
    const documentLink = drawerCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(documentLink).toBeVisible();
    await documentLink.click();
    await expect(page).toHaveURL(specPath);
    await expect(drawer(page)).toHaveCount(0);

    // An anchored ask still lands beside its document, and an unanchored question still lands
    // on its Conversation turn: the approval fallback only fires when neither applies.
    await page.goto(`/issues/${issue.key}/asks/${anchoredAsk.id}`);
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${anchoredAsk.id}`);

    await page.goto(`/issues/${issue.key}/asks/${unanchoredAsk.id}`);
    await expect(page).toHaveURL(`/issues/${issue.key}/asks/${unanchoredAsk.id}`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(
      page.locator("#issue-conversation-panel").getByTestId(`ask-${unanchoredAsk.id}`)
    ).toContainText("Ship it?");
  } finally {
    await alice.close();
  }
});

test("an open approval ask's document link reaches the issue's Conversation, and a project document's own approval link reaches its document, while the margin omits the link on both", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue(
    { project: "GATE", spec: "The plan.", title: "Design gate" },
    session
  );
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));

  const doc = await createProjectDocument(
    "GATE",
    { content: "# Runbook\n", name: "runbook.md" },
    session
  );
  const docApproval = withOpenedAsk(await requestApproval(doc.artifact.id, {}, session));
  const docPath = `/projects/GATE/documents/${doc.artifact.slug}?ask=${docApproval.ask.id}`;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The issue's Conversation names the document the open request is about.
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page
      .locator("#issue-conversation-panel")
      .getByTestId(`ask-${requested.ask.id}`);
    const conversationLink = conversationCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(conversationLink).toBeVisible();
    await conversationLink.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${requested.ask.id}`);

    // The issue margin lists the same ask (it is already on that document), without the link.
    const marginCard = needsYouCards(page).getByTestId(`ask-${requested.ask.id}`);
    await expect(marginCard).toBeVisible();
    await expect(marginCard.getByRole("link", { name: /spec\.md, version/ })).toHaveCount(0);

    // A project-level document's approval request links its own document the same way. A
    // document has no assignee, so the Unassigned filter is where the Inbox lists it. The link
    // is the card's named version below 1280 px, and from there the short row's own owner
    // link, the reading column having drawn the document itself in place of a card
    // (INFRA-575); both carry the same address.
    await page.goto("/?assignee=unassigned");
    const inboxLink = hasReadingPane(page)
      ? inboxRow(page, docApproval.ask.id).locator("[data-inbox-owner]")
      : page
          .getByTestId(`ask-${docApproval.ask.id}`)
          .getByRole("link", { name: "runbook.md, version 1" });
    await expect(inboxLink).toHaveAttribute("href", docPath);

    await page.goto(`/projects/GATE/documents/${doc.artifact.slug}`);
    const docMarginCard = needsYouCards(page).getByTestId(`ask-${docApproval.ask.id}`);
    await expect(docMarginCard).toBeVisible();
    await expect(docMarginCard.getByRole("link", { name: /runbook\.md, version/ })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an approval ask keeps its document link and the version it asked about once it is answered, even after the document moves further", async ({
  browser,
}) => {
  await createProject({ key: "GATE", name: "Gate" });
  const issue = await createIssue(
    { project: "GATE", spec: "The plan.", title: "Design gate" },
    session
  );
  const requested = withOpenedAsk(await requestApproval(issue.primary_artifact_id, {}, session));
  await answerAsk(
    requested.ask.id,
    { expected_edited_at: null, selected: ["Approve"] },
    { login: "alice" }
  );

  // The document moves to a later version after the approval; the completed card still names
  // the version this request approved, and its link still opens the current document.
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ op: "replace", find: "The plan.", with: "The revised plan." }] },
    session
  );
  await expect
    .poll(
      async () =>
        ((await getArtifact(issue.primary_artifact_id, { login: "alice" })).versions ?? []).length
    )
    .toBeGreaterThan(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page
      .locator("#issue-conversation-panel")
      .getByTestId(`ask-${requested.ask.id}`);
    await expect(conversationCard).toContainText("Answered by");
    const link = conversationCard.getByRole("link", { name: "spec.md, version 1" });
    await expect(link).toBeVisible();
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?ask=${requested.ask.id}`);
  } finally {
    await alice.close();
  }
});
