import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { removeTemporaryDirectories, temporaryDirectory } from "../../test/temp-dir";

afterAll(removeTemporaryDirectories);

/**
 * Runs `body` as the whole of its own Bun process, with this package's helper imported as
 * `temporaryDirectory`, and returns the process's exit code with everything it printed. The
 * directory a test makes must not outlive the process that made it, and only a real process that
 * has ended can show that.
 */
async function inItsOwnProcess(
  body: string
): Promise<{ readonly code: number; readonly stdout: string }> {
  const scripts = temporaryDirectory("temp-dir-process-");
  const script = join(scripts, "run.ts");
  const helper = join(import.meta.dir, "..", "..", "test", "temp-dir.ts");
  writeFileSync(script, `import { temporaryDirectory } from ${JSON.stringify(helper)};\n${body}\n`);
  const run = Bun.spawn([process.execPath, script], { stdout: "pipe", stderr: "pipe" });
  const stdout = await new Response(run.stdout).text();
  await new Response(run.stderr).text();
  return { code: await run.exited, stdout: stdout.trim() };
}

describe("temporaryDirectory", () => {
  test("makes a directory under the system temporary directory, named for its prefix", () => {
    const directory = temporaryDirectory("temp-dir-named-");
    expect(existsSync(directory)).toBe(true);
    expect(directory.startsWith(join(tmpdir(), "temp-dir-named-"))).toBe(true);
    removeTemporaryDirectories();
    expect(existsSync(directory)).toBe(false);
  });

  test("removes a tree whose own directories and files are read-only", () => {
    const directory = temporaryDirectory("temp-dir-readonly-");
    const inner = join(directory, "inner");
    mkdirSync(inner);
    writeFileSync(join(inner, "file"), "x");
    // What Go's module cache writes on purpose, and what `rm -rf` then refuses.
    chmodSync(join(inner, "file"), 0o444);
    chmodSync(inner, 0o555);
    chmodSync(directory, 0o555);

    removeTemporaryDirectories();

    expect(existsSync(directory)).toBe(false);
  });

  test("never reaches through a symlink to a tree it did not make", async () => {
    // The target is made with `mkdtemp` in the child, so the helper is never told to remove it:
    // if the removal followed the link, the read-only tree behind it would lose its mode or its
    // files. The parent removes it afterwards either way.
    const { code, stdout } = await inItsOwnProcess(
      `import { chmodSync, mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from "node:fs";
       import { tmpdir } from "node:os";
       import { join } from "node:path";
       const target = mkdtempSync(join(tmpdir(), "temp-dir-target-"));
       writeFileSync(join(target, "file"), "x");
       chmodSync(join(target, "file"), 0o444);
       chmodSync(target, 0o555);
       const directory = temporaryDirectory("temp-dir-linking-");
       mkdirSync(join(directory, "inner"));
       symlinkSync(target, join(directory, "inner", "link"));
       console.log(JSON.stringify({ directory, target }));`
    );
    expect(code).toBe(0);
    const { directory, target } = JSON.parse(stdout) as { directory: string; target: string };

    expect(existsSync(directory)).toBe(false);
    expect(existsSync(join(target, "file"))).toBe(true);
    expect(statSync(join(target, "file")).mode & 0o777).toBe(0o444);
    expect(statSync(target).mode & 0o777).toBe(0o555);

    chmodSync(target, 0o700);
    rmSync(target, { force: true, recursive: true });
  });

  test("the directory is gone once the process that made it returns, successfully", async () => {
    const { code, stdout } = await inItsOwnProcess(
      `const directory = temporaryDirectory("temp-dir-exit-0-");
       console.log(directory);`
    );
    expect(code).toBe(0);
    expect(stdout.startsWith(join(tmpdir(), "temp-dir-exit-0-"))).toBe(true);
    expect(existsSync(stdout)).toBe(false);
  });

  test("the directory is gone once the process that made it returns, having thrown", async () => {
    const { code, stdout } = await inItsOwnProcess(
      `const directory = temporaryDirectory("temp-dir-throw-");
       console.log(directory);
       throw new Error("the command failed");`
    );
    expect(code).toBeGreaterThan(0);
    expect(stdout.startsWith(join(tmpdir(), "temp-dir-throw-"))).toBe(true);
    expect(existsSync(stdout)).toBe(false);
  });

  test("the directory is gone once the process that made it exits nonzero", async () => {
    const { code, stdout } = await inItsOwnProcess(
      `const directory = temporaryDirectory("temp-dir-exit-2-");
       console.log(directory);
       process.exit(2);`
    );
    expect(code).toBe(2);
    expect(existsSync(stdout)).toBe(false);
  });
});

/**
 * Bun's test runner emits no `exit`, so a test file that makes a temporary directory and does not
 * register the removal itself leaves it behind — which is how 709 `dispatch-cli-*` and 680
 * `dispatch-config-*` directories gathered in one machine's temporary directory. This is
 * the check that a new one does not start it again.
 */
describe("this package's test files", () => {
  /** `src/__tests__/*.test.ts` and `test/*.ts`, with the helper and its test left out. */
  function files(): { readonly path: string; readonly source: string }[] {
    const roots = [join(import.meta.dir), join(import.meta.dir, "..", "..", "test")];
    const found: { path: string; source: string }[] = [];
    for (const root of roots) {
      for (const name of readdirSync(root)) {
        if (!name.endsWith(".ts") || name === "temp-dir.ts") {
          continue;
        }
        const path = join(root, name);
        found.push({ path, source: readFileSync(path, "utf8") });
      }
    }
    return found;
  }

  test("make their temporary directories through the helper", () => {
    // This file is the exception: its child process makes one with `mkdtemp` on purpose, to show
    // that the removal does not follow a symlink into a tree the helper was not given.
    const direct = files()
      .filter(
        ({ path, source }) => !path.endsWith("temp-dir.test.ts") && source.includes("mkdtempSync(")
      )
      .map(({ path }) => path);
    expect(direct).toEqual([]);
  });

  // A module that is not a test file (`nats-server.ts`, `test/envoy-stack.ts`) has no hook of its
  // own to register: it removes its directory where it stops the thing that used it, and the
  // helper's own process-exit removal is what covers a process that never reaches that.
  test("register the removal, which the test runner's own exit does not", () => {
    const unregistered = files()
      .filter(
        ({ path, source }) =>
          path.endsWith(".test.ts") &&
          source.includes("temporaryDirectory(") &&
          !source.includes("afterAll(removeTemporaryDirectories)")
      )
      .map(({ path }) => path);
    expect(unregistered).toEqual([]);
  });
});
