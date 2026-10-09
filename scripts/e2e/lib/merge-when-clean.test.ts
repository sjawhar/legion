import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// merge_when_clean (lib/workflow.sh) against a fake gh first on PATH. The proof reads the pull
// request's state first and merges by hand only when the merger armed nothing; under the merger
// prompt (packages/daemon/internal/prompts/roles/merger.md, step 4) a pull request the merger
// already submitted — merged, auto-merge armed, or in the merge queue — is the expected read.
//
// The fake answers the merge record's GraphQL query from FAKE_GH_DIR/answers/<n>, one answer per
// read in the order the proof reads (the last answer repeats), each the nine lines the query's
// --jq filter prints: state, mergeStateStatus, headRefOid, headRefName, mergeCommit.oid,
// autoMergeRequest.enabledAt, autoMergeRequest.mergeMethod, mergeQueueEntry.position and
// mergeQueueEntry.state, empty where GitHub answers null (`answer` below writes them, and the
// first test ties that order to the filter). FAKE_GH_READ_FAIL fails the first N reads, as a gh
// that answered nothing within its timeout; they consume no answer. It records every call, one
// line each, in FAKE_GH_DIR/calls; `pr merge` is refused with FAKE_GH_MERGE_FAIL's text when set,
// the statusCheckRollup read a timeout's message makes answers one queued check, and the branch
// delete is refused 422 when FAKE_GH_BRANCH_GONE is set, as GitHub refuses a branch its own
// auto-delete already removed.
const lib = join(import.meta.dir, "workflow.sh");
const dir = mkdtempSync(join(tmpdir(), "merge-when-clean-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(
  join(bin, "gh"),
  `#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_GH_DIR
printf '%s\\n' "\${*//$'\\n'/ }" >>"$state/calls"
case " $* " in
  *" api graphql "*)
    n=$(($(cat "$state/reads" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$state/reads"
    [ "$n" -gt "\${FAKE_GH_READ_FAIL:-0}" ] || { echo "gh: HTTP 502: Bad Gateway" >&2; exit 1; }
    n=$((n - \${FAKE_GH_READ_FAIL:-0}))
    last=$(cat "$state/answers/count")
    [ "$n" -le "$last" ] || n=$last
    cat "$state/answers/$n" ;;
  *" pr merge "*)
    [ -z "\${FAKE_GH_MERGE_FAIL:-}" ] || { printf '%s\\n' "$FAKE_GH_MERGE_FAIL" >&2; exit 1; }
    echo "✓ Squashed and merged pull request #7" >&2 ;;
  *" pr view "*" statusCheckRollup "*)
    echo '[{"name":"gate","workflow":"fail-on-demand","status":"QUEUED","conclusion":null}]' ;;
  *" api -X DELETE repos/"*"/git/refs/heads/"*)
    [ -z "\${FAKE_GH_BRANCH_GONE:-}" ] || { echo "gh: Reference does not exist (HTTP 422)" >&2; exit 1; } ;;
  *) echo "fake gh: no answer for $*" >&2; exit 1 ;;
esac
`,
  { mode: 0o755 }
);

// The stage scripts' shell options, ERR trap and check helpers, then the lib, then the call and
// its record.
const prelude = `set -Eeuo pipefail
check=ordinary-human-squash-merge
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
trap 'printf "FAIL %s: line %s exited %s: %s\\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
repo=sjawhar/legion-smoke project=S3TEST ptoken=s3test ok=
. "$LIB"
`;
const record = `
printf 'by=%s\\ncommit=%s\\n' "$merge_when_clean_by" "$merge_when_clean_commit"
`;

const head = "0123456789abcdef0123456789abcdef01234567";
const branch = "legion/S3TEST-1-smoke";
const mergeCommit = "fedcba9876543210fedcba9876543210fedcba98";
const enabledAt = "2026-10-09T12:00:00Z";

type PullRequest = {
  state: "OPEN" | "MERGED" | "CLOSED";
  mergeStateStatus: string;
  mergeCommit?: string;
  autoMerge?: { enabledAt: string; mergeMethod: string };
  queue?: { position: number; state: string };
};
// One GraphQL answer as the --jq filter prints it: nine lines, empty where GitHub answers null.
const answer = (pr: PullRequest) =>
  `${[
    pr.state,
    pr.mergeStateStatus,
    head,
    branch,
    pr.mergeCommit ?? "",
    pr.autoMerge?.enabledAt ?? "",
    pr.autoMerge?.mergeMethod ?? "",
    pr.queue?.position ?? "",
    pr.queue?.state ?? "",
  ].join("\n")}\n`;

const merged: PullRequest = { state: "MERGED", mergeStateStatus: "UNKNOWN", mergeCommit };
const clean: PullRequest = { state: "OPEN", mergeStateStatus: "CLEAN" };
const blocked: PullRequest = { state: "OPEN", mergeStateStatus: "BLOCKED" };
const autoMerge: PullRequest = {
  state: "OPEN",
  mergeStateStatus: "BLOCKED",
  autoMerge: { enabledAt, mergeMethod: "SQUASH" },
};
const queued = (position: number, state: string): PullRequest => ({
  state: "OPEN",
  mergeStateStatus: "BLOCKED",
  queue: { position, state },
});

type Fake = { readFail?: number; mergeFail?: string; branchGone?: boolean; bound?: number };
let runs = 0;
function run(reads: PullRequest[], flags = "--squash --delete-branch", fake: Fake = {}) {
  const state = join(dir, `run-${++runs}`);
  mkdirSync(join(state, "answers"), { recursive: true });
  for (const [i, pr] of reads.entries()) {
    writeFileSync(join(state, "answers", String(i + 1)), answer(pr));
  }
  writeFileSync(join(state, "answers", "count"), String(reads.length));
  writeFileSync(join(state, "calls"), "");
  const script = `${prelude}merge_when_clean "$repo" 7 ${flags}${record}`;
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: {
      PATH: `${bin}:${process.env.PATH}`,
      HOME: state,
      LIB: lib,
      FAKE_GH_DIR: state,
      FAKE_GH_READ_FAIL: String(fake.readFail ?? 0),
      FAKE_GH_MERGE_FAIL: fake.mergeFail ?? "",
      FAKE_GH_BRANCH_GONE: fake.branchGone ? "1" : "",
      MERGE_WHEN_CLEAN_BOUND: String(fake.bound ?? 5),
      MERGE_WHEN_CLEAN_POLL: "0.02",
    },
  });
  const stdout = result.stdout.toString();
  const calls = readFileSync(join(state, "calls"), "utf8").split("\n").filter(Boolean);
  return {
    status: result.exitCode,
    stdout,
    stderr: result.stderr.toString(),
    calls,
    reads: calls.filter((c) => c.startsWith("api graphql ")),
    merges: calls.filter((c) => / pr merge /.test(c)),
    deletes: calls.filter((c) => c.startsWith("api -X DELETE ")),
    by: stdout.match(/^by=(.*)$/m)?.[1],
    commit: stdout.match(/^commit=(.*)$/m)?.[1],
  };
}

