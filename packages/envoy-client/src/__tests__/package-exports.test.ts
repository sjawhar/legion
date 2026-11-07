import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

interface PackageManifest {
  readonly exports: Record<string, { readonly bun: string }>;
  readonly scripts: { readonly build: string };
}

const manifest = JSON.parse(
  readFileSync(new URL("../../package.json", import.meta.url), "utf-8")
) as PackageManifest;
const buildEntries = manifest.scripts.build
  .match(/^bun build (.+) --outdir /)?.[1]
  .split(" ")
  .filter((entry) => entry.endsWith(".ts"));

// The 30 s limit is a hang guard, not a budget: the row resolves and imports every build entry, and
// bun's unwritten 5,000 ms per-test default is a limit on that work rather than on a hang. Measured
// in a 2-CPU pod: 0.34 s idle, 4.33 s at most under 16 busy loops (8 x nproc), which leaves that
// default 0.67 s. It fires only if an import never resolves.
test("exposes and resolves every package build entry", async () => {
  expect(buildEntries).toBeDefined();
  for (const entry of buildEntries ?? []) {
    const exportPath = `./${entry.slice("src/".length, -".ts".length)}`;
    expect(manifest.exports[exportPath]?.bun).toBe(`./${entry}`);
    // The runtime-selected specifier exercises each package export's actual resolver path.
    await import(`@legion/envoy-client/${exportPath.slice(2)}`);
  }
}, 30_000);
