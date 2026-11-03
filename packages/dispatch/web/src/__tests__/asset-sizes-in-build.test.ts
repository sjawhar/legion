import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { MAX_ASSET_BYTES, oversizedAssets } from "../asset-sizes-in-build";

const dirs: string[] = [];

afterEach(() => {
  for (const dir of dirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

function assetsDir(files: Record<string, number>): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-assets-"));
  dirs.push(dir);
  for (const [name, bytes] of Object.entries(files)) {
    const file = join(dir, name);
    mkdirSync(join(file, ".."), { recursive: true });
    writeFileSync(file, new Uint8Array(bytes));
  }
  return dir;
}

test("a build whose every file fits the server's limit passes", () => {
  const dir = assetsDir({
    "index-a.js": MAX_ASSET_BYTES,
    "lib-a.js": 1_747_955,
    "fonts/inter.woff2": 1,
  });
  expect(oversizedAssets(dir)).toEqual([]);
});

test("a file one byte over the limit, at any depth, is named with its size", () => {
  const dir = assetsDir({
    "index-a.js": 10,
    "huge-a.js": MAX_ASSET_BYTES + 1,
    "nested/huge-b.wasm": 9 * 1024 * 1024,
  });
  expect(oversizedAssets(dir)).toEqual([
    { path: "huge-a.js", bytes: MAX_ASSET_BYTES + 1 },
    { path: "nested/huge-b.wasm", bytes: 9 * 1024 * 1024 },
  ]);
});
