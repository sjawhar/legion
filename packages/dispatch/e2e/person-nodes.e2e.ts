import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test("a person's graph lists authored and recorded received nodes @phone", async ({
  browser,
}, testInfo) => {
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Graph identities" });
  const authored = await createComment(issue.key, { body: "Alice authored this node." });
  const session = await createComment(
    issue.key,
    { body: "A session tied to Alice." },
    { as: "agent", actor: { kind: "session", id: "person-alice-session" } }
  );
  const received = await createComment(
    issue.key,
    { body: "Bob sent this to Alice's session." },
    { login: "bob" }
  );
  await createComment(issue.key, { body: "Bob's unrelated node." }, { login: "bob" });
  // A stored session actor carries owner and session_verified only where the server itself proved
  // the session: bearerSessionActor copies the owner from the authenticated token, and only a
  // broker-issued credential sets session_verified. The alias view takes a session alias from that
  // flag alone, so the fixture writes both, the way a broker-proved session is stored.
  await sql(
    `UPDATE comments SET author = author || '{"owner":"alice","session_verified":true}'::jsonb WHERE id = '${session.id}'`,
    `UPDATE events SET published_destinations = array['notifications.agent.person-alice-session']
     WHERE type = 'comment.created' AND payload->>'id' = '${received.id}'`
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const profile = page.getByRole("link", { name: "Signed in as alice", exact: true });
    await expect(profile).toBeVisible();
    await profile.click();
    await expect(page).toHaveURL("/people/alice");
    await expect(page.getByRole("heading", { name: "alice", exact: true })).toBeVisible();
    const rows = page.getByRole("list", { name: "Person nodes" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toContainText("Received");
    await expect(rows.nth(0)).toContainText("Bob sent this to Alice's session.");
    await expect(rows.nth(1)).toContainText("Authored");
    await expect(rows.nth(1)).toContainText("Alice authored this node.");
    await expect(rows.nth(0).getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${received.id}`
    );
    await expect(rows.nth(1).getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${authored.id}`
    );
    await page.screenshot({ path: testInfo.outputPath("person-nodes.png"), fullPage: true });
    await rows.nth(0).getByRole("link", { name: "Open" }).click();
    await expect(page).toHaveURL(`/issues/${issue.key}/comments/${received.id}`);
    const surface =
      testInfo.project.name === "iphone" ? page.getByRole("dialog", { name: "Thread" }) : page;
    await expect(surface.locator(`[data-turn="comment:${received.id}"]`)).toBeVisible();
  } finally {
    await alice.close();
  }
});
