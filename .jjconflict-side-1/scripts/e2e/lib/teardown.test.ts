import { describe, expect, test } from "bun:test";
import { join } from "node:path";

// Regression for the bug this file's own doc comment describes: an interrupt during bridge_stop
// (simulated here as bridge_stop itself returning nonzero, the same effect a signal or a set -e
// abort inside it has) must not skip teardown, the scratch database, the reminter or
// namespace_clean on the retry that follows — the single `teardown_sequence_done` flag this
// replaced made exactly that mistake, since it was set before bridge_stop ever ran.
const teardownPath = join(import.meta.dir, "teardown.sh");

function run(script: string) {
  return Bun.spawnSync({
    cmd: ["bash", "-c", script],
    stdout: "pipe",
    stderr: "pipe",
  });
}

describe("teardown_sequence", () => {
  test("a bridge_stop failure on the first call still runs every other step, and retries only bridge_stop", () => {
    const script = `
      set -euo pipefail
      calls=/tmp/teardown-test-calls-$$
      : >"$calls"
      bridge_stop_attempt=0
      bridge_stop() {
        bridge_stop_attempt=$((bridge_stop_attempt + 1))
        echo "bridge_stop:$bridge_stop_attempt" >>"$calls"
        [ "$bridge_stop_attempt" -gt 1 ] || return 1
      }
      teardown() { echo "teardown" >>"$calls"; }
      namespace_clean() { echo "namespace_clean" >>"$calls"; compared=1; }
      work=/tmp/teardown-test-work-$$
      mkdir -p "$work"
      session_db_started=
      agent_secrets_reminter=
      compared=
      snapshotted=1
      check=harness
      ok=
      evidence=/tmp/teardown-test-evidence-$$
      . "${teardownPath}"
      teardown_sequence && echo "FIRST CALL SHOULD HAVE FAILED" || echo "first call failed as expected (bridge_stop failed)"
      teardown_sequence
      echo "---calls---"
      cat "$calls"
      rm -rf "$work" "$calls" "$evidence"
    `;
    const result = run(script);
    const stdout = result.stdout.toString();
    const stderr = result.stderr.toString();
    expect(result.exitCode, `stderr: ${stderr}`).toBe(0);
    expect(stdout).toContain("first call failed as expected");
    const calls = stdout.split("---calls---\n")[1]?.trim().split("\n") ?? [];
    // bridge_stop fails on the first call, but teardown and namespace_clean still run in that same
    // call (best effort — one step failing never skips the rest). The second call retries only
    // bridge_stop (the step that did not succeed); teardown and namespace_clean, already done, do
    // not run again.
    expect(calls).toEqual(["bridge_stop:1", "teardown", "namespace_clean", "bridge_stop:2"]);
  });

  test("a fully completed sequence is a no-op on a second call", () => {
    const script = `
      set -euo pipefail
      calls=/tmp/teardown-test-calls-$$
      : >"$calls"
      bridge_stop() { echo "bridge_stop" >>"$calls"; }
      teardown() { echo "teardown" >>"$calls"; }
      namespace_clean() { echo "namespace_clean" >>"$calls"; compared=1; }
      work=/tmp/teardown-test-work-$$
      mkdir -p "$work"
      session_db_started=
      agent_secrets_reminter=
      compared=
      snapshotted=1
      check=harness
      ok=1
      evidence=/tmp/teardown-test-evidence-$$
      . "${teardownPath}"
      teardown_sequence
      teardown_sequence
      echo "---calls---"
      cat "$calls"
      rm -rf "$work" "$calls" "$evidence"
    `;
    const result = run(script);
    const stdout = result.stdout.toString();
    const stderr = result.stderr.toString();
    expect(result.exitCode, `stderr: ${stderr}`).toBe(0);
    const calls = stdout.split("---calls---\n")[1]?.trim().split("\n") ?? [];
    // Every step ran exactly once total, across both calls: the second call is a pure no-op.
    expect(calls).toEqual(["bridge_stop", "teardown", "namespace_clean"]);
  });

  test("namespace_clean does not run when the run never snapshotted", () => {
    const script = `
      set -euo pipefail
      calls=/tmp/teardown-test-calls-$$
      : >"$calls"
      bridge_stop() { echo "bridge_stop" >>"$calls"; }
      teardown() { echo "teardown" >>"$calls"; }
      namespace_clean() { echo "namespace_clean" >>"$calls"; compared=1; }
      work=/tmp/teardown-test-work-$$
      mkdir -p "$work"
      session_db_started=
      agent_secrets_reminter=
      compared=
      snapshotted=
      check=prerequisites
      ok=
      evidence=/tmp/teardown-test-evidence-$$
      . "${teardownPath}"
      teardown_sequence
      echo "---calls---"
      cat "$calls"
      rm -rf "$work" "$calls" "$evidence"
    `;
    const result = run(script);
    const stdout = result.stdout.toString();
    const stderr = result.stderr.toString();
    expect(result.exitCode, `stderr: ${stderr}`).toBe(0);
    const calls = stdout.split("---calls---\n")[1]?.trim().split("\n").filter(Boolean) ?? [];
    expect(calls).toEqual(["bridge_stop", "teardown"]);
  });
});
