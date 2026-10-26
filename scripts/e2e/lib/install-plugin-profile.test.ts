import { afterAll, describe, expect, test } from "bun:test";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Which Oh My Pi install-plugin-profile.sh runs against the profile. The script runs from a fake
// checkout (it finds the checkout from its own path), with a fake pack-plugin.sh that packs a
// manifest alone and a `mise` first on PATH that records every call and answers `omp plugin list`.
const dir = mkdtempSync(join(tmpdir(), "install-plugin-profile-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const repoPin = "github:sjawhar/oh-my-pi@18.12.2-sami.20261025-144204";

let runs = 0;
function install(extra: string[]) {
  const run = join(dir, `run-${++runs}`);
  const checkout = join(run, "checkout");
  const lib = join(checkout, "scripts", "e2e", "lib");
  mkdirSync(lib, { recursive: true });
  copyFileSync(
    join(import.meta.dir, "install-plugin-profile.sh"),
    join(lib, "install-plugin-profile.sh")
  );
  writeFileSync(join(checkout, ".omp-pin"), `${repoPin}\n`);
  writeFileSync(
    join(lib, "pack-plugin.sh"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$2/package"
printf '{"name":"@sjawhar/%s","version":"0.0.0-test"}\\n' "$1" >"$2/package/package.json"
tar czf "$2/plugin.tgz" -C "$2" package
printf '%s\\n' "$2/plugin.tgz"
`
  );
  const log = join(run, "mise.log");
  const bin = join(run, "bin");
  mkdirSync(bin);
  writeFileSync(
    join(bin, "mise"),
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>${JSON.stringify(log)}
case $1 in
where) exit 0 ;;
x)
  if [ "$5 $6" = "plugin list" ]; then
    printf '{"npm":[{"name":"@sjawhar/pi-envoy","version":"0.0.0-test","enabled":true,"path":"%s"}]}\\n' "$PWD"
  fi
  ;;
esac
`,
    { mode: 0o755 }
  );
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const dest = join(run, "pi-envoy");
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-plugin-profile.sh"),
      "--package",
      "pi-envoy",
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      ...extra,
    ],
    { env: { PATH: `${bin}:${process.env.PATH ?? ""}`, HOME: join(run, "caller-home") } }
  );
  const calls = (() => {
    try {
      return readFileSync(log, "utf8").trim().split("\n");
    } catch {
      return [];
    }
  })();
  return { dest, calls, code: result.exitCode, stderr: result.stderr.toString() };
}

describe("install-plugin-profile.sh's Oh My Pi", () => {
  test("runs the build --omp names for the install and the verification, never .omp-pin", () => {
    const spec = "github:sjawhar/oh-my-pi@18.12.2-sami.20261026-004924";
    const r = install(["--omp", spec]);
    expect(r.stderr).toContain("enabled in OMP profile legion-e2e-test");
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([
      `where ${spec}`,
      `x ${spec} -- omp plugin install ${r.dest}`,
      `x ${spec} -- omp plugin list --json`,
    ]);
  });

  test("runs .omp-pin's build when no --omp is given", () => {
    const r = install([]);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([
      `where ${repoPin}`,
      `x ${repoPin} -- omp plugin install ${r.dest}`,
      `x ${repoPin} -- omp plugin list --json`,
    ]);
  });

  test("refuses an empty --omp, running nothing", () => {
    const r = install(["--omp", ""]);
    expect(r.code).toBe(2);
    expect(r.stderr).toContain("--omp needs a value");
    expect(r.calls).toEqual([]);
  });
});
