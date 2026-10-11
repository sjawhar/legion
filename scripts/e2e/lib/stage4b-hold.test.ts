import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's hold loops end a worker's launch while its task is outstanding and count on the daemon
// charging the death. death_charge reads how the daemon charged one end from its log, and
// hold_end_charged gives a relaunch a new task, within a bound, when the end came after its task's
// turn had already ended, so no death was charged. Both run as the script defines them, on a daemon
// log shaped like the run where an end of tree 3's implementer landed 2.4 s after its turn ended.
const scriptPath = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const script = readFileSync(scriptPath, "utf8");
const fn = scriptFunctions(scriptPath);
const dir = mkdtempSync(join(tmpdir(), "stage4b-hold-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

/** The value the script assigns NAME at the start of a line. */
function scriptNumber(name: string): number {
  const found = new RegExp(`^${name}=(\\d+)$`, "m").exec(script);
  if (found === null) throw new Error(`stage4b-sandbox-tree.sh assigns no ${name}`);
  return Number(found[1]);
}
const launchFailureLimit = scriptNumber("launch_failure_limit");
const rearmLimit = scriptNumber("hold_rearm_limit");

const claim = "legion-legsmoke-legsmoke-563-implementer";
const other = "legion-legsmoke-legsmoke-563-architect";
type Line = Record<string, unknown>;
const at = (seconds: number) =>
  new Date(Date.UTC(2026, 9, 9, 9, 4, 30) + seconds * 1000).toISOString();
const launched = (t: number, generation: number, c = claim): Line => ({
  time: at(t),
  level: "INFO",
  msg: "supervise: launched",
  claim: c,
  generation,
  incarnation: `pod-a/${generation}`,
  resumed: generation > 1,
});
const registered = (t: number, generation: number): Line => ({
  time: at(t),
  level: "INFO",
  msg: "api: claim registered",
  claim,
  generation,
  session: "ses-implementer",
});
const died = (t: number, incarnation: string, c = claim): Line => ({
  time: at(t),
  level: "WARN",
  msg: "supervise: process died",
  claim: c,
  incarnation,
  observed: "gone",
  detail: "role container implementer terminated (Completed, exit code 0)",
});
const inTurn = (t: number): Line => ({
  time: at(t),
  level: "WARN",
  msg: "supervise: the process died in the task's turn; the task waits for the relaunch",
  claim,
  delivery: "d1",
});
const charged = (t: number, deaths: number, c = claim): Line => ({
  time: at(t),
  level: "WARN",
  msg: "supervise: the agent died with work outstanding",
  claim: c,
  deaths,
  limit: 3,
});

// Generation 1 dies after its turn ended (the run's end), with another claim's charge logged
// before generation 2 launches; generations 2 to 4 die inside their turns, the last at the limit,
// after which nothing launches. A line that is not JSON, and one that is JSON but no object, are
// read past.
const log = join(dir, "daemon.log");
writeFileSync(
  log,
  `${[
    launched(4, 1),
    registered(5, 1),
    "panic: a line the daemon's stderr wrote, not JSON",
    died(60, "pod-a/1"),
    charged(60.1, 1, other),
    42,
    launched(60.4, 2),
    registered(61.3, 2),
    died(90, "pod-a/2"),
    inTurn(90.1),
    charged(90.1, 1),
    launched(90.4, 3),
    registered(91.2, 3),
    died(120, "pod-a/3"),
    inTurn(120.1),
    charged(120.1, 2),
    launched(120.4, 4),
    registered(121.2, 4),
    died(150, "pod-a/4"),
    inTurn(150.1),
    charged(150.1, 3),
    {
      time: at(150.2),
      level: "WARN",
      msg: "supervise: claim failed",
      claim,
      why: "deaths with work outstanding ran out",
    },
  ]
    .map((line) => (typeof line === "string" ? line : JSON.stringify(line)))
    .join("\n")}\n`
);

let runs = 0;
interface Run {
  readonly code: number;
  readonly out: string;
  readonly delivered: string[][];
}
/** Runs BODY after the two functions, with note, fail and claims_cli stubbed. */
function run(body: string, env: Record<string, string> = {}): Run {
  const calls = join(dir, `calls-${++runs}`);
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -euo pipefail
daemon_log=${JSON.stringify(log)} launch_failure_limit=${launchFailureLimit} hold_rearm_limit=${rearmLimit}
note() { printf 'NOTE %s\\n' "$*"; }
fail() { printf 'FAIL %s\\n' "$*"; exit 3; }
claims_cli() {
  printf '%s\\0' "$@" >>${JSON.stringify(calls)}
  printf '\\n' >>${JSON.stringify(calls)}
  [ -z "\${DELIVER_FAILS-}" ] || { echo "answered 409: the claim is failed" >&2; return 1; }
}
${fn("death_charge")}
${fn("hold_end_charged")}
${body}`,
    ],
    { env: { ...process.env, ...env } }
  );
  const delivered = existsSync(calls)
    ? readFileSync(calls, "utf8")
        .split("\n")
        .filter((call) => call !== "")
        .map((call) => call.split("\0").filter((arg) => arg !== ""))
    : [];
  expect(result.stderr.toString()).toBe("");
  return { code: result.exitCode, out: result.stdout.toString(), delivered };
}

describe("death_charge", () => {
  test("an end after the task's turn had ended is uncharged, whatever another claim logged", () => {
    expect(run(`death_charge ${claim} pod-a/1`)).toMatchObject({ code: 0, out: "uncharged\n" });
  });

  test("an end inside the turn prints the daemon's death count, the last with nothing after it", () => {
    for (const [incarnation, count] of [
      ["pod-a/2", 1],
      ["pod-a/3", 2],
      ["pod-a/4", 3],
    ] as const) {
      expect(run(`death_charge ${claim} ${incarnation}`)).toMatchObject({
        code: 0,
        out: `charged ${count}\n`,
      });
    }
  });

  test("a process the log has no death of, or another claim's, exits non-zero with nothing", () => {
    for (const [c, incarnation] of [
      [claim, "pod-a/9"],
      [other, "pod-a/2"],
    ]) {
      const result = run(`death_charge ${c} ${incarnation} || echo "exit $?"`);
      expect(result.out).toBe("exit 4\n");
    }
  });
});

describe("hold_end_charged", () => {
  const judge = (incarnation: string, misses: number) => `ended_incarnation=${incarnation}
hold_misses=${misses}
if hold_end_charged ${claim}; then echo "RET 0"; else echo "RET 1"; fi
echo "MISSES $hold_misses"`;

  test("a charged end counts, and nothing is delivered", () => {
    const result = run(judge("pod-a/2", 0));
    expect(result.code).toBe(0);
    expect(result.out).toBe(
      `NOTE the daemon charged the death of pod-a/2 as one with work outstanding, 1 of ${launchFailureLimit}\nRET 0\nMISSES 0\n`
    );
    expect(result.delivered).toEqual([]);
  });

  test("an uncharged end is a miss: its relaunch is given a new task through the daemon", () => {
    const result = run(judge("pod-a/1", 0));
    expect(result.code).toBe(0);
    expect(result.out).toEndWith("RET 1\nMISSES 1\n");
    expect(result.out).toContain(
      `NOTE the end of pod-a/1 came after its task's turn had ended, so the daemon charged no death and its relaunch has no task (miss 1 of at most ${rearmLimit}); gave ${claim} a new task with legion claims deliver`
    );
    expect(result.delivered).toHaveLength(1);
    const [subcommand, claimFlag, token, taskFlag, task] = result.delivered[0] ?? [];
    expect([subcommand, claimFlag, token, taskFlag]).toEqual([
      "deliver",
      "--claim",
      claim,
      "--task",
    ]);
    // The deployment instructions let a worker read its role and reply WAITING to any task but a
    // targeted human message, so the task asks for that reading in the instructions' own words, and
    // names no tool, since the tools a worker reads with change with its plugin.
    const reading = "Read what your role says to read, then reply WAITING";
    expect(script).toContain(`phase and is not that message. ${reading} and wait for`);
    expect(task).toContain(reading);
    expect(task).not.toMatch(/dispatch_\w+|handoff_read/);
  });

  test("a miss past the bound fails the loop loudly, and delivers nothing", () => {
    const result = run(judge("pod-a/1", rearmLimit));
    expect(result.code).toBe(3);
    expect(result.out).toBe(
      `FAIL ${rearmLimit + 1} ends of ${claim} came after their task's turn had ended, the last its process pod-a/1, so the daemon charged no death for them; ${rearmLimit} new tasks did not put an end inside a turn\n`
    );
    expect(result.delivered).toEqual([]);
  });

  test("a delivery the daemon refuses fails the loop with the daemon's answer", () => {
    const result = run(judge("pod-a/1", 0), { DELIVER_FAILS: "1" });
    expect(result.code).toBe(3);
    expect(result.out).toBe(
      `FAIL legion claims deliver could not give ${claim} a new task after the end of pod-a/1 missed its turn: answered 409: the claim is failed\n`
    );
  });

  test("an end the daemon log has no death of fails the loop", () => {
    const result = run(judge("pod-a/9", 0));
    expect(result.code).toBe(3);
    expect(result.out).toBe(
      `FAIL the daemon log has no death of ${claim}'s process pod-a/9, which the proof ended\n`
    );
  });
});
