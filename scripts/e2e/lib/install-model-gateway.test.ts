import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The key command install-model-gateway.sh writes, against a fake hawk-token first on PATH. The
// fake appends its pid to FAKE_MINT_LOG (one line per mint), sleeps FAKE_MINT_SLEEP seconds, then
// by FAKE_MINT_MODE prints a JWT that expires in an hour (ok), fails the way the devbox wrapper
// does when its 9000 ms budget runs out (budget), fails at once on the login (refused), or is
// killed by a signal (killed).
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "install-model-gateway-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
writeFileSync(
  join(bin, "hawk-token"),
  `#!/usr/bin/env bash
printf '%s\\n' "$$" >>"$FAKE_MINT_LOG"
sleep "\${FAKE_MINT_SLEEP:-0}"
case "\${FAKE_MINT_MODE:-ok}" in
ok)
  payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
  printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
  ;;
budget)
  echo "hawk-token: mint produced no token after 1 attempt(s) in 9021 ms of a 9000 ms budget; last stderr follows" >&2
  exit 1
  ;;
refused)
  echo "hawk-token: mint produced no token after 2 attempt(s) in 812 ms of a 9000 ms budget; last stderr follows" >&2
  echo "error: no usable hawk login: run hawk login" >&2
  exit 1
  ;;
killed) kill -KILL $$ ;;
esac
`,
  { mode: 0o755 }
);
const operatorHome = join(dir, "operator-home");
mkdirSync(operatorHome);

interface Fake {
  mode?: "ok" | "budget" | "refused" | "killed";
  sleep?: number;
  // The MODEL_GATEWAY_CALLS_FILE a harness names in one agent's environment.
  callsFile?: string;
  // A Legion pane's identity, as the Go tmux runtime sets it on the pane's environment.
  role?: string;
  generation?: string;
}
let installs = 0;
function env(mints: string, fake: Fake = {}) {
  return {
    PATH: `${bin}:${process.env.PATH}`,
    HOME: operatorHome,
    DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
    LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
    FAKE_MINT_LOG: mints,
    FAKE_MINT_MODE: fake.mode ?? "ok",
    FAKE_MINT_SLEEP: String(fake.sleep ?? 0),
    ...(fake.callsFile === undefined ? {} : { MODEL_GATEWAY_CALLS_FILE: fake.callsFile }),
    ...(fake.role === undefined ? {} : { LEGION_ROLE: fake.role }),
    ...(fake.generation === undefined ? {} : { LEGION_GENERATION: fake.generation }),
  };
}

// install writes a fresh profile's route, its preflight mint (with the given fake) included, and
// then removes the key that mint kept, so the next call finds the cache cold (the state the kept
// key's expiry leaves).
function install(fake: Fake = {}) {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const mints = join(run, "mints");
  const dest = join(run, "model-gateway");
  const cache = join(run, "model-gateway-cache");
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-model-gateway.sh"),
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      "--cache-dir",
      cache,
    ],
    { env: env(mints, fake) }
  );
  expect(result.exitCode).toBe(0);
  rmSync(join(cache, "hawk-token.key"));
  writeFileSync(mints, "");
  return { run, dest, cache, mints, keyCommand: result.stdout.toString().trim() };
}

// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string, mints: string, fake: Fake = {}) {
  mkdirSync(cwd, { recursive: true });
  const started = performance.now();
  const child = Bun.spawn(["/bin/sh", "-c", `exec ${keyCommand}`], {
    cwd,
    env: env(mints, fake),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  return { code, stdout: stdout.trim(), ms: performance.now() - started };
}

function unserved(...args: string[]) {
  const result = Bun.spawnSync(["bash", join(lib, "model-gateway-unserved.sh"), ...args], {
    env: { PATH: process.env.PATH ?? "" },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

const mintCount = (mints: string) => readFileSync(mints, "utf8").split("\n").filter(Boolean).length;
// The record's own time format, UTC to the second.
const now = () => new Date().toISOString().replace(/\.\d+Z$/, "Z");

describe("the model gateway key command", () => {
  test("mints once for a wave of callers that finds the kept key expired, and serves them all", async () => {
    const { run, mints, keyCommand } = install();
    const calls = await Promise.all(
      [1, 2, 3, 4, 5].map((n) => call(keyCommand, join(run, `pane-${n}`), mints, { sleep: 1 }))
    );

    expect(mintCount(mints)).toBe(1);
    for (const c of calls) expect(c.code).toBe(0);
    expect(new Set(calls.map((c) => c.stdout)).size).toBe(1);
    expect(calls[0].stdout).toMatch(/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  });

  test("tells an agent that starved from one whose login was refused or whose mint was killed, in its own file", async () => {
    const { run, mints, keyCommand } = install();
    const calls = (name: string) => join(run, name, "model-gateway-calls");
    const outcome = async (name: string, fake: Fake) => {
      const c = await call(keyCommand, join(run, name, "cwd"), mints, {
        ...fake,
        callsFile: calls(name),
      });
      return [c.code, c.stdout];
    };

    // What Oh My Pi sees is the same for each, which is why the run itself cannot tell them apart.
    expect(await outcome("starved", { mode: "budget" })).toEqual([1, ""]);
    expect(await outcome("refused", { mode: "refused" })).toEqual([1, ""]);
    expect(await outcome("killed", { mode: "killed" })).toEqual([1, ""]);
    // Each agent's own file does.
    const s = unserved("--record", calls("starved"));
    expect(s.code).toBe(75);
    expect(s.stdout).toContain(`in ${join(run, "starved", "cwd")} (pid `);
    expect(s.stdout).toContain("): timeout: ");
    expect(s.stdout).toContain("9021 ms of a 9000 ms budget");
    const r = unserved("--record", calls("refused"));
    expect(r.code).toBe(77);
    expect(r.stdout).toContain(": failed: ");
    expect(r.stdout).toContain("no usable hawk login");
    // A signal is no login problem: it is a starve, which a rerun can fix.
    const k = unserved("--record", calls("killed"));
    expect(k.code).toBe(75);
    expect(k.stdout).toContain(": killed: ");
    // An agent never called, and a path whose directory does not exist, which is a harness mistake.
    expect(unserved("--record", calls("never"))).toMatchObject({ code: 2 });
    mkdirSync(join(run, "never"));
    expect(unserved("--record", calls("never"))).toMatchObject({ code: 0, stdout: "" });
  });

  test("judges a run by its agent's last call, so a starve Oh My Pi retried past counts as served", async () => {
    const { run, mints, keyCommand } = install();
    const file = join(run, "pane", "model-gateway-calls");
    const pane = join(run, "pane", "cwd");
    await call(keyCommand, pane, mints, { mode: "budget", callsFile: file });
    expect(unserved("--record", file).code).toBe(75);

    // Oh My Pi retries the key command, and this time it mints.
    expect((await call(keyCommand, pane, mints, { callsFile: file })).code).toBe(0);
    expect(unserved("--record", file)).toMatchObject({ code: 0, stdout: "" });
  });

  test("tells apart the agents of one issue, which share its workspace, by their Legion pane", async () => {
    const { run, dest, mints, keyCommand } = install();
    const since = now();
    const workspace = join(run, "issue-workspace");
    // The tester's mint fails, and the architect's call in the same directory is served after it.
    await call(keyCommand, workspace, mints, { mode: "refused", role: "tester", generation: "1" });
    await call(keyCommand, workspace, mints, { role: "architect", generation: "2" });

    const notes = unserved("--notes", dest, since, "tester-handoff");
    expect(notes.code).toBe(0);
    const listed = notes.stdout.split("\n").filter((line) => line.startsWith("  "));
    expect(listed).toHaveLength(1);
    expect(listed[0]).toContain(` tester/1 in ${workspace} (pid `);
    expect(listed[0]).toContain(
      ": failed: hawk-token exited 1 without a key: error: no usable hawk login"
    );
    expect(listed[0]).not.toContain("served again");
    const record = readFileSync(join(dest, "hawk-token.calls"), "utf8");
    expect(record).toContain("\ttester/1\tfailed\t");
    expect(record).toContain("\tarchitect/2\tserved\t");

    // `legion controller start` sets LEGION_ROLE=controller and no generation; a harness sets
    // neither.
    await call(keyCommand, join(run, "controller"), mints, { role: "controller" });
    await call(keyCommand, join(run, "harness"), mints);
    const fields = readFileSync(join(dest, "hawk-token.calls"), "utf8")
      .split("\n")
      .filter(Boolean)
      .map((l) => l.split("\t"));
    expect(fields.every((f) => f.length === 6 && f.every(Boolean))).toBe(true);
    expect(fields.at(-2)?.[3]).toBe("controller");
    expect(fields.at(-1)?.[3]).toBe("-");
  });

  test("lists for a failed check each agent still without a key, and each that recovered from a starve within 30 s of the check's start", () => {
    // The rules are the reader's over the record's own lines, so this writes them at chosen times.
    const dest = join(dir, "notes-window");
    mkdirSync(dest);
    const since = "2026-09-30T12:00:00Z";
    const line = (at: string, agent: string, outcome: string) =>
      `${at}\t42\t/ws\t${agent}\t${outcome}\t${outcome === "served" ? "the kept key" : "why"}\n`;
    writeFileSync(
      join(dest, "hawk-token.calls"),
      [
        line("2026-09-30T11:50:00Z", "implementer/1", "timeout"), // still without, long before
        line("2026-09-30T11:50:00Z", "reviewer/1", "timeout"),
        line("2026-09-30T11:50:10Z", "reviewer/1", "served"), // recovered before the window
        line("2026-09-30T11:59:50Z", "planner/1", "killed"), // inside the 30 s window
        line("2026-09-30T12:00:02Z", "planner/1", "served"),
        line("2026-09-30T11:59:50Z", "merger/1", "killed"),
        line("2026-09-30T12:00:02Z", "merger/1", "served"),
        // A second starve after that service: its own first service is the one that counts.
        line("2026-09-30T12:00:10Z", "merger/1", "timeout"),
        line("2026-09-30T12:00:15Z", "merger/1", "served"),
        line("2026-09-30T12:00:20Z", "merger/1", "served"),
        line("2026-09-30T12:00:05Z", "tester/1", "timeout"), // still without, during the check
        line("2026-09-30T12:00:06Z", "architect/1", "served"),
      ].join("")
    );

    const notes = unserved("--notes", dest, since, "merge");
    expect(notes.code).toBe(0);
    expect(notes.stdout).toStartWith(
      "model-gateway-unserved: 4 agent(s) may have failed check merge for want of a model key: 2 still without one on their last call, and 2 served again after a starve from 2026-09-30T11:59:30Z on"
    );
    const listed = notes.stdout.split("\n").filter((l) => l.startsWith("  "));
    expect(listed).toEqual([
      "  2026-09-30T11:50:00Z implementer/1 in /ws (pid 42): timeout: why (before check merge began)",
      "  2026-09-30T11:59:50Z planner/1 in /ws (pid 42): killed: why (before check merge began); served again at 2026-09-30T12:00:02Z",
      "  2026-09-30T12:00:05Z tester/1 in /ws (pid 42): timeout: why",
      "  2026-09-30T12:00:10Z merger/1 in /ws (pid 42): timeout: why; served again at 2026-09-30T12:00:15Z",
    ]);
    // No record yet, a malformed time, and a line of the old five-field shape.
    expect(unserved("--notes", join(dir, "not-installed"), since, "setup")).toMatchObject({
      code: 0,
      stdout: "",
    });
    expect(unserved("--notes", dest, "yesterday", "setup").code).toBe(2);
    writeFileSync(join(dest, "hawk-token.calls"), "2026-09-30T12:00:05Z\t42\t/ws\ttimeout\twhy\n");
    expect(unserved("--notes", dest, since, "merge").code).toBe(1);
  });

  test("gives up on a mint that outlasts the caller, and on the wait behind it, before Oh My Pi's ten seconds", async () => {
    const { run, dest, mints, keyCommand } = install();
    const since = now();
    // Real time throughout: the deadline under test is the key command's own wall clock, in another
    // process, which no fake timer reaches. The second caller starts 300 ms after the first, so it
    // can take the lock the first releases when its mint is stopped with only a few hundred
    // milliseconds left: too little to mint.
    const calls = await Promise.all([
      call(keyCommand, join(run, "pane-1"), mints, { sleep: 12 }),
      Bun.sleep(300).then(() => call(keyCommand, join(run, "pane-2"), mints, { sleep: 12 })),
    ]);

    expect(mintCount(mints)).toBe(1);
    for (const c of calls) {
      expect([c.code, c.stdout]).toEqual([1, ""]);
      expect(c.ms).toBeLessThan(10_000);
    }
    const notes = unserved("--notes", dest, since, "outlast").stdout;
    expect(notes).toContain(": timeout: the mint ran past the call's 9500 ms");
    expect(notes).toContain("for another call's mint");
  }, 30_000);

  test("exempts the installer's preflight mint from the deadline, for hawk-token's first-run build", () => {
    // Real time: the deadline is the key command's own wall clock, in another process.
    const started = performance.now();
    install({ sleep: 11 });
    expect(performance.now() - started).toBeGreaterThan(11_000);
  }, 30_000);

  test("keeps the installer's preflight out of the record, and says why it got no key once", () => {
    const run = join(dir, "preflight-refused");
    mkdirSync(join(run, "omp-home", ".omp"), { recursive: true });
    const result = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-model-gateway.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        join(run, "omp-home"),
        "--dest",
        join(run, "model-gateway"),
        "--cache-dir",
        join(run, "model-gateway-cache"),
      ],
      { env: env(join(run, "mints"), { mode: "refused" }) }
    );

    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(
      "the preflight got no gateway key: hawk-token key command: no key (failed): hawk-token exited 1 without a key: error: no usable hawk login"
    );
    // The preflight is no agent: a failed run's notes do not count it, nor repeat its reason.
    expect(existsSync(join(run, "model-gateway", "hawk-token.calls"))).toBe(false);
  });

  test("refuses any stage proof's evidence directory that is not empty, whether or not it reached its key command", () => {
    const evidence = join(dir, "evidence");
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    mkdirSync(evidence);
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    // An earlier Stage 4b run that stopped before its controller checkpoint left only these.
    writeFileSync(join(evidence, "transcript.log"), "== setup\n");
    const stopped = unserved("--fresh", evidence);
    expect(stopped.code).toBe(1);
    expect(stopped.stdout).toContain(`${evidence} is not an empty directory`);
    mkdirSync(join(evidence, "model-gateway"));
    expect(unserved("--fresh", evidence).code).toBe(1);
    // One it cannot list is refused too, never read as empty.
    chmodSync(evidence, 0o000);
    try {
      const unlistable = unserved("--fresh", evidence);
      expect(unlistable.code).toBe(1);
      expect(unlistable.stdout).toContain(`${evidence} cannot be listed`);
    } finally {
      chmodSync(evidence, 0o755);
    }
  });

  test("a stage proof that refuses a reused evidence directory lists none of the earlier run's agents", async () => {
    // The earlier run's evidence directory: its key command, and a record whose last call starved.
    const { run: evidence, mints, keyCommand } = install();
    await call(keyCommand, join(evidence, "issue-workspace"), mints, {
      mode: "refused",
      role: "tester",
      generation: "1",
    });
    // Stage 3's teardown removes its containers; this run starts none, and needs no Docker.
    const stageBin = join(dir, "stage-bin");
    mkdirSync(stageBin, { recursive: true });
    writeFileSync(join(stageBin, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const stages: [string, Record<string, string>][] = [
      ["stage3-devbox-workflow.sh", { STAGE3_EVIDENCE_DIR: evidence }],
      [
        "stage3-4b13b-acceptance.sh",
        {
          ACCEPT_EVIDENCE_DIR: evidence,
          ACCEPT_PG_CONTAINER: "none",
          ACCEPT_PG_PORT: "1",
          ACCEPT_NATS_BIN: "/bin/false",
        },
      ],
    ];
    for (const [script, inputs] of stages) {
      const result = Bun.spawnSync(["bash", join(lib, "..", script)], {
        env: { PATH: `${stageBin}:${process.env.PATH}`, HOME: operatorHome, ...inputs },
      });
      const stderr = result.stderr.toString();
      const scratch = /the run's scratch workspace(?:, kept for review,)? is (\S+)/.exec(
        stderr
      )?.[1];
      if (scratch !== undefined) rmSync(scratch, { recursive: true, force: true });
      expect(result.exitCode).toBe(1);
      expect(stderr).toContain(`FAIL setup: ${evidence} is not an empty directory`);
      // The earlier run's tester could not have failed this run's setup, which never started one.
      expect(`${result.stdout}${stderr}`).not.toContain("may have failed check");
    }
  }, 120_000);
});
