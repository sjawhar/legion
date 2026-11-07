import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// stop_agent_daemons (parked-decision-devbox.sh), the teardown of what the run's Oh My Pi sessions
// start detached: a per-project daemon broker, a session leader of its own that writes broker.pid
// in its runtime directory, and the services it supervises. On the pinned build each service runs
// in a session of its own and ends when its broker ends; the stand-in instead keeps its two members
// in its own session, so the test also holds the function to every member of a broker's session.
// The stand-in is started from a tmux pane as Oh My Pi starts its broker, `setsid` into a session
// of its own with OMP_DAEMON_RUNTIME_DIR set, and runs two members: a service working outside the
// run's directory, found by that variable, and a helper working inside it. tmux kill-server ends
// the pane and leaves all three alive, as the brokers outlived cleanup-is-complete in the devbox
// run at 6512c7fc. The pane the stand-in broker starts is a real tmux pane, and the teardown this
// test is about is tmux kill-server's, so a host with no tmux runs nothing here and says what is
// missing. CI is not such a host: its `test` job runs `bun test scripts/e2e/lib` before its
// `Install tmux` step (.github/workflows/ci.yaml), and the row runs there anyway, because the
// ubuntu-24.04 image the job runs on already carries tmux.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const scriptPath = join(e2e, "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);

// Read once, at module scope: a call inside the test body would print the warning per row.
const skip = skipWithout("tmux");

const dir = temporaryDirectory("parked-decision-teardown-test");
const started: number[] = [];
// What the test started that no pid of its own names: a tmux server to kill, a session a process
// writes its own pid file for. Each is registered the moment the test starts it, before the waits
// that can time out, so an unfinished test body still leaves nothing of the run alive.
const teardowns: (() => void)[] = [];
afterAll(() => {
  for (const teardown of teardowns.reverse()) {
    try {
      teardown();
    } catch {}
  }
  for (const pid of started) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});

const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  // A zombie still answers signal 0; it is gone for every purpose here.
  return !readFileSync(`/proc/${pid}/stat`, "utf8")
    .replace(/^.*\) /, "")
    .startsWith("Z");
};
const sessionOf = (pid: number) =>
  Number(
    readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^.*\) /, "")
      .split(" ")[3]
  );
const membersOf = (sid: number) =>
  [...new Bun.Glob("[0-9]*").scanSync({ cwd: "/proc", onlyFiles: false })]
    .map(Number)
    .filter((pid) => {
      try {
        return sessionOf(pid) === sid && alive(pid);
      } catch {
        return false;
      }
    });
// Every live pid whose command line holds TEXT: how a teardown finds a process the test started
// in a session of its own before that process has named itself in a file.
const namingPids = (text: string) =>
  [...new Bun.Glob("[0-9]*").scanSync({ cwd: "/proc", onlyFiles: false })]
    .map(Number)
    .filter((pid) => {
      try {
        return readFileSync(`/proc/${pid}/cmdline`, "utf8").includes(text) && alive(pid);
      } catch {
        return false;
      }
    });
// killSession ends every member of the session SID leads, the whole of a stand-in broker. This
// test's own session is never one of them: a teardown that lands on a pid of the runner leaves it.
const killSession = (sid: number) => {
  if (sid === sessionOf(process.pid)) return;
  for (const pid of membersOf(sid)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
};
// until polls real processes, which no fake clock moves, for the condition it names.
async function until(what: string, ok: () => boolean) {
  for (let i = 0; i < 100; i++) {
    if (ok()) return;
    await Bun.sleep(100);
  }
  throw new Error(`timed out waiting for ${what}`);
}

// harness runs BODY with the script's `set` line, lib/rig.sh sourced as the script sources it (for
// run_processes) and stop_agent_daemons taken from the script by name. $work comes in through the
// environment, so the harness's own command line does not name it and run_processes does not name
// the harness.
function harness(work: string, body: string) {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `${set}
root=${JSON.stringify(root)}
work=$WORK
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL: %s\\n' "$*" >&2; exit 1; }
. "$root/scripts/e2e/lib/rig.sh"
${fn("stop_agent_daemons")}
${body}`,
    ],
    { env: { PATH: process.env.PATH ?? "", WORK: work }, stdout: "pipe", stderr: "pipe" }
  );
  return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

