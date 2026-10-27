import { existsSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

/**
 * The largest file the server keeps of a dashboard build: `routes.MaxRetainedAssetSize` in
 * `packages/envoy/internal/dispatch/routes/assets.go`. Every server process publishes its build's
 * assets to Dispatch's database at boot, so another build's tab can load them from it, and refuses
 * to boot on a build holding a larger file (`dispatch: publish dashboard assets`), which stalls a
 * rolling update. This check refuses such a build in CI instead.
 */
export const MAX_ASSET_BYTES = 8 * 1024 * 1024;

const DIST_ASSETS_DIR = join(import.meta.dir, "..", "dist", "assets");

export interface OversizedAsset {
  /** The file's path below the assets directory, slash-separated. */
  path: string;
  bytes: number;
}

/** Every file below `dir`, at any depth, larger than `limit` bytes, in path order. */
export function oversizedAssets(dir: string, limit = MAX_ASSET_BYTES): OversizedAsset[] {
  const oversized: OversizedAsset[] = [];
  for (const name of readdirSync(dir, { recursive: true, encoding: "utf8" })) {
    const stat = statSync(join(dir, name));
    if (stat.isFile() && stat.size > limit) {
      oversized.push({ path: name.replaceAll("\\", "/"), bytes: stat.size });
    }
  }
  return oversized.sort((a, b) => a.path.localeCompare(b.path));
}

function main(): void {
  if (!existsSync(DIST_ASSETS_DIR)) {
    throw new Error(
      `No built assets found at ${DIST_ASSETS_DIR}. Run "bun run build:web" before "bun run check:asset-sizes".`
    );
  }
  const oversized = oversizedAssets(DIST_ASSETS_DIR);
  for (const asset of oversized) {
    console.error(
      `web/dist/assets/${asset.path} is ${asset.bytes} bytes, over the ${MAX_ASSET_BYTES}-byte limit: the server refuses to boot on this build.`
    );
  }
  if (oversized.length > 0) {
    process.exit(1);
  }
  console.log(`Every file of web/dist/assets is at most ${MAX_ASSET_BYTES} bytes.`);
}

if (import.meta.main) {
  main();
}
