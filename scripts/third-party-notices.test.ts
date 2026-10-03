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

test("rejects a bundled package whose declared license is not an SPDX expression", async () => {
  const root = await mkdtemp(join(tmpdir(), "third-party-notices-"));
  scratchDirectories.push(root);
  const packageRoot = join(root, "node_modules", "not-a-license");
  await mkdir(packageRoot, { recursive: true });
  await writeFile(
    join(packageRoot, "package.json"),
    JSON.stringify({ name: "not-a-license", version: "1.0.0", license: "DefinitelyNotALicense" })
  );
  await writeFile(join(packageRoot, "index.js"), "export {}\n");

  await expect(
    thirdPartyNotices([join("node_modules", "not-a-license", "index.js")], root)
  ).rejects.toThrow("not-a-license@1.0.0 declares an unrecognized license DefinitelyNotALicense");
});
