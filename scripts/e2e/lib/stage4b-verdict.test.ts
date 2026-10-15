import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's own blocked, fail, pass, until_reached, cleanup, audit_verdict and audit_failure,
// taken from the script by name and run after the controller checkpoint ends (blocked, failed, or
// passed as a STAGE4B_UNTIL run's last checkpoint), or after done fails while it cleans the smoke
// main, with the teardown's cluster, NATS and GitHub helpers stubbed and production_audit's
// Dispatch read replaced by its result: a write outside LEGSMOKE, or none. A timed-out wait runs
// the script's own capacity hook and limit_pending against a kubectl that answers pods (filtered by
// the -l selector, as the API server does) and FailedScheduling events from the test's fixture.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const rigFn = scriptFunctions(join(import.meta.dir, "rig.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-verdict-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(join(bin, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
writeFileSync(
  join(bin, "kubectl"),
  `#!/bin/bash
selector=
args=("$@")
for ((i = 0; i < \${#args[@]}; i++)); do [ "\${args[i]}" = -l ] && selector=\${args[i + 1]}; done
case " $* " in
  *" get pods "*) jq -c --arg sel "$selector" '{items: [.items[] | select(.metadata.labels as $l | $sel | split(",") | map(split("=")) | all(.[]; $l[.[0]] == .[1]))]}' <<<"$PODS_JSON" ;;
  *" get events "*) printf '%s\\n' "$EVENTS_JSON" ;;
  *) exit 1 ;;
esac
`,
  { mode: 0o755 }
);

// pendingPod is a pod of tree TREE (run label x), its own root issue, Pending and Unschedulable
// since SINCE.
const pendingPod = (name: string, uid: string, tree: string, since: string) => ({
  metadata: {
    name,
    uid,
    labels: { "legion.dev/project": "x", "legion.dev/tree": tree, "legion.dev/issue": tree },
  },
  status: {
    phase: "Pending",
    conditions: [
      {
        type: "PodScheduled",
        status: "False",
        reason: "Unschedulable",
        message: "0/16 nodes are available: 16 node(s) didn't match Pod's node affinity/selector.",
        lastTransitionTime: since,
      },
    ],
  },
});
// podEvent is an event REASON from COMPONENT for pod NAME with UID, first observed FIRST_AT
// (default AT, a single-observation event) and last AT: Karpenter folds a repeat within its
// DedupeTimeout into one Event whose last observation keeps moving, firstTimestamp fixed.
const podEvent =
  (component: string, reason: string, message: string) =>
  (name: string, uid: string, at: string, firstAt: string = at) => ({
    involvedObject: { kind: "Pod", name, uid },
    source: { component },
    reason,
    firstTimestamp: firstAt,
    lastTimestamp: at,
    message,
  });
const limitMessage =
  'Failed to schedule pod, incompatible with nodepool "other"; all available instance types exceed limits for nodepool (NodePool=legion)';
// limitEvent is Karpenter's verdict that every instance type exceeds the legion pool's limits.
const limitEvent = podEvent("karpenter", "FailedScheduling", limitMessage);
// affinityEvent is Karpenter's verdict for a genuine scheduling reason other than the pool's
// limits (a topology constraint; no Legion pod carries one, LEGION-632, so it stands for any such
// reason).
const affinityEvent = podEvent(
  "karpenter",
  "FailedScheduling",
  "Failed to schedule pod, unsatisfiable topology constraint for pod affinity, key=kubernetes.io/hostname"
);
// nominatedEvent is Karpenter's word once room frees: it expects the pod to schedule.
const nominatedEvent = podEvent(
  "karpenter",
  "Nominated",
  "Pod should schedule on: nodeclaim/legion-abcde"
);
// schedulerEvent is the default scheduler's FailedScheduling, which every Pending pod gets.
const schedulerMessage =
  "0/17 nodes are available: 17 node(s) didn't match Pod's node affinity/selector.";
const schedulerEvent = podEvent("default-scheduler", "FailedScheduling", schedulerMessage);
// watchPod is a pod-watch.json entry for pod NAME (uid UID, issue ISSUE, carrying ISSUE as both
// its legion.dev/issue and legion.dev/tree label) with a single PodScheduled condition — STATUS,
// REASON (only for False) — that last transitioned AT, deleted at DELETED_AT when given (an
// earlier, already-abandoned incarnation of the issue's claim).
const watchPod = (
  name: string,
  uid: string,
  issue: string,
  status: "True" | "False",
  reason: string | undefined,
  at: string,
  deletedAt?: string
) => ({
  object: {
    kind: "Pod",
    metadata: {
      name,
      uid,
      labels: { "legion.dev/project": "x", "legion.dev/issue": issue, "legion.dev/tree": issue },
      ...(deletedAt ? { deletionTimestamp: deletedAt } : {}),
    },
    status: {
      conditions: [
        { type: "PodScheduled", status, ...(reason ? { reason } : {}), lastTransitionTime: at },
      ],
    },
  },
});
// claimFailed is a supervise: claim failed daemon-log line for ISSUE's ROLE, dated AT, with the
// one reason limit_pending_exhausted reads (launch failures ran out), the run label fixed at x.
const claimFailed = (issue: string, role: string, at: string) => ({
  msg: "supervise: claim failed",
  claim: `legion-x-${issue.toLowerCase()}-${role}`,
  why: "launch failures ran out",
  time: at,
});
interface Capacity {
  subject?: string;
  pods?: object[];
  events?: object[];
  watch?: object[];
  daemonLog?: object[];
}

