import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The key command install-model-gateway.sh writes, against a fake hawk-token first on PATH. The
// fake appends its pid to FAKE_MINT_LOG (one line per mint), sleeps FAKE_MINT_SLEEP seconds, then
// prints a JWT that expires in an hour.
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
payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
`,
  { mode: 0o755 }
);
const operatorHome = join(dir, "operator-home");
mkdirSync(operatorHome);

let installs = 0;
function env(mints: string, sleep = 0) {
  return {
    PATH: `${bin}:${process.env.PATH}`,
    HOME: operatorHome,
    DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
    LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
    FAKE_MINT_LOG: mints,
    FAKE_MINT_SLEEP: String(sleep),
  };
}

// install writes a fresh profile's route, its preflight mint (sleeping `sleep` seconds) included,
// and then removes the key that mint kept, so the next call finds the cache cold (the state the
// kept key's expiry leaves).
function install(sleep = 0) {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const mints = join(run, "mints");
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
      join(run, "model-gateway"),
      "--cache-dir",
      cache,
    ],
    { env: env(mints, sleep) }
  );
  expect(result.exitCode).toBe(0);
  rmSync(join(cache, "hawk-token.key"));
  writeFileSync(mints, "");
  return { run, mints, keyCommand: result.stdout.toString().trim() };
}

// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string, mints: string, sleep = 0) {
  mkdirSync(cwd, { recursive: true });
  const started = performance.now();
  const child = Bun.spawn(["/bin/sh", "-c", keyCommand], {
    cwd,
    env: env(mints, sleep),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout] = await Promise.all([child.exited, new Response(child.stdout).text()]);
  return { code, stdout: stdout.trim(), ms: performance.now() - started };
}

const mintCount = (mints: string) => readFileSync(mints, "utf8").split("\n").filter(Boolean).length;

describe("the model gateway key command", () => {
  test("mints once for a wave of callers that finds the kept key expired, and serves them all", async () => {
    const { run, mints, keyCommand } = install();
    const calls = await Promise.all(
      [1, 2, 3, 4, 5].map((n) => call(keyCommand, join(run, `pane-${n}`), mints, 1))
    );

    expect(mintCount(mints)).toBe(1);
    for (const c of calls) expect(c.code).toBe(0);
    expect(new Set(calls.map((c) => c.stdout)).size).toBe(1);
    expect(calls[0].stdout).toMatch(/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  });

  test("gives up on a mint that outlasts the caller, and on the wait behind it, before Oh My Pi's ten seconds", async () => {
    const { run, mints, keyCommand } = install();
    // Real time throughout: the deadline under test is the key command's own wall clock, in another
    // process, which no fake timer reaches. The second caller starts 300 ms after the first, so it
    // can take the lock the first releases when its mint is stopped with only a few hundred
    // milliseconds left: too little to mint.
    const calls = await Promise.all([
      call(keyCommand, join(run, "pane-1"), mints, 12),
      Bun.sleep(300).then(() => call(keyCommand, join(run, "pane-2"), mints, 12)),
    ]);

    expect(mintCount(mints)).toBe(1);
    for (const c of calls) {
      expect([c.code, c.stdout]).toEqual([1, ""]);
      expect(c.ms).toBeLessThan(10_000);
    }
  }, 30_000);

  test("exempts the installer's preflight mint from the deadline, for hawk-token's first-run build", () => {
    // Real time: the deadline is the key command's own wall clock, in another process.
    const started = performance.now();
    install(11);
    expect(performance.now() - started).toBeGreaterThan(11_000);
  }, 30_000);
});
