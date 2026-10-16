import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// review_threads and reviewer_thread, taken from lib/workflow.sh by name and run against a
// stubbed gh api graphql answer: reviewer_thread must pick the thread the Legion reviewer's
// CHANGES_REQUESTED review opened, never a thread a COMMENT review's reviewer also left (the
// round-no-review-decides checkpoint in stage4b-sandbox-tree.sh runs that COMMENT review first,
// on the same proof pull request, before the requested round that actually opens a thread
// answered by a correction), nor one another account opened. The review App's login is the one
// read_app_logins would have read from GitHub; GraphQL names its bot by the bare slug.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));
const reviewApp = "example-review";

const dir = mkdtempSync(join(tmpdir(), "reviewer-thread-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
// The stub answers every `gh api graphql …` call with the fixture GH_GRAPHQL_FIXTURE names,
// whatever variables or query text the call carries: review_threads reads only the response body.
writeFileSync(join(bin, "gh"), '#!/bin/sh\ncat "$GH_GRAPHQL_FIXTURE"\n', { mode: 0o755 });

// threadsFixture writes a reviewThreads GraphQL page: THREADS is an array of
// { author, state, reviewState } for each thread's one comment.
function threadsFixture(
  threads: { author: string; state: string; reviewState: string | null }[]
): string {
  const path = join(dir, `fixture-${Math.random().toString(36).slice(2)}.json`);
  const nodes = threads.map((t) => ({
    id: `T_${t.author}_${t.reviewState ?? "none"}_${Math.random().toString(36).slice(2, 6)}`,
    isResolved: false,
    comments: {
      pageInfo: { hasNextPage: false },
      nodes: [
        {
          author: { login: t.author },
          body: "a finding",
          state: t.state,
          pullRequestReview: t.reviewState === null ? null : { state: t.reviewState },
        },
      ],
    },
  }));
  writeFileSync(
    path,
    JSON.stringify({
      data: {
        repository: {
          pullRequest: {
            reviewThreads: { pageInfo: { hasNextPage: false }, nodes },
          },
        },
      },
    })
  );
  return path;
}

function run(fixture: string, call: string) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
repo=sjawhar/legion-smoke pr_number=1
implement_app_login='example-implement[bot]' review_app_login='${reviewApp}[bot]'
${fn("role_app")}
${fn("role_app_slug")}
${fn("review_threads")}
${fn("reviewer_thread")}
${call}
`,
    ],
    { env: { PATH: `${bin}:${process.env.PATH}`, GH_GRAPHQL_FIXTURE: fixture } }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString().trim(),
    stderr: result.stderr.toString().trim(),
  };
}

describe("reviewer_thread", () => {
  test("picks the CHANGES_REQUESTED review's thread, not a COMMENT review's leftover thread", () => {
    const fixture = threadsFixture([
      { author: reviewApp, state: "SUBMITTED", reviewState: "COMMENTED" },
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
    ]);
    const { code, stdout, stderr } = run(fixture, "reviewer_thread");
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("CHANGES_REQUESTED");
    expect(stdout).not.toContain("COMMENTED");
  });

  test("still fails naming the count when two threads are genuinely CHANGES_REQUESTED", () => {
    const fixture = threadsFixture([
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
    ]);
    const { code, stdout, stderr } = run(fixture, "reviewer_thread");
    expect(code).not.toBe(0);
    expect(stdout + stderr).toContain("2 CHANGES_REQUESTED review threads, want exactly one");
  });

  test("picks the one thread when there is only ever a single CHANGES_REQUESTED review (the pre-merge case)", () => {
    const fixture = threadsFixture([
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
    ]);
    const { code, stdout, stderr } = run(fixture, "reviewer_thread");
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("CHANGES_REQUESTED");
  });

  test("still fails naming zero when the reviewer's only thread is a COMMENT review's", () => {
    const fixture = threadsFixture([
      { author: reviewApp, state: "SUBMITTED", reviewState: "COMMENTED" },
    ]);
    const { code, stdout, stderr } = run(fixture, "reviewer_thread");
    expect(code).not.toBe(0);
    expect(stdout + stderr).toContain("0 CHANGES_REQUESTED review threads, want exactly one");
  });

  test("a CHANGES_REQUESTED thread another account opened is never the review App's", () => {
    const fixture = threadsFixture([
      { author: "another-review", state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
    ]);
    const picked = run(fixture, "reviewer_thread");
    expect(picked.code).toBe(0);
    expect(picked.stdout).toStartWith(`T_${reviewApp}_CHANGES_REQUESTED_`);
    const none = run(
      threadsFixture([
        { author: "another-review", state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
      ]),
      "reviewer_thread"
    );
    expect(none.code).not.toBe(0);
    expect(none.stdout + none.stderr).toContain("0 CHANGES_REQUESTED review threads");
  });

  test("fails saying so when read_app_logins has not run", () => {
    const fixture = threadsFixture([
      { author: reviewApp, state: "SUBMITTED", reviewState: "CHANGES_REQUESTED" },
    ]);
    const { code, stderr } = run(fixture, "review_app_login=\nreviewer_thread");
    expect(code).not.toBe(0);
    expect(stderr).toContain("role_app: no App login for reviewer: read_app_logins has not run");
  });
});
