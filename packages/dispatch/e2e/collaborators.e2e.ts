import { type LegionRole, roleToken } from "@legion/contracts";
import { expect, type Locator, test } from "@playwright/test";

import { type FakeLegionAgent, setLegionAgents, setLiveSessions, setSessionRecord } from "./agents";
import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  listComments,
  patchIssue,
} from "./api";
import { resetDatabase } from "./seed";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

// An issue's agents stay reachable from its page once they stop (dispatch://INFRA-470): the
// Collaborators tab lists everyone who worked the issue, running or not, each agent one click from
// its live conversation, and the mention picker offers the issue's Legion roles nobody live holds
// under "Not running", addressed to the role rather than to a session that has ended.
//
// Fixture: a closed issue assigned to alice, which bob commented on and messaged before the close;
// one live Legion tester, two reviewer sessions of one role token (the old one started first) and a
// stopped implementer. Every role token is built from a Legion project token, `e2elegion`, that is
// not the Dispatch project, so a token built from the wrong segment fails the picker's read-back.

const LEGION_PROJECT = "e2elegion";
const spec = "The quick brown fox jumps over the lazy dog.";

const liveTester = "e2e-live-tester";
const oldReviewer = "e2e-old-reviewer";
const newReviewer = "e2e-new-reviewer";
const oldImplementer = "e2e-old-implementer";

interface Fixture {
  readonly issueKey: string;
  readonly tokens: Readonly<Record<"implementer" | "reviewer" | "tester", string>>;
}

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<Fixture> {
  await createProject({ key: "E2E", name: "Collaborators" });
  const issue = await createIssue({ project: "E2E", spec, title: "Asked after it closed" });
  const issueKey = issue.key;
  await patchIssue(issueKey, { assignee: "alice" });
  await createComment(issueKey, { body: "Bob was here before the close" }, { login: "bob" });
  await createMessage(issueKey, { body: "Bob's question" }, { login: "bob" });
  await patchIssue(issueKey, { status: "done" }, { login: "bob" });

  const token = (role: LegionRole) => roleToken(LEGION_PROJECT, issueKey, role);
  const entry = (sessionId: string, role: LegionRole, startedAt: string): FakeLegionAgent => ({
    harness: "omp",
    issue: issueKey,
    legion_project: LEGION_PROJECT,
    role,
    role_token: token(role),
    session_id: sessionId,
    started_at: startedAt,
    title: `Legion ${role} · ${issueKey}`,
  });
  await setLiveSessions([
    {
      capabilities: ["aside", "btw", "steer"],
      roles: [token("tester")],
      session_id: liveTester,
      title: `Legion tester · ${issueKey}`,
    },
    { session_id: oldReviewer, title: `Legion reviewer · ${issueKey}` },
    { session_id: newReviewer, title: `Legion reviewer · ${issueKey}` },
    { session_id: oldImplementer, title: `Legion implementer · ${issueKey}` },
  ]);
  for (const stopped of [oldReviewer, newReviewer, oldImplementer]) {
    await setSessionRecord(stopped, "absent");
  }
  await setLegionAgents([
    entry(oldImplementer, "implementer", "2026-10-26T00:00:00Z"),
    entry(liveTester, "tester", "2026-10-26T01:00:00Z"),
    entry(oldReviewer, "reviewer", "2026-10-26T02:00:00Z"),
    entry(newReviewer, "reviewer", "2026-10-26T03:00:00Z"),
  ]);
  return {
    issueKey,
    tokens: {
      implementer: token("implementer"),
      reviewer: token("reviewer"),
      tester: token("tester"),
    },
  };
}

/** An agent's own row name in the Legion agents section: the `AgentName` of the row's first line
 *  (`Row` in `CollaboratorsTab.tsx`), not one that names it as another row's replacement. */
function agentRowName(legion: Locator, sessionId: string): Locator {
  return legion.locator(`li > div:first-child [data-agent-name="${sessionId}"]`);
}

