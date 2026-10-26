import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's `kill` end of one role: role_kill_sh signals the worker-shim of one role-init, and
// role_generation_ended watches the kill land on that role alone. role_kill_sh runs here on real
// processes this test starts: a stand-in role-init whose children are a stand-in shim (argv
// `legion worker-shim`, the leader of a process group of its own) and a helper the agent detached.
// role_generation_ended runs as the script defines it, with the cluster reads stubbed.
const lib = join(import.meta.dir, "role-namespaces.sh");
const stage4b = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const fn = scriptFunctions(stage4b);
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

  test("a child naming worker-shim in a later argv word is not the shim", () => {
    // argv `bash -c <script> worker-shim`: worker-shim is argv[3], not argv[1]. The decoy waits in
    // bash's builtin read on a FIFO it holds open itself, so it starts no process of its own.
    const fifo = join(dir, "decoy.fifo");
    const lines = `. ${JSON.stringify(lib)}\nmkfifo ${JSON.stringify(fifo)}\nbash -c 'exec 3<>"$1"; read -r -t 300 -u 3 _' worker-shim ${JSON.stringify(fifo)} & h=$!\nsleep 0.2\nsh -c "$role_kill_sh" role-kill $$; rc=$?\nkill -0 $h && echo "decoy alive"\nkill -KILL $h\nexit $rc`;
    const kill = Bun.spawnSync(["bash", "-c", lines]);
    expect(kill.exitCode).toBe(1);
    expect(kill.stderr.toString()).toContain("runs no worker-shim");
    expect(kill.stdout.toString()).toBe("decoy alive\n");
  }, 120_000);
});

describe("role_generation_ended", () => {
  let runs = 0;
  // BEFORE is the pod's role generations at the kill, AFTER what a poll reads; OP is the pod uid
  // the poll's `op get pod` prints, `fails` for a read that exits 1 with nothing printed, or the
  // empty string for the pod the API server answers for and does not have (`--ignore-not-found`).
  // The op stub records its arguments, so the read's own `--ignore-not-found` is proved too.
  function poll(before: string, after: string, op = "uid-a") {
    const evidence = join(dir, `evidence-${++runs}`);
    const args = join(dir, `op-args-${runs}.txt`);
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -euo pipefail
mkdir -p ${JSON.stringify(evidence)}
evidence=${JSON.stringify(evidence)}
ended_pod=pod-a ended_pod_uid=uid-a ended_role=implementer ended_generation=2 ended_identity=container-1
ended_peers=$(awk '$1 != "implementer"' <<<"$BEFORE")
op() { echo "$*" >>${JSON.stringify(args)}; [ "$OP" != fails ] || return 1; echo "$OP"; }
roles_container_identity() { echo container-1; }
role_inits() { printf '%s' "$AFTER"; }
${fn("role_generation_ended")}
if role_generation_ended; then echo ended; else echo waiting; fi`,
      ],
      { env: { ...process.env, BEFORE: before, AFTER: after, OP: op } }
    );
    expect(result.stderr.toString()).toBe("");
    const violation = join(evidence, "pane-endpoint-violation.txt");
    return {
      out: result.stdout.toString(),
      violation: existsSync(violation) ? readFileSync(violation, "utf8") : "",
      op: existsSync(args) ? readFileSync(args, "utf8") : "",
    };
  }
  const before = "architect 1 10\nimplementer 2 20\nplanner 1 30\n";
  // Every poll reads the pod with --ignore-not-found, so a pod the kill deleted reads empty
  // rather than failing.
  const read = "get pod pod-a -o jsonpath={.metadata.uid} --ignore-not-found\n";

  test("the killed generation still running is a wait", () => {
    expect(poll(before, before)).toEqual({ out: "waiting\n", violation: "", op: read });
  });

  test("the killed generation gone, every peer as it was, is the end", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\n")).toEqual({
      out: "ended\n",
      violation: "",
      op: read,
    });
  });

  test("a sibling's first launch meanwhile is no violation", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\ntester 1 40\n")).toEqual({
      out: "ended\n",
      violation: "",
      op: read,
    });
  });

  test("a pod read that fails is a wait, no violation", () => {
    expect(poll(before, "architect 1 10\nplanner 1 30\n", "fails")).toEqual({
      out: "waiting\n",
      violation: "",
      op: read,
    });
  });

  // A kill that took the whole pod is the loudest violation of all, and the one a wait would hide
  // behind its own timeout: the pod is gone, so no later poll can ever say more.
  test("a pod the kill deleted is a violation naming it, never a wait for the pod to come back", () => {
    const result = poll(before, "", "");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toBe(
      "the kill of implementer generation 2 deleted pod pod-a (uid uid-a)\n"
    );
  });

  test("a pod of another uid is a violation", () => {
    const result = poll(before, before, "uid-b");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toContain("replaced pod uid-a");
  });

  test("a peer gone or replaced is a violation naming it", () => {
    const result = poll(before, "architect 2 11\nplanner 1 30\n");
    expect(result.out).toBe("waiting\n");
    expect(result.violation).toContain(
      "ended another role's generation in pod pod-a: gone architect 1 10;"
    );
  });
});

// The teardown removes full-agent-negative's workspace file with the planner killed: the removal
// must not go through the planner's namespaces, which a killed planner no longer has.
describe("remove_negative_config", () => {
  function teardown(pod: string) {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -euo pipefail
negative_config_pod=${JSON.stringify(pod)} negative_config_workspace=/legion/workspaces/o/r/legsmoke-3
pod_exec() { echo "no role-init of $2 runs in this container" >&2; return 1; }
roles_exec() { echo "roles_exec $*"; }
${fn("remove_negative_config")}
remove_negative_config && echo removed`,
    ]);
    return { out: result.stdout.toString(), code: result.exitCode };
  }

  test("removes the file in the roles container, no planner needed", () => {
    expect(teardown("pod-3")).toEqual({
      out: "roles_exec pod-3 rm -f /legion/workspaces/o/r/legsmoke-3/.omp/config.yml\nremoved\n",
      code: 0,
    });
  });

  test("no file written is nothing to remove", () => {
    expect(teardown("")).toEqual({ out: "removed\n", code: 0 });
  });
});

