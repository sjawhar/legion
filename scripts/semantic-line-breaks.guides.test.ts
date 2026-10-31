// Checks the two guides' layout; runs on demand (bun test scripts/semantic-line-breaks.guides.test.ts) and is deliberately not in CI: the deployment's rule is an instruction first, and a gate only once a measurement shows one is needed.
import { expect, test } from "bun:test";
import { resolve } from "node:path";

const SCRIPT = resolve(import.meta.dir, "semantic-line-breaks.ts");
const REPO_ROOT = resolve(import.meta.dir, "..");

/** Runs the script as a contributor does: `bun scripts/semantic-line-breaks.ts [--write] <files>`. */
function run(args: string[]) {
  const result = Bun.spawnSync(["bun", SCRIPT, ...args], { cwd: REPO_ROOT });
  return { exitCode: result.exitCode, stdout: result.stdout.toString() };
}

test("the Envoy guide and the Dispatch server README keep the layout", () => {
  const { exitCode, stdout } = run([
    "packages/envoy/AGENTS.md",
    "packages/envoy/cmd/dispatch/README.md",
  ]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});
