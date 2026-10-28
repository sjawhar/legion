import { afterAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The key-file route's own installer: unlike the Hawk gateway route, its key command has no mint
// and no cache, so there is nothing to fake — every call is a plain read of a caller-supplied file.
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "install-key-file-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const operatorHome = join(dir, "operator-home");
mkdirSync(operatorHome);

// install writes a fresh profile's route from a fresh --key-file. --dest and --key-dir are always
// two different directories (the installer refuses otherwise): --dest gets only the call record,
// which a stage proof keeps as evidence after every outcome; --key-dir gets the key command
// itself, which bakes in --key-file's resolved path and so can read the key for as long as that
// path holds one — the caller's own cleanup must remove it unconditionally, on every outcome,
// which is exactly what this suite's own scrub asserts the installer makes possible.
let installs = 0;
function install(keyContents = "sk-ant-test-key-00000000000000000000000") {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const dest = join(run, "model-gateway");
  const keyDir = join(run, "model-gateway-key");
  const keyFile = join(run, "anthropic-key-file");
  writeFileSync(keyFile, `${keyContents}\n`, { mode: 0o600 });
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-key-file-model-route.sh"),
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      "--key-dir",
      keyDir,
      "--key-file",
      keyFile,
    ],
    { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
  );
  return { run, home, dest, keyDir, keyFile, keyContents, result };
}

// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string) {
  mkdirSync(cwd, { recursive: true });
  const child = Bun.spawn(["/bin/sh", "-c", `exec ${keyCommand}`], {
    cwd,
    env: { PATH: process.env.PATH ?? "" },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout: stdout.trim(), stderr };
}

describe("the key-file model route installer", () => {
  test("writes the key command under --key-dir, serves the key, and exits 0", async () => {
    const { keyDir, keyContents, result } = install();
    expect(result.exitCode).toBe(0);
    const keyCommand = result.stdout.toString().trim();
    expect(keyCommand).toBe(join(keyDir, "anthropic-key"));
    expect(existsSync(keyCommand)).toBe(true);
    const served = await call(keyCommand, join(dir, "pane-cwd"));
    expect(served.code).toBe(0);
    expect(served.stdout).toBe(keyContents);
  });

  test("--dest holds only the call record: never the key command, and never the key's own value", async () => {
    const { dest, keyDir, keyContents } = install();
    await call(join(keyDir, "anthropic-key"), join(dir, "pane-cwd-dest-check"));
    const entries = readdirSync(dest).sort();
    expect(entries).toEqual(["anthropic-key.calls", "anthropic-key.log"]);
    // The call record legitimately names --key-file's own path (a diagnostic: where a served call
    // read from), documented in model-gateway-unserved.sh's header — only the key's own value,
    // which nothing here ever needs to log, is the thing --dest must never hold.
    for (const entry of entries) {
      expect(readFileSync(join(dest, entry), "utf8")).not.toContain(keyContents);
    }
  });

  test("refuses --key-dir equal to --dest, so the key command can never land where the call record is kept", () => {
    const run = join(dir, `install-${++installs}`);
    const home = join(run, "omp-home");
    mkdirSync(join(home, ".omp"), { recursive: true });
    const dest = join(run, "model-gateway");
    const keyFile = join(run, "anthropic-key-file");
    writeFileSync(keyFile, "sk-ant-x\n", { mode: 0o600 });
    const result = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-key-file-model-route.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        home,
        "--dest",
        dest,
        "--key-dir",
        dest,
        "--key-file",
        keyFile,
      ],
      { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
    );
    expect(result.exitCode).toBe(2);
    expect(result.stderr.toString()).toContain("--key-dir");
  });

  test("refuses --key-dir nested inside --dest, and --dest nested inside --key-dir", () => {
    const run = join(dir, `install-${++installs}`);
    const home = join(run, "omp-home");
    mkdirSync(join(home, ".omp"), { recursive: true });
    const keyFile = join(run, "anthropic-key-file");
    writeFileSync(keyFile, "sk-ant-x\n", { mode: 0o600 });
    const dest = join(run, "model-gateway");
    const nestedKeyDir = join(dest, "key");
    const result = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-key-file-model-route.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        home,
        "--dest",
        dest,
        "--key-dir",
        nestedKeyDir,
        "--key-file",
        keyFile,
      ],
      { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
    );
    expect(result.exitCode).toBe(2);
    expect(result.stderr.toString()).toContain("--key-dir");

    const keyDir = join(run, "model-gateway-key");
    const nestedDest = join(keyDir, "dest");
    const reversed = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-key-file-model-route.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        home,
        "--dest",
        nestedDest,
        "--key-dir",
        keyDir,
        "--key-file",
        keyFile,
      ],
      { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
    );
    expect(reversed.exitCode).toBe(2);
    expect(reversed.stderr.toString()).toContain("--dest");
  });

  test("refuses with no --key-dir at all", () => {
    const run = join(dir, `install-${++installs}`);
    const home = join(run, "omp-home");
    mkdirSync(join(home, ".omp"), { recursive: true });
    const keyFile = join(run, "anthropic-key-file");
    writeFileSync(keyFile, "sk-ant-x\n", { mode: 0o600 });
    const result = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-key-file-model-route.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        home,
        "--dest",
        join(run, "model-gateway"),
        "--key-file",
        keyFile,
      ],
      { env: { PATH: process.env.PATH ?? "", HOME: operatorHome } }
    );
    expect(result.exitCode).toBe(2);
    expect(result.stderr.toString()).toContain("--key-dir is required");
  });

  test("removing --key-dir (the caller's own unconditional cleanup) leaves nothing under --dest that can serve the key", async () => {
    const { dest, keyDir, keyContents } = install();
    rmSync(keyDir, { recursive: true, force: true });
    expect(existsSync(dest)).toBe(true); // the call record survives, as evidence does
    for (const entry of readdirSync(dest)) {
      expect(entry).not.toBe("anthropic-key");
      expect(readFileSync(join(dest, entry), "utf8")).not.toContain(keyContents);
    }
  });

  test("models.yml's !exec names the key command under --key-dir, not --dest", () => {
    const { home, keyDir } = install();
    const modelsYml = readFileSync(
      join(home, ".omp/profiles/legion-e2e-test/agent/models.yml"),
      "utf8"
    );
    expect(modelsYml).toContain(join(keyDir, "anthropic-key"));
  });

  test("config.yml raises Oh My Pi's tool_call handler limit to 120 s, so AskGate's review fits inside it", () => {
    const { home } = install();
    const config = Bun.YAML.parse(
      readFileSync(join(home, ".omp/profiles/legion-e2e-test/agent/config.yml"), "utf8")
    ) as { extensionHandlers?: { toolCallTimeoutMs?: unknown } };
    expect(config.extensionHandlers?.toolCallTimeoutMs).toBe(120_000);
  });
});