// Every script that enters a role finds its worker-shim by argv[1] alone, through role_enter_sh,
// rather than by a matcher of its own that a later argv word could satisfy.
describe("role entry", () => {
  test("multi-repo-acceptance.sh enters a role through role-namespaces.sh's role_enter_sh", () => {
    const script = readFileSync(join(import.meta.dir, "..", "multi-repo-acceptance.sh"), "utf8");
    expect(script).toContain('. "$script_dir/lib/role-namespaces.sh"');
    expect(script).toContain('sh -c "$role_enter_sh" role-enter');
    expect(script).not.toMatch(/grep -qx worker-shim|^role_enter=/m);
  });

  test("role_enter_sh takes the shim by argv[1] alone", () => {
    const text = readFileSync(lib, "utf8");
    const enter = /^role_enter_sh='[\s\S]*?^'$/m.exec(text)?.[0] ?? "";
    expect(enter).toContain('| sed -n 2p)\\" = worker-shim ]');
    expect(enter).not.toContain("grep -qx worker-shim");
  });
});

// agent_pid_sh finds a role's Oh My Pi by the same rule: the process whose argv[0] is omp under a
// parent whose argv[1] is worker-shim. It runs here on real processes this test starts, since the
// host's own /proc is what it reads in a pod.
describe("agent_pid_sh", () => {
  const agentPidSh = /^agent_pid_sh='[\s\S]*?'$/m.exec(readFileSync(stage4b, "utf8"))?.[0] ?? "";
  // The shim stand-in is a bash whose script file is named worker-shim, so its argv is
  // `<argv0> worker-shim`; its child execs sleep under the name omp, as a pod's agent runs. It
  // lives in a directory of its own, since role_kill_sh's own stand-in makes a FIFO of that name.
  const shimDir = join(dir, "agent-pid");
  mkdirSync(shimDir);
  writeFileSync(join(shimDir, "worker-shim"), 'exec -a omp sleep 300 &\necho "agent $!"\nwait\n');

  // parent is the bash command line that becomes the agent's parent process.
  async function agentPid(parent: string) {
    const started = Bun.spawn(["bash", "-c", `cd ${JSON.stringify(shimDir)} && ${parent}`], {
      stdout: "pipe",
    });
    const reader = started.stdout.getReader();
    let out = "";
    while (!/agent \d+/.test(out)) {
      const { value, done } = await reader.read();
      if (done) throw new Error(`the stand-in shim ended early: ${out}`);
      out += new TextDecoder().decode(value);
    }
    const agent = Number(/agent (\d+)/.exec(out)?.[1]);
    try {
      // The agent's exec may not have happened yet; agent_pid_sh reads argv[0].
      for (let i = 0; i < 100; i++) {
        if (readFileSync(`/proc/${agent}/cmdline`, "utf8").split("\0")[0] === "omp") break;
        await Bun.sleep(20);
      }
      const probe = Bun.spawnSync([
        "bash",
        "-c",
        `${agentPidSh}\nsh -c "$agent_pid_sh"'\n    echo "$pid"'`,
      ]);
      return {
        found: probe.stdout.toString().trim(),
        err: probe.stderr.toString().trim(),
        agent: String(agent),
      };
    } finally {
      process.kill(agent, "SIGKILL");
      started.kill("SIGKILL");
      await started.exited;
    }
  }

  test("finds the one Oh My Pi whose parent's argv[1] is worker-shim", async () => {
    const run = await agentPid("exec -a legion bash worker-shim");
    expect(run.err).toBe("");
    expect(run.found).toBe(run.agent);
  }, 120_000);

  test("a parent naming worker-shim in a later argv word holds no agent", async () => {
    const run = await agentPid(
      "exec -a legion bash -c 'exec -a omp sleep 300 &\necho \"agent $!\"\nwait' launcher --role worker-shim"
    );
    expect(run.err).toContain("no Oh My Pi under a legion worker-shim");
    expect(run.found).toBe("");
  }, 120_000);
});
