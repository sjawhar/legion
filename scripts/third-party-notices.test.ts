import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { thirdPartyNotices } from "./third-party-notices";

const scratchDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    scratchDirectories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

/** One bundled package under a scratch root, with the manifest and files given; its module's path. */
async function bundledPackage(manifest: object, files: Record<string, string> = {}) {
  const root = await mkdtemp(join(tmpdir(), "third-party-notices-"));
  scratchDirectories.push(root);
  const pkgDir = join(root, "node_modules", "pkg");
  await mkdir(pkgDir, { recursive: true });
  await writeFile(join(pkgDir, "package.json"), JSON.stringify(manifest));
  await writeFile(join(pkgDir, "index.js"), "export {}\n");
  for (const [name, text] of Object.entries(files)) await writeFile(join(pkgDir, name), text);
  return { root, input: join("node_modules", "pkg", "index.js") };
}

test("rejects a bundled package whose declared license is not an SPDX expression", async () => {
  const { root, input } = await bundledPackage({
    name: "pkg",
    version: "1.0.0",
    license: "DefinitelyNotALicense",
  });
  await expect(thirdPartyNotices([input], root)).rejects.toThrow(
    "pkg@1.0.0 declares an unrecognized license DefinitelyNotALicense"
  );
});

test("rejects a package that declares no license and ships only a NOTICE file", async () => {
  const { root, input } = await bundledPackage(
    { name: "pkg", version: "1.0.0" },
    { NOTICE: "pkg includes software developed by Example.\n" }
  );
  await expect(thirdPartyNotices([input], root)).rejects.toThrow("pkg@1.0.0");
});

test("rejects a LicenseRef license whose terms the package does not ship", async () => {
  const { root, input } = await bundledPackage({
    name: "pkg",
    version: "1.0.0",
    license: "LicenseRef-Proprietary",
  });
  await expect(thirdPartyNotices([input], root)).rejects.toThrow("pkg@1.0.0");
});

test("rejects a license field in a shape npm does not define", async () => {
  const { root, input } = await bundledPackage(
    { name: "pkg", version: "1.0.0", license: ["MIT"] },
    { LICENSE: "MIT License\n" }
  );
  await expect(thirdPartyNotices([input], root)).rejects.toThrow("pkg@1.0.0");
});

test("carries the declared license's text for a package that ships no license file", async () => {
  const { root, input } = await bundledPackage({ name: "pkg", version: "1.0.0", license: "MIT" });
  expect(await thirdPartyNotices([input], root)).toContain(
    'THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND'
  );
});

test("treats a null license the same as nothing declared, when a license file ships", async () => {
  const { root, input } = await bundledPackage(
    { name: "pkg", version: "1.0.0", license: null },
    { LICENSE: "MIT License\n" }
  );
  expect(await thirdPartyNotices([input], root)).toContain(
    "License: (not declared in package.json)"
  );
});

test("reads the legacy `licenses` array as a declared license", async () => {
  const { root, input } = await bundledPackage({
    name: "pkg",
    version: "1.0.0",
    licenses: [{ type: "MIT" }],
  });
  expect(await thirdPartyNotices([input], root)).toContain("License: MIT");
});

test("reads the deprecated `{ type }` license object as a declared license", async () => {
  const { root, input } = await bundledPackage({
    name: "pkg",
    version: "1.0.0",
    license: { type: "ISC" },
  });
  expect(await thirdPartyNotices([input], root)).toContain("License: ISC");
});

test("the command lists every package any of its metafiles names, one bundle's alone included", async () => {
  // A package that ships two bundles (an extension and a CLI) passes one metafile per build: a
  // package only the second bundle inlines must reach the notices too.
  const root = await mkdtemp(join(tmpdir(), "third-party-notices-"));
  scratchDirectories.push(root);
  for (const name of ["shared", "cli-only"]) {
    const pkgDir = join(root, "node_modules", name);
    await mkdir(pkgDir, { recursive: true });
    await writeFile(
      join(pkgDir, "package.json"),
      JSON.stringify({ name, version: "1.0.0", license: "MIT" })
    );
    await writeFile(join(pkgDir, "index.js"), "export {}\n");
  }
  const input = (name: string) => join("node_modules", name, "index.js");
  await writeFile(
    join(root, "extensions.json"),
    JSON.stringify({ inputs: { [input("shared")]: {} } })
  );
  await writeFile(
    join(root, "cli.json"),
    JSON.stringify({ inputs: { [input("shared")]: {}, [input("cli-only")]: {} } })
  );

  const run = Bun.spawnSync(
    [
      process.execPath,
      join(import.meta.dir, "third-party-notices.ts"),
      "extensions.json",
      "cli.json",
      "NOTICES",
    ],
    { cwd: root, stderr: "pipe" }
  );
  expect({ exit: run.exitCode, stderr: run.stderr.toString() }).toEqual({ exit: 0, stderr: "" });
  const notices = await readFile(join(root, "NOTICES"), "utf8");
  expect(notices).toContain("cli-only@1.0.0\nLicense: MIT");
  expect(notices).toContain("shared@1.0.0\nLicense: MIT");
});
