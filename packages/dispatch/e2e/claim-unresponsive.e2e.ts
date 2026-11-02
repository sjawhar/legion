import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { expect, test } from "@playwright/test";

import { agentRow, setLiveSessions, setSessionLastSeen, setSessionState } from "./agents";
import {
  claimIssue,
  createIssue,
  createProject,
  getIssue,
  mintAgentToken,
  patchIssue,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A session that has missed its heartbeats has not ended: it keeps the issue it is working, the
// Agents page still lists it, a route still reaches it, and the pages a person reads have to say
// so rather than call it gone. The dashboard and the server answer the same question here - the
// chip names the moment the claim comes free, and a second agent asking for it before that
// moment is refused and after it granted (dispatch://INFRA-392, R5 and A5).

test.beforeEach(async () => {
  await resetDatabase();
});

const ROLE = "claim-stalled-role";
const holder = {
  session_id: "claim-stalled-session",
  title: "Stalled holder",
  roles: [ROLE],
  machine_id: "box-1",
  dir: "/srv/stalled",
};
const taker = { session_id: "claim-taker-session", title: "Second agent" };

/** An issue claimed by `holder`, with both sessions live, and the agent token the takes below
 *  are made with. */
async function seedClaim(title: string): Promise<{ issueKey: string; token: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  const token = await mintAgentToken("claim-unresponsive", "alice");
  await setLiveSessions([holder, taker]);
  await claimIssue(issue.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  return { issueKey: issue.key, token };
}

/** Puts the holder past the listener's live window, inside its gone horizon: the listener still
 *  holds the record and says the session is unresponsive, with the heartbeat it was last seen
 *  at. Returns that heartbeat. */
async function stall(msAgo: number): Promise<number> {
  const lastSeen = Date.now() - msAgo;
  await setSessionLastSeen(holder.session_id, lastSeen);
  await setSessionState(holder.session_id, "unresponsive");
  return lastSeen;
}

test("an unresponsive holder keeps its claim: the issue page says unresponsive since its last heartbeat and names when it becomes takeable, and a second agent is refused", async ({
  browser,
}) => {
  const { issueKey, token } = await seedClaim("Worked by a stalled session");
  const lastSeen = await stall(6 * 60_000);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}`);
    const chip = page.getByTestId("issue-claim");
    const stalled = page.getByTestId("issue-claim-unresponsive");
    await expect(stalled).toBeVisible();
    await expect(page.getByTestId("issue-claim-lapsed")).toHaveCount(0);
    await expect(chip).toHaveAttribute("title", /missed its heartbeats without ending/);
    // R5's wording: the state, and the heartbeat the silence is dated from.
    await expect(stalled).toContainText("unresponsive since");
    await expect(
      stalled.getByTestId("issue-claim-unresponsive-since").locator("time")
    ).toHaveAttribute("datetime", new Date(lastSeen).toISOString());
    // The moment named is the holder's last heartbeat plus the shared grace, which is the sum
    // the server's own refusal below is written from.
    await expect(stalled.getByTestId("issue-claim-takeable").locator("time")).toHaveAttribute(
      "datetime",
      new Date(lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS).toISOString()
    );
    // The holder is drawn as unresponsive wherever it is named, never as not running.
    await expect(chip.locator(`[data-agent-name="${holder.session_id}"]`)).toHaveAttribute(
      "data-agent-state",
      "unresponsive"
    );

    // The issue is nobody's to pick up while the claim holds, so the Unclaimed filter hides it.
    await page.goto("/projects/CORE/issues?unclaimed=1");
    await expect(page.getByTestId("unclaimed-unchecked")).toHaveCount(0);
    await expect(page.locator("[data-issue-row]").filter({ hasText: issueKey })).toHaveCount(0);

    await expect(
      claimIssue(issueKey, {
        actor: { id: taker.session_id, kind: "session", origin: { session_title: taker.title } },
        as: "agent",
        token,
      })
    ).rejects.toThrow(/409 .*unresponsive since/s);
  } finally {
    await alice.close();
  }
});

// Past the grace the server grants the next agent that asks, so a chip that still read as held,
// and a filter that still hid the issue, would hide work anyone may pick up. The holder has not
// ended, so it is still drawn as unresponsive and never as not running.
test("an unresponsive holder past its takeable moment reads takeable now, is listed as unclaimed, and a second agent's claim is granted", async ({
  browser,
}) => {
  const { issueKey, token } = await seedClaim("Stalled past the claim's grace");
  await stall(ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS + 60_000);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}`);
    const chip = page.getByTestId("issue-claim");
    const stalled = page.getByTestId("issue-claim-unresponsive");
    await expect(stalled).toContainText("unresponsive since");
    await expect(stalled.getByTestId("issue-claim-takeable")).toHaveText(", takeable now");
    await expect(page.getByTestId("issue-claim-lapsed")).toHaveCount(0);
    await expect(chip).toHaveAttribute("title", /another agent may take it now/);
    await expect(chip.locator(`[data-agent-name="${holder.session_id}"]`)).toHaveAttribute(
      "data-agent-state",
      "unresponsive"
    );

    // The filter that lists pickable work lists it, because the server would hand it over.
    await page.goto("/projects/CORE/issues?unclaimed=1");
    await expect(page.getByTestId("unclaimed-unchecked")).toHaveCount(0);
    await expect(page.locator("[data-issue-row]").filter({ hasText: issueKey })).toHaveCount(1);

    // And it does hand it over: the page and the server agree.
    const taken = await claimIssue(issueKey, {
      actor: { id: taker.session_id, kind: "session", origin: { session_title: taker.title } },
      as: "agent",
      token,
    });
    expect(taken.claim?.actor.id).toBe(taker.session_id);
  } finally {
    await alice.close();
  }
});

