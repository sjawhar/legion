import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The key command install-model-gateway.sh writes, against a fake hawk-token first on PATH. The
// fake appends its pid to FAKE_MINT_LOG (one line per mint), sleeps FAKE_MINT_SLEEP seconds, then
// by FAKE_MINT_MODE prints a JWT that expires in an hour (ok), fails the way the devbox wrapper
// does when its 9000 ms budget runs out (budget), or fails at once on the login (refused).
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
esac
`,
  { mode: 0o755 }
);
const operatorHome = join(dir, "operator-home");
mkdirSync(operatorHome);

interface Fake {
  mode?: "ok" | "budget" | "refused";
  sleep?: number;
  // The MODEL_GATEWAY_UNSERVED_FILE a harness names in one agent's environment.
  unservedFile?: string;
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
    ...(fake.unservedFile === undefined ? {} : { MODEL_GATEWAY_UNSERVED_FILE: fake.unservedFile }),
  };
}

// install writes a fresh profile's route, its preflight mint included, and then removes the key
// that mint kept, so the next call finds the cache cold (the state the kept key's expiry leaves).
function install() {
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
    { env: env(mints) }
  );
  expect(result.stderr.toString()).not.toContain("install-model-gateway: FAIL");
  expect(result.exitCode).toBe(0);
  rmSync(join(cache, "hawk-token.key"));
  writeFileSync(mints, "");
  return { run, dest, mints, keyCommand: result.stdout.toString().trim() };
}

// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string, mints: string, fake: Fake = {}) {
  mkdirSync(cwd, { recursive: true });
  const started = performance.now();
  const child = Bun.spawn(["/bin/sh", "-c", keyCommand], {
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

  test("records a call whose mint ran out of time as starved, apart from a mint the login refused", async () => {
    const { run, dest, mints, keyCommand } = install();
    const starved = join(run, "pane-starved");
    const refused = join(run, "pane-refused");
    const idle = join(run, "pane-idle");
    mkdirSync(idle);

    const a = await call(keyCommand, starved, mints, { mode: "budget" });
    const b = await call(keyCommand, refused, mints, { mode: "refused" });

    // What Oh My Pi sees is the same for both, which is why the run itself cannot tell them apart.
    expect([a.code, a.stdout]).toEqual([1, ""]);
    expect([b.code, b.stdout]).toEqual([1, ""]);
    // The record does.
    const s = unserved(dest, starved);
    expect(s.code).toBe(75);
    expect(s.stdout).toContain("timeout");
    expect(s.stdout).toContain("9021 ms of a 9000 ms budget");
    const r = unserved(dest, refused);
    expect(r.code).toBe(77);
    expect(r.stdout).toContain("failed");
    expect(r.stdout).toContain("no usable hawk login");
    // A key failure anywhere in the run outranks a starve: it is the one a rerun does not fix.
    expect(unserved(dest).code).toBe(77);
    expect(unserved(dest, idle)).toMatchObject({ code: 0, stdout: "" });
  });

  test("writes each agent's own file too, so one run's directory holds that run's verdict", async () => {
    const { run, mints, keyCommand } = install();
    const starvedRun = join(run, "run-starved");
    const servedRun = join(run, "run-served");
    const starved = await call(keyCommand, join(starvedRun, "cwd"), mints, {
      mode: "budget",
      unservedFile: join(starvedRun, "model-gateway-unserved"),
    });
    const served = await call(keyCommand, join(servedRun, "cwd"), mints, {
      unservedFile: join(servedRun, "model-gateway-unserved"),
    });

    expect(starved.code).toBe(1);
    expect(served.code).toBe(0);
    const s = unserved("--record", join(starvedRun, "model-gateway-unserved"));
    expect(s.code).toBe(75);
    expect(s.stdout).toContain(join(starvedRun, "cwd"));
    expect(unserved("--record", join(servedRun, "model-gateway-unserved"))).toMatchObject({
      code: 0,
      stdout: "",
    });
    // A path whose directory does not exist is a harness mistake, never "every call served".
    expect(unserved("--record", join(run, "no-such-run", "model-gateway-unserved")).code).toBe(2);
  });

  test("gives up on a mint that outlasts the caller, and on the wait behind it, before Oh My Pi's ten seconds", async () => {
    const { run, dest, mints, keyCommand } = install();
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
    const u = unserved(dest);
    expect(u.code).toBe(75);
    expect(u.stdout.match(/: timeout: /g)).toHaveLength(2);
    expect(u.stdout).toContain("for another call's mint");
  }, 30_000);

  test("refuses to read a directory the key command was never written to", () => {
    const empty = join(dir, "not-a-key-dir");
    mkdirSync(empty);
    const r = unserved(empty);
    expect(r.code).toBe(2);
    expect(r.stderr).toContain("hawk-token");
  });
});
