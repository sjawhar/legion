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

// run runs BODY after the guards, with the rig's globals set, a scratch directory of its own in
// $scratch, and two helpers. spawn ARGS… starts ARGS in the background, its output away from the
// test's pipes, and sets pid to its process. await_child EXE sets child to pid's first child once
// that child is running EXE (its /proc/<pid>/exe), not merely once it exists: a forked child carries
// its parent's command line until it execs, and a guard asked about it then answers for the parent.
// Every process either names is killed when BODY ends.
function run(body: string) {
  const prelude = `set -Eeuo pipefail
${guards}
work=$WORK claude_bin=$CLAUDE port_dispatch=1 port_listener=2 port_nats=3 state=/rig
scratch=$(mktemp -d "$SCRATCH/run.XXXXXX")
started=()
trap 'kill -- "\${started[@]}" 2>/dev/null || true' EXIT
spawn() {
  "$@" </dev/null >/dev/null 2>&1 &
  pid=$!
  started+=("$pid")
}
await_child() {
  local want seen= i
  want=$(realpath -e -- "$1")
  child=
  for i in $(seq 1 200); do
    child=$(cat "/proc/$pid/task/$pid/children" 2>/dev/null) || true
    child=\${child%% *}
    if [ -n "$child" ]; then
      seen=$(readlink "/proc/$child/exe" 2>/dev/null) || seen=
      [ "$seen" != "$want" ] || break
    fi
    sleep 0.05
  done
  [ -z "$child" ] || started+=("$child")
  [ -n "$child" ] && [ "$seen" = "$want" ] ||
    { echo "no child of $pid running $want within 10 s (child \${child:-none}, running \${seen:-nothing})" >&2; exit 1; }
}
`;
  const result = Bun.spawnSync(["bash", "-c", `${prelude}\n${body}`], {
    cwd,
    env: { PATH: process.env.PATH ?? "", WORK: work, CLAUDE: claude, SCRATCH: dir },
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
await_child "$claude_bin"
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
await_child "$(command -v sleep)"
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
await_child "$claude_bin"
if bridged "$child"; then echo "worker-shim: accepted"; else echo "worker-shim: refused"; fi
spawn "$work/legion" claude-bridge -- "$(command -v sleep)"
await_child "$(command -v sleep)"
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
await_child "$claude_bin"
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
  // environment is the one it was given, failing the run when it never is.
  const agent = (env: string[]) => `
spawn env -i ${env.join(" ")} "$(command -v sleep)" 600
ready=
for i in $(seq 1 200); do grep -qz '^ENVOY_NATS_URL=' "/proc/$pid/environ" 2>/dev/null && { ready=1; break; }; sleep 0.05; done
[ -n "$ready" ] || { echo "the agent stand-in $pid never carried its environment" >&2; exit 1; }
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
    "reports an environment it cannot read as not checked, never clean, while the agent still runs with a credential variable in it",
    () => {
      // A process that makes itself non-dumpable (prctl PR_SET_DUMPABLE 0) has its /proc entries
      // owned by root, so its own user can no longer read its environment. The stand-in writes its
      // ready file only once prctl succeeded and PR_GET_DUMPABLE reads 0, and keeps its stderr; the
      // run asserts it is still running, not a zombie, on both sides of the check.
      const r = run(`
ready=$scratch/ready err=$scratch/stderr
env -i ${rig.join(" ")} CLAUDE_CODE_OAUTH_TOKEN=dummy "$(command -v python3)" -c '
import ctypes, os, sys, time
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(4, 0, 0, 0, 0) != 0:
    sys.exit("prctl(PR_SET_DUMPABLE, 0) failed: errno %d" % ctypes.get_errno())
if libc.prctl(3, 0, 0, 0, 0) != 0:
    sys.exit("PR_GET_DUMPABLE is not 0 after PR_SET_DUMPABLE 0")
with open(sys.argv[1] + ".tmp", "w") as f:
    f.write("not dumpable\\n")
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(600)
' "$ready" </dev/null >/dev/null 2>"$err" &
pid=$!
started+=("$pid")
state() { local s; s=$(cat "/proc/$1/stat" 2>/dev/null) || { echo gone; return; }; s=\${s##*) }; echo "\${s%% *}"; }
for i in $(seq 1 200); do
  [ ! -s "$ready" ] || break
  case $(state "$pid") in Z | X | gone) break ;; esac
  sleep 0.05
done
[ -s "$ready" ] || { echo "the stand-in $pid did not start (state $(state "$pid")): $(cat "$err")" >&2; exit 1; }
echo "fixture: $(cat "$ready")"
echo "before: $(state "$pid")"
if { : <"/proc/$pid/environ"; } 2>/dev/null; then echo "environ: readable"; else echo "environ: unreadable"; fi
${check}
echo "after: $(state "$pid")"
printf 'stderr: [%s]\\n' "$(cat "$err")"`);
      expect(r.err).toBe("");
      const lines = r.out.trim().split("\n");
      expect(lines[0]).toBe("fixture: not dumpable");
      expect(lines[1]).toMatch(/^before: [RSD]$/);
      expect(lines[2]).toBe("environ: unreadable");
      expect(lines[3]).toMatch(
        /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
      );
      expect(lines[4]).toMatch(/^after: [RSD]$/);
      expect(lines[5]).toBe("stderr: []");
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
