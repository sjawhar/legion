import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

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
