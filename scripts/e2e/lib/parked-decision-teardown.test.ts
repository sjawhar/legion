import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// stop_agent_daemons (parked-decision-devbox.sh), the teardown of what the run's Oh My Pi sessions
// start detached: a per-project daemon broker, a session leader of its own that writes broker.pid
// in its runtime directory, and the services it supervises. The stand-in below is started from a
// tmux pane as Oh My Pi starts its broker, `setsid` into a session of its own with
// OMP_DAEMON_RUNTIME_DIR set, and runs two members: a service working outside the run's directory,
// found by that variable, and a helper working inside it. tmux kill-server ends the pane and leaves
// all three alive, which is how the devbox run at 6512c7fc failed cleanup-is-complete. Where no
// tmux is on PATH (CI installs it after this job's step), the pane is a session of its own that the
// test ends as kill-server ends a pane: SIGHUP, then SIGKILL, to the pane's session.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const scriptPath = join(e2e, "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);

const dir = mkdtempSync(join(tmpdir(), "parked-decision-teardown-test."));
const started: number[] = [];
afterAll(() => {
  for (const pid of started) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  rmSync(dir, { recursive: true, force: true });
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

test("stops a detached broker's session that outlives tmux kill-server, and nothing it cannot tie to one", async () => {
  const work = join(dir, "work");
  const ws = join(work, "ws");
  const home = join(work, "home");
  const runtime = join(home, ".local/state/omp/profiles/p/run/daemons/0123456789abcdef");
  const stale = join(work, "state/omp/profiles/p/run/daemons/fedcba9876543210");
  for (const d of [ws, home, runtime, stale, join(work, "tmux")]) mkdirSync(d, { recursive: true });
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

  const tmux = Bun.which("tmux");
  const env = { ...process.env, TMUX_TMPDIR: join(work, "tmux") };
  let paneSession: number | undefined;
  let panePid: number;
  if (tmux) {
    const made = Bun.spawnSync(
      [tmux, "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    expect(made.stderr.toString()).toBe("");
    panePid = Number(
      Bun.spawnSync([tmux, "-L", "teardown", "display-message", "-p", "#{pane_pid}"], {
        env,
      }).stdout.toString()
    );
  } else {
    const shell = Bun.spawn(["setsid", "bash", "-c", pane], {
      cwd: ws,
      stdout: "ignore",
      stderr: "ignore",
    });
    started.push(shell.pid);
    paneSession = shell.pid;
    panePid = shell.pid;
  }
  started.push(panePid);
  const pidFile = join(runtime, "broker.pid");
  await until("the stand-in broker to write broker.pid", () => existsSync(pidFile));
  const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
  started.push(leader);
  await until("the broker and its two members", () => membersOf(leader).length === 3);
  const members = membersOf(leader);
  started.push(...members);

  // The pane goes, as cleanup-is-complete's kill-server takes it.
  if (tmux) {
    Bun.spawnSync([tmux, "-L", "teardown", "kill-server"], { env });
  } else if (paneSession !== undefined) {
    for (const signal of ["SIGHUP", "SIGKILL"] as const) {
      for (const pid of membersOf(paneSession)) {
        try {
          process.kill(pid, signal);
        } catch {}
      }
    }
  }
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
}, 30_000);
