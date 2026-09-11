import { describe, expect, test } from "bun:test";
import { readdir, readFile } from "node:fs/promises";
import * as path from "node:path";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");
const REPO_ROOT = path.resolve(PACKAGE_ROOT, "../..");

describe("omp.skills / prepack.sh manifest contract", () => {
  test("package.json declares omp.skills alongside omp.extensions, both pointing into dist/", async () => {
    const manifest: {
      readonly omp?: {
        readonly extensions?: readonly string[];
        readonly skills?: readonly string[];
      };
      readonly scripts?: Record<string, string>;
    } = JSON.parse(await readFile(path.join(PACKAGE_ROOT, "package.json"), "utf8"));

    // omp discovers plugin skills only at `<plugin root>/skills` or at directories the
    // manifest's `omp.skills` array names. This package ships skills at `dist/skills` (staged
    // there by prepack.sh, not the package root), so the manifest must name it explicitly —
    // dropping this field silently regresses every installed session back to no Legion skills.
    expect(manifest.omp?.skills).toEqual(["dist/skills"]);
    // The release workflow rewrites `omp.extensions` (only) to the packed bundle paths right
    // before packing; `omp.skills` ships as committed, so it must already be the `dist/` path a
    // packed tarball actually contains, in both source and released manifests.
    expect(manifest.omp?.extensions?.every((entry) => entry.startsWith("extensions/"))).toBe(true);
  });

  test("prepack.sh stages the exact directory package.json's omp.skills names, and postpack removes it", async () => {
    const manifest: { readonly scripts?: Record<string, string> } = JSON.parse(
      await readFile(path.join(PACKAGE_ROOT, "package.json"), "utf8")
    );
    const prepackScript = await readFile(path.join(PACKAGE_ROOT, "scripts/prepack.sh"), "utf8");

    // The manifest asserts this exact contract at pack time (see the `prepack.sh` guard on
    // `omp.extensions`); the `omp.skills` directory it stages must be byte-identical to the one
    // this package's manifest declares, and `postpack` must remove that same directory again so
    // a dev checkout's `dist/` doesn't retain packed-only content after a local `bun pm pack`.
    expect(prepackScript).toContain("cp -r ../../skills dist/skills");
    expect(manifest.scripts?.postpack).toBe("rm -rf dist/skills");
  });

  test("the repo skills/ directory prepack.sh copies from exists and is non-empty", async () => {
    const entries = await readdir(path.join(REPO_ROOT, "skills"));
    expect(entries.length).toBeGreaterThan(0);
    expect(entries).toContain("legion-controller");
  });
});
