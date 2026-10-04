import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
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
  const packageRoot = join(root, "node_modules", "pkg");
  await mkdir(packageRoot, { recursive: true });
  await writeFile(join(packageRoot, "package.json"), JSON.stringify(manifest));
  await writeFile(join(packageRoot, "index.js"), "export {}\n");
  for (const [name, text] of Object.entries(files)) await writeFile(join(packageRoot, name), text);
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
