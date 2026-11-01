// The rule each plugin's no-cross-import.test.ts runs: nothing a plugin ships imports the sibling
// plugin's source. Each plugin bundles on its own, so an import of the sibling would inline a copy
// of its module into this plugin's bundle, where it is a second instance of whatever state the
// module holds; what both need lives in @legion/pi-shared, which both bundle on purpose.
import { readdirSync, readFileSync, statSync } from "node:fs";
import * as path from "node:path";

// A static or dynamic import's specifier, when it is relative; `../x`, `./x`, with or without a
// query (`./envoy.ts?entry`), which Bun uses to force a second module instance.
const RELATIVE_IMPORT = /(?:from|import)\s*\(?\s*["'](\.\.?\/[^"']+)["']/g;

/** Every shipped `.ts` under `packageRoot`'s extensions/ and src/: no test file. */
export function shippedSources(packageRoot: string): string[] {
  return ["extensions", "src"].flatMap((directory) => {
    const root = path.join(packageRoot, directory);
    return readdirSync(root, { recursive: true, encoding: "utf8" })
      .map((entry) => path.join(root, entry))
      .filter(
        (file) => file.endsWith(".ts") && !file.endsWith(".test.ts") && statSync(file).isFile()
      );
  });
}

/**
 * The relative imports of `packageRoot`'s shipped sources that resolve into `siblingRoot`, each as
 * one sentence naming the importing file and the specifier. An import of `@legion/*` is a package
 * import, not a relative one, and is never reported.
 */
export function crossImports(packageRoot: string, siblingRoot: string): string[] {
  const sibling = `${path.resolve(siblingRoot)}${path.sep}`;
  return shippedSources(packageRoot).flatMap((file) =>
    [...readFileSync(file, "utf8").matchAll(RELATIVE_IMPORT)].flatMap(([, specifier = ""]) => {
      const resolved = path.resolve(path.dirname(file), specifier.split("?")[0] ?? "");
      return resolved.startsWith(sibling)
        ? [`${path.relative(packageRoot, file)} imports ${specifier}, which is ${siblingRoot}'s`]
        : [];
    })
  );
}