let runs = 0;
const outsideWrite =
  '[{"issue":"OTHER-12","seq":3,"type":"comment.created","actor":"legion-daemon:LEGSMOKE"}]';
// controllerRun ends the controller checkpoint in blocked, in fail (the case the notes are for), in
// pass as the last checkpoint of a run with STAGE4B_UNTIL=controller, or in a wait that times out
// under the run's capacity hook, on_tree's subject the capacity's (none: a plain until_true), with
// the cluster answering the capacity's pods and events.
function controllerRun(
  outside: string,
  ending: "blocked" | "fail" | "pass" | "timeout" = "blocked",
  capacity: Capacity = {}
) {
  return verdictRun(
    outside,
    ending !== "timeout"
      ? `${ending} "the controller's model route could not be installed"`
      : `timeout_hook=limit_pending_blocked; ${capacity.subject === undefined ? "" : `on_tree ${capacity.subject} `}until_true 1 "the controller to take its first turn" false`,
    capacity
  );
}
// cleaningRun fails done while it cleans the smoke main, as clean_smoke_main's timed-out wait does;
// with a violation, a production guard recorded it first.
function cleaningRun(outside: string, violation = false) {
  return verdictRun(
    outside,
    `check=done check_started=2026-09-30T13:00:00Z repo=sjawhar/legion-smoke smoke_main_cleaning=1
${violation ? `printf 'pod x (uid u1): runtimeClassName is runc\\n' >"$evidence/pane-endpoint-violation.txt"` : ""}
fail "timed out after 600s (240 polls) waiting for sjawhar/legion-smoke#600 to pass its required checks"`
  );
}
// verdictRun runs the tail after the checkpoint's variables, then the trap.
function verdictRun(outside: string, tail: string, capacity: Capacity = {}) {
  const run = join(dir, `run-${++runs}`);
  const evidence = join(run, "evidence");
  mkdirSync(evidence, { recursive: true });
  writeFileSync(
    join(evidence, "pod-watch.json"),
    (capacity.watch ?? []).map((o) => JSON.stringify(o)).join("\n")
  );
  writeFileSync(
    join(evidence, "daemon.log"),
    (capacity.daemonLog ?? []).map((o) => JSON.stringify(o)).join("\n")
  );
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(join(import.meta.dir, "..", "..", ".."))}
work=${JSON.stringify(join(run, "work"))} evidence=${JSON.stringify(evidence)}
check=controller check_started=2026-09-30T12:00:00Z daemon_log=${JSON.stringify(join(evidence, "daemon.log"))}
ok= was_blocked= until=controller locked=1 compared= snapshotted= audited= prod_baseline=2026-09-30T11:00:00.000000000Z
tree1= tree2= tree3= tree4= pair_session= shape_pid= daemon_pid= watch_pid= events_pid= leaks_pid= sampler_pid= interests_pid= pg_container=none project=LEGSMOKE run_label=x smoke_main_cleaning=
negative_config_pod= negative_config_workspace= block_tree=
operator=production namespace=legion capacity_subject=
mkdir -p "$work" "$evidence/model-gateway"
# The controller starved during the checkpoint: a blocked checkpoint's notes, which must not print,
# would list it, and a failed checkpoint's do.
printf '%s\\t%s\\t%s\\t%s\\t%s\\t%s\\n' 2026-09-30T12:00:05Z 4242 /x controller timeout no-key >"$evidence/model-gateway/hawk-token.calls"
exec 7>&1
stop_tree() { :; }; stop_pid() { :; }; collect_transcripts() { :; }; record_pair() { :; }; op() { :; }
teardown() { :; }; namespace_clean() { :; }; delete_consumers() { :; }; remove_run_branches() { :; }
run_processes() { :; }; note() { echo "   $*"; }
production_audit() {
  audited=1
  printf '%s\\n' "$OUTSIDE" >"$evidence/production-issues-touched-outside.json"
  printf '[]\\n' >"$evidence/production-interests-outside.json"
  audit_verdict "$evidence/production-issues-touched-outside.json" "$evidence/production-interests-outside.json"
}
${fn("audit_verdict")}
${fn("audit_failure")}
${fn("blocked")}
${fn("fail")}
${fn("until_reached")}
${fn("pass")}
${fn("limit_pending")}
${fn("limit_pending_exhausted")}
${fn("on_subject")}
${fn("on_tree")}
${fn("limit_pending_blocked")}
${rigFn("until_true")}
${fn("cleanup")}
trap cleanup EXIT
${tail}
`,
    ],
    {
      env: {
        PATH: `${bin}:${process.env.PATH}`,
        OUTSIDE: outside,
        PODS_JSON: JSON.stringify({ items: capacity.pods ?? [] }),
        EVENTS_JSON: JSON.stringify({ items: capacity.events ?? [] }),
      },
    }
  );
  const stdout = result.stdout.toString();
  // cleanup's ERR trap reports every teardown command that fails, a helper this harness neither
  // takes nor stubs included (exit 127).
  expect(stdout).not.toContain("cleanup warning:");
  return { code: result.exitCode, stdout };
}

describe("stage 4b's verdict line", () => {
  test("says BLOCKED for a checkpoint that could not run, when the teardown's checks pass", () => {
    const clean = controllerRun("[]");
    expect(clean.code).toBe(1);
    expect(clean.stdout).toContain("stage 4b e2e: BLOCKED (check controller)");
    expect(clean.stdout).not.toContain("model-gateway-unserved:");
  });

  test("never says BLOCKED once the teardown's production audit finds a write outside LEGSMOKE", () => {
    const outside = controllerRun(outsideWrite);
    expect(outside.code).toBe(1);
    // The audit is the one check whose failure the run's verdict must carry.
    expect(outside.stdout).toContain("stage 4b e2e: FAIL");
    expect(outside.stdout).not.toContain("stage 4b e2e: BLOCKED");
    // The transcript says why: the audit's own failure line, the verdict naming it rather than the
    // checkpoint that could not run, and no model-key notes for a checkpoint that never failed.
    expect(outside.stdout).toContain(
      "CHECK production-audit: FAIL: the run wrote outside LEGSMOKE or subscribed outside it:"
    );
    expect(outside.stdout).toContain(
      "stage 4b e2e: FAIL (check production-audit, in the teardown after check controller)"
    );
    expect(outside.stdout).not.toContain("model-gateway-unserved:");
  });

  test("prints the notes for a checkpoint that failed itself, so the seeded starve is read", () => {
    const failed = controllerRun("[]", "fail");
    expect(failed.code).toBe(1);
    expect(failed.stdout).toContain(
      "model-gateway-unserved: 1 agent(s) may have failed check controller for want of a model key"
    );
    expect(failed.stdout).toContain("stage 4b e2e: FAIL (check controller)");
  });

  test("ends a STAGE4B_UNTIL run FAIL, naming the audit, once the teardown's audit finds a write outside LEGSMOKE", () => {
    const until = controllerRun(outsideWrite, "pass");
    expect(until.code).toBe(1);
    expect(until.stdout).toContain(
      "stage 4b e2e: development run until controller finished (not the proof)"
    );
    // The development run's own line is not the last word: the audit's failure and the verdict
    // naming it follow, and the checkpoint, which passed, gets no notes.
    expect(until.stdout).toContain(
      "CHECK production-audit: FAIL: the run wrote outside LEGSMOKE or subscribed outside it:"
    );
    expect(until.stdout).toContain(
      "stage 4b e2e: FAIL (check production-audit, in the teardown after check controller)"
    );
    expect(until.stdout).not.toContain("model-gateway-unserved:");
  });

  test("sets deployment-pod-block's root done once the daemon has stopped, never before", () => {
    // The live daemon sets back a status an outside session writes on a root it runs, so the
    // teardown writes it after stop_pid has stopped the daemon.
    const r = verdictRun(
      "[]",
      `stop_pid() { echo "stop_pid \${1:-none}"; }
