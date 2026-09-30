import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
    expect(s.stdout).toContain(`${join(run, "starved", "cwd")}: timeout: `);
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

  test("tells a failed stage proof which agents got no key since its failing check began, and never changes its status", async () => {
    const { run, dest, mints, keyCommand } = install();
    await call(keyCommand, join(run, "pane-earlier"), mints, { mode: "budget" });
    // The failing check begins after that call. The record's times are whole seconds, so it begins
    // a second later: real time, since no fake timer reaches another process's clock.
    await Bun.sleep(1100);
    const since = now();
    await call(keyCommand, join(run, "pane-starved"), mints, { mode: "budget" });
    await call(keyCommand, join(run, "pane-refused"), mints, { mode: "refused" });
    await call(keyCommand, join(run, "pane-recovered"), mints, { mode: "budget" });
    await call(keyCommand, join(run, "pane-recovered"), mints);

    const notes = unserved("--notes", "1", dest, since, "held-worker");
    expect(notes.code).toBe(0);
    expect(notes.stdout).toStartWith(
      `model-gateway-unserved: since check held-worker began (${since}), 3 agent(s) got no model key, 2 of them still without one`
    );
    const listed = notes.stdout.split("\n").filter((line) => line.startsWith("  "));
    // The starve before the check began is not the check's.
    expect(listed).toHaveLength(3);
    const of = (pane: string) =>
      listed.find((line) => line.includes(`in ${join(run, pane)} `)) ?? "";
    expect(of("pane-refused")).toContain(
      ": failed: hawk-token exited 1 without a key: error: no usable hawk login"
    );
    expect(of("pane-starved")).toContain(": timeout: hawk-token: mint produced no token");
    expect(of("pane-starved")).not.toContain("served again");
    // An agent served again after its starve recovered, but spent the wait the check may have needed.
    expect(of("pane-recovered")).toMatch(/: timeout: .*; served again at \d{4}-/);
    // A pass says nothing, nor does a run that failed before it installed the key command (an
    // empty directory), since a directory an earlier run left holds that run's calls.
    expect(unserved("--notes", "0", dest, since, "held-worker")).toMatchObject({
      code: 0,
      stdout: "",
    });
    expect(unserved("--notes", "1", "", since, "setup")).toMatchObject({ code: 0, stdout: "" });
    expect(unserved("--notes", "x", dest, since, "setup").code).toBe(2);
    expect(unserved("--notes", "1", dest, "yesterday", "setup").code).toBe(2);
  }, 30_000);

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
    const notes = unserved("--notes", "1", dest, since, "outlast").stdout;
    expect(notes).toContain(": timeout: the mint ran past the call's 9500 ms");
    expect(notes).toContain("for another call's mint");
  }, 30_000);

  test("exempts the installer's preflight mint from the deadline, for hawk-token's first-run build", () => {
    // Real time: the deadline is the key command's own wall clock, in another process.
    const started = performance.now();
    install({ sleep: 11 });
    expect(performance.now() - started).toBeGreaterThan(11_000);
  }, 30_000);
});
