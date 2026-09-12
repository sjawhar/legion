import { expect, test } from "@playwright/test";

import {
  createIssue,
  createNamedVersion,
  createProject,
  editArtifact,
  getArtifact,
  getIssueEvents,
  requestApproval,
} from "./api";
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
    await expect(card).toContainText("Approval requested");
    await expect(card).toContainText("Approve spec.md (version 1)?");
    await expect(card.getByRole("radio", { name: "Other" })).toHaveCount(0);
    await card.getByRole("radio", { name: /^Approve Approve/ }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(card).toHaveCount(0);

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
