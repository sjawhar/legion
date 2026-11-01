import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { expect, test } from "@playwright/test";

import { setLiveSessions, setSessionLastSeen, setSessionState } from "./agents";
import { claimIssue, createIssue, createProject, mintAgentToken } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A session that has missed its heartbeats has not ended: it keeps the issue it is working, and
// the page a person reads has to say so rather than call it gone. The dashboard and the server
// answer the same question here - the chip names the moment the claim comes free, and a second
// agent asking for it before that moment is refused (dispatch://INFRA-392).

test.beforeEach(async () => {
  await resetDatabase();
});

const holder = { session_id: "claim-stalled-session", title: "Stalled holder" };
const taker = { session_id: "claim-taker-session", title: "Second agent" };

test("an unresponsive holder keeps its claim: the issue page says so and names when it becomes takeable, and a second agent is refused", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Worked by a stalled session" });
  const token = await mintAgentToken("claim-unresponsive", "alice");
  await setLiveSessions([holder, taker]);
  await claimIssue(issue.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });

  // Past the listener's live window, inside its gone horizon: the listener still holds the
  // record and says the session is unresponsive, with the heartbeat it was last seen at.
  const lastSeen = Date.now() - 6 * 60_000;
  await setSessionLastSeen(holder.session_id, lastSeen);
  await setSessionState(holder.session_id, "unresponsive");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const chip = page.getByTestId("issue-claim");
    const stalled = page.getByTestId("issue-claim-unresponsive");
    await expect(stalled).toBeVisible();
    await expect(page.getByTestId("issue-claim-lapsed")).toHaveCount(0);
    await expect(chip).toHaveAttribute("title", /missed its heartbeats without ending/);
    // The moment named is the holder's last heartbeat plus the shared grace, which is the sum
    // the server's own refusal below is written from.
    const takeableAt = await stalled.locator("time").getAttribute("datetime");
    expect(takeableAt).not.toBeNull();
    expect(Date.parse(takeableAt ?? "")).toBe(lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS);
    // The holder is drawn as unresponsive wherever it is named, never as not running.
    await expect(chip.locator(`[data-agent-name="${holder.session_id}"]`)).toHaveAttribute(
      "data-agent-state",
      "unresponsive"
    );

    await expect(
      claimIssue(issue.key, {
        actor: { id: taker.session_id, kind: "session", origin: { session_title: taker.title } },
        as: "agent",
        token,
      })
    ).rejects.toThrow(/409 .*unresponsive since/s);
  } finally {
    await alice.close();
  }
});
