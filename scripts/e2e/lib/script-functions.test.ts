import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";

const dir = mkdtempSync(join(tmpdir(), "script-functions-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const path = join(dir, "fixture.sh");
writeFileSync(
  path,
  `#!/usr/bin/env bash
one_line() { echo "one line"; }
# A multi-line function follows a one-line one: the one-line definition ends at its own line.
several() {
  local x=1
  echo "$x"
}
`
);
const fn = scriptFunctions(path);

describe("scriptFunctions", () => {
  test("a one-line function is that line alone, not the functions after it", () => {
    expect(fn("one_line")).toBe('one_line() { echo "one line"; }');
  });

  test("a multi-line function runs to the first closing brace alone on its line", () => {
    expect(fn("several")).toBe('several() {\n  local x=1\n  echo "$x"\n}');
  });

  test("a function the script does not define throws, naming the script", () => {
    expect(() => fn("absent")).toThrow(/fixture\.sh defines no absent\(\)/);
  });
});

// A harness that extracts a function must extract or stub every helper of the script that it
// calls; one it leaves out is a bare exit 127 the caller has to trace. The guard names it.
describe("nameMissingHelpers", () => {
  // Runs the fixture's several() with the guard above it, as a harness composes a run.
  function guarded(stubs: string) {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\n${stubs}\n${fn("several")}\nseveral\none_line`,
    ]);
    return { out: result.stdout.toString(), err: result.stderr.toString() };
  }

  test("names a helper of the script the harness left undefined", () => {
    const run = guarded("");
    expect(run.err).toContain("harness: no definition for one_line");
    expect(() => expectNoMissingHelpers(run.out, run.err)).toThrow(
      /neither extracts nor stubs one_line/
    );
  });

  test("says nothing once the harness stubs it", () => {
    const run = guarded("one_line() { :; }");
    expect(run.err).toBe("");
    expect(() => expectNoMissingHelpers(run.out, run.err)).not.toThrow();
  });

  test("names a helper the extracted function calls with its stdout and stderr silenced", () => {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\nquiet() { one_line >/dev/null 2>&1 || echo "code $?"; }\nquiet`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).toBe("harness: no definition for one_line\n");
  });

  test("names a helper a library the script sources defines", () => {
    const sourcing = join(dir, "sourcing.sh");
    writeFileSync(sourcing, '#!/usr/bin/env bash\n. "$root/scripts/e2e/lib/rig.sh"\n');
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(sourcing)}\nuntil_true 1 x true 2>/dev/null; echo "code $?"`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).toBe("harness: no definition for until_true\n");
  });

  test("leaves a missing command the script does not define as it was: exit 127, unnamed", () => {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\nno-such-command-${process.pid}\necho "code $?"`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).not.toContain("harness: no definition");
    expectNoMissingHelpers(result.stdout.toString(), result.stderr.toString());
  });
});

// The mirror of that rule: a stage script may define no helper nothing calls. A cutover that
// leaves one behind leaves a reader to work out which of two ways is the live one.
describe("every helper a stage script defines is called", () => {
  const e2e = join(import.meta.dir, "..");
  const readers = [
    ...new Bun.Glob("*.sh").scanSync({ cwd: e2e, absolute: true }),
    ...new Bun.Glob("lib/*.{sh,ts,py,jq}").scanSync({ cwd: e2e, absolute: true }),
  ].map((file) => readFileSync(file, "utf8"));

  for (const name of ["stage4b-sandbox-tree.sh", "stage4a-sandbox-runtime.sh"]) {
    test(name, () => {
      const text = readFileSync(join(e2e, name), "utf8");
      const defined = [...text.matchAll(/^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm)].map((m) => m[1]);
      const dead = defined.filter((helper) => {
        const word = new RegExp(`\\b${helper}\\b`, "g");
        return readers.reduce((seen, file) => seen + (file.match(word)?.length ?? 0), 0) < 2;
      });
      expect(dead).toEqual([]);
    });
  }
});
