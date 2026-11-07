import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, CheckBlock, CheckResult } from "../../api/types";
import { VerificationTab } from "./VerificationTab";

const commit = "0123456789abcdef0123456789abcdef01234567";

function result(overrides: Partial<CheckResult> = {}): CheckResult {
  return {
    actor: { id: "alice", kind: "user" },
    check_artifact_id: "artifact-1",
    check_block_id: "c1",
    check_issue_key: "CORE-1",
    check_sha256: "sha256",
    check_version: 2,
    commit_sha: commit,
    created_at: "2026-10-20T10:00:00Z",
    currency: "current",
    excluded: 0,
    failed: 0,
    id: "result-1",
    issue_key: "CORE-1",
    negative_control: "Removed the panel and the check failed.",
    observed: "41 passed, 0 failed",
    role: "tester",
    run: "bun test web/src",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "cli",
    verdict: "pass",
    ...overrides,
  };
}

function check(blockId: string, results: CheckResult[]): CheckBlock {
  return {
    artifact_id: "artifact-1",
    block_id: blockId,
    issue_key: "CORE-1",
    negative_control: "Delete the tab and the check fails.",
    ref: `dispatch://CORE-1/artifact/verification-md#b-${blockId}`,
    results,
    run: "bun test web/src",
    sha256: "sha256",
    surface: "cli",
    text: `Check ${blockId}`,
    version: 2,
    when: "before_merge",
  };
}

const verification: Artifact = {
  created_at: "2026-10-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "verification.md",
  primary: false,
  project: "CORE",
  role: "verification",
  ref_key: "CORE-1/verification-md",
  session_id: null,
  slug: "verification-md",
  versions: [],
};

function renderTab(artifact: Artifact | undefined, checks: CheckBlock[]) {
  const getIssueResults = spyOn(api, "getIssueResults").mockResolvedValue({
    checks,
    issue: "CORE-1",
    verification_artifact_id: artifact?.id ?? "",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <VerificationTab artifact={artifact} issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueResults, view };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

// The five readings of a check, which are the newest result's verdict and its currency and
// nothing else: a result that no longer stands says nothing about whether the check passes now,
// and a currency Dispatch could not judge is not a stale result.
test("each check reads as passed, failed, older, no result or unmarked", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c-pass", [result()]),
    check("c-fail", [result({ failed: 1, id: "result-2", verdict: "fail" })]),
    check("c-older", [result({ currency: "older", id: "result-3" })]),
    check("c-none", []),
    check("c-unknown", [result({ currency: "unknown", id: "result-5" })]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("verification-checks");
  expect(
    screen.getAllByTestId("verification-check").map((row) => row.getAttribute("data-check-state"))
  ).toEqual(["passed", "failed", "older", "no-result", "unmarked"]);
  expect(screen.getAllByTestId("verification-check-state").map((pill) => pill.textContent)).toEqual(
    ["Passed", "Failed", "Older", "No result", "Unmarked"]
  );
  // The check with no result says so where its results would be.
  expect(screen.getByTestId("verification-check-no-results").textContent).toBe(
    "No result recorded against this check."
  );
});

// An unmarked result still shows its verdict and its commit; what it does not show is a currency
// mark, because Dispatch could not read the repository to decide one.
test("an unmarked check draws its result's verdict and commit with no currency mark", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c-unknown", [result({ currency: "unknown" })]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const row = await screen.findByTestId("verification-result");
  expect(within(row).getByTestId("verification-result-verdict").textContent).toBe("Pass");
  expect(within(row).getByTestId("verification-result-commit").textContent).toBe(
    commit.slice(0, 12)
  );
  expect(within(row).queryByTestId("verification-result-currency")).toBeNull();
});

test("a result shows how it ran, what was seen, its counts, its role and its screenshots", async () => {
  const { getIssueResults, view } = renderTab(verification, [
    check("c1", [
      result({
        excluded: 2,
        failed: 3,
        screenshots: ["dispatch://CORE-1/artifact/shot-png"],
        skipped: 1,
        verdict: "fail",
      }),
    ]),
  ]);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const row = await screen.findByTestId("verification-result");
  const field = (name: string) =>
    within(row).getByTestId(`verification-result-${name}`).textContent;
  expect(field("verdict")).toBe("Fail");
  expect(field("currency")).toBe("Current");
  expect(field("role")).toBe("tester");
  expect(field("surface")).toBe("cli");
  expect(field("run")).toBe("bun test web/src");
  expect(field("observed")).toBe("41 passed, 0 failed");
  expect(field("commit")).toBe(commit.slice(0, 12));
  expect(field("negative-control")).toBe("Removed the panel and the check failed.");
  expect([field("failed"), field("skipped"), field("excluded")]).toEqual(["3", "1", "2"]);
  expect(within(row).getByTestId("verification-result-screenshot").getAttribute("href")).toBe(
    "/issues/CORE-1/artifacts/shot-png"
  );
});

test("an issue with no Verification document shows the command that makes one", () => {
  const { getIssueResults, view } = renderTab(undefined, []);
  restore = () => {
    getIssueResults.mockRestore();
    view.unmount();
  };

  const empty = screen.getByTestId("verification-empty");
  expect(empty.textContent).toContain("CORE-1 has no Verification document.");
  expect(empty.textContent).toContain("dispatch artifact --issue CORE-1 --name verification.md");
  // Nothing is read for an issue that has no such document.
  expect(getIssueResults).not.toHaveBeenCalled();
});
