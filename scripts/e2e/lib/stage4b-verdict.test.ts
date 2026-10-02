import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Stage 4b's own blocked, fail, pass, until_reached, cleanup, audit_verdict and audit_failure,
// taken from the script by name and run after the controller checkpoint ends (blocked, failed, or
// passed as a STAGE4B_UNTIL run's last checkpoint), with the teardown's cluster, NATS and GitHub
// helpers stubbed and production_audit's Dispatch read replaced by its result: a write outside
// LEGSMOKE, or none. A timed-out wait runs the script's own capacity hook and limit_pending
// against a kubectl that answers pods (filtered by the -l selector, as the API server does) and
// FailedScheduling events from the test's fixture.
const script = readFileSync(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"), "utf8");
const rig = readFileSync(join(import.meta.dir, "rig.sh"), "utf8");
const fnOf = (source: string, file: string, name: string) => {
  const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(source);
  if (found === null) throw new Error(`${file} defines no ${name}()`);
  return found[0];
};
const fn = (name: string) => fnOf(script, "stage4b-sandbox-tree.sh", name);
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
      { type: "PodScheduled", status: "False", reason: "Unschedulable", lastTransitionTime: since },
    ],
  },
});
// schedulingEvent is a FailedScheduling event from COMPONENT for pod NAME with UID, last seen AT.
const schedulingEvent =
  (component: string, message: string) => (name: string, uid: string, at: string) => ({
    involvedObject: { kind: "Pod", name, uid },
    source: { component },
    lastTimestamp: at,
    message,
  });
// limitEvent is Karpenter's verdict that every instance type exceeds the legion pool's limits.
const limitEvent = schedulingEvent(
  "karpenter",
  'Failed to schedule pod, incompatible with nodepool "other"; all available instance types exceed limits for nodepool (NodePool=legion)'
);
// affinityEvent is Karpenter's verdict for a genuine scheduling reason, the tree's affinity.
const affinityEvent = schedulingEvent(
  "karpenter",
  "Failed to schedule pod, unsatisfiable topology constraint for pod affinity, key=kubernetes.io/hostname"
);
// schedulerEvent is the default scheduler's FailedScheduling, which every Pending pod gets.
const schedulerEvent = schedulingEvent(
  "default-scheduler",
  "0/16 nodes are available: 14 node(s) didn't match Pod's node affinity/selector."
);
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
tree1= tree2= tree3= tree4= shape_pid= daemon_pid= watch_pid= events_pid= leaks_pid= sampler_pid= interests_pid= pg_container=none run_label=x
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
${fnOf(rig, "rig.sh", "until_true")}
${fn("cleanup")}
trap cleanup EXIT
${ending !== "timeout" ? `${ending} "the controller's model route could not be installed"` : `timeout_hook=limit_pending_blocked; ${capacity.subject === undefined ? "" : `on_tree ${capacity.subject} `}until_true 1 "the controller to take its first turn" false`}
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
  for (const [name, events] of [
    ["Karpenter's limit verdict", [limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z")]],
    [
      "Karpenter's limit verdict, with the default scheduler's own event newer",
      [
        limitEvent("legion-x-t1", "uid-now", "2026-10-02T10:00:05Z"),
        schedulerEvent("legion-x-t1", "uid-now", "2026-10-02T10:05:00Z"),
      ],
    ],
  ] as [string, object[]][]) {
    test(`says BLOCKED on capacity for a wait its own tree's pod starved, on ${name}`, () => {
      const starved = controllerRun("[]", "timeout", { subject: "T1", pods: [starvedPod], events });
      expect(starved.code).toBe(1);
      expect(starved.stdout).toContain(
        "CHECK controller: BLOCKED: capacity: the legion pool is at its limits, so the scheduler cannot place pod legion-x-t1 (uid uid-now; Karpenter: all available instance types exceed limits for nodepool legion)"
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
