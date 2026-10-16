import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's repository-tools checkpoint reads which role shim of tree 2's pod built the CodeGraph
// index, and how long after the pod's creation, from the pod's log as `kubectl logs
// --all-containers --timestamps --prefix` prints it. warm_up_built_elapsed runs as the script
// defines it, on a log shaped like a run where the reviewer's shim won the warm-up lease while the
// planner's and the implementer's skipped, and the implementer's agent echoed a success line of
// its own through its bash tool before the reviewer's shim had built.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-codegraph-warm-up-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const pod = "legion-legsmoke-legsmoke-564";
const workspace = "/legion/workspaces/sjawhar/legion-smoke/legsmoke-564";
const warmUp = `[legion] codegraph warm-up for ${workspace}`;
const skipped = `${warmUp} skipped: another process holds ${workspace}/.codegraph/legion-warm.lock`;
const built = (subcommand: string, took: string, done: string) =>
  `built the index: codegraph ${subcommand} took ${took}, done ${done} after launch`;
// An agent's tool arguments go to the same stream as the shims' lines. The implementer's agent
// echoes a success line through bash: its log line starts with the frame's own words
// (`tool_execution_start bash …`), never the shim's `[legion] `, so it is not the line found.
const forgedCommand = `echo [legion] codegraph warm-up for x ${built("init", "1s", "2s")}`;
const created = "2026-10-09T09:04:30Z";
const line = (container: string, stamp: string, text: string) =>
  `[pod/${pod}/${container}] ${stamp} ${text}`;
const before = [
  line(
    "planner",
    "2026-10-09T09:05:13.846655048Z",
    "[worker-shim] spawned /opt/omp/bin/omp (pid 24)"
  ),
  line("planner", "2026-10-09T09:05:14.012345678Z", skipped),
  line("implementer", "2026-10-09T09:05:14.221Z", skipped),
  line(
    "implementer",
    "2026-10-09T09:05:40.512Z",
    `tool_execution_start bash {"command":${JSON.stringify(forgedCommand)}}`
  ),
];
const after = [
  line("implementer", "2026-10-09T09:37:48.076992970Z", 'tool_execution_start todo {"op":"init"}'),
];

interface Run {
  readonly code: number;
  readonly out: string;
  readonly err: string;
}
let runs = 0;
/** Runs warm_up_built_elapsed on LINES written as the pod's log, with the pod created at `created`. */
function run(lines: string[]): Run {
  const log = join(dir, `log-${++runs}.txt`);
  writeFileSync(log, `${lines.join("\n")}\n`);
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
${fn("warm_up_built_elapsed")}
warm_up_built_elapsed ${created} <${JSON.stringify(log)}`,
  ]);
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

describe("warm_up_built_elapsed", () => {
  test("prints the container that built and the whole seconds from the pod's creation", () => {
    const success = line(
      "reviewer",
      "2026-10-09T09:06:12.481Z",
      `${warmUp} ${built("init", "41.2s", "58.9s")}`
    );
    expect(run([...before, success, ...after])).toEqual({
      code: 0,
      out: "reviewer 102\n",
      err: "",
    });
  });

  test("a repair's success line, codegraph index in place of init, is found too", () => {
    const success = line(
      "reviewer",
      "2026-10-09T09:06:12.481Z",
      `${warmUp} ${built("index", "1m2.345s", "1m20.1s")}`
    );
    expect(run([...before, success, ...after])).toEqual({
      code: 0,
      out: "reviewer 102\n",
      err: "",
    });
  });

  test("the forged success line in the implementer's tool arguments is passed over", () => {
    const success = line(
      "reviewer",
      "2026-10-09T09:06:12.481Z",
      `${warmUp} ${built("init", "1s", "2s")}`
    );
    const log = [...before, success, ...after];
    expect(log.findIndex((l) => l.includes(forgedCommand))).toBeLessThan(log.indexOf(success));
    expect(run(log)).toEqual({ code: 0, out: "reviewer 102\n", err: "" });
  });

  test("with no success line, the forged one still present, it prints nothing and exits 1", () => {
    expect(run([...before, ...after])).toEqual({ code: 1, out: "", err: "" });
  });
});
