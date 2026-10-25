import { afterAll, describe, expect, test } from "bun:test";
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// The Claude Code backend proof's process guards (claude-backend-tmux.sh), taken from the script by
// name and run under its own `set -Eeuo pipefail` against live processes this suite starts:
// bridged, whether a claude's parent is the run's `legion claude-bridge -- <claude>`; claude_pid,
// a pane's Claude Code process; and endpoint_mismatch, an agent's environment against the rig.
const fn = scriptFunctions(join(import.meta.dir, "..", "claude-backend-tmux.sh"));
const guards = [
  "process_args",
  "read_environ",
  "parent_pid",
  "agent_descendant",
  "claude_pid",
  "bridged",
  "endpoint_mismatch",
]
  .map(fn)
  .join("\n");

const dir = mkdtempSync(join(tmpdir(), "claude-backend-guards-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
function tool(name: string): string {
  const found = Bun.which(name);
  if (found === null) throw new Error(`${name} is not on PATH`);
  return found;
}
// The run's legion is a copy of bash, so a process can carry it as its executable (/proc/<pid>/exe)
// with `claude-bridge` as its first argument: bash runs the script of that name from its working
// directory, which starts its second argument as its child and waits on it, as the bridge waits on
// Claude Code.
const work = join(dir, "work");
mkdirSync(work);
copyFileSync(tool("bash"), join(work, "legion"));
// The run's claude is a copy of sleep, under a directory whose name holds a space.
const claudeDir = join(dir, "bin with space");
mkdirSync(claudeDir);
const claude = join(claudeDir, "claude");
copyFileSync(tool("sleep"), claude);
const cwd = join(dir, "cwd");
mkdirSync(cwd);
for (const script of ["claude-bridge", "worker-shim"]) {
  writeFileSync(join(cwd, script), '"$2" 600\n:\n');
}

// run runs BODY after the guards, with the rig's globals set and two helpers: spawn ARGS… starts
// ARGS in the background, its output away from the test's pipes, and sets pid to its process;
// await_child sets child to pid's first child once it exists. Every process either names is killed
// when BODY ends.
function run(body: string) {
  const prelude = `set -Eeuo pipefail
${guards}
work=$WORK claude_bin=$CLAUDE port_dispatch=1 port_listener=2 port_nats=3 state=/rig
started=()
trap 'kill -- "\${started[@]}" 2>/dev/null || true' EXIT
spawn() {
  "$@" </dev/null >/dev/null 2>&1 &
  pid=$!
  started+=("$pid")
}
await_child() {
  local i
  child=
  for i in $(seq 1 200); do
    child=$(cat "/proc/$pid/task/$pid/children" 2>/dev/null) || true
    child=\${child%% *}
    [ -z "$child" ] || break
    sleep 0.05
  done
  [ -n "$child" ] || { echo "no child of $pid" >&2; exit 1; }
  started+=("$child")
}
`;
  const result = Bun.spawnSync(["bash", "-c", `${prelude}\n${body}`], {
    cwd,
    env: { PATH: process.env.PATH ?? "", WORK: work, CLAUDE: claude },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

const slow = 60_000;

describe("bridged", () => {
  test(
    "accepts the run's legion claude-bridge -- <claude> every time, with a pane-sized multi-line argument",
    () => {
      const r = run(`
big=$(for i in $(seq 1 3600); do printf 'line %05d of a system prompt\\n' "$i"; done)
spawn "$work/legion" claude-bridge -- "$claude_bin" "$big"
await_child
printf 'argv bytes %s\\n' "$(wc -c <"/proc/$pid/cmdline")"
ok=0
for i in $(seq 1 20); do if bridged "$child"; then ok=$((ok + 1)); fi; done
printf 'accepted %s of 20\\n' "$ok"
`);
      expect(r.err).toBe("");
      expect(r.out).toMatch(/^argv bytes 1\d{5}$/m);
      expect(r.out).toContain("accepted 20 of 20");
    },
    slow
  );

  test(
    "refuses a parent that is not the run's legion, whose argument only mentions the bridge",
    () => {
      const r = run(`
spawn bash -c 'sleep 600; :' prompt "your parent is $work/legion claude-bridge -- $claude_bin"
await_child
if bridged "$child"; then echo accepted; else echo refused; fi
`);
      expect(r.err).toBe("");
      expect(r.out.trim()).toBe("refused");
    },
    slow
  );

  test(
    "refuses the run's legion when claude-bridge is not its subcommand, or the child is not the run's claude",
    () => {
      const r = run(`
spawn "$work/legion" worker-shim -- "$claude_bin" claude-bridge -- "$claude_bin"
await_child
if bridged "$child"; then echo "worker-shim: accepted"; else echo "worker-shim: refused"; fi
spawn "$work/legion" claude-bridge -- "$(command -v sleep)"
await_child
if bridged "$child"; then echo "other claude: accepted"; else echo "other claude: refused"; fi
`);
      expect(r.err).toBe("");
      expect(r.out).toContain("worker-shim: refused");
      expect(r.out).toContain("other claude: refused");
    },
    slow
  );
});

describe("claude_pid", () => {
  test(
    "finds Claude Code under the bridge when its executable's path holds a space",
    () => {
      const r = run(`
spawn "$work/legion" claude-bridge -- "$claude_bin"
await_child
found=$(claude_pid "$pid") || found=none
if [ "$found" = "$child" ]; then echo "found the child"; else echo "found $found, want $child"; fi
`);
      expect(r.err).toBe("");
      expect(r.out.trim()).toBe("found the child");
    },
    slow
  );
});

describe("endpoint_mismatch", () => {
  const rig = [
    "DISPATCH_URL=http://127.0.0.1:1",
    "DISPATCH_TOKEN_FILE=/rig/secrets/dispatch-token",
    "ENVOY_URL=http://127.0.0.1:2",
    "ENVOY_NATS_URL=nats://127.0.0.1:3",
  ];
  // agent ENV… starts an agent stand-in, sleep under env -i with exactly ENV, and checks it once its
  // environment is the one it was given.
  const agent = (env: string[]) => `
spawn env -i ${env.join(" ")} "$(command -v sleep)" 600
for i in $(seq 1 200); do grep -qz '^ENVOY_NATS_URL=' "/proc/$pid/environ" 2>/dev/null && break; sleep 0.05; done
${check}`;
  const check = `status=0
found=$(endpoint_mismatch "$pid") || status=$?
printf 'status %s: %s\\n' "$status" "$found"`;

  test(
    "is clean for an agent with the rig's endpoints and no credential variable",
    () => {
      expect(run(agent(rig)).out.trim()).toBe("status 1:");
    },
    slow
  );

  test(
    "reports a credential variable or a wrong endpoint as a mismatch",
    () => {
      expect(run(agent([...rig, "CLAUDE_CODE_OAUTH_TOKEN=dummy"])).out.trim()).toBe(
        "status 0: CLAUDE_CODE_OAUTH_TOKEN set, want unset"
      );
      const wrong = rig.map((pair) =>
        pair.startsWith("ENVOY_URL=") ? "ENVOY_URL=http://127.0.0.1:9" : pair
      );
      expect(run(agent(wrong)).out.trim()).toBe(
        "status 0: ENVOY_URL=http://127.0.0.1:9, want http://127.0.0.1:2"
      );
    },
    slow
  );

  test(
    "reports an environment it cannot read as not checked, never clean, with a credential variable in it",
    () => {
      // A process that makes itself non-dumpable (prctl PR_SET_DUMPABLE 0) has its /proc entries
      // owned by root, so its own user can no longer read its environment.
      const r = run(`
spawn env -i ${rig.join(" ")} CLAUDE_CODE_OAUTH_TOKEN=dummy "$(command -v python3)" -c 'import ctypes, time; ctypes.CDLL(None).prctl(4, 0, 0, 0, 0); time.sleep(600)'
for i in $(seq 1 200); do { : <"/proc/$pid/environ"; } 2>/dev/null || break; sleep 0.05; done
${check}`);
      expect(r.out.trim()).toMatch(
        /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
      );
    },
    slow
  );

  test(
    "reports an agent that has exited as not checked",
    () => {
      const r = run(`true &\npid=$!\nwait "$pid"\n${check}`);
      expect(r.out.trim()).toMatch(
        /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
      );
    },
    slow
  );
});
