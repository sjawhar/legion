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

test("exposes and resolves every package build entry", async () => {
  expect(buildEntries).toBeDefined();
  for (const entry of buildEntries ?? []) {
    const exportPath = `./${entry.slice("src/".length, -".ts".length)}`;
    expect(manifest.exports[exportPath]?.bun).toBe(`./${entry}`);
    // The runtime-selected specifier exercises each package export's actual resolver path.
    await import(`@legion/envoy-client/${exportPath.slice(2)}`);
  }
});
