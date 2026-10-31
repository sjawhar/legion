import { type Browser, expect, test } from "@playwright/test";

import { createIssue, createProject, patchIssue } from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

// The repository is used by no other spec: the server remembers which installation covers a
// repository it has read, so reusing one another spec expects to be unavailable would leak.
const repository = "acme/checks-pill";

/** Seeds pull request #7 of `repository` at `headSHA` with those runs and opens its issue. */
async function expectChecksPill(
  browser: Browser,
  headSHA: string,
  title: string,
  checkRuns: {
    check_suite_id: number;
    conclusion: string | null;
    id: number;
    name: string;
    status: string;
  }[],
  workflowRuns: { check_suite_id: number; event: string; id: number; name: string }[],
  checks: string
): Promise<void> {
  await seedFakeGithub({
    [repository]: {
      check_runs: { [headSHA]: checkRuns },
      contents: "read",
      installation_id: 256,
      pulls: { "7": { head_sha: headSHA, state: "open", title } },
      pull_requests: "read",
      workflow_runs: { [headSHA]: workflowRuns },
    },
  });
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Checks pill" });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: `https://github.com/${repository}/pull/7` }],
  });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}`);
    const link = page.getByRole("link", { name: `#7 ${title}` });
    await expect(link).toHaveAttribute("title", `${repository}#7: ${title}`);
    await expect(link.getByText(/^checks: /)).toHaveText(`checks: ${checks}`);
  } finally {
    await context.close();
  }
}

// GitHub's merge reads the newest run of each check, workflow and event: a PR Title run that a
// newer PR Title run of the same event replaced no longer counts.
test("the checks pill ignores a cancelled run its newer run of the same event passed", async ({
  browser,
}) => {
  await expectChecksPill(
    browser,
    "1111111111111111111111111111111111111111",
    "A re-run title check",
    [
      {
        check_suite_id: 501,
        conclusion: "cancelled",
        id: 9001,
        name: "pr-title",
        status: "completed",
      },
      { check_suite_id: 502, conclusion: "success", id: 9002, name: "lint", status: "completed" },
      {
        check_suite_id: 503,
        conclusion: "success",
        id: 9003,
        name: "pr-title",
        status: "completed",
      },
    ],
    [
      { check_suite_id: 501, event: "pull_request", id: 701, name: "PR Title" },
      { check_suite_id: 502, event: "pull_request", id: 702, name: "CI" },
      { check_suite_id: 503, event: "pull_request", id: 703, name: "PR Title" },
    ],
    "success"
  );
});

// The newest run per event: a pull_request run that failed still blocks beside a later
// pull_request_review run of the same check that passed.
test("the checks pill keeps a check that failed for one event and passed for another", async ({
  browser,
}) => {
  await expectChecksPill(
    browser,
    "2222222222222222222222222222222222222222",
    "A title check failed on one event",
    [
      {
        check_suite_id: 601,
        conclusion: "failure",
        id: 9101,
        name: "pr-title",
        status: "completed",
      },
      {
        check_suite_id: 602,
        conclusion: "success",
        id: 9102,
        name: "pr-title",
        status: "completed",
      },
    ],
    [
      { check_suite_id: 601, event: "pull_request", id: 801, name: "PR Title" },
      { check_suite_id: 602, event: "pull_request_review", id: 802, name: "PR Title" },
    ],
    "failure"
  );
});

// GitHub evaluates a workflow job's check for a pull request only when a push, pull_request,
// pull_request_review, pull_request_target, deployment or deployment_status event started its run
// ("Checks from some workflow jobs are not evaluated"): an inline review comment's PR Title run
// that failed, or a workflow_dispatch run still queued, is not in the pull request's checks.
test("the checks pill ignores checks of events GitHub does not evaluate for a pull request", async ({
  browser,
}) => {
  await expectChecksPill(
    browser,
    "3333333333333333333333333333333333333333",
    "A review comment check failed",
    [
      {
        check_suite_id: 701,
        conclusion: "success",
        id: 9201,
        name: "pr-title",
        status: "completed",
      },
      {
        check_suite_id: 702,
        conclusion: "failure",
        id: 9202,
        name: "pr-title",
        status: "completed",
      },
      { check_suite_id: 703, conclusion: null, id: 9203, name: "build", status: "queued" },
    ],
    [
      { check_suite_id: 701, event: "pull_request", id: 901, name: "PR Title" },
      { check_suite_id: 702, event: "pull_request_review_comment", id: 902, name: "PR Title" },
      { check_suite_id: 703, event: "workflow_dispatch", id: 903, name: "Build" },
    ],
    "success"
  );
});
