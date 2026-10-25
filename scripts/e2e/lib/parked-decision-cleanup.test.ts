import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// wait_for_run_processes, the cleanup-is-complete checkpoint's wait for every process of the run to
// end (parked-decision-devbox.sh). Each case takes the function from the script by name and runs it
// under the script's own `set` line, with stubs for the helpers it calls: run_processes, which
// counts its calls in a file, and note, fail and since.
const scriptPath = join(import.meta.dir, "..", "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);
const dir = mkdtempSync(join(tmpdir(), "parked-decision-cleanup-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

let runs = 0;
// wait runs `wait_for_run_processes ARGS` with RUN_PROCESSES as run_processes's body; the body sees
// the call it is in as $call, counting from 1.
function wait(runProcesses: string, args: string) {
  const calls = join(dir, `calls-${++runs}`);
  writeFileSync(calls, "");
  const script = `${set}
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL: %s\\n' "$*" >&2; exit 1; }
since() { printf '%ss' "$((SECONDS - $1))"; }
run_processes() {
  echo call >>"$CALLS"
  local call
  call=$(wc -l <"$CALLS")
  ${runProcesses}
}
${fn("wait_for_run_processes")}
wait_for_run_processes ${args}`;
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: { PATH: process.env.PATH, CALLS: calls },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    calls: readFileSync(calls, "utf8").split("\n").filter(Boolean).length,
  };
}

describe("wait_for_run_processes", () => {
  test("a poll that costs more than the whole deadline does not end the wait", () => {
    // The first walk outlasts the deadline and still finds a process; the next finds none. Like the
    // real walk, the stub's status is that of the last thing it ran: 1 here.
    const r = wait('if ((call == 1)); then sleep 1.1; echo 4242; fi; [ "$call" = 1 ]', "0 2");
    expect({ code: r.code, stderr: r.stderr }).toEqual({ code: 0, stderr: "" });
    expect(r.calls).toBeGreaterThan(1);
  });

  test("a process of the run that never ends fails the wait once the poll floor is reached", () => {
    const r = wait("printf '%s\\n' 4242 4343", "0 3");
    expect(r.code).not.toBe(0);
    expect(r.calls).toBe(3);
    expect(r.stderr).toContain("4242");
    expect(r.stderr).toContain("4343");
  });

  test("a call with no arguments waits the production bounds, 600 s and 10 polls", () => {
    // The walk finds nothing at once and, like the real walk, exits 1 from its last test.
    const r = wait("false", "");
    expect({ code: r.code, stderr: r.stderr }).toEqual({ code: 0, stderr: "" });
    const announced = r.stdout.split("\n")[0].match(/\d+/g)?.map(Number);
    expect(announced).toEqual([600, 10]);
  });
});
