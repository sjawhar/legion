import { afterAll, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, rmSync } from "node:fs";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { stageSkills } from "@legion/pi-shared/test/skills-guard";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");
const REPO_ROOT = path.resolve(PACKAGE_ROOT, "../..");
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

describe("the dispatch CLI the package ships", () => {
  test("package.json ships bin/ and builds dist/dispatch.js with envoy-client's one builder", async () => {
    const manifest: {
      readonly files?: readonly string[];
      readonly scripts?: Record<string, string>;
    } = JSON.parse(await readFile(path.join(PACKAGE_ROOT, "package.json"), "utf8"));

    expect(manifest.files).toContain("bin");
    expect(manifest.scripts?.build).toContain("../envoy-client/scripts/build-dispatch-cli.sh dist");
    expect(manifest.scripts?.build).not.toContain("bin/dispatch.ts");
  });

  test("the prepack gives each bundle its own metafile and reads both for the notices", async () => {
    // A chained `bun run build --metafile=…` hands the flag to its last command only, so the
    // extension's packages would be missing from THIRD_PARTY_NOTICES.
    const lines = (await readFile(path.join(REPO_ROOT, "scripts/pi-plugin-prepack.sh"), "utf8"))
      .split("\n")
      .map((line) => line.trimStart());
    const envoy = lines.find((line) => line.startsWith("bun build extensions/envoy.ts "));
    const dispatch = lines.find(
      (line) => !line.startsWith("#") && line.includes("build-dispatch-cli.sh")
    );
    expect(lines.some((line) => line.includes("bin/dispatch.ts"))).toBe(false);
    // `--metafile="$metafiles/…"` for bun build, `"$metafiles/…"` as the builder's argument.
    const metafiles = [envoy, dispatch].map(
      (line) => /"(\$metafiles\/[^"]+)"/.exec(line ?? "")?.[1]
    );
    expect(metafiles.every((metafile) => metafile !== undefined)).toBe(true);
    expect(new Set(metafiles).size).toBe(2);
    const notices = lines.find((line) => line.includes("third-party-notices.ts"));
    for (const metafile of metafiles) expect(notices).toContain(`"${metafile}"`);
  });

  test("the packed tarball carries an executable bin/dispatch that runs the bundled CLI", async () => {
    // Packed the way the release packs (scripts/e2e/lib/pack-plugin.sh), so this is the tarball an
    // installed plugin unpacks.
    const work = await mkdtemp(path.join(tmpdir(), "pi-envoy-pack-"));
    try {
      const pack = Bun.spawnSync(
        [
          "bash",
          path.join(REPO_ROOT, "scripts/e2e/lib/pack-plugin.sh"),
          "pi-envoy",
          path.join(work, "out"),
        ],
        { stderr: "pipe" }
      );
      expect({
        exit: pack.exitCode,
        stderr: pack.exitCode === 0 ? "" : pack.stderr.toString(),
      }).toEqual({ exit: 0, stderr: "" });
      const tarball = pack.stdout.toString().trim();
      const listing = Bun.spawnSync(["tar", "tvzf", tarball]).stdout.toString().split("\n");
      const entry = (name: string) => listing.find((line) => line.endsWith(` package/${name}`));
      expect(entry("bin/dispatch")).toStartWith("-rwxr-xr-x ");
      expect(entry("dist/dispatch.js")).toBeDefined();

      const unpacked = path.join(work, "plugin");
      Bun.spawnSync(["mkdir", "-p", unpacked]);
      expect(
        Bun.spawnSync(["tar", "xzf", tarball, "-C", unpacked, "--strip-components=1"]).exitCode
      ).toBe(0);
      expect(await readdir(path.join(unpacked, "bin"))).toEqual(["dispatch"]);
      const help = Bun.spawnSync([path.join(unpacked, "bin/dispatch"), "--help"], {
        env: { ...process.env, DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "pack-check" },
      });
      expect(help.exitCode).toBe(0);
      expect(help.stdout.toString()).toStartWith("Usage: dispatch ");
    } finally {
      await rm(work, { recursive: true, force: true });
    }
  }, 120_000);
});
