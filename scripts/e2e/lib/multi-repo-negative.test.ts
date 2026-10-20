import { afterEach, describe, expect, test } from "bun:test";
import {
  closeServers,
  daemonActorEvent,
  daemonStub,
  dispatchStub,
  fakeBin,
  fullInputs,
  humanActorEvent,
  isolatedPath,
  issueUpdatedEvent,
  makeStateBody,
  mutableDaemonStub,
  newFakeDir,
  noneNote,
  run,
  spawnScript,
  stateBody,
} from "./multi-repo-acceptance-harness";

// The `negative` observations of scripts/e2e/multi-repo-acceptance.sh (no-repo, pr-in-x,
// repo-edit); the other observations are in multi-repo-acceptance.test.ts. The stubs and fakes
// they run against are in multi-repo-acceptance-harness.ts.
afterEach(() => {
  closeServers();
});

describe("negative no-repo: exact daemon actor, outbox marker, resolved_repo, and the issues-type guard", () => {
  test("the exact none-note from the daemon's own actor, Dispatch still resolving to none: PASS", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK negative-no-repo-ACME-9: PASS");
  });

  test("a human message quoting the exact none-note is a FAIL: the actor must be the daemon's own session", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      { "ACME-9": JSON.stringify([humanActorEvent(`FYI the daemon said: ${noneNote}`)]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("Dispatch now resolves the issue to a real repository (a stale none-note from before): FAIL", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: "acme/gadgets" }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("the daemon's latest repository note is a different one (App not installed), not the none-note: FAIL", async () => {
    const daemonServer = daemonStub();
    const laterNote =
      "Legion did not admit this issue: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again.";
    const dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 6, `${laterNote}\n\n<!-- legion-outbox:42 -->`),
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("a state body of {} or null never satisfies `.issues has(key)` vacuously: FAIL", async () => {
    const daemonServer = daemonStub("{}");
    const dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });
});

// prsGhFake is a fake `gh` answering both calls negative_pr_in_x makes against X's pull-request
// list: the `--jq [.[].number] | max // 0` cursor read at `before`, and the raw list read at
// `after` (raw_fetch, no --jq) — both must answer the same prsJson consistently within one test.
function prsGhFake(prsJson: string): string {
  const highest = Math.max(
    0,
    ...(JSON.parse(prsJson) as Array<{ number: number }>).map((pr) => pr.number)
  );
  return `
case "$*" in
  "api repos/other/outside/pulls?state=all&per_page=100 --jq [.[].number] | max // 0") printf '${highest}' ;;
  "api repos/other/outside/pulls?state=all&per_page=100") printf '${prsJson}' ;;
  *) echo "gh: unexpected: $*" >&2; exit 1 ;;
esac`;
}

// baselineRunId pulls the minted run id out of a `before` call's stdout.
function baselineRunId(stdout: string): string {
  const runId = stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
  expect(runId).not.toBe("");
  return runId;
}

describe("negative pr-in-x: PR-number cursor and the daemon-side check", () => {
  test("before prints BASELINE, never PASS, for a recorded child", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { status, stdout } = await run(["negative", "pr-in-x", "before", "ACME-2"], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("BASELINE pr-in-x-ACME-2: captured");
    expect(stdout).not.toContain("PASS");
  });

  test("before on an unrecorded issue is refused: no recorded child, nothing to baseline", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "pr-in-x", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  test("after with an unknown run id is refused", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "nonexistent-run", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("no baseline for run");
  });

  // A run id with a path-traversal shape is refused outright, never used to open a
  // baseline file outside this run's own evidence directory.
  test("a run id containing '..' is refused as unsafe, never read as a path", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "../../etc/passwd", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a bare id");
  });

  test("after immediately (no PR opened yet) with highest-PR 0 in X: FAIL", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  // A PR opened and closed in X, with the daemon's own record also showing
  // the change (pullRequest now names X), must never PASS — the operation proof alone is not
  // enough; the daemon side must be unaffected too.
  test("a closed PR exists in X, but the daemon's own pullRequest now names X too: FAIL", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody); // ACME-2 starts as acme/widgets, no PR
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "needs_review",
            status: "todo",
            repo: "acme/widgets",
            pullRequest: { repo: "other/outside", number: 5 },
          },
        },
      })
    );
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  test("a closed PR numbered above the baseline's cursor, daemon side unaffected: PASS", async () => {
    const daemonServer = daemonStub(stateBody);
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake('[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"}]'),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"},{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
  });

  // A generation bump between before and after (a hand-over or re-admission) is a
  // foreign baseline, refused outright rather than compared across two different issue lifetimes.
  test("the issue's generation moved since the baseline: refused as foreign", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody);
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 2,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
        },
      })
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("a hand-over or re-admission since");
  });

  test("two overlapping before calls for the same issue never collide: each gets its own run id and baseline", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { bin, evidence } = newFakeDir({ gh });
    const path = isolatedPath(bin);
    const [first, second] = await Promise.all([
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
    ]);
    expect(first.status).toBe(0);
    expect(second.status).toBe(0);
    expect(baselineRunId(first.stdout)).not.toBe(baselineRunId(second.stdout));
  });
});

describe("negative repo-edit: Dispatch's seq cursor and the exact kept-repository message", () => {
  test("before refuses when Dispatch already resolves to X", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "other/outside" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("already resolves to X");
  });

  test("before on an unrecorded issue is refused", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({ "ACME-404": JSON.stringify({ key: "ACME-404" }) });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  const xTargetedNote =
    "Legion keeps acme/widgets as this issue's repository; the change to other/outside in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for other/outside instead.";

  // The repository and the message both already reflect the edit, but the
  // only post-baseline event is the daemon's own routine status write (also issue.updated) — this
  // must never satisfy the seq-cursor check, which requires payload.repo == X on that event.
  test("the only issue.updated after the baseline is a routine status write (payload.repo unset): FAIL", async () => {
    const daemonServer = daemonStub(stateBody); // ACME-2: acme/widgets
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const laterDispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, null), // a routine status write: no repo in its payload
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${laterDispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-repo-edit-ACME-2: FAIL");
  });

  test("an issue.updated after the baseline naming X, with the exact kept-repository message from the daemon: PASS", async () => {
    const daemonServer = daemonStub(stateBody);
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const laterDispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${laterDispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });

  // The operator spells X differently than Dispatch resolved and wrote it
  // (LEGION_E2E_REPO_X=Other/Outside here; Dispatch's own .repo and the daemon's note both read
  // "other/outside"). The comparisons above fold case; the note's own quoted spelling must
  // come from Dispatch's resolved spelling (dispatch_repo_now), never the operator's literal
  // LEGION_E2E_REPO_X, or this would wrongly FAIL on a mere case difference.
  test("the operator's case for X differs from Dispatch's own resolved spelling: still PASS", async () => {
    const daemonServer = daemonStub(stateBody);
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    env.LEGION_E2E_REPO_X = "Other/Outside";
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const laterDispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${laterDispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });

  // noteOnce posts the kept-repository note only once per resolution: a rerun that has no fresh
  // message must still PASS as long as the seq-bound issue.updated and the daemon's repo are
  // right, since the message check only verifies the format, never requiring a brand-new one.
  test("no fresh message after the baseline, but the seq-bound issue.updated and an already-present matching note both exist: PASS", async () => {
    const daemonServer = daemonStub(stateBody);
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const laterDispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 3, `${xTargetedNote}\n\n<!-- legion-outbox:1 -->`), // an older message, before the baseline's own last_seq
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${laterDispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });
});
