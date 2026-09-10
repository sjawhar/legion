import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * The mechanical half of the dark-mode palette contract: `classes.ts` is the only place a
 * component-facing color className gets built, and `palette.test.ts` verifies every role it
 * exports. Neither guarantees anything about the *components* unless nothing outside `theme/`
 * can write a color utility literal directly — this test is that guarantee. A component that
 * writes `className="bg-white ..."` instead of importing `surfaceBg` from `classes.ts` compiles
 * fine and fails only here. Scans every `.ts`/`.tsx` source file in `web/src` (outside `theme/`)
 * and every `.ts` file in `e2e/` — Playwright specs and helpers are just as capable of writing
 * a raw Tailwind color utility (e.g. as a `page.locator` selector or a `toHaveClass` pattern) as
 * a component is.
 */
const COLOR_FAMILIES = [
  "slate",
  "sky",
  "rose",
  "amber",
  "emerald",
  "red",
  "orange",
  "yellow",
  "lime",
  "green",
  "teal",
  "cyan",
  "blue",
  "indigo",
  "violet",
  "purple",
  "fuchsia",
  "pink",
  "gray",
  "zinc",
  "neutral",
  "stone",
] as const;

/** Numbered shades (`bg-sky-700`) plus the three unnumbered keyword colors (`text-white`,
 * `bg-black`, `border-transparent`) — all three are just as capable of drifting from the
 * documented palette/composite layer as a numbered shade is. */
const RAW_COLOR_PATTERN = new RegExp(
  `\\b(?:text|bg|border|ring|placeholder|divide|outline|caret|accent|decoration|from|via|to)-(?:(?:${COLOR_FAMILIES.join("|")})-\\d{2,3}|white|black|transparent)\\b`
);

const WEB_SRC_ROOT = join(import.meta.dir, "..");
const E2E_ROOT = join(import.meta.dir, "..", "..", "..", "e2e");

function collectSourceFiles(dir: string, root: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (dir === root && entry.name === "theme") {
      continue;
    }
    const entryPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectSourceFiles(entryPath, root));
      continue;
    }
    const isSource = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
    const isUnitTest = entry.name.endsWith(".test.ts") || entry.name.endsWith(".test.tsx");
    if (isSource && !isUnitTest) {
      files.push(entryPath);
    }
  }
  return files;
}

const SCANNED_FILES = [
  ...collectSourceFiles(WEB_SRC_ROOT, WEB_SRC_ROOT).map(
    (file) => [join("web/src", file.slice(WEB_SRC_ROOT.length + 1)), file] as const
  ),
  ...collectSourceFiles(E2E_ROOT, E2E_ROOT).map(
    (file) => [join("e2e", file.slice(E2E_ROOT.length + 1)), file] as const
  ),
];

describe("no raw Tailwind color utilities outside theme/", () => {
  test("scans a non-trivial number of source files", () => {
    // A regression guard on the scan itself: if this collapsed to 0, every check below would
    // vacuously pass without covering anything.
    expect(SCANNED_FILES.length).toBeGreaterThan(20);
  });

  test.each(SCANNED_FILES)("%s has no raw color utility literal", (_relativePath, file) => {
    const source = readFileSync(file, "utf8");
    const lines = source.split("\n");
    const offenders = lines
      .map((line, index) => ({ line, number: index + 1 }))
      .filter(({ line }) => RAW_COLOR_PATTERN.test(line));
    expect(offenders).toEqual([]);
  });
});

/**
 * The bug that motivated the two tests above: a class name built at runtime by concatenating a
 * prefix onto a swatch name (`` `${prefix}-${swatch.name}` ``) never appears as literal text in
 * any source file, so Tailwind's scanner cannot see it and silently drops it from the built
 * stylesheet (see `classes-in-build-css.test.ts`). This rejects the construction pattern itself
 * — a hyphen glued directly onto a template-literal expression, with no whitespace between them
 * — inside `classes.ts`, the one file allowed to compose Tailwind classes at all. Comments are
 * stripped first so this test can describe the pattern in prose without failing on itself.
 */
const DYNAMIC_CLASS_CONSTRUCTION_PATTERN = /\$\{[^}]+\}-|-\$\{[^}]+\}/;

function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

describe("no dynamic Tailwind class construction in classes.ts", () => {
  test("classes.ts never glues a hyphen directly onto a template expression", () => {
    const source = stripComments(readFileSync(join(WEB_SRC_ROOT, "theme", "classes.ts"), "utf8"));
    const lines = source.split("\n");
    const offenders = lines
      .map((line, index) => ({ line, number: index + 1 }))
      .filter(({ line }) => DYNAMIC_CLASS_CONSTRUCTION_PATTERN.test(line));
    expect(offenders).toEqual([]);
  });
});
