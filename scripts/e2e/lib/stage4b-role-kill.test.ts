import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's `kill` end of one role: role_kill_sh signals the worker-shim of one role-init, and
// role_generation_ended watches the kill land on that role alone. role_kill_sh runs here on real
// processes this test starts: a stand-in role-init whose children are a stand-in shim (argv
// `legion worker-shim`, the leader of a process group of its own) and a helper the agent detached.
// role_generation_ended runs as the script defines it, with the cluster reads stubbed.
const lib = join(import.meta.dir, "role-namespaces.sh");
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-role-kill-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

// The polls below wait on real processes (an exec, a SIGKILL's delivery) that no fake clock drives,
// and role_kill_sh reads every process of the host's /proc, so a busy host takes seconds.
describe("role_kill_sh", () => {
  test("kills the shim's group and leaves a detached helper of the role-init alone", async () => {
    const init = Bun.spawn(
      [
        "bash",
        "-c",
        `cd ${JSON.stringify(dir)} && mkfifo worker-shim
setsid bash -c 'exec -a legion cat worker-shim' & echo "shim $!"
sleep 300 & echo "helper $!"
wait`,
      ],
      { stdout: "pipe" }
    );
    const reader = init.stdout.getReader();
    let out = "";
    while (!/helper \d+\n/.test(out)) {
      const { value, done } = await reader.read();
      if (done) throw new Error(`the stand-in role-init ended early: ${out}`);
      out += new TextDecoder().decode(value);
    }
    const shim = Number(/shim (\d+)/.exec(out)?.[1]);
    const helper = Number(/helper (\d+)/.exec(out)?.[1]);
    try {
      // The shim's argv is in place once setsid and bash have exec'd.
      for (let i = 0; i < 100; i++) {
        const argv = readFileSync(`/proc/${shim}/cmdline`, "utf8").split("\0");
        if (argv[1] === "worker-shim") break;
        await Bun.sleep(20);
      }
      const lines = `. ${JSON.stringify(lib)}\nsh -c "$role_kill_sh" role-kill ${init.pid}`;
      const kill = Bun.spawnSync(["bash", "-c", lines]);
      expect(kill.stderr.toString()).toBe("");
      expect(kill.exitCode).toBe(0);
      for (let i = 0; i < 100 && alive(shim); i++) await Bun.sleep(20);
      expect(alive(shim)).toBe(false);
      expect(alive(helper)).toBe(true);
    } finally {
      // Only the processes this test started, by the pids it recorded.
      for (const pid of [helper, shim, init.pid]) {
        if (alive(pid)) process.kill(pid, "SIGKILL");
      }
      await init.exited;
    }
  }, 120_000);

  test("a role-init with no worker-shim child is refused, nothing signalled", () => {
    const lines = `. ${JSON.stringify(lib)}\nsleep 300 & h=$!\nsh -c "$role_kill_sh" role-kill $$; rc=$?\nkill -0 $h && echo "helper alive"\nkill -KILL $h\nexit $rc`;
    const kill = Bun.spawnSync(["bash", "-c", lines]);
    expect(kill.exitCode).toBe(1);
    expect(kill.stderr.toString()).toContain("runs no worker-shim");
    expect(kill.stdout.toString()).toBe("helper alive\n");
  }, 120_000);
});

describe("role_generation_ended", () => {
  let runs = 0;
  // BEFORE is the pod's role generations at the kill, AFTER what a poll reads.
  function poll(before: string, after: string) {
    const evidence = join(dir, `evidence-${++runs}`);
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -euo pipefail
mkdir -p ${JSON.stringify(evidence)}
evidence=${JSON.stringify(evidence)}
ended_pod=pod-a ended_pod_uid=uid-a ended_role=implementer ended_generation=2 ended_identity=container-1
ended_peers=$(awk '$1 != "implementer"' <<<"$BEFORE")
op() { echo uid-a; }
roles_container_identity() { echo container-1; }
role_inits() { printf '%s' "$AFTER"; }
${fn("role_generation_ended")}
if role_generation_ended; then echo ended; else echo waiting; fi`,
      ],
      { env: { ...process.env, BEFORE: before, AFTER: after } }
    );
    expect(result.stderr.toString()).toBe("");
    const violation = join(evidence, "pane-endpoint-violation.txt");
    return {
      out: result.stdout.toString(),
      violation: existsSync(violation) ? readFileSync(violation, "utf8") : "",
    };
  }
  const before = "architect 1 10\nimplementer 2 20\nplanner 1 30\n";

  test("the killed generation still running is a wait", () => {
    expect(poll(before, before)).toEqual({ out: "waiting\n", violation: "" });
  });

  test("the killed generation gone, every peer as it was, is the end", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\n")).toEqual({
      out: "ended\n",
      violation: "",
    });
  });

  test("a sibling's first launch meanwhile is no violation", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\ntester 1 40\n")).toEqual({
      out: "ended\n",
      violation: "",
    });
  });

  test("a peer gone or replaced is a violation naming it", () => {
    const result = poll(before, "architect 2 11\nplanner 1 30\n");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toContain(
      "ended another role's generation in pod pod-a: gone architect 1 10;"
    );
  });
});