set_status() { echo "set_status $*"; }
daemon_pid=4242 block_tree=LEGSMOKE-9
pass`
    );
    const stopped = r.stdout.indexOf("stop_pid 4242");
    const closed = r.stdout.indexOf("set_status LEGSMOKE-9 done");
    expect(stopped).toBeGreaterThanOrEqual(0);
    expect(closed).toBeGreaterThan(stopped);
  });

  test("writes no status for a run that never reached deployment-pod-block", () => {
    const r = verdictRun("[]", `set_status() { echo "set_status $*"; }\npass`);
    expect(r.stdout).not.toContain("set_status");
  });

  const starvedPod = pendingPod("legion-x-t1", "uid-now", "T1", "2026-10-02T10:00:00Z");
  for (const [name, events, scheduler] of [
    [
      "Karpenter's limit verdict",
      [limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z")],
      "none",
    ],
    [
      "Karpenter's limit verdict, with the default scheduler's own event newer",
      [
        limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z"),
        schedulerEvent("legion-x-t1", "uid-now", "2026-10-02T10:05:00Z"),
      ],
      JSON.stringify(schedulerMessage),
    ],
  ] as [string, object[], string][]) {
    test(`says BLOCKED on capacity for a wait its own tree's pod starved, on ${name}, with its evidence`, () => {
      const starved = controllerRun("[]", "timeout", { subject: "T1", pods: [starvedPod], events });
      expect(starved.code).toBe(1);
      // The line carries the limit event's time and message, the pod's PodScheduled condition and
      // the default scheduler's newest word, so a misclassification can be read off it.
      expect(starved.stdout).toContain(
        `CHECK controller: BLOCKED: capacity: the legion pool is at its limits, so the scheduler cannot place pod legion-x-t1 (uid uid-now): Karpenter at 2026-10-02T10:00:05Z: ${JSON.stringify(limitMessage)}; PodScheduled Unschedulable since 2026-10-02T10:00:00Z: ${JSON.stringify("0/16 nodes are available: 16 node(s) didn't match Pod's node affinity/selector.")}; default scheduler: ${scheduler}`
      );
      expect(starved.stdout).toContain("stage 4b e2e: BLOCKED (check controller)");
      expect(starved.stdout).not.toContain("CHECK controller: FAIL");
    });
  }

  // Each of these timed out with a limit event in the cluster that is no evidence about the wait's
  // own pod: it must fail as any timeout does.
  for (const [name, capacity] of [
    [
      "an event for an earlier pod of the same name (another uid)",
      {
        subject: "T1",
        pods: [starvedPod],
        events: [limitEvent("legion-x-t1", "uid-before", "2026-10-02T10:00:05Z")],
      },
    ],
    [
      "an event older than the pod's Unschedulable transition",
      {
        subject: "T1",
        pods: [starvedPod],
        events: [limitEvent("legion-x-t1", "uid-now", "2026-10-02T09:59:59Z")],
      },
    ],
    [
      "a limit-Pending pod of another tree than the wait's",
      {
        subject: "T1",
        pods: [pendingPod("legion-x-t2", "uid-t2", "T2", "2026-10-02T10:00:00Z")],
        events: [limitEvent("legion-x-t2", "uid-t2", "2026-10-02T10:00:05Z")],
      },
    ],
    [
      "a wait with no subject",
      {
        pods: [starvedPod],
        events: [limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z")],
      },
    ],
    [
      "a limit verdict Karpenter has since replaced with a genuine reason",
      {
        subject: "T1",
        pods: [starvedPod],
        events: [
          limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z"),
          affinityEvent("legion-x-t1", "uid-now", "2026-10-02T10:05:00Z"),
        ],
      },
    ],
    [
      "a limit verdict Karpenter has since replaced with a nomination",
      {
        subject: "T1",
        pods: [starvedPod],
        events: [
          limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z"),
          nominatedEvent("legion-x-t1", "uid-now", "2026-10-02T10:05:00Z"),
        ],
      },
    ],
    [
      "a pod starved before an earlier check began, its claim long since failed",
      {
        subject: "T1",
        watch: [
          watchPod(
            "legion-x-t1",
            "uid-earlier",
            "T1",
            "False",
            "Unschedulable",
            "2026-09-30T11:00:00Z"
          ),
        ],
        daemonLog: [claimFailed("T1", "implementer", "2026-09-30T11:30:00Z")],
        events: [limitEvent("legion-x-t1", "uid-earlier", "2026-09-30T11:30:05Z")],
      },
    ],
    [
      "an earlier incarnation of the issue's pod deleted before the claim failed",
      {
        subject: "T1",
        watch: [
          watchPod(
            "legion-x-t1",
            "uid-deleted-earlier",
            "T1",
            "False",
            "Unschedulable",
            "2026-09-30T11:50:00Z",
            "2026-09-30T12:02:00Z"
          ),
        ],
        daemonLog: [claimFailed("T1", "implementer", "2026-09-30T12:05:00Z")],
        events: [limitEvent("legion-x-t1", "uid-deleted-earlier", "2026-09-30T11:50:05Z")],
      },
    ],
    ["no pod Pending and no claim failed on the pool's limits", { subject: "T1" }],
  ] as [string, Capacity][]) {
    test(`still fails a wait that timed out with ${name}`, () => {
      const timedOut = controllerRun("[]", "timeout", capacity);
      expect(timedOut.code).toBe(1);
      expect(timedOut.stdout).toContain("CHECK controller: FAIL: timed out after 1s");
      expect(timedOut.stdout).toContain("stage 4b e2e: FAIL (check controller)");
      expect(timedOut.stdout).not.toContain("BLOCKED");
    });
  }

  test("says BLOCKED on capacity for a pod the scheduler placed only after its claim's launch failures ran out", () => {
    const exhausted = controllerRun("[]", "timeout", {
      subject: "T1",
      watch: [
        watchPod(
          "legion-x-t1",
          "uid-exhausted",
          "T1",
          "False",
          "Unschedulable",
          "2026-09-30T11:55:00Z"
        ),
        watchPod("legion-x-t1", "uid-exhausted", "T1", "True", undefined, "2026-10-02T10:10:00Z"),
      ],
      daemonLog: [claimFailed("T1", "implementer", "2026-09-30T12:00:30Z")],
      events: [limitEvent("legion-x-t1", "uid-exhausted", "2026-09-30T12:00:05Z")],
    });
    expect(exhausted.code).toBe(1);
    expect(exhausted.stdout).toContain(
      `CHECK controller: BLOCKED: capacity: the legion pool is at its limits, so the scheduler cannot place pod uid uid-exhausted, issue T1, unschedulable before its claims launch failures ran out: Karpenter at 2026-09-30T12:00:05Z: ${JSON.stringify(limitMessage)}`
    );
    expect(exhausted.stdout).toContain("stage 4b e2e: BLOCKED (check controller)");
    expect(exhausted.stdout).not.toContain("CHECK controller: FAIL");
  });

  test("says BLOCKED on capacity for a Karpenter limit event Karpenter re-emitted past the failure", () => {
    // Karpenter re-emits the same event on its own DedupeTimeout (5 minutes) while a pod stays
    // Pending, and the API server folds the repeats into one Event whose last observation keeps
    // moving forward; only its first observation, fixed at the event's creation, says whether it
    // was already true by the time the claim failed.
    const redone = controllerRun("[]", "timeout", {
      subject: "T1",
      watch: [
        watchPod(
          "legion-x-t1",
          "uid-redone",
          "T1",
          "False",
          "Unschedulable",
          "2026-09-30T11:55:00Z"
        ),
        watchPod("legion-x-t1", "uid-redone", "T1", "True", undefined, "2026-10-02T10:10:00Z"),
      ],
      daemonLog: [claimFailed("T1", "implementer", "2026-09-30T12:00:30Z")],
      events: [
        limitEvent("legion-x-t1", "uid-redone", "2026-09-30T12:10:00Z", "2026-09-30T12:00:05Z"),
      ],
    });
    expect(redone.code).toBe(1);
    expect(redone.stdout).toContain(
      `CHECK controller: BLOCKED: capacity: the legion pool is at its limits, so the scheduler cannot place pod uid uid-redone, issue T1, unschedulable before its claims launch failures ran out: Karpenter at 2026-09-30T12:10:00Z: ${JSON.stringify(limitMessage)}`
    );
    expect(redone.stdout).toContain("stage 4b e2e: BLOCKED (check controller)");
    expect(redone.stdout).not.toContain("CHECK controller: FAIL");
  });
});

