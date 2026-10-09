import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { runJq } from "./run-jq.ts";
import { scriptFunctions } from "./script-functions";

// Stage 4b's controller liveness verdict (daemon-controller-liveness): the two lines a log monitor
// on the daemon counts, judged on a daemon log as the daemon writes it, one JSON object a line, and
// in the down phase the relaunches as the Sandbox runtime reports them: failed launches, and the pod
// watch's record of the pods they made.

const library = fileURLToPath(new URL(".", import.meta.url));
const program = fileURLToPath(new URL("./controller-liveness-verdict.jq", import.meta.url));
const root = join(import.meta.dir, "..", "..", "..");
const dir = mkdtempSync(join(tmpdir(), "controller-liveness-verdict-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

interface Verdict {
  notRegistered: { time: string; msg: string; claimState?: string; registered?: boolean }[];
  noHolder: { time: string; session: string }[];
  launchFailures: { time: string; generation: number; launchFailures: number }[];
  launches: { time: string; incarnation: string }[];
  pods: {
    uid: string;
    name: string;
    unschedulable: boolean;
    scheduled: boolean;
    node: string | null;
  }[];
  missing: string[];
  wrong: string[];
}

const notRegistered = "controller not registered; run legion controller start";
const daemonForm = `${notRegistered} only under controller: operator; this daemon launches its own controller and relaunches it`;
const noHolder =
  "controller liveness: the controller role has no live holder; the controller is gone";
const claim = "legion-legsmoke-controller";
const session = "ses_launched";
const deletedPod = "uid-deleted";
const downArgs = { claim, session, deleted_pod: deletedPod, cpu: "100000" };

type Line = Record<string, unknown>;
const at = (seconds: number) =>
  new Date(Date.UTC(2026, 9, 8, 20, 0, 0) + seconds * 1000).toISOString();
const prober = (t: number, extra: Line = {}): Line => ({
  time: at(t),
  level: "INFO",
  msg: noHolder,
  mode: "daemon",
  url: `https://listener.internal.example/v1/roles/${claim}`,
  session,
  ...extra,
});
const warn = (t: number, extra: Line = {}): Line => ({
  time: at(t),
  level: "WARN",
  msg: daemonForm,
  mode: "daemon",
  project: "LEGSMOKE",
  registered: true,
  claimState: "launching",
  ...extra,
});
const supervise = (t: number, msg: string, extra: Line): Line => ({
  time: at(t),
  level: "WARN",
  msg,
  claim,
  ...extra,
});
const died = (t: number, incarnation: string) =>
  supervise(t, "supervise: process died", { incarnation, observed: "gone", detail: "absent" });
const launched = (t: number, incarnation: string) =>
  supervise(t, "supervise: launched", { level: "INFO", incarnation, resumed: true });
const failed = (t: number, generation: number) =>
  supervise(t, "supervise: launch failed", {
    generation,
    launchFailures: 1,
    limit: 3,
    error: `launch ${claim}: wait for its role launcher: timed out`,
  });

/** A pod watch event of the controller's pod: uid, its controller container's CPU request, and its scheduling. */
const pod = (uid: string, cpu: string, scheduling: { node?: string; unschedulable?: boolean }) => ({
  type: "MODIFIED",
  object: {
    kind: "Pod",
    metadata: {
      name: claim,
      uid,
      labels: { "legion.dev/project": "legsmoke", "legion.dev/role": "controller" },
    },
    spec: {
      ...(scheduling.node ? { nodeName: scheduling.node } : {}),
      containers: [{ name: "controller", resources: { requests: { cpu, memory: "1Gi" } } }],
    },
    status: {
      phase: scheduling.node ? "Running" : "Pending",
      conditions: scheduling.node
        ? [{ type: "PodScheduled", status: "True" }]
        : scheduling.unschedulable
          ? [{ type: "PodScheduled", status: "False", reason: "Unschedulable" }]
          : [],
    },
  },
});
// The pod watch as the down phase leaves it: the deleted pod and the pod recreated from the old
// template, both scheduled at the negative control's 250m, then the relaunch's pod, first Pending
// and then Unschedulable, at 100000 CPU in the API server's canonical form.
const watch = [
  pod(deletedPod, "250m", { node: "node-a" }),
  pod("uid-recreated", "250m", { node: "node-b" }),
  pod("uid-down", "100k", {}),
  pod("uid-down", "100k", { unschedulable: true }),
];

let runs = 0;
/** Runs the verdict as the driver does: the log raw, its arguments by name, the watch slurped. */
function verdict(
  log: (Line | string)[],
  phase: string,
  named: Record<string, string> = downArgs,
  podWatch?: unknown[]
): Verdict {
  const args = [
    "-R",
    "-s",
    "-c",
    "-L",
    library,
    "--arg",
    "phase",
    phase,
    "--argjson",
    "boot",
    "120",
  ];
  for (const [name, value] of Object.entries(named)) args.push("--arg", name, value);
  if (podWatch !== undefined) {
    const file = join(dir, `watch-${++runs}.json`);
    writeFileSync(file, podWatch.map((event) => `${JSON.stringify(event)}\n`).join(""));
    args.push("--slurpfile", "watch", file);
  }
  const input = log
    .map((line) => (typeof line === "string" ? line : JSON.stringify(line)))
    .join("\n");
  return JSON.parse(runJq([...args, "-f", program], `${input}\n`));
}

// The down phase's log as the live run sees it: the deleted pod's death, a sweep's two lines, more
// sweeps, the relaunch's launch failing at the boot timeout, and the lines after it.
const down: (Line | string)[] = [
  died(5, `${deletedPod}/1`),
  prober(30),
  warn(30.2),
  prober(90),
  failed(130, 2),
  prober(150),
  prober(210),
  warn(210.3),
];

describe("controller-liveness-verdict.jq, down", () => {
  test("the lines of a controller that cannot come back pass, a line that is not JSON skipped", () => {
    const v = verdict(
      ["a line the daemon's stderr wrote, not JSON", ...down],
      "down",
      downArgs,
      watch
    );
    expect(v.missing).toEqual([]);
    expect(v.wrong).toEqual([]);
    expect(v.noHolder).toHaveLength(4);
    expect(v.notRegistered.map((line) => line.claimState)).toEqual(["launching", "launching"]);
    expect(v.launchFailures).toEqual([{ time: at(130), generation: 2, launchFailures: 1 }]);
    // The deleted pod and the pod recreated from the old template request 250m: neither is a relaunch's.
    expect(v.pods).toEqual([
      { uid: "uid-down", name: claim, unschedulable: true, node: null, scheduled: false },
    ]);
  });

  test("a relaunch that caught the deleted pod while it terminated launches on it, dies, and passes", () => {
    // The relaunch read the deleted pod still bound to its launcher, returned it at the next
    // generation once it went, and that process died; the relaunch after it made the 100000 CPU pod.
    const caught = [
      died(5, `${deletedPod}/1`),
      launched(8, `${deletedPod}/2`),
      died(12, `${deletedPod}/2`),
      ...down.slice(1),
    ];
    const v = verdict(caught, "down", downArgs, watch);
    expect(v.missing).toEqual([]);
    expect(v.wrong).toEqual([]);
    expect(v.launches).toEqual([{ time: at(8), incarnation: `${deletedPod}/2` }]);
  });

  test("a launch on any pod but the deleted one is the controller coming back", () => {
    const v = verdict([...down, launched(300, "uid-recreated/4")], "down", downArgs, watch);
    expect(v.wrong).toEqual([
      `${at(300)} ${claim} launched at "uid-recreated/4", not on the deleted pod ${deletedPod}: its controller came back`,
    ]);
  });

  test("a pod a relaunch made that was scheduled departs; one not yet seen Unschedulable is missing", () => {
    const scheduled = verdict(down, "down", downArgs, [
      ...watch,
      pod("uid-down", "100k", { node: "node-c" }),
    ]);
    expect(scheduled.wrong).toEqual([
      `pod ${claim} (uid uid-down), made by a relaunch requesting 100000 CPU, was scheduled on node-c`,
    ]);
    expect(verdict(down, "down", downArgs, watch.slice(0, 3)).missing).toEqual([
      `pod ${claim} (uid uid-down) seen Unschedulable`,
    ]);
    expect(verdict(down, "down", downArgs, watch.slice(0, 2)).missing).toEqual([
      "a pod a relaunch made, requesting 100000 CPU",
    ]);
    // The request is compared as a quantity: 100000m is a hundred CPU, not a relaunch's.
    expect(
      verdict(down, "down", downArgs, [...watch.slice(0, 2), pod("uid-small", "100000m", {})]).pods
    ).toEqual([]);
  });

  test("times with a fraction or an offset are judged as the instants they name", () => {
    // 22:00:30.2+02:00 is the first warning's instant; the second, 180 s later, is past the boot timeout.
    const offset = down.map((line) =>
      typeof line !== "string" && line.time === at(30.2)
        ? { ...line, time: "2026-10-08T22:00:30.2+02:00" }
        : line
    );
    expect(verdict(offset, "down", downArgs, watch).wrong).toEqual([]);
  });

  test("each part of the phase not seen yet is missing", () => {
    const early = verdict(down.slice(0, 3), "down", downArgs, []);
    expect(early.missing).toEqual([
      "the no-holder line on 3 sweeps (seen 1)",
      "the not-registered line twice (seen 1)",
      `a failed launch of ${claim}`,
      "a pod a relaunch made, requesting 100000 CPU",
    ]);
    expect(early.wrong).toEqual([]);
    // The deleted pod's death, or a launch that died, is no failed launch.
    expect(
      verdict(
        [...down.slice(0, 4), died(100, `${deletedPod}/2`), prober(150), warn(160)],
        "down",
        downArgs,
        watch
      ).missing
    ).toEqual([`a failed launch of ${claim}`]);
    const beforeOnly = verdict(
      [...down.slice(0, 4), prober(150), warn(160), failed(170, 2)],
      "down",
      downArgs,
      watch
    );
    expect(beforeOnly.missing).toEqual([
      `a not-registered line after the failed launch at ${at(170)}`,
    ]);
  });

  test("a not-registered line inside the boot timeout of the one before departs from the cadence", () => {
    const v = verdict([...down, warn(260)], "down", downArgs, watch);
    expect(v.wrong).toEqual([
      `${at(260)} the not-registered line came 49.7 s after the one before, under the boot timeout of 120 s`,
    ]);
  });

  test("the operator's form, another mode, an unregistered record or a live claim state departs", () => {
    const v = verdict(
      [
        ...down,
        warn(400, { msg: notRegistered }),
        warn(600, { mode: "operator" }),
        warn(800, { registered: false }),
        warn(1000, { claimState: "idle" }),
        warn(1200, { claimState: undefined }),
        prober(1300, { session: "ses_operator" }),
      ],
      "down",
      downArgs,
      watch
    );
    expect(v.wrong).toEqual([
      `${at(600)} ${JSON.stringify(daemonForm)} names mode "operator", not daemon`,
      `${at(400)} ${JSON.stringify(notRegistered)} is not the daemon's form: the monitored text leading the remedy that names controller: operator`,
      `${at(800)} the not-registered line says the record is registered: false, want true (it still names ${session})`,
      `${at(1000)} the not-registered line names claim state "idle", not a claim that is down`,
      `${at(1200)} the not-registered line names claim state null, not a claim that is down`,
      `${at(1300)} the no-holder line names session "ses_operator", not ${session}`,
    ]);
  });

  test("the down phase refuses to judge without the relaunches' CPU or the pod watch", () => {
    expect(() => verdict(down, "down", { claim, session, deleted_pod: deletedPod }, watch)).toThrow(
      /cpu .* is required in the down phase/
    );
    expect(() => verdict(down, "down", downArgs)).toThrow(/watch .* is required in the down phase/);
  });
});

describe("controller-liveness-verdict.jq, up and operator", () => {
  test("up: no line passes, and either line departs", () => {
    expect(verdict([died(5, "uid-other/1")], "up", {})).toMatchObject({ missing: [], wrong: [] });
    expect(verdict([prober(30), warn(30.2)], "up", {}).wrong).toEqual([
      `${at(30.2)} ${JSON.stringify(daemonForm)} while the daemon's controller holds the role`,
      `${at(30)} ${JSON.stringify(noHolder)} while the daemon's controller holds the role`,
    ]);
  });

  test("operator: the monitored text exactly, in mode operator", () => {
    const line = warn(30, {
      msg: notRegistered,
      mode: "operator",
      claimState: undefined,
      registered: false,
    });
    expect(verdict([line], "operator", {})).toMatchObject({ missing: [], wrong: [] });
    expect(verdict([], "operator", {}).missing).toEqual(["the operator's not-registered line"]);
    expect(verdict([warn(30, { mode: "operator" })], "operator", {}).wrong).toEqual([
      `${at(30)} ${JSON.stringify(daemonForm)} is not exactly the monitored text`,
    ]);
    expect(verdict([{ ...line, mode: "daemon" }], "operator", {}).wrong).toEqual([
      `${at(30)} ${JSON.stringify(notRegistered)} names mode "daemon", not operator`,
    ]);
  });

  test("a phase it does not know, or no boot timeout, is refused", () => {
    expect(() => verdict([], "sideways", {})).toThrow(
      /phase must be down, up or operator, not "sideways"/
    );
    expect(() =>
      runJq(["-R", "-s", "-L", library, "--arg", "phase", "up", "-f", program], "")
    ).toThrow(/boot .* is required/);
  });
});

describe("the texts the verdict counts are the daemon's own", () => {
  test("the daemon writes both lines, and the machine its launch lines, with the text the verdict matches", () => {
    const daemon = join(root, "packages", "daemon", "internal");
    const controller = readFileSync(join(daemon, "daemon", "controller.go"), "utf8");
    expect(controller).toContain(JSON.stringify(notRegistered));
    expect(controller).toContain(JSON.stringify(daemonForm.slice(notRegistered.length)));
    expect(readFileSync(join(daemon, "controller", "liveness.go"), "utf8")).toContain(
      JSON.stringify(noHolder)
    );
    const machine = readFileSync(join(daemon, "supervise", "machine.go"), "utf8");
    const jq = readFileSync(program, "utf8");
    for (const text of ["supervise: launch failed", "supervise: launched"]) {
      expect(machine).toContain(JSON.stringify(text));
      expect(jq).toContain(JSON.stringify(text));
    }
    expect(jq).toContain(`def not_registered: ${JSON.stringify(notRegistered)};`);
    expect(jq).toContain(`def no_holder: ${JSON.stringify(noHolder)};`);
  });
});

describe("liveness_verdict, as the driver runs it", () => {
  const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));

  test("judges only the daemon log's lines after the mark", () => {
    const log = join(dir, "daemon.log");
    // Line 1 is before the mark: a not-registered line the up phase would refuse.
    writeFileSync(
      log,
      `${[warn(10), died(20, "uid-other/1")].map((line) => JSON.stringify(line)).join("\n")}\n`
    );
    const run = Bun.spawnSync([
      "bash",
      "-c",
      `set -euo pipefail
root=${JSON.stringify(root)} daemon_log=${JSON.stringify(log)} boot_timeout=120
${fn("liveness_verdict")}
liveness_verdict up 1
liveness_verdict up 0`,
    ]);
    expect(run.stderr.toString()).toBe("");
    const [afterMark, whole] = run.stdout
      .toString()
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Verdict);
    expect(afterMark.wrong).toEqual([]);
    expect(whole.wrong).toEqual([
      `${at(10)} ${JSON.stringify(daemonForm)} while the daemon's controller holds the role`,
    ]);
  });

  test("passes the down phase's arguments and the pod watch through to the verdict", () => {
    const log = join(dir, "daemon-down.log");
    const podWatch = join(dir, "pod-watch.json");
    writeFileSync(log, `${down.map((line) => JSON.stringify(line)).join("\n")}\n`);
    writeFileSync(podWatch, watch.map((event) => `${JSON.stringify(event)}\n`).join(""));
    const run = Bun.spawnSync([
      "bash",
      "-c",
      `set -euo pipefail
root=${JSON.stringify(root)} daemon_log=${JSON.stringify(log)} boot_timeout=120
${fn("liveness_verdict")}
liveness_verdict down 0 --arg claim ${claim} --arg session ${session} --arg deleted_pod ${deletedPod} \\
  --arg cpu 100000 --slurpfile watch ${JSON.stringify(podWatch)}`,
    ]);
    expect(run.stderr.toString()).toBe("");
    const v = JSON.parse(run.stdout.toString()) as Verdict;
    expect(v).toMatchObject({ missing: [], wrong: [] });
    expect(v.pods.map((p) => p.uid)).toEqual(["uid-down"]);
  });
});
