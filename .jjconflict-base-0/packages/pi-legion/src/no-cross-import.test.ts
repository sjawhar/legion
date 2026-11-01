import { expect, test } from "bun:test";
import * as path from "node:path";
import { crossImports } from "@legion/pi-shared/test/cross-imports";

const packageRoot = path.resolve(import.meta.dir, "..");

// What this plugin ships bundles on its own: an import of packages/pi-envoy would inline a copy of
// an Envoy module into the Legion bundle, a second instance of its state beside the Envoy plugin's
// own. The Legion entry reaches the Envoy plugin through @legion/pi-shared's interface instead;
// only the tests load the sibling's entry by path.
test("no shipped source imports the Envoy plugin's", () => {
  expect(crossImports(packageRoot, path.resolve(packageRoot, "../pi-envoy"))).toEqual([]);
});
