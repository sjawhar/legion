import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// request_changes_as_reviewer and reviewer_requested_changes_from, taken from lib/workflow.sh by
// name. The daemon sets aside a review-App review whose Legion footer does not name the session
// recorded on the reviewer's claim (workflow.Engine.DecidesRound), so a scripted round must leave
// the footer to the reviewer and wait for a review that carries it. The fake gh answers the
// reviews listing from GH_REVIEWS_FIXTURE through the call's own --jq filter, as gh does.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));
const reviewApp = "example-review[bot]";
const session = "01a151b4-1122-7147-a350-9e54db101525";

const dir = mkdtempSync(join(tmpdir(), "reviewer-footer-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(
  join(bin, "gh"),
  `#!/usr/bin/env bash
jq=
while [ $# -gt 0 ]; do
  case $1 in --jq) jq=$2; shift 2 ;; *) shift ;; esac
done
jq -c "$jq" "$GH_REVIEWS_FIXTURE"
`,
  { mode: 0o755 }
);

const footer = (s: string) => `<!-- legion: {"session":"${s}","phase":"review"} -->`;
type Review = { login?: string; state?: string; body: string };

let fixtures = 0;
function reviewsFixture(reviews: Review[]): string {
  const path = join(dir, `reviews-${++fixtures}.json`);
  writeFileSync(
    path,
    JSON.stringify(
      reviews.map((r, i) => ({
        id: i + 1,
        user: { login: r.login ?? reviewApp },
        state: r.state ?? "CHANGES_REQUESTED",
        body: r.body,
      }))
    )
  );
  return path;
}

function run(fixture: string, call: string, args: { body?: string; note?: string } = {}) {
  const sent = join(dir, `sent-${fixtures}-${Math.random().toString(36).slice(2)}`);
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
repo=sjawhar/legion-smoke pr_number=614
implement_app_login='example-implement[bot]' review_app_login='${reviewApp}'
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s\\n' "$*" >&2; exit 1; }
issue_worker_session() { printf '%s\\n' "\${REVIEWER_SESSION}"; }
send_agent() { printf '%s' "$3" >"$SENT"; }
until_true() { shift 2; "$@"; }
${fn("role_app")}
${fn("request_changes_as_reviewer")}
${fn("reviewer_requested_changes_from")}
${call}
`,
    ],
    {
      env: {
        PATH: `${bin}:${process.env.PATH}`,
        GH_REVIEWS_FIXTURE: fixture,
        REVIEWER_SESSION: session,
        SENT: sent,
        BODY: args.body ?? "",
        NOTE: args.note ?? "",
      },
    }
  );
  let message = "";
  try {
    message = readFileSync(sent, "utf8");
  } catch {}
  return {
    code: result.exitCode,
    stdout: result.stdout.toString().trim(),
    stderr: result.stderr.toString().trim(),
    message,
  };
}

const roundBody = "Stage 3 proof review, round 2: append the line `x` to the end of the file.";
const decided = (round: number, s: string, reviews: Review[]) =>
  run(reviewsFixture(reviews), `reviewer_requested_changes_from ${round} '${s}'`).code === 0;

describe("reviewer_requested_changes_from", () => {
  test("holds when the newest changes-requested review's footer names the reviewer's session", () => {
    expect(
      decided(2, session, [
        { body: "round 1, before the session was recorded" },
        { body: `${roundBody}\n\n${footer(session)}` },
      ])
    ).toBe(true);
  });

  test("does not hold for a review with no footer, the one the daemon set aside", () => {
    expect(
      decided(2, session, [{ body: `round 1\n\n${footer(session)}` }, { body: roundBody }])
    ).toBe(false);
  });

  test("does not hold for a footer naming another session", () => {
    expect(
      decided(1, session, [{ body: `${roundBody}\n\n${footer("01a151ae-another-session")}` }])
    ).toBe(false);
  });

  test("reads the last footer, as the daemon does, so a quoted earlier one is not the review's", () => {
    expect(
      decided(1, session, [
        { body: `quoting: ${footer("01a151ae-another-session")}\n\n${footer(session)}` },
      ])
    ).toBe(true);
    expect(
      decided(1, session, [
        { body: `${footer(session)}\n\nquoting: ${footer("01a151ae-another-session")}` },
      ])
    ).toBe(false);
  });

  test("does not hold before the round's count of changes-requested reviews", () => {
    expect(decided(2, session, [{ body: `${roundBody}\n\n${footer(session)}` }])).toBe(false);
  });

  test("counts only the review App's changes-requested reviews", () => {
    expect(
      decided(1, session, [
        { body: `${roundBody}\n\n${footer(session)}`, login: "someone-else" },
        { body: `${roundBody}\n\n${footer(session)}`, state: "COMMENTED" },
      ])
    ).toBe(false);
  });

  test("a footer with no closing marker or unparseable JSON names no session", () => {
    expect(
      decided(1, session, [{ body: `${roundBody}\n\n<!-- legion: {"session":"${session}"}` }])
    ).toBe(false);
    expect(
      decided(1, session, [{ body: `${roundBody}\n\n<!-- legion: {"session":"${session}" -->` }])
    ).toBe(false);
  });
});

describe("request_changes_as_reviewer", () => {
  test("leaves the footer to the reviewer: the body is quoted, never the whole review", () => {
    const fixture = reviewsFixture([{ body: `${roundBody}\n\n${footer(session)}` }]);
    const { code, stderr, message } = run(fixture, `request_changes_as_reviewer LEG-1 1 "$BODY"`, {
      body: roundBody,
    });
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(message).not.toContain("exactly this body");
    expect(message).toContain(
      `submitted as ${reviewApp}. The review's body is this quoted text, then the Legion footer naming your own session that your role appends to every review: "${roundBody}" Take the round's steps`
    );
  });

  test("puts a round's note outside the quoted body", () => {
    const fixture = reviewsFixture([{ body: `${roundBody}\n\n${footer(session)}` }]);
    const note = "Reply on the bot's thread in your own words.";
    const { code, message } = run(fixture, `request_changes_as_reviewer LEG-1 1 "$BODY" "$NOTE"`, {
      body: roundBody,
      note,
    });
    expect(code).toBe(0);
    expect(message).toContain(`"${roundBody}" ${note} Take the round's steps`);
  });

  test("fails when the review the reviewer posted carries no footer", () => {
    const fixture = reviewsFixture([{ body: roundBody }]);
    const { code } = run(fixture, `request_changes_as_reviewer LEG-1 1 "$BODY"`, {
      body: roundBody,
    });
    expect(code).not.toBe(0);
  });

  test("fails naming the reviewer when the daemon's state has no session for it", () => {
    const fixture = reviewsFixture([]);
    const { code, stderr } = run(
      fixture,
      `REVIEWER_SESSION= request_changes_as_reviewer LEG-1 1 "$BODY"`,
      { body: roundBody }
    );
    expect(code).not.toBe(0);
    expect(stderr).toContain("FAIL the daemon's state names no session for LEG-1's reviewer");
  });
});
