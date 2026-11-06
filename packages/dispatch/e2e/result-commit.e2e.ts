import { expect, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  putRepoProject,
  recordCheckResult,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Dispatch stored a result at a mistyped commit once, and a reader could not tell it from the
// result at the real head: every line cut the commit to twelve characters, and both printed the
// same twelve. These two commits are that pair - the same first twelve characters, a different
// remaining twenty-eight - so a line that still cuts them reads identically for the two results
// and every assertion below fails on the cut form, holding only on the whole forty characters.
const repository = "acme/widgets";
const sharedPrefix = "a1b2c3d4e5f6";
/** The head of the pull request Dispatch holds for the issue. */
const head = `${sharedPrefix}${"0".repeat(28)}`;
/** A commit of the repository that heads no pull request, whose first twelve characters are
 *  the head's. */
const mistyped = `${sharedPrefix}${"9".repeat(28)}`;

/** The spec the check serves. A `serves` pointer is refused unless it names a block that
 *  exists, so the requirement is written with the issue, before any check points at one. */
const spec = `:::requirement{#r1}
A recorded result names the commit it ran at, whole.
:::
`;

test.beforeEach(async () => {
  await resetDatabase();
});

// Two pages print a result's line as plain text, where no title attribute can carry the commit
// the line cut: the Activity tab, through `activity-model.ts`, and the review margin's Pinned
// tab, which is the only place `event-description.ts` draws an event's own line (`EventBody`).
// This row drives both, since a row that drove one would leave the other's cut unproven. The
// Verification tab is not here: it renders `<code title={result.commit_sha}>`, so a reader
// already reaches the whole commit there.
//
// SLOW(>=2s): 3.1 s - a project, its default repository, a pull request in the fake GitHub, an
// issue with its spec and its Verification document, and two results recorded against that
// document's one check, each result's record route reading through to the fake for the commit
// it holds and the pull request's head; then one load of the Activity tab, two pin writes and
// the margin's Pinned tab, which draws each pinned event's own line.
test("a result's line names the whole commit, so a result at a mistyped commit is told from one at the head", async ({
  browser,
}) => {
  await createProject({ key: "VER", name: "Verification" });
  await putRepoProject(repository, { default: true, project: "VER" });
  const issue = await createIssue({ project: "VER", spec, title: "Whole commits in a result" });
  // A result at a commit the repository does not hold is refused (`refuseUnknownCommit`), so the
  // mistyped commit is seeded as one the fake GitHub answers for: `commits` is the field that
  // says the repository holds it and nothing else does, which is what keeps it a commit of its
  // own rather than another name for the head.
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [head]: [7] },
      commits: [mistyped],
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: head, state: "open", title: `${issue.key} the lines` } },
    },
  });
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-whole serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/result-commit.e2e.ts" negative_control="Cut the commit to twelve characters and the row fails."}
A result's line names the whole commit it ran at.
:::
`,
    name: "verification.md",
  });
  // The two results differ in the commit they ran at and in the role that recorded them; the
  // role is how each line is found below, so no assertion depends on the commit it reads.
  await recordCheckResult(issue.key, {
    check: "c-whole",
    commit: mistyped,
    negative_control: "Cut the commit and the row fails.",
    observed: "1 check listed",
    role: "tester",
    run: "bun run e2e -- e2e/result-commit.e2e.ts",
    surface: "browser",
    verdict: "pass",
  });
  await recordCheckResult(issue.key, {
    check: "c-whole",
    commit: head,
    negative_control: "Cut the commit and the row fails.",
    observed: "1 check listed",
    role: "implementer",
    run: "bun run e2e -- e2e/result-commit.e2e.ts",
    surface: "browser",
    verdict: "pass",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/activity`);
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    const mistypedRow = rows.filter({ hasText: "as tester" });
    const headRow = rows.filter({ hasText: "as implementer" });
    await expect(mistypedRow).toHaveCount(1);
    await expect(headRow).toHaveCount(1);

    // The Activity tab: each line carries its own whole commit, and neither carries the other's.
    await expect(mistypedRow).toContainText(
      `recorded pass for check c-whole at ${mistyped} as tester`
    );
    await expect(mistypedRow).not.toContainText(head);
    await expect(headRow).toContainText(
      `recorded pass for check c-whole at ${head} as implementer`
    );
    await expect(headRow).not.toContainText(mistyped);

    await mistypedRow.getByRole("button", { name: "Pin" }).click();
    await headRow.getByRole("button", { name: "Pin" }).click();

    // The Pinned tab, where the same two events are drawn by the issue's own event line.
    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    const cards = margin.getByRole("article");
    const mistypedCard = cards.filter({ hasText: "recorded by tester" });
    const headCard = cards.filter({ hasText: "recorded by implementer" });
    await expect(mistypedCard).toHaveCount(1);
    await expect(headCard).toHaveCount(1);
    await expect(mistypedCard).toContainText(
      `Check c-whole passed at ${mistyped}, recorded by tester`
    );
    await expect(mistypedCard).not.toContainText(head);
    await expect(headCard).toContainText(
      `Check c-whole passed at ${head}, recorded by implementer`
    );
    await expect(headCard).not.toContainText(mistyped);
  } finally {
    await alice.close();
  }
});