// A5: the Agents page shows the same holder the same way. A person looking for the session that
// is working an issue must find it listed, marked unresponsive and dated, rather than dropped
// from the page or drawn as running.
test("the Agents page lists an unresponsive claim holder as unresponsive since its last heartbeat", async ({
  browser,
}) => {
  await seedClaim("Worked by a session the Agents page lists");
  const lastSeen = await stall(6 * 60_000);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    const row = agentRow(page, holder.session_id);
    await expect(row).toBeVisible();
    const note = row.getByTestId("agent-unresponsive");
    await expect(note).toBeVisible();
    await expect(note).toContainText("Unresponsive");
    await expect(note.locator("time")).toHaveAttribute(
      "datetime",
      new Date(lastSeen).toISOString()
    );
    await expect(note).toHaveAttribute("title", /It has not ended/);
    // The name's own mark reads the row's state, not the list's membership.
    await expect(row.getByRole("img", { name: "Unresponsive" })).toHaveCount(1);
    await expect(row.getByRole("img", { name: "Not running" })).toHaveCount(0);
    // The session the listener still lists live is untouched by its neighbour's silence.
    await expect(agentRow(page, taker.session_id).getByTestId("agent-unresponsive")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A5: route status `holder_unresponsive`, for a route to the session and for a route to a role
// it holds. The route still reaches that session - what is sent is stored for it - so the issue
// header says so instead of the warning it paints for a route that reaches nobody.
test("a route to an unresponsive session, and to a role it holds, reads holder_unresponsive and still reached", async ({
  browser,
}) => {
  await seedClaim("Worked by a session two routes point at");
  const direct = await createIssue({ project: "CORE", title: "Addressed to the stalled session" });
  const byRole = await createIssue({ project: "CORE", title: "Waiting on whoever holds the role" });
  await patchIssue(direct.key, { route: `session:${holder.session_id}` });
  await patchIssue(byRole.key, { route: `role:${ROLE}` });
  await stall(6 * 60_000);

  const alice = await asUser(browser, "alice");
  try {
    for (const key of [direct.key, byRole.key]) {
      // The server resolves the route against the holder's own record, not list membership.
      await expect.poll(async () => (await getIssue(key)).route_status).toBe("holder_unresponsive");
      expect((await getIssue(key)).route_holder).toBe(holder.session_id);

      const page = await alice.newPage();
      await page.goto(`/issues/${key}`);
      const marker = page.getByTestId("issue-route-unresponsive");
      await expect(marker).toBeVisible();
      await expect(marker).toContainText("Unresponsive, still reached");
      await expect(marker).toHaveAttribute("title", /waits until it answers/);
      // Neither of the other two answers: the route is not unreachable, and it is not unjudged.
      await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
      await expect(page.getByTestId("issue-route-unknown")).toHaveCount(0);
      await page.close();
    }
  } finally {
    await alice.close();
  }
});