// interests_outside is tested directly, by value: it is a pure jq filter over a fixture of
// sampled topics and the run's own pull request numbers, with no cluster or GitHub read of its
// own (those are run_pull_request_numbers' job, a live gh call this harness does not stub).
describe("interests_outside", () => {
  function runInterestsOutside(topics: object[], prs: number[]) {
    const sub = mkdtempSync(join(tmpdir(), "interests-outside-test."));
    const file = join(sub, "interests.jsonl");
    writeFileSync(file, topics.map((t) => JSON.stringify(t)).join("\n"));
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
project=LEGSMOKE run_label=legsmoke repo=sjawhar/legion-smoke optree=S4BOP-999 opchild=S4BOP-9991
${fn("interests_outside")}
interests_outside ${JSON.stringify(file)} ${JSON.stringify(JSON.stringify(prs))}
`,
    ]);
    rmSync(sub, { recursive: true, force: true });
    return { code: result.exitCode, stdout: result.stdout.toString() };
  }

  test("accepts a GitHub subject naming one of the run's own pull requests", () => {
    const r = runInterestsOutside(
      [{ session_id: "s1", topics: ["notifications.github.sjawhar.legion-smoke.pr.42"] }],
      [42]
    );
    expect(r.code).toBe(0);
    expect(JSON.parse(r.stdout)).toEqual([]);
  });

  test("catches a GitHub subject naming a pull request outside the run's own, on the same repository (another lane's)", () => {
    const r = runInterestsOutside(
      [{ session_id: "s1", topics: ["notifications.github.sjawhar.legion-smoke.pr.99"] }],
      [42]
    );
    expect(r.code).toBe(0);
    expect(JSON.parse(r.stdout)).toEqual([
      { session: "s1", topic: "notifications.github.sjawhar.legion-smoke.pr.99" },
    ]);
  });

  test("accepts a review subject on one of the run's own pull requests", () => {
    const r = runInterestsOutside(
      [{ session_id: "s1", topics: ["notifications.github.sjawhar.legion-smoke.pr.42.review"] }],
      [42]
    );
    expect(r.code).toBe(0);
    expect(JSON.parse(r.stdout)).toEqual([]);
  });

  test("accepts the operator-close child issue's own topic ($opchild)", () => {
    const r = runInterestsOutside(
      [{ session_id: "s1", topics: ["notifications.dispatch.issue.S4BOP-9991-comment"] }],
      []
    );
    expect(r.code).toBe(0);
    expect(JSON.parse(r.stdout)).toEqual([]);
  });
});

// shape_checked_blocked is shape_checked's own timeout_hook, tested directly: live_claims stubbed
// to return specific issue/podUid pairs, pods-checked.txt holding whichever the watcher has
// already seen, and the kubectl stub answering limit_pending/limit_pending_exhausted's cluster
// reads (the same stub stage 4b's verdict line tests use).
describe("shape_checked_blocked", () => {
  function runShapeCheckedBlocked(claims: object[], checkedUids: string[], capacity: Capacity) {
    const run = join(dir, `shape-${++runs}`);
    const evidence = join(run, "evidence");
    mkdirSync(evidence, { recursive: true });
    writeFileSync(
      join(evidence, "pods-checked.txt"),
      checkedUids.map((u) => `pod-name  ${u}  2026-01-01T00:00:00.000Z`).join("\n")
    );
    writeFileSync(
      join(evidence, "pod-watch.json"),
      (capacity.watch ?? []).map((o) => JSON.stringify(o)).join("\n")
    );
    writeFileSync(
      join(evidence, "daemon.log"),
      (capacity.daemonLog ?? []).map((o) => JSON.stringify(o)).join("\n")
    );
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -Eeuo pipefail
work=${JSON.stringify(join(run, "work"))} evidence=${JSON.stringify(evidence)}
mkdir -p "$work"
run_label=x operator=production namespace=legion check=shapetest check_started=2026-09-30T12:00:00Z
daemon_log=${JSON.stringify(join(evidence, "daemon.log"))}
live_claims() { printf '%s\\n' ${JSON.stringify(JSON.stringify(claims))}; }
${fn("blocked")}
${fn("limit_pending")}
${fn("limit_pending_exhausted")}
${fn("shape_checked_blocked")}
shape_checked_blocked
`,
      ],
      {
        env: {
          PATH: `${bin}:${process.env.PATH}`,
          PODS_JSON: JSON.stringify({ items: capacity.pods ?? [] }),
          EVENTS_JSON: JSON.stringify({ items: capacity.events ?? [] }),
        },
      }
    );
    return { code: result.exitCode, stdout: result.stdout.toString() };
  }

  test("ignores a Pending pod of the run that shape_checked is not waiting for", () => {
    const r = runShapeCheckedBlocked(
      [
        { issue: "T1", podUid: "uid-checked" },
        { issue: "T2", podUid: "uid-waiting" },
      ],
      ["uid-checked"],
      {
        pods: [pendingPod("legion-x-t3", "uid-unrelated", "T3", "2026-09-30T11:00:00Z")],
        events: [limitEvent("legion-x-t3", "uid-unrelated", "2026-09-30T11:00:05Z")],
      }
    );
    expect(r.code).toBe(0);
    expect(r.stdout).not.toContain("BLOCKED");
  });

  test("ends BLOCKED when the pod shape_checked is still waiting for is itself starved", () => {
    const r = runShapeCheckedBlocked(
      [
        { issue: "T1", podUid: "uid-checked" },
        { issue: "T2", podUid: "uid-waiting" },
      ],
      ["uid-checked"],
      {
        pods: [pendingPod("legion-x-t2", "uid-waiting", "T2", "2026-09-30T11:00:00Z")],
        events: [limitEvent("legion-x-t2", "uid-waiting", "2026-09-30T11:00:05Z")],
      }
    );
    expect(r.code).toBe(1);
    expect(r.stdout).toContain("CHECK shapetest: BLOCKED: capacity:");
    expect(r.stdout).toContain("uid-waiting");
  });
});

