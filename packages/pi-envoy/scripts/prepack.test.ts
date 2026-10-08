import { afterAll, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import { stageSkills } from "@legion/pi-shared/test/skills-guard";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");
const manifest: {
  readonly name: string;
  readonly omp?: {
    readonly extensions?: readonly string[];
    readonly skills?: readonly string[];
  };
  readonly scripts?: Record<string, string>;
} = JSON.parse(readFileSync(path.join(PACKAGE_ROOT, "package.json"), "utf8"));

describe("omp.skills / prepack manifest contract", () => {
  test("package.json declares omp.skills alongside omp.extensions, both pointing into dist/", () => {
    // omp discovers plugin skills only at `<plugin root>/skills` or at directories the
    // manifest's `omp.skills` array names. This package ships skills at `dist/skills` (staged
    // there by the prepack, not the package root), so the manifest must name it explicitly —
    // dropping this field silently regresses every installed session back to no Dispatch skills.
    expect(manifest.omp?.skills).toEqual(["dist/skills"]);
    // The release workflow rewrites `omp.extensions` (only) to the packed bundle path right
    // before packing; `omp.skills` ships as committed, so it must already be the `dist/` path a
    // packed tarball actually contains, in both source and released manifests.
    expect(manifest.omp?.extensions?.every((entry) => entry.startsWith("extensions/"))).toBe(true);
  });

  test("prepack is the shared script, and postpack removes the directory it stages", () => {
    // The one script packs both plugins and holds the skills partition; `postpack` removes the
    // directory it staged so a dev checkout's `dist/` doesn't retain packed-only content after a
    // local `bun pm pack`.
    expect(manifest.scripts?.prepack).toBe("../../scripts/pi-plugin-prepack.sh");
    expect(manifest.scripts?.postpack).toBe("rm -rf dist/skills");
  });

  describe("the staged skills", () => {
    const staged = stageSkills(manifest.name);
    afterAll(() => {
      rmSync(path.dirname(staged), { recursive: true, force: true });
    });

    test("are exactly this plugin's partition: the Dispatch and Envoy skills every session uses", () => {
      expect(readdirSync(staged).sort()).toEqual([
        "dispatch",
        "dispatch-brainstorming",
        "dispatch-first",
        "envoy",
      ]);
    });
  });
});
