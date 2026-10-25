import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// pod_runs POD ROLE COMMAND (stage4b-sandbox-tree.sh): whether a process whose command line
// contains COMMAND runs in ROLE's container, read from pod_commands (the container's /proc through
// kubectl exec). It runs under the script's `set -Eeuo pipefail` (lib/rig.sh:16-21 names the
// hazard): an early-exiting consumer downstream of a live producer turns the producer's SIGPIPE
// into the pipeline's status, so a match followed by more output than the pipe holds reads as
// absent. The run at 59e0337c failed a since-removed checkpoint that way: a planner's hold
// `/usr/bin/sleep 13` stood at line 118 of a 37 KB process list, 20 KB before its end, and every
// poll answered false. pod_runs is taken from the script by name and run over a pod_commands stub that prints the
// match with a process list after it; the stub writes from a subprocess, as kubectl does, so a
// consumer that stops reading early gives it SIGPIPE.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "pod-runs-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

function podRuns(listing: string, command: string): { ok: boolean; status: number } {
  const file = join(dir, "listing");
  writeFileSync(file, listing);
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
# pod_commands as kubectl exec prints a container's /proc: a child process writing the whole
# list, so an early close of the pipe reaches a live writer.
pod_commands() { cat "$LISTING"; }
${fn("pod_runs")}
pod_runs pod role "$COMMAND"
`,
    ],
    { env: { PATH: process.env.PATH ?? "", LISTING: file, COMMAND: command } }
  );
  return { ok: result.exitCode === 0, status: result.exitCode ?? -1 };
}

// A process list of the shape the planner's container printed at 59e0337c: the hold, then the
// listing shell, then Oh My Pi's own argv with its long prompt, well past a pipe's buffer.
const filler = `node /opt/legion/omp --mode rpc --append-system-prompt ${"x".repeat(200_000)}\n`;

describe("pod_runs", () => {
  test("finds a command that runs, however much of the process list follows it", () => {
    const got = podRuns(
      `/usr/bin/sleep 13 \nsh -c for f in /proc/[0-9]*/cmdline; do tr\n${filler}`,
      "/usr/bin/sleep 13"
    );
    expect(
      got,
      `pod_runs read a running hold as absent (exit ${got.status}): the producer's SIGPIPE became the pipeline's status`
    ).toEqual({ ok: true, status: 0 });
  });

  test("finds a command at the end of the process list, and refuses one that does not run", () => {
    expect(podRuns(`${filler}/usr/bin/sleep 13 \n`, "/usr/bin/sleep 13").ok).toBe(true);
    expect(podRuns(`${filler}/usr/bin/sleep 14 \n`, "/usr/bin/sleep 13").ok).toBe(false);
  });
});
