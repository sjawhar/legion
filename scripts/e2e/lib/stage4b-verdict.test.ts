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

// pendingPod is a pod of tree TREE (run label x) Pending and Unschedulable since SINCE.
const pendingPod = (name: string, uid: string, tree: string, since: string) => ({
  metadata: { name, uid, labels: { "legion.dev/project": "x", "legion.dev/tree": tree } },
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
// podEvent is an event REASON from COMPONENT for pod NAME with UID, last seen AT.
const podEvent =
  (component: string, reason: string, message: string) =>
  (name: string, uid: string, at: string) => ({
    involvedObject: { kind: "Pod", name, uid },
    source: { component },
    reason,
    lastTimestamp: at,
    message,
  });
const limitMessage =
  'Failed to schedule pod, incompatible with nodepool "other"; all available instance types exceed limits for nodepool (NodePool=legion)';
// limitEvent is Karpenter's verdict that every instance type exceeds the legion pool's limits.
const limitEvent = podEvent("karpenter", "FailedScheduling", limitMessage);
// affinityEvent is Karpenter's verdict for a genuine scheduling reason, the tree's affinity.
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
interface Capacity {
  subject?: string;
  pods?: object[];
  events?: object[];
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
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(join(import.meta.dir, "..", "..", ".."))}
work=${JSON.stringify(join(run, "work"))} evidence=${JSON.stringify(evidence)}
check=controller check_started=2026-09-30T12:00:00Z
ok= was_blocked= until=controller locked=1 compared= snapshotted= audited= prod_baseline=2026-09-30T11:00:00.000000000Z
tree1= tree2= tree3= tree4= pair_session= shape_pid= daemon_pid= watch_pid= events_pid= leaks_pid= sampler_pid= interests_pid= pg_container=none run_label=x smoke_main_cleaning=
negative_config_pod= negative_config_workspace=
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
    ["no pod Pending on the pool's limits", { subject: "T1" }],
  ] as [string, Capacity][]) {
    test(`still fails a wait that timed out with ${name}`, () => {
      const timedOut = controllerRun("[]", "timeout", capacity);
      expect(timedOut.code).toBe(1);
      expect(timedOut.stdout).toContain("CHECK controller: FAIL: timed out after 1s");
      expect(timedOut.stdout).toContain("stage 4b e2e: FAIL (check controller)");
      expect(timedOut.stdout).not.toContain("BLOCKED");
    });
  }
});

// The tree-separation checkpoint reads whether tree 2's planner held from on_tree's exit status
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
