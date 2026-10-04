import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Stage 4b's own blocked, fail, pass, until_reached, cleanup, audit_verdict and audit_failure,
// taken from the script by name and run after the controller checkpoint ends (blocked, failed, or
// passed as a STAGE4B_UNTIL run's last checkpoint), with the teardown's cluster, NATS and GitHub
// helpers stubbed and production_audit's Dispatch read replaced by its result: a write outside
// LEGSMOKE, or none.
const script = readFileSync(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"), "utf8");
const fn = (name: string) => {
  const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(script);
  if (found === null) throw new Error(`stage4b-sandbox-tree.sh defines no ${name}()`);
  return found[0];
};
const dir = mkdtempSync(join(tmpdir(), "stage4b-verdict-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(join(bin, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });

let runs = 0;
const outsideWrite =
  '[{"issue":"OTHER-12","seq":3,"type":"comment.created","actor":"legion-daemon:LEGSMOKE"}]';
// controllerRun ends the controller checkpoint in blocked, in fail (the case the notes are for), or
// in pass as the last checkpoint of a run with STAGE4B_UNTIL=controller.
function controllerRun(outside: string, ending: "blocked" | "fail" | "pass" = "blocked") {
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
${fn("cleanup")}
trap cleanup EXIT
${ending} "the controller's model route could not be installed"
`,
    ],
    { env: { PATH: `${bin}:${process.env.PATH}`, OUTSIDE: outside } }
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
});
