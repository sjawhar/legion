import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
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

  test("leaves the mode of a file hard-linked from outside the tree alone", async () => {
    // A file's mode is a property of its inode, which a hard link shares: a `chmod` on the copy
    // inside the tree is a chmod on the one outside it. The outside file is made with `mkdtemp`
    // in the child, so the helper is never told to remove it.
    const { code, stdout } = await inItsOwnProcess(
      `import { chmodSync, linkSync, mkdtempSync, writeFileSync } from "node:fs";
       import { tmpdir } from "node:os";
       import { join } from "node:path";
       const outside = mkdtempSync(join(tmpdir(), "temp-dir-fixture-"));
       const fixture = join(outside, "fixture");
       writeFileSync(fixture, "x");
       chmodSync(fixture, 0o444);
       const directory = temporaryDirectory("temp-dir-linking-hard-");
       linkSync(fixture, join(directory, "link"));
       console.log(JSON.stringify({ directory, outside, fixture }));`
    );
    expect(code).toBe(0);
    const { directory, outside, fixture } = JSON.parse(stdout) as {
      directory: string;
      outside: string;
      fixture: string;
    };

    try {
      expect(existsSync(directory)).toBe(false);
      expect(existsSync(fixture)).toBe(true);
      expect(statSync(fixture).mode & 0o777).toBe(0o444);
    } finally {
      rmSync(outside, { force: true, recursive: true });
    }
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

    // In a `finally`, because an assertion that fails here would otherwise leave the target: the
    // very thing this file is about.
    try {
      expect(existsSync(directory)).toBe(false);
      expect(existsSync(join(target, "file"))).toBe(true);
      expect(statSync(join(target, "file")).mode & 0o777).toBe(0o444);
      expect(statSync(target).mode & 0o777).toBe(0o555);
    } finally {
      chmodSync(target, 0o700);
      rmSync(target, { force: true, recursive: true });
    }
  });

  test("removes a directory made inside another of its own, read-only directories", () => {
    const parent = temporaryDirectory("temp-dir-parent-");
    const child = temporaryDirectory("temp-dir-child-", parent);
    writeFileSync(join(child, "file"), "x");
    // Unlinking the child needs write permission on the parent, which is the helper's own.
    chmodSync(parent, 0o555);

    removeTemporaryDirectories();

    expect(existsSync(child)).toBe(false);
    expect(existsSync(parent)).toBe(false);
  });

  /**
   * Whether this process removes a directory from one with no write permission for it. Root does,
   * and so does any process holding CAP_DAC_OVERRIDE, so the refusal the test below is about never
   * happens there and the test would fail on a fact about the process rather than the helper.
   * Probed rather than read from the uid, which names only one of the two.
   */
  function removesFromAReadOnlyDirectory(): boolean {
    const holder = join(temporaryDirectory("temp-dir-probe-"), "holder");
    mkdirSync(holder);
    mkdirSync(join(holder, "inner"));
    chmodSync(holder, 0o555);
    try {
      rmSync(join(holder, "inner"), { recursive: true });
      return true;
    } catch {
      return false;
    } finally {
      chmodSync(holder, 0o700);
    }
  }

  test.skipIf(removesFromAReadOnlyDirectory())(
    "attempts every directory even when one of them cannot be removed (skipped: this process removes from a read-only directory, as root and CAP_DAC_OVERRIDE do)",
    () => {
      const removable = temporaryDirectory("temp-dir-removable-");
      // `refusing` is not the helper's own, so the removal of the directory inside it cannot make
      // it writable, and that one unlink fails. Its own holder is the helper's, so the walk that
      // removes the holder makes `refusing` writable on the way down.
      const held = temporaryDirectory("temp-dir-held-");
      const refusing = join(held, "inner");
      mkdirSync(refusing);
      temporaryDirectory("temp-dir-stuck-", refusing);
      chmodSync(refusing, 0o555);
      const other = temporaryDirectory("temp-dir-other-");

      expect(() => removeTemporaryDirectories()).toThrow("temporary directories left behind");

      // Every other directory went, and the one the failure was about is gone with its holder.
      expect(existsSync(removable)).toBe(false);
      expect(existsSync(other)).toBe(false);
      expect(existsSync(held)).toBe(false);
    }
  );

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
 * `dispatch-config-*` directories gathered in one machine's temporary directory.
 *
 * What a test file left behind is a fact about a finished run, not about its source, so this runs
 * `bun test` over a fixture file of each kind and reads the temporary directory it ran with. The
 * same reading over this package's whole suite is a step of its own in CI, beside the step that
 * runs it ("Test Envoy client" in `.github/workflows/ci.yaml`).
 */
describe("a test file's run", () => {
  /**
   * Runs `bun test` over one fixture file whose body is `body`, with a temporary directory of its
   * own, and returns that directory's entries with the run's exit code. The fixture imports the
   * helper by its path, as a file outside the package would not resolve the package's exports.
   */
  async function leftBehind(
    body: string
  ): Promise<{ readonly code: number; readonly entries: string[] }> {
    const fixtures = temporaryDirectory("temp-dir-fixtures-");
    const runTmp = temporaryDirectory("temp-dir-fixture-tmp-");
    const helper = join(import.meta.dir, "..", "..", "test", "temp-dir.ts");
    const fixture = join(fixtures, "fixture.test.ts");
    writeFileSync(
      fixture,
      `import { expect, afterAll, test } from "bun:test";\n` +
        `import { removeTemporaryDirectories, temporaryDirectory } from ${JSON.stringify(helper)};\n` +
        `${body}\n`
    );
    const run = Bun.spawn([process.execPath, "test", fixture], {
      cwd: join(import.meta.dir, "..", ".."),
      env: { ...process.env, TMPDIR: runTmp },
      stdout: "pipe",
      stderr: "pipe",
    });
    await new Response(run.stdout).text();
    await new Response(run.stderr).text();
    return { code: await run.exited, entries: readdirSync(runTmp) };
  }

  test("leaves nothing when it registers the removal", async () => {
    const { code, entries } = await leftBehind(
      `afterAll(removeTemporaryDirectories);
       test("makes one", () => {
         expect(temporaryDirectory("fixture-registered-")).toBeTruthy();
       });`
    );
    expect(code).toBe(0);
    expect(entries).toEqual([]);
  });

  // The negative control: without this, a check that reads an empty directory proves nothing.
  test("leaves its directory behind when it does not, which this check sees", async () => {
    const { code, entries } = await leftBehind(
      `test("makes one", () => {
         expect(temporaryDirectory("fixture-unregistered-")).toBeTruthy();
       });`
    );
    expect(code).toBe(0);
    expect(entries).toEqual([expect.stringMatching(/^fixture-unregistered-/)]);
  });
});
