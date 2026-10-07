import { describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
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

describe("the dispatch CLI the package ships", () => {
  test("package.json ships bin/ and builds dist/dispatch.js from the envoy-client entry", async () => {
    const manifest: {
      readonly files?: readonly string[];
      readonly scripts?: Record<string, string>;
    } = JSON.parse(await readFile(path.join(PACKAGE_ROOT, "package.json"), "utf8"));

    expect(manifest.files).toContain("bin");
    expect(manifest.scripts?.build).toContain(
      "bun build ../envoy-client/bin/dispatch.ts --outfile dist/dispatch.js"
    );
  });

  test("prepack.sh gives every bun build its own metafile and reads all of them for the notices", async () => {
    // A chained `bun run build --metafile=…` hands the flag to its last command only, so the
    // extensions' packages would be missing from THIRD_PARTY_NOTICES.
    const lines = (await readFile(path.join(PACKAGE_ROOT, "scripts/prepack.sh"), "utf8")).split(
      "\n"
    );
    const builds = lines.filter((line) => line.startsWith("bun build "));
    expect(builds.map((line) => line.split(" ")[2])).toEqual([
      "extensions/envoy.ts",
      "../envoy-client/bin/dispatch.ts",
    ]);
    const metafiles = builds.map((line) => /--metafile="([^"]+)"/.exec(line)?.[1]);
    expect(metafiles.every((metafile) => metafile !== undefined)).toBe(true);
    expect(new Set(metafiles).size).toBe(builds.length);
    const notices = lines.find((line) => line.includes("third-party-notices.ts"));
    for (const metafile of metafiles) expect(notices).toContain(`"${metafile}"`);
  });

  test("the packed tarball carries an executable bin/dispatch that runs the bundled CLI", async () => {
    // Packed the way the release packs (scripts/e2e/lib/pack-plugin.sh), so this is the tarball an
    // installed plugin unpacks.
    const work = await mkdtemp(path.join(tmpdir(), "pi-envoy-pack-"));
    try {
      const pack = Bun.spawnSync(
        ["bash", path.join(REPO_ROOT, "scripts/e2e/lib/pack-plugin.sh"), path.join(work, "out")],
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
