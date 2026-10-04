import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The 4b.13b acceptance's own soft and cleanup, taken from the script by name and run after the
// ending each case names, with the teardown's processes, GitHub and production audit stubbed and a
// merger starve seeded in the key command's record, so any notes call the trap makes prints.
const script = readFileSync(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"), "utf8");
const fn = (name: string) => {
  const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(script);
  if (found === null) throw new Error(`stage3-4b13b-acceptance.sh defines no ${name}()`);
  return found[0];
};
const dir = mkdtempSync(join(tmpdir(), "stage3-4b13b-notes-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(join(bin, "tmux"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });

let runs = 0;
// acceptanceRun runs the ending after the checks' own variables, then the trap.
function acceptanceRun(ending: string) {
  const run = join(dir, `run-${++runs}`);
  const evidence = join(run, "evidence");
  mkdirSync(evidence, { recursive: true });
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
work=${JSON.stringify(join(run, "work"))} evidence=${JSON.stringify(evidence)}
unserved_reader=${JSON.stringify(join(import.meta.dir, "model-gateway-unserved.sh"))}
ok= audited= prod_baseline= daemon_pid= watcher_pid= dispatch_pid= listener_pid= bridge_pid= nats_pid= ptoken=x
check=setup check_started=2026-09-30T11:00:00Z
soft_failures="$evidence/soft-failures.txt"
mkdir -p "$work" "$evidence/model-gateway"
: >"$soft_failures"
# The merger starved during merge-held and was never served again.
printf '%s\\t%s\\t%s\\t%s\\t%s\\t%s\\n' 2026-09-30T12:00:10Z 4242 /x merger/1 timeout no-key >"$evidence/model-gateway/hawk-token.calls"
stop_pid() { :; }; run_processes() { :; }; collect_transcripts() { :; }; github_cleanup() { :; }
first_soft_check=
first_soft_since=
${fn("soft")}
${fn("fail")}
${fn("cleanup")}
trap cleanup EXIT
${ending}
`,
    ],
    { env: { PATH: `${bin}:${process.env.PATH}` } }
  );
  return { code: result.exitCode, output: `${result.stdout}${result.stderr}` };
}

describe("the 4b.13b acceptance's notes", () => {
  test("are for the check that failed, when a check fails", () => {
    const failed = acceptanceRun(
      'check=merge-held check_started=2026-09-30T12:00:00Z\nfail "the merger was not held"'
    );
    expect(failed.code).toBe(1);
    expect(failed.output).toContain(
      "model-gateway-unserved: 1 agent(s) may have failed check merge-held for want of a model key"
    );
  });

  test("are for the first soft-failing check, from its start, when the run ends on its soft failures", () => {
    const soft = acceptanceRun(`check=merge-held check_started=2026-09-30T12:00:00Z
soft "the merger was not held"
check=reviewer-pair check_started=2026-09-30T12:05:00Z
soft "the reviewer pair did not answer"
check=profile-stays-in-the-run check_started=2026-09-30T12:30:00Z
ok=1
exit 1`);
    expect(soft.code).toBe(1);
    expect(soft.output).toContain(
      "model-gateway-unserved: 1 agent(s) may have failed check merge-held for want of a model key"
    );
    expect(soft.output).not.toContain("check profile-stays-in-the-run");
  });

  test("are none when every check passed and only the PASS line could not be written", () => {
    const pass = acceptanceRun(`check=profile-stays-in-the-run check_started=2026-09-30T12:30:00Z
ok=1
echo "acceptance 4b.13b: PASS at x" >/dev/full`);
    expect(pass.code).toBe(1);
    // No check failed, so the reader is not run at all, not even to refuse an empty check.
    expect(pass.output).not.toContain("model-gateway-unserved:");
  });
});
