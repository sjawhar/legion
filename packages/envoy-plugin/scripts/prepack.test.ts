import { describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");

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
    // server bundle's packages would be missing from THIRD_PARTY_NOTICES.
    const lines = (await readFile(path.join(PACKAGE_ROOT, "scripts/prepack.sh"), "utf8")).split(
      "\n"
    );
    const builds = lines.filter((line) => line.startsWith("bun build "));
    expect(builds.map((line) => line.split(" ")[2])).toEqual([
      "src/server.ts",
      "../envoy-client/bin/dispatch.ts",
    ]);
    const metafiles = builds.map((line) => /--metafile="([^"]+)"/.exec(line)?.[1]);
    expect(metafiles.every((metafile) => metafile !== undefined)).toBe(true);
    expect(new Set(metafiles).size).toBe(builds.length);
    const notices = lines.find((line) => line.includes("third-party-notices.ts"));
    for (const metafile of metafiles) expect(notices).toContain(`"${metafile}"`);
  });

  test("the packed tarball carries an executable bin/dispatch that runs the bundled CLI", async () => {
    // Packed as release.yaml packs it (`bun pm pack` in the package), so this is the tarball an
    // OpenCode install unpacks.
    const work = await mkdtemp(path.join(tmpdir(), "envoy-plugin-pack-"));
    try {
      const pack = Bun.spawnSync(["bun", "pm", "pack", "--destination", work], {
        cwd: PACKAGE_ROOT,
        stderr: "pipe",
      });
      expect({
        exit: pack.exitCode,
        stderr: pack.exitCode === 0 ? "" : pack.stderr.toString(),
      }).toEqual({ exit: 0, stderr: "" });
      const [tarball] = (await readdir(work)).filter((name) => name.endsWith(".tgz"));
      const archive = path.join(work, tarball ?? "");
      const listing = Bun.spawnSync(["tar", "tvzf", archive]).stdout.toString().split("\n");
      const entry = (name: string) => listing.find((line) => line.endsWith(` package/${name}`));
      expect(entry("bin/dispatch")).toStartWith("-rwxr-xr-x ");
      expect(entry("dist/dispatch.js")).toBeDefined();
      expect(entry("dist/THIRD_PARTY_NOTICES")).toBeDefined();

      const unpacked = path.join(work, "plugin");
      Bun.spawnSync(["mkdir", "-p", unpacked]);
      expect(
        Bun.spawnSync(["tar", "xzf", archive, "-C", unpacked, "--strip-components=1"]).exitCode
      ).toBe(0);
      const help = Bun.spawnSync([path.join(unpacked, "bin/dispatch"), "--help"], {
        env: { ...process.env, DISPATCH_HOST: "opencode", DISPATCH_SESSION_ID: "pack-check" },
      });
      expect(help.exitCode).toBe(0);
      expect(help.stdout.toString()).toStartWith("Usage: dispatch ");
    } finally {
      await rm(work, { recursive: true, force: true });
    }
  }, 120_000);
});