describe("merge_when_clean", () => {
  test("reads the merge record in one GraphQL query whose filter prints the fake's nine lines", () => {
    const r = run([merged]);
    expect(r.status).toBe(0);
    expect(r.reads).toHaveLength(1);
    const read = r.reads[0];
    expect(read).toMatch(
      /^api graphql -F owner=sjawhar -F name=legion-smoke -F number=7 -f query=.* --jq /
    );
    expect(read).toMatch(
      /pullRequest\(number: \$number\) \{\s+state mergeStateStatus headRefOid headRefName mergeCommit \{ oid \}\s+autoMergeRequest \{ enabledAt mergeMethod \} mergeQueueEntry \{ position state \}/
    );
    expect(read).toMatch(
      /--jq \.data\.repository\.pullRequest \| \.state, \.mergeStateStatus, \.headRefOid, \.headRefName, \(\.mergeCommit\.oid \/\/ ""\),\s+\(\.autoMergeRequest\.enabledAt \/\/ ""\), \(\.autoMergeRequest\.mergeMethod \/\/ ""\), \(\.mergeQueueEntry\.position \/\/ ""\), \(\.mergeQueueEntry\.state \/\/ ""\)$/
    );
  });

  test("a pull request already merged is the merger's submission: no hand merge, the branch deleted", () => {
    const r = run([merged]);
    expect(r.status).toBe(0);
    expect(r.merges).toHaveLength(0);
    expect(r.by).toBe("the merger's submission");
    expect(r.commit).toBe(mergeCommit);
    expect(r.deletes).toEqual([
      `api -X DELETE repos/sjawhar/legion-smoke/git/refs/heads/${branch}`,
    ]);
    expect(r.stdout).toContain(
      `sjawhar/legion-smoke#7 merged by the merger's submission at ${mergeCommit}`
    );
    expect(r.stdout).toContain(`deleted sjawhar/legion-smoke's branch ${branch}`);
  });

  test("a branch GitHub's auto-delete already removed is fine", () => {
    const r = run([merged], "--squash --delete-branch", { branchGone: true });
    expect(r.status).toBe(0);
    expect(r.deletes).toHaveLength(1);
    expect(r.stdout).toContain(`branch ${branch} is already gone`);
    expect(r.stderr).toBe("");
  });

  test("without --delete-branch the merger's merged pull request keeps its branch", () => {
    const r = run([merged], "--squash");
    expect(r.status).toBe(0);
    expect(r.merges).toHaveLength(0);
    expect(r.deletes).toHaveLength(0);
  });

  test("auto-merge armed: waits for the merge to land with no hand merge", () => {
    const r = run([autoMerge, autoMerge, autoMerge, merged]);
    expect(r.status).toBe(0);
    expect(r.reads).toHaveLength(4);
    expect(r.merges).toHaveLength(0);
    expect(r.by).toBe("the merger's submission");
    expect(r.commit).toBe(mergeCommit);
    expect(r.deletes).toHaveLength(1);
    const armed = r.stdout
      .split("\n")
      .filter((l) => l.includes("armed by the merger's submission"));
    expect(armed).toHaveLength(1);
    expect(armed[0]).toContain(
      `(auto-merge enabled at ${enabledAt} (SQUASH); mergeStateStatus BLOCKED): no hand merge`
    );
  });

  test("a merge queue entry: the same, naming the position", () => {
    const r = run([queued(2, "AWAITING_CHECKS"), queued(0, "MERGEABLE"), merged]);
    expect(r.status).toBe(0);
    expect(r.reads).toHaveLength(3);
    expect(r.merges).toHaveLength(0);
    expect(r.by).toBe("the merger's submission");
    expect(r.commit).toBe(mergeCommit);
    expect(r.stdout).toContain(
      "(merge queue position 2, AWAITING_CHECKS; mergeStateStatus BLOCKED)"
    );
    expect(r.stdout).toContain("(merge queue position 0, MERGEABLE; mergeStateStatus BLOCKED)");
  });

  test("nothing armed and CLEAN: one hand merge at the head read, then MERGED on re-read", () => {
    const r = run([blocked, clean, merged]);
    expect(r.status).toBe(0);
    expect(r.reads).toHaveLength(3);
    expect(r.merges).toEqual([
      `-R sjawhar/legion-smoke pr merge 7 --match-head-commit ${head} --squash --delete-branch`,
    ]);
    expect(r.deletes).toHaveLength(0);
    expect(r.by).toBe("the proof human");
    expect(r.commit).toBe(mergeCommit);
    expect(r.stdout).toContain(
      `sjawhar/legion-smoke#7 reads CLEAN with nothing armed; the proof human merges it by hand at ${head}`
    );
    expect(r.stdout).toContain(
      `sjawhar/legion-smoke#7 merged by the proof human at ${mergeCommit}`
    );
  });

  test("the race: a hand merge refused as already merged is the merger's outcome", () => {
    const r = run([clean, merged], "--squash --delete-branch", {
      mergeFail: "! Pull request sjawhar/legion-smoke#7 was already merged",
    });
    expect(r.status).toBe(0);
    expect(r.merges).toHaveLength(1);
    expect(r.reads).toHaveLength(2);
    expect(r.by).toBe("the merger's submission");
    expect(r.commit).toBe(mergeCommit);
    expect(r.deletes).toHaveLength(1);
    expect(r.stdout).toContain("the hand merge of sjawhar/legion-smoke#7 was refused");
    expect(r.stderr).toBe("! Pull request sjawhar/legion-smoke#7 was already merged\n");
  });

  test("a refused hand merge on a pull request that stays open fails naming what it reads", () => {
    const r = run([clean, blocked], "--squash --delete-branch", { mergeFail: "gh: HTTP 405" });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(
      "FAIL ordinary-human-squash-merge: sjawhar/legion-smoke#7 is not merged: it reads OPEN, mergeStateStatus BLOCKED, merge commit none"
    );
    expect(r.by).toBeUndefined();
  });

  test("nothing armed and BLOCKED past the bound fails naming the state and the checks", () => {
    const r = run([blocked], "--squash --delete-branch", { bound: 1 });
    expect(r.status).toBe(1);
    expect(r.merges).toHaveLength(0);
    expect(r.deletes).toHaveLength(0);
    expect(r.stderr).toMatch(
      /^FAIL ordinary-human-squash-merge: timed out after \ds \(\d+ polls\) waiting for sjawhar\/legion-smoke#7 to merge: it reads OPEN, mergeStateStatus BLOCKED, nothing armed; unsettled checks: \[\{"name":"gate"/
    );
    expect(r.stderr.trimEnd().split("\n")).toHaveLength(1);
  });

  test("armed past the bound fails naming what is armed", () => {
    const r = run([autoMerge], "--squash --delete-branch", { bound: 1 });
    expect(r.status).toBe(1);
    expect(r.merges).toHaveLength(0);
    expect(r.stderr).toContain(
      `it reads OPEN, mergeStateStatus BLOCKED, auto-merge enabled at ${enabledAt} (SQUASH); unsettled checks:`
    );
  });

  test("a read that fails is not yet, with no false ERR line", () => {
    const r = run([blocked, clean, merged], "--squash --delete-branch", { readFail: 2 });
    expect(r.status).toBe(0);
    expect(r.reads).toHaveLength(5);
    expect(r.merges).toHaveLength(1);
    expect(r.by).toBe("the proof human");
    expect(r.stderr).not.toContain("FAIL");
    expect(r.stderr).toContain("gh: HTTP 502: Bad Gateway");
  });

  test("a pull request closed without a merge fails at once", () => {
    const r = run([{ state: "CLOSED", mergeStateStatus: "UNKNOWN" }]);
    expect(r.status).toBe(1);
    expect(r.reads).toHaveLength(1);
    expect(r.stderr).toBe(
      "FAIL ordinary-human-squash-merge: sjawhar/legion-smoke#7 was closed without a merge\n"
    );
  });
});