// The issue-independence checkpoint reads whether tree 2's planner held from on_tree's exit status
// (`if left=$(on_tree "$tree2" left_planning "$tree2")`), so the wrapper must give back the
// command's status and not its own capacity_subject restore, which would report every tree as one
// that left planning, with an empty reason.
describe("stage 4b's tree-2 planner hold", () => {
  const phaseChanged = (issue: string, from: string, to: string, time: string) => ({
    time,
    level: "INFO",
    msg: "workflow: phase changed",
    issue,
    tree: issue,
    from,
    to,
  });
  // holdVerdict is what the checkpoint concludes from a daemon log holding LINES.
  const holdVerdict = (lines: object[]) => {
    const log = join(dir, `daemon-${++runs}.log`);
    writeFileSync(log, `${lines.map((line) => JSON.stringify(line)).join("\n")}\n`);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
daemon_log=${JSON.stringify(log)} capacity_subject=
${fn("log_lines")}
${fn("left_planning")}
${fn("on_subject")}
${fn("on_tree")}
if left=$(on_tree T2 left_planning T2); then echo "left planning ($left)"; else echo held; fi
`,
    ]);
    expect(result.exitCode).toBe(0);
    return result.stdout.toString().trim();
  };

  test("is held for a planner the daemon never took out of planning", () => {
    expect(holdVerdict([phaseChanged("T2", "admitted", "planning", "2026-10-04T18:47:12Z")])).toBe(
      "held"
    );
  });

  test("names the daemon's own line for a planner that left planning", () => {
    expect(
      holdVerdict([
        phaseChanged("T2", "admitted", "planning", "2026-10-04T18:47:12Z"),
        phaseChanged("T2", "planning", "implementing", "2026-10-04T19:02:06Z"),
      ])
    ).toBe('left planning ({"time":"2026-10-04T19:02:06Z","to":"implementing"})');
  });
  test("names a failure while done cleans the smoke main as the fixture teardown's, when the teardown is clean", () => {
    const cleaning = cleaningRun("[]");
    expect(cleaning.code).toBe(1);
    expect(cleaning.stdout).toContain(
      "stage 4b e2e: FAIL (fixture teardown, in check done): every checkpoint before done passed, and done failed only at the cleanup of sjawhar/legion-smoke main"
    );
    expect(cleaning.stdout).not.toContain("stage 4b e2e: FAIL (check done)");
  });

  test("keeps the ordinary verdict for a failure while done cleans the smoke main once the teardown's audit finds a write outside LEGSMOKE", () => {
    const outside = cleaningRun(outsideWrite);
    expect(outside.code).toBe(1);
    expect(outside.stdout).toContain(
      "CHECK production-audit: FAIL: the run wrote outside LEGSMOKE or subscribed outside it:"
    );
    expect(outside.stdout).toContain("stage 4b e2e: FAIL (check done)");
    expect(outside.stdout).not.toContain("fixture teardown");
  });

  test("keeps the ordinary verdict for a failure while done cleans the smoke main once a production guard recorded a violation", () => {
    const violated = cleaningRun("[]", true);
    expect(violated.code).toBe(1);
    expect(violated.stdout).toContain("stage 4b e2e: FAIL (check done)");
    expect(violated.stdout).not.toContain("fixture teardown");
  });
});

// carried-child judges child 3's hand-over write ridden out through a Dispatch that predates
// labels_add (handover_ridden_out): the outbox retried it at least twice on that field's refusal and
// never dropped it as refused for good. The checkpoint's control appends one fabricated drop to the
// live log and needs the verdict to fail; these rows hold the verdict to that, and to counting only
// the issue's own write.
describe("stage 4b's hand-over verdict", () => {
  const refused = (issue: string, row: number) => ({
    time: "2026-10-11T12:00:00Z",
    level: "ERROR",
    msg: "outbox row failed",
    row,
    kind: "dispatch_status",
    error: `hand Dispatch issue ${issue} to Legion as todo: INVALID_JSON: invalid JSON body: json: unknown field "labels_add"`,
  });
  const dropped = (issue: string) => ({
    time: "2026-10-11T12:01:00Z",
    level: "ERROR",
    msg: "outbox status write refused by Dispatch and dropped; the issue's later writes go on",
    row: 7,
    issue,
    status: 400,
    code: "INVALID_JSON",
    error: "fabricated by the driver",
  });
  // ridden is the verdict on a daemon log holding LINES, for issue LEGSMOKE-13.
  const ridden = (lines: object[]) => {
    const log = join(dir, `daemon-${++runs}.log`);
    writeFileSync(log, `${lines.map((line) => JSON.stringify(line)).join("\n")}\n`);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
${fn("handover_refusals")}
${fn("handover_drops")}
${fn("handover_ridden_out")}
if handover_ridden_out ${JSON.stringify(log)} LEGSMOKE-13; then echo ridden-out; else echo not; fi
`,
    ]);
    expect(result.exitCode).toBe(0);
    return result.stdout.toString().trim();
  };

  test("passes a write refused twice for labels_add and never dropped", () => {
    expect(ridden([refused("LEGSMOKE-13", 7), refused("LEGSMOKE-13", 7)])).toBe("ridden-out");
  });

  test("fails the same log with one fabricated drop of the write", () => {
    expect(
      ridden([refused("LEGSMOKE-13", 7), refused("LEGSMOKE-13", 7), dropped("LEGSMOKE-13")])
    ).toBe("not");
  });

  test("fails a write refused only once", () => {
    expect(ridden([refused("LEGSMOKE-13", 7)])).toBe("not");
  });

  test("counts only the issue's own write, not another issue's or a key it prefixes", () => {
    expect(ridden([refused("LEGSMOKE-130", 8), refused("LEGSMOKE-1", 9)])).toBe("not");
    expect(
      ridden([refused("LEGSMOKE-13", 7), refused("LEGSMOKE-13", 7), dropped("LEGSMOKE-130")])
    ).toBe("ridden-out");
  });
});

