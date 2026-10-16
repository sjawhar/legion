import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";

// Stage 4b's prerequisites checkpoint compiles every lib/*.jq module the script includes on the
// host's jq before the run starts: included_jq_modules reads the module names from the script's own
// `include "…"` strings, and jq_modules_compile includes each from a lib directory, failing with
// jq's error on the first that does not compile and noting them all otherwise. Both run as the
// script defines them, on the real jq on PATH: CI's runner jq, or locally each of the three jqs in
// turn (`PATH=/tmp/jqs/<v>:$PATH bun test …`), since jq's error wording moves between versions.
const scriptPath = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const fn = scriptFunctions(scriptPath);
const dir = mkdtempSync(join(tmpdir(), "stage4b-jq-modules-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

writeFileSync(join(dir, "good.jq"), "def ok: true;\n");
writeFileSync(join(dir, "includer.jq"), 'include "good"; def also: ok;\n');
writeFileSync(join(dir, "broken.jq"), "def broken: (;\n");
const version = runJq(["--version"], "").trim();

interface Run {
  readonly code: number;
  readonly out: string;
}
/** Runs BODY after the two functions, with note and fail stubbed. */
function run(body: string): Run {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
note() { printf 'NOTE %s\\n' "$*"; }
fail() { printf 'FAIL %s\\n' "$*"; exit 3; }
${fn("included_jq_modules")}
${fn("jq_modules_compile")}
${body}`,
  ]);
  expect(result.stderr.toString()).toBe("");
  return { code: result.exitCode, out: result.stdout.toString() };
}

describe("jq_modules_compile", () => {
  test("notes the modules the host's jq compiles, a nested include among them", () => {
    expect(run(`jq_modules_compile ${JSON.stringify(dir)} good includer`)).toEqual({
      code: 0,
      out: `NOTE ${version} compiles lib/good.jq lib/includer.jq\n`,
    });
  });

  test("fails on the module jq cannot parse, quoting its syntax error", () => {
    const result = run(`jq_modules_compile ${JSON.stringify(dir)} good broken`);
    expect(result.code).toBe(3);
    expect(result.out.split("\n")).toHaveLength(2);
    expect(result.out).toStartWith(`FAIL the host's ${version} cannot compile lib/broken.jq: `);
    expect(result.out).toContain("syntax error");
  });

  test("fails on a module the lib directory lacks", () => {
    const result = run(`jq_modules_compile ${JSON.stringify(dir)} missing`);
    expect(result.code).toBe(3);
    expect(result.out.split("\n")).toHaveLength(2);
    expect(result.out).toStartWith(`FAIL the host's ${version} cannot compile lib/missing.jq: `);
  });
});

describe("included_jq_modules", () => {
  test("reads the script's own included modules, sorted and unique", () => {
    expect(run(`included_jq_modules ${JSON.stringify(scriptPath)}`)).toEqual({
      code: 0,
      out: [
        "controller-ticks",
        "design-gate-approval-requests",
        "omp-tool-calls",
        "stage4b-pods",
        "stage4b-room",
        "stage4b-tools",
        "",
      ].join("\n"),
    });
  });

  test("sorts, deduplicates and reads text, a commented include counted", () => {
    const script = join(dir, "script.sh");
    writeFileSync(
      script,
      [
        "jq 'include \"zeta\"; z'",
        "jq 'include \"alpha\"; a'",
        '# include "alpha"',
        "jq 'include \"alpha\"; a2'",
        "",
      ].join("\n")
    );
    expect(run(`included_jq_modules ${JSON.stringify(script)}`)).toEqual({
      code: 0,
      out: "alpha\nzeta\n",
    });
  });
});