test.skipIf(skip)(
  "stops a detached broker's session that outlives tmux kill-server, and nothing it cannot tie to one",
  async () => {
    const work = join(dir, "work");
    const ws = join(work, "ws");
    const home = join(work, "home");
    const runtime = join(home, ".local/state/omp/profiles/p/run/daemons/0123456789abcdef");
    const stale = join(work, "state/omp/profiles/p/run/daemons/fedcba9876543210");
    for (const d of [ws, home, runtime, stale, join(work, "tmux")])
      mkdirSync(d, { recursive: true });
    // The stand-in broker lives outside $work, so only its working directory ties it to the run.
    const broker = join(dir, "fake-broker.sh");
    writeFileSync(
      broker,
      `#!/usr/bin/env bash
runtime=$1 home=$2
(cd / && exec sleep 300) &
(cd "$home" && exec sleep 301) &
printf '{"pid":%s,"domain":"test"}\\n' "$$" >"$runtime/broker.pid.next"
mv "$runtime/broker.pid.next" "$runtime/broker.pid"
wait
`,
      { mode: 0o755 }
    );
    const pane = `setsid -f env OMP_DAEMON_RUNTIME_DIR=${runtime} ${broker} ${runtime} ${home}; exec sleep 600`;

    // A process of the run that no broker started, in a session of its own: the teardown must leave it
    // for cleanup-is-complete's wait to name.
    const bystander = Bun.spawn(["setsid", "sleep", "303"], {
      cwd: ws,
      stdout: "ignore",
      stderr: "ignore",
    });
    // A broker.pid naming a pid whose session holds nothing of the run: a pid the kernel gave to
    // another session leader after the broker ended.
    const stranger = Bun.spawn(["setsid", "sleep", "304"], {
      cwd: dir,
      stdout: "ignore",
      stderr: "ignore",
    });
    started.push(bystander.pid, stranger.pid);
    writeFileSync(join(stale, "broker.pid"), JSON.stringify({ pid: stranger.pid, domain: "test" }));
    await until("the bystander and the stranger to lead sessions of their own", () =>
      [bystander.pid, stranger.pid].every((pid) => alive(pid) && sessionOf(pid) === pid)
    );

    const env = { PATH: process.env.PATH ?? "", HOME: home, TMUX_TMPDIR: join(work, "tmux") };
    const pidFile = join(runtime, "broker.pid");
    // The stand-in broker the pane starts leads a session of its own and names it in broker.pid.
    // Ending that session is registered before the pane that starts it, so a wait below that times
    // out before the test has read the pid still leaves no member of the broker's session running.
    // Before the file is there the broker's command line, which names the run's runtime directory,
    // is the only hold on it, `setsid -f` having already taken it out of the pane's session.
    teardowns.push(() => {
      const leaders = existsSync(pidFile)
        ? [Number(JSON.parse(readFileSync(pidFile, "utf8")).pid)]
        : namingPids(runtime);
      for (const pid of leaders) {
        try {
          killSession(sessionOf(pid));
        } catch {}
      }
    });
    const made = Bun.spawnSync(
      ["tmux", "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    // The server this started holds the pane; it outlives a test body that never reaches its own
    // kill-server, so the kill is registered here, where the server exists.
    teardowns.push(() => {
      Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    });
    expect(made.stderr.toString()).toBe("");
    const panePid = Number(
      Bun.spawnSync(["tmux", "-L", "teardown", "display-message", "-p", "#{pane_pid}"], {
        env,
      }).stdout.toString()
    );
    await until("the stand-in broker to write broker.pid", () => existsSync(pidFile));
    const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
    await until("the broker and its two members", () => membersOf(leader).length === 3);
    const members = membersOf(leader);

    // The pane goes, as cleanup-is-complete's kill-server takes it.
    Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    await until("the pane to end", () => !alive(panePid));
    // The red half: the broker's whole session outlives the pane, and run_processes, the check's own
    // reading, still names the members that work inside $work.
    expect(members.every(alive)).toBe(true);
    const before = harness(work, "run_processes || true")
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(before).toContain(leader);

    const stopped = harness(work, "stop_agent_daemons");
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    await until("every member of the broker's session to end", () => !members.some(alive));
    // What no broker of the run ties to the run is left alone, and the check still names the one that
    // works inside $work.
    expect(alive(bystander.pid)).toBe(true);
    expect(alive(stranger.pid)).toBe(true);
    const after = harness(work, "run_processes || true")
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(after).toEqual([bystander.pid]);
  },
  30_000
);
