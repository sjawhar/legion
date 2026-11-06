import { expect, type Page, test } from "@playwright/test";

import { EMPTY_STATE_LINES, NODE_STATE_WORDS } from "../web/src/features/issue/page-words";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  putRepoProject,
  recordCheckResult,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  blockNodeId,
  branchMoreRows,
  drawnRows,
  gotoIssue,
  nodeRow,
  rowBody,
  rowsOfType,
  visibleText,
} from "./issue-page";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The issue's checks, on the Checks preset of the redesigned issue page (dispatch://INFRA-1196):
// each check of the Verification document is a node of the issue's tree, with its state told by
// a word and a shape, its own attributes in its body, and each result recorded against it a node
// under it carrying what that run recorded. Until 2026-11-06 this file read the Verification
// tab's list of cards (`features/verification/VerificationTab.tsx`, `[data-testid=
// "verification-check"]`), which drew every check a second time under the document; the tree
// replaced both the tab and its list, so every row below reads the same facts off the tree.
//
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

/** The Verification document's slug, which is how its blocks are addressed on the page. */
const verification = "verification-md";

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

/** The whole text of one row's body, which is where a node's facts are drawn: a body opens
 *  because the keyboard focus is on its row (R2). */
async function bodyOf(page: Page, id: string): Promise<string> {
  const row = nodeRow(page, id).first();
  await row.focus();
  return (await rowBody(row).innerText()).replace(/\s+/g, " ");
}

/** Opens every branch the fold left as one row, which is where a check's results sit: a check
 *  is the third drawn level under the issue, so the results under it fold to a row that says
 *  how many they are until a reader opens them (R1). */
async function openResults(page: Page): Promise<void> {
  for (let opened = 0; opened < 8 && (await branchMoreRows(page).count()) > 0; opened += 1) {
    await branchMoreRows(page).first().getByRole("button").first().click();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): 2.3 s - a project, its default repository, a pull request in the fake GitHub, a
// Verification document of three checks and two recorded results, each result's record route
// reading through to that fake for the pull request's head, before one load of the page, which
// opens the document's live room and draws all three checks with every field of both results.
test("the Checks preset draws every check of the document, each with the results recorded against it", async ({
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);

    // Every check of the document, each drawn once, with the state the newest result gives it.
    await expect(rowsOfType(page, "check")).toHaveCount(3);
    const checks = (await drawnRows(page)).filter((row) => row.type === "check");
    expect(checks.map((row) => row.id)).toEqual([
      blockNodeId(verification, "c-passed"),
      blockNodeId(verification, "c-failed"),
      blockNodeId(verification, "c-none"),
    ]);
    expect(checks.map((row) => row.state)).toEqual(["passed", "failed", "no-result"]);
    expect(checks.map((row) => row.stateWord)).toEqual([
      NODE_STATE_WORDS.check.passed,
      NODE_STATE_WORDS.check.failed,
      NODE_STATE_WORDS.check["no-result"],
    ]);

    // The check's own attributes are in its body, with `when` as words.
    const passed = await bodyOf(page, blockNodeId(verification, "c-passed"));
    expect(passed).toContain("before the merge");
    expect(passed).toContain("browser");
    expect(passed).toContain("bun run e2e -- e2e/verification.e2e.ts");
    expect(passed).toContain("Draw no checks and the row fails.");

    // The result recorded through the API is a node under its own check, and says what it saw.
    await openResults(page);
    const results = (await drawnRows(page)).filter((row) => row.type === "result");
    expect(results.map((row) => row.firstLine)).toEqual(["3 checks listed", "2 failed"]);
    const current = await bodyOf(page, results[0]?.id ?? "");
    expect(current).toContain("Pass");
    // The commit the result ran at is the pull request's head, so the result still stands.
    expect(current).toContain("Current");
    expect(current).toContain("tester");
    expect(current).toContain("browser");
    expect(current).toContain(secondHead);
    expect(current).toContain("Removed the list and the run failed.");
    // A run that counted nothing draws no count at all, where the old tab drew three zeroes.
    expect(current).not.toContain("failed:");

    // A check whose newest result failed reads failed, and the result counts what that run
    // counted.
    const failed = await bodyOf(page, results[1]?.id ?? "");
    expect(failed).toContain("Fail");
    expect(failed).toContain("failed");
    expect(failed).toContain("2");

    // A check nobody has run says so in its state and has no result under it.
    const none = (await drawnRows(page)).find(
      (row) => row.id === blockNodeId(verification, "c-none")
    );
    expect(none?.stateWord).toBe(NODE_STATE_WORDS.check["no-result"]);
    expect(none?.expanded).toBeNull();
    await page.screenshot({ path: testInfo.outputPath("checks-preset.png") });
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 1.9 s here, over the bar on the four-lane report this rule is read against, and
// this pod puts it over in a third of runs. Two results recorded against the document's two
// checks, each one's record route reading through to the fake GitHub for the pull request's
// head, with the fake re-seeded between them so the first result ages; then one load of the
// issue page. Most of it is the floor every row of these files pays, measured on this
// row: a fresh Chromium context (0.25 s), the cold parse of the dashboard bundle a fresh
// context cannot cache (0.40 s) and the dashboard's own fifteen boot reads before the first
// check draws (0.80 s). INFRA-1109 holds that floor; nothing in the row is removable.
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
    await gotoIssue(page, `/issues/${issue.key}/verification`);
    const checks = (await drawnRows(page)).filter((row) => row.type === "check");
    const older = checks.find((row) => row.id === blockNodeId(verification, "c-older"));
    expect(older?.state).toBe("older");
    expect(older?.stateWord).toBe(NODE_STATE_WORDS.check.older);
    const head = checks.find((row) => row.id === blockNodeId(verification, "c-head"));
    expect(head?.state).toBe("passed");

    // The result itself still says it passed; what changed is that it no longer speaks for the
    // work as it stands.
    await openResults(page);
    const results = (await drawnRows(page)).filter((row) => row.type === "result");
    const aged = await bodyOf(page, results[0]?.id ?? "");
    expect(aged).toContain("Pass");
    expect(aged).toContain("Older");
    expect(aged).toContain(firstHead);
    const atHead = await bodyOf(page, results[1]?.id ?? "");
    expect(atHead).toContain("Current");
  } finally {
    await alice.close();
  }
});

test("an issue with no Verification document says the preset holds no check", async ({
  browser,
}) => {
  await createProject({ key: "VER", name: "Verification" });
  const issue = await createIssue({ project: "VER", title: "Nothing to verify yet" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/verification`);

    // The old tab printed the command that makes the document; the preset says what is absent
    // and never what the reader should do about it (`EMPTY_STATE_LINES`).
    expect(await visibleText(page)).toContain(EMPTY_STATE_LINES.checks);
    await expect(rowsOfType(page, "check")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
