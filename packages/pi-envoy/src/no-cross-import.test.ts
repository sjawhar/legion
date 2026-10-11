import { expect, test } from "bun:test";
import * as path from "node:path";
import { crossImports } from "@legion/pi-shared/test/cross-imports";

const packageRoot = path.resolve(import.meta.dir, "..");

// What this plugin ships bundles on its own: an import of packages/pi-legion would inline a copy of
// a Legion module into the Envoy bundle. What both plugins need lives in @legion/pi-shared.
test("no shipped source imports the Legion plugin's", () => {
  expect(crossImports(packageRoot, path.resolve(packageRoot, "../pi-legion"))).toEqual([]);
});