// carried-child judges how the carried child's planner came back in its own tree
// (carried_child_sessions). Any of the child's roles may find its volume lost, in either order, so
// the rows hold every such order to a pass, a claim that logs the loss twice to a failure, and the
// planner's session to its log: a new session must have a loss behind it, and a kept one none.
describe("stage 4b's carried-child sessions verdict", () => {
  const prefix = "legion-legsmoke-legsmoke-13-";
  const planner = `${prefix}planner`;
  const architect = `${prefix}architect`;
  const line = (msg: string, claim: string, extra: object = {}) => ({
    time: "2026-10-12T10:46:00Z",
    level: "INFO",
    msg,
    claim,
    ...extra,
  });
  const lost = (claim: string) =>
    line(
      "supervise: the issue's volume was lost with the session; relaunching a fresh session",
      claim,
      { session: "" }
    );
  const dropped = (claim: string) =>
    line(
      "supervise: the issue's volume the session lived on was lost; the next launch is a fresh session",
      claim,
      { session: "kept" }
    );
  // cameBack is the verdict on a daemon log holding the planner's registration as session `kept`,
  // its move, the LINES between, and its registration after them as session AFTER (none if null).
  const cameBack = (lines: object[], after: string | null) => {
    const log = join(dir, `daemon-${++runs}.log`);
    const all = [
      line("api: claim registered", planner, { session: "kept", generation: 1 }),
      line("supervise: the claim's issue moved to another tree", planner, {
        from: "LEGSMOKE-12",
        to: "LEGSMOKE-13",
      }),
      ...lines,
      ...(after === null
        ? []
        : [line("api: claim registered", planner, { session: after, generation: 2 })]),
    ];
    writeFileSync(log, `${all.map((entry) => JSON.stringify(entry)).join("\n")}\n`);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
${fn("carried_child_sessions")}
if out=$(carried_child_sessions ${JSON.stringify(log)} ${prefix} ${planner} 2>&1); then echo "ok $out"; else echo "fail $out"; fi
`,
    ]);
    expect(result.exitCode).toBe(0);
    return result.stdout.toString().trim();
  };

  test("passes a fresh planner when the architect and then the planner each found the volume lost", () => {
    expect(cameBack([lost(architect), dropped(planner), lost(planner)], "new")).toBe("ok fresh");
  });

  test("passes a fresh planner that found the volume lost first, or alone", () => {
    expect(cameBack([lost(planner), lost(architect)], "new")).toBe("ok fresh");
    expect(cameBack([lost(planner)], "new")).toBe("ok fresh");
  });

  test("passes a fresh planner whose session a sibling's loss dropped before its own launch", () => {
    expect(cameBack([lost(architect), dropped(planner)], "new")).toBe("ok fresh");
  });

  test("passes a planner back on its kept session with no loss logged, the relabel path", () => {
    expect(cameBack([], "kept")).toBe("ok kept");
  });

  test("fails a fresh planner with no loss of its kept session logged", () => {
    expect(cameBack([], "new")).toBe(
      `fail ${planner} came back as session new, not its kept kept, with no loss of the kept one logged`
    );
    expect(cameBack([lost(architect)], "new")).toBe(
      `fail ${planner} came back as session new, not its kept kept, with no loss of the kept one logged`
    );
  });

  test("fails a claim that logged the volume lost twice", () => {
    expect(cameBack([lost(planner), lost(planner)], "new")).toBe(
      `fail the issue's volume was logged lost more than once for ${planner}`
    );
    expect(cameBack([lost(architect), lost(planner), lost(architect)], "new")).toBe(
      `fail the issue's volume was logged lost more than once for ${architect}`
    );
  });

  test("fails a planner back on the session it logged lost", () => {
    expect(cameBack([lost(planner)], "kept")).toBe(
      `fail ${planner} logged its session kept lost, yet came back on it`
    );
  });

  test("fails a planner that has not registered since its move", () => {
    expect(cameBack([lost(planner)], null)).toBe(
      `fail ${planner} has not registered since its move`
    );
  });

  test("counts only the child's own claims, not those of a key it prefixes", () => {
    const other = "legion-legsmoke-legsmoke-130-architect";
    expect(cameBack([lost(other), lost(other)], "kept")).toBe("ok kept");
  });
});

