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

// A result's currency is judged against the head of the pull request Dispatch holds for the
// check's issue. The browser harness runs with NATS disabled, so no `pull_request` envelope can
// reach the server to teach it that head; the record route's own read-through does it instead
// (`readThroughPullRequests`), asking the fake GitHub which pull requests name the result's
// commit and storing that pull request's head. Re-seeding the fake with a later head and
// recording another result moves the head exactly as a push does, which is how the row below
// ages a result that is no longer at it.
const repository = "acme/widgets";
const firstHead = "1".repeat(40);
const secondHead = "2".repeat(40);

/** The spec the checks serve. A `serves` pointer is refused unless it names a block that
 *  exists, so the requirements are written with the issue, before any check points at one. */
const spec = `:::requirement{#r1}
Every check of the Verification document is listed.
:::

:::requirement{#r2}
A check says how its newest result stands.
:::
`;

/** Seeds the project and its default repository, the issue and its spec, and the pull request
 *  the results are measured against, whose head is `head` and whose title attributes it to the
 *  issue. */
async function seedIssue(title: string, head: string) {
  await createProject({ key: "VER", name: "Verification" });
  await putRepoProject(repository, { default: true, project: "VER" });
  const issue = await createIssue({ project: "VER", spec, title });
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [head]: [7] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: head, state: "open", title: `${issue.key} the tab` } },
    },
  });
  return issue;
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the Verification tab lists every check of the document, each with the results recorded against it", async ({
  browser,
}, testInfo) => {
  const issue = await seedIssue("Verification tab", secondHead);
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-passed serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/verification.e2e.ts" negative_control="Draw no checks and the row fails."}
The tab lists every check of the document.
:::

:::check{#c-failed serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Record no result and the check reads no result."}
A check whose newest result failed reads failed.
:::

:::check{#c-none serves="dispatch://${issue.key}/spec#b-r2" surface="api" run="curl the results route" negative_control="Record a result and the check stops reading no result."}
A check nobody has run yet reads no result.
:::
`,
    name: "verification.md",
  });
  await recordCheckResult(issue.key, {
    check: "c-passed",
    commit: secondHead,
    negative_control: "Removed the list and the run failed.",
    observed: "3 checks listed",
    role: "tester",
    run: "bun run e2e -- e2e/verification.e2e.ts",
    surface: "browser",
    verdict: "pass",
  });
  await recordCheckResult(issue.key, {
    check: "c-failed",
    commit: secondHead,
    failed: 2,
    negative_control: "Fixed the panel and the run passed.",
    observed: "2 failed",
    role: "tester",
    run: "bun test web/src",
    surface: "cli",
    verdict: "fail",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    await expect(page.getByTestId("verification-tab")).toBeVisible();

    // Every check of the document, in document order, whatever anyone has recorded against it.
    const checks = page.getByTestId("verification-check");
    await expect(checks).toHaveCount(3);
    await expect(checks.getByTestId("verification-check-state")).toHaveText([
      "Passed",
      "Failed",
      "No result",
    ]);

    // The result recorded through the API sits under its own check, and the check reads passed.
    const passed = checks.filter({ hasText: "The tab lists every check of the document." });
    await expect(passed).toHaveAttribute("data-check-state", "passed");
    await expect(passed.getByTestId("verification-check-when")).toHaveText("before_merge");
    await expect(passed.getByTestId("verification-check-surface")).toHaveText("browser");
    const result = passed.getByTestId("verification-result");
    await expect(result).toHaveCount(1);
    await expect(result.getByTestId("verification-result-verdict")).toHaveText("Pass");
    // The commit the result ran at is the pull request's head, so the result still stands.
    await expect(result.getByTestId("verification-result-currency")).toHaveText("current");
    await expect(result.getByTestId("verification-result-role")).toHaveText("tester");
    await expect(result.getByTestId("verification-result-surface")).toHaveText("browser");
    await expect(result.getByTestId("verification-result-observed")).toHaveText("3 checks listed");
    await expect(result.getByTestId("verification-result-commit")).toHaveText(
      secondHead.slice(0, 12)
    );
    await expect(result.getByTestId("verification-result-negative-control")).toHaveText(
      "Removed the list and the run failed."
    );
    await expect(result.getByTestId("verification-result-failed")).toHaveText("0");

    // A check whose newest result failed reads failed, and counts what that run counted.
    const failed = checks.filter({ hasText: "A check whose newest result failed reads failed." });
    await expect(failed).toHaveAttribute("data-check-state", "failed");
    await expect(failed.getByTestId("verification-result-verdict")).toHaveText("Fail");
    await expect(failed.getByTestId("verification-result-failed")).toHaveText("2");

    // A check nobody has run says so instead of showing an empty list.
    const none = checks.filter({ hasText: "A check nobody has run yet reads no result." });
    await expect(none).toHaveAttribute("data-check-state", "no-result");
    await expect(none.getByTestId("verification-check-no-results")).toHaveText(
      "No result recorded against this check."
    );
    await expect(none.getByTestId("verification-result")).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("verification-checks.png") });
  } finally {
    await alice.close();
  }
});

test("a result at a commit the pull request has moved past reads older", async ({ browser }) => {
  const issue = await seedIssue("Verification currency", firstHead);
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is proved.

:::check{#c-older serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Keep the head and the result stays current."}
A result the head has moved past reads older.
:::

:::check{#c-head serves="dispatch://${issue.key}/spec#b-r2" surface="cli" run="bun test web/src" negative_control="Move the head again and this one ages too."}
A result at the head reads passed.
:::
`,
    name: "verification.md",
  });
  // Recorded at the head as it stands, so the read-through stores that head and the result is
  // current the moment it is written.
  await recordCheckResult(issue.key, {
    check: "c-older",
    commit: firstHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });

  // The work moves on: the pull request's head is now a later commit, and the next result's
  // read-through teaches the server that head.
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [secondHead]: [7] },
      contents: "read",
      installation_id: 256,
      pull_requests: "read",
      pulls: { "7": { head_sha: secondHead, state: "open", title: `${issue.key} the tab` } },
    },
  });
  await recordCheckResult(issue.key, {
    check: "c-head",
    commit: secondHead,
    observed: "41 passed",
    role: "tester",
    verdict: "pass",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);
    const checks = page.getByTestId("verification-check");

    const older = checks.filter({ hasText: "A result the head has moved past reads older." });
    await expect(older).toHaveAttribute("data-check-state", "older");
    await expect(older.getByTestId("verification-check-state")).toHaveText("Older");
    // The result itself still says it passed; what changed is that it no longer speaks for the
    // work as it stands.
    await expect(older.getByTestId("verification-result-verdict")).toHaveText("Pass");
    await expect(older.getByTestId("verification-result-currency")).toHaveText("older");
    await expect(older.getByTestId("verification-result-commit")).toHaveText(
      firstHead.slice(0, 12)
    );

    const head = checks.filter({ hasText: "A result at the head reads passed." });
    await expect(head).toHaveAttribute("data-check-state", "passed");
    await expect(head.getByTestId("verification-result-currency")).toHaveText("current");
  } finally {
    await alice.close();
  }
});

test("an issue with no Verification document shows the command that makes one", async ({
  browser,
}) => {
  await createProject({ key: "VER", name: "Verification" });
  const issue = await createIssue({ project: "VER", title: "Nothing to verify yet" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/verification`);

    const empty = page.getByTestId("verification-empty");
    await expect(empty).toContainText(`${issue.key} has no Verification document.`);
    await expect(empty).toContainText(
      `dispatch artifact --issue ${issue.key} --name verification.md`
    );
    await expect(page.getByTestId("verification-checks")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
