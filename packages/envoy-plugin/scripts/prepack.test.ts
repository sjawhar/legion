import { describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");

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

  test("prepack.sh gives each bundle its own metafile and reads both for the notices", async () => {
    // A chained `bun run build --metafile=…` hands the flag to its last command only, so the
    // server bundle's packages would be missing from THIRD_PARTY_NOTICES.
    const lines = (await readFile(path.join(PACKAGE_ROOT, "scripts/prepack.sh"), "utf8")).split(
      "\n"
    );
    const server = lines.find((line) => line.startsWith("bun build src/server.ts "));
    const dispatch = lines.find((line) => line.includes("build-dispatch-cli.sh dist "));
    expect(lines.some((line) => line.includes("bin/dispatch.ts"))).toBe(false);
    const metafiles = [server, dispatch].map(
      (line) => /"(\$metafiles\/[^"]+)"/.exec(line ?? "")?.[1]
    );
    expect(metafiles.every((metafile) => metafile !== undefined)).toBe(true);
    expect(new Set(metafiles).size).toBe(2);
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