// mid-turn-carry judges a child carried while its planner still ran a turn in its old tree
// (carried_mid_turn_ordered): a start of the new tree waited for the planner to stop, the planner's
// suspension landed after that wait and after the driver's release, with no mid-turn stop, and the
// architect's first launch came after it. The rows hold the daemon's order to a pass and each way a
// new tree that did not wait, or a turn the hold's deadline cut, shows to a failure: a launch refused
// for the Sandbox still running the old tree's roles, a turn cut mid-turn, no wait, a planner never
// stopped or stopped before the release, and an architect launched before it.
describe("stage 4b's mid-turn carry verdict", () => {
  const prefix = "legion-legsmoke-legsmoke-14-";
  const planner = `${prefix}planner`;
  const architect = `${prefix}architect`;
  const line = (msg: string, extra: object) => ({
    time: "2026-10-13T00:10:00Z",
    level: "INFO",
    msg,
    ...extra,
  });
  const wait = line("outbox row waits", {
    row: 9,
    kind: "supervise",
    issue: "LEGSMOKE-14",
    attempts: 1,
    error: `start of LEGSMOKE-14 in tree LEGSMOKE-14 waits for ${planner}:working in tree LEGSMOKE-12 to stop`,
  });
  const stopped = line("supervise: suspended", {
    claim: planner,
    reason: "the tree of LEGSMOKE-12 lingers",
    by: "daemon",
  });
  const launched = line("supervise: launched", { claim: architect, generation: 1, resumed: false });
  const cut = line("supervise: the agent's turn has not ended; suspending it mid-turn", {
    claim: planner,
    level: "WARN",
    by: "daemon",
    deadline: "2026-10-13T00:18:00Z",
  });
  // released is the driver's release of the hold, before the suspension in `stopped`.
  const released = "2026-10-13T00:09:30.123456789Z";
  const refused = line("supervise: launch failed", {
    claim: architect,
    level: "WARN",
    error:
      "ensure its sandbox: sandbox legion-legsmoke-legsmoke-14, made for tree LEGSMOKE-12, does not fit this launch of tree LEGSMOKE-14 and still runs roles",
  });
  const verdict = (lines: object[], releasedAt: string = released) => {
    const log = join(dir, `daemon-${++runs}.log`);
    writeFileSync(log, `${lines.map((entry) => JSON.stringify(entry)).join("\n")}\n`);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
${fn("carried_mid_turn_ordered")}
if out=$(carried_mid_turn_ordered ${JSON.stringify(log)} ${prefix} ${planner} ${architect} LEGSMOKE-12 ${releasedAt} 2>&1); then echo "ok$out"; else echo "fail $out"; fi
`,
    ]);
    expect(result.exitCode).toBe(0);
    return result.stdout.toString().trim();
  };

  test("passes a new tree that waited for the planner, which stopped before the architect launched", () => {
    expect(verdict([wait, stopped, launched])).toBe("ok");
  });

  test("fails a launch refused for the Sandbox still running the old tree's roles", () => {
    expect(verdict([refused, wait, stopped, launched])).toBe(
      `fail a launch of ${architect} met the Sandbox still running tree LEGSMOKE-12's roles: ${refused.error}`
    );
  });

  test("fails a planner whose turn the hold's deadline cut mid-turn", () => {
    expect(verdict([wait, cut, stopped, launched])).toBe(
      `fail ${planner}'s turn was cut mid-turn at 2026-10-13T00:10:00Z (deadline 2026-10-13T00:18:00Z), not ended by the release at ${released}`
    );
  });

  test("fails a planner suspended before the driver's release", () => {
    expect(verdict([wait, stopped, launched], "2026-10-13T00:11:00.5Z")).toBe(
      `fail ${planner} was suspended at 2026-10-13T00:10:00Z, before the release at 2026-10-13T00:11:00.5Z`
    );
  });

  test("compares fractions of a second of different lengths as times, not as text", () => {
    const at = (time: string) => ({ ...stopped, time });
    // .25574538 is .255745380: one nanosecond after .255745379, though shorter as text.
    expect(
      verdict(
        [wait, at("2026-10-13T00:10:00.25574538Z"), launched],
        "2026-10-13T00:10:00.255745379Z"
      )
    ).toBe("ok");
    expect(
      verdict(
        [wait, at("2026-10-13T00:10:00.255745379Z"), launched],
        "2026-10-13T00:10:00.25574538Z"
      )
    ).toBe(
      `fail ${planner} was suspended at 2026-10-13T00:10:00.255745379Z, before the release at 2026-10-13T00:10:00.25574538Z`
    );
  });

  test("fails a new tree whose starts never waited for the planner", () => {
    expect(verdict([stopped, launched])).toBe(
      `fail no start of the new tree waited for ${planner} to stop in tree LEGSMOKE-12`
    );
  });

  test("fails a planner never suspended after the wait, counting no suspension before it", () => {
    expect(verdict([stopped, wait, launched])).toBe(
      `fail ${planner} was never suspended after the new tree's start waited for it`
    );
  });

  test("fails an architect launched before the planner stopped", () => {
    expect(verdict([wait, launched, stopped])).toBe(
      `fail ${architect} launched before ${planner} stopped in tree LEGSMOKE-12`
    );
  });

  test("fails an architect that never launched", () => {
    expect(verdict([wait, stopped])).toBe(`fail ${architect} never launched`);
  });

  test("reads only the child's own claims and its own old tree", () => {
    const other = { ...refused, claim: "legion-legsmoke-legsmoke-140-architect" };
    const elsewhere = {
      ...wait,
      error: wait.error.replace("in tree LEGSMOKE-12", "in tree LEGSMOKE-99"),
    };
    expect(verdict([other, wait, stopped, launched])).toBe("ok");
    expect(verdict([elsewhere, stopped, launched])).toBe(
      `fail no start of the new tree waited for ${planner} to stop in tree LEGSMOKE-12`
    );
  });
});