// No `@phone` tag: this row starts at the project's own desktop width and narrows the window
// itself, so under `iphone` it would start from the wrong layout (packages/dispatch/AGENTS.md,
// "Phone acceptance").
test("the Collaborators tab lists an issue's people and its Legion agents, running or not", async ({
  browser,
}) => {
  const { issueKey } = await seed();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}/collaborators`);
    await expect(page.getByRole("tab", { name: "Collaborators" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    // People, with how each of them worked the issue.
    const people = page.getByRole("region", { name: "People" });
    await expect(people.getByRole("listitem").filter({ hasText: "alice" })).toContainText(
      "assignee"
    );
    await expect(people.getByRole("listitem").filter({ hasText: "bob" })).toContainText("posted");

    // The Legion agents, by role in workflow order (`LEGION_ROLES`), each role's newest first.
    const legion = page.getByRole("region", { name: "Legion agents" });
    await expect(legion.getByRole("heading", { level: 3 })).toHaveText([
      "implementer",
      "tester",
      "reviewer",
    ]);
    const reviewers = legion.getByRole("region", { name: "reviewer" });
    const reviewerNames = reviewers.locator("li > div:first-child [data-agent-name]");
    await expect(reviewerNames).toHaveCount(2);
    expect(
      await reviewerNames.evaluateAll((nodes) =>
        nodes.map((node) => node.getAttribute("data-agent-name"))
      )
    ).toEqual([newReviewer, oldReviewer]);
    // Only the old reviewer's own row names its replacement; the newest row names nobody else.
    const rowOf = (sessionId: string) =>
      reviewers
        .getByRole("listitem")
        .filter({ has: page.locator(`[data-agent-name="${sessionId}"]`) });
    await expect(rowOf(oldReviewer).locator(`[data-replaced-by="${newReviewer}"]`)).toContainText(
      `replaced by Legion reviewer · ${issueKey}`
    );
    await expect(rowOf(newReviewer).locator("[data-replaced-by]")).toHaveCount(0);

    // Each agent's running state, as the agents registry has it: the tester is live, and every
    // session the listener has lost reads as stopped.
    await expect(agentRowName(legion, liveTester)).toHaveAttribute("data-agent-state", "running");
    await expect(
      agentRowName(legion, liveTester).getByRole("img", { name: "Running" })
    ).toBeVisible();
    for (const stopped of [oldReviewer, newReviewer, oldImplementer]) {
      await expect(agentRowName(legion, stopped)).toHaveAttribute(
        "data-agent-state",
        "not-running"
      );
      await expect(
        agentRowName(legion, stopped).getByRole("img", { name: "Not running" })
      ).toBeVisible();
    }

    // A session title is a whole sentence of its own, and the tab holds four of them: the page
    // stays as wide as the window at a desktop width and at a phone's.
    await assertPageFits(page);
    await page.setViewportSize({ height: page.viewportSize()?.height ?? 800, width: 390 });
    await expect(agentRowName(legion, oldReviewer)).toBeVisible();
    await assertPageFits(page);

    // A stopped agent is one click from the conversation it had, which names it.
    await agentRowName(legion, oldReviewer).getByRole("link").click();
    await expect(page).toHaveURL(`/agents/${oldReviewer}/live`);
    await expect(
      page.getByRole("heading", { level: 1 }).locator(`[data-agent-name="${oldReviewer}"]`)
    ).toBeVisible();
    await page.goBack();
    await agentRowName(legion, oldImplementer).getByRole("link").click();
    await expect(page).toHaveURL(`/agents/${oldImplementer}/live`);
    await expect(
      page.getByRole("heading", { level: 1 }).locator(`[data-agent-name="${oldImplementer}"]`)
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("the mention picker offers the issue's stopped roles and sends one as a role mention", async ({
  browser,
}) => {
  const { issueKey, tokens } = await seed();
  // The issue is reopened for this row: a closed issue takes no send - its composer renders
  // nothing (`closed` on `MentionComposer`) and Dispatch refuses the write `409 ISSUE_CLOSED` -
  // while the picker's "Not running" options come from the collaborators read, which the close
  // does not touch.
  await patchIssue(issueKey, { status: "backlog" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    await field.fill("@");

    // The roles no live agent holds, in workflow order: the implementer, then the reviewer, each
    // drawn by the newest session of its token. The tester is live, so it is a live option alone.
    const listbox = page.getByRole("listbox", { name: "Mention suggestions" });
    const notRunning = listbox.getByRole("group", { name: "Not running" });
    await expect(notRunning.getByRole("option")).toHaveCount(2);
    await expect(notRunning.getByRole("option").nth(0)).toHaveAccessibleName(tokens.implementer);
    const reviewer = notRunning.getByRole("option").nth(1);
    await expect(reviewer).toHaveAccessibleName(tokens.reviewer);
    await expect(reviewer).toContainText("reviewer · not running");
    await expect(reviewer.locator(`[data-agent-name="${newReviewer}"]`)).toHaveCount(1);
    await expect(listbox.getByRole("option", { exact: true, name: tokens.tester })).toHaveCount(1);
    await expect(notRunning.getByRole("option", { name: tokens.tester })).toHaveCount(0);

    // Choosing the reviewer addresses the comment to its role, not to a session that has ended.
    await notRunning.getByRole("option", { exact: true, name: tokens.reviewer }).click();
    await expect(field).toHaveValue(`@${tokens.reviewer}`);
    await field.press("End");
    await field.pressSequentially(" please review the fix");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toHaveValue("");
    await expect
      .poll(
        async () =>
          (await listComments(issueKey)).find((comment) =>
            comment.body.endsWith("please review the fix")
          )?.mentions[0]?.target
      )
      .toBe(`role:${tokens.reviewer}`);
  } finally {
    await alice.close();
  }
});
