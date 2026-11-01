import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { oklchToSrgb8 } from "./contrast";
import * as P from "./palette";

/**
 * `styles.css` only uses hand-written color literals for the pre-hydration `:root` fallback,
 * outside components' `dark:`-className mechanism. Each literal is annotated with the matching
 * palette swatch so a manual fallback change cannot silently drift from the rendered theme.
 */

const STYLES_CSS_PATH = join(import.meta.dir, "..", "styles.css");
const stylesCss = readFileSync(STYLES_CSS_PATH, "utf8");

const swatchByName = new Map<string, P.Swatch>();
for (const value of Object.values(P)) {
  if (typeof value === "object" && value !== null && "name" in value && "oklch" in value) {
    swatchByName.set((value as P.Swatch).name, value as P.Swatch);
  }
}

/** A hex or `rgb()`/`rgb(... / N%)` color literal immediately followed by `;` and an annotation
 * comment naming the swatch it's meant to equal. */
const ANNOTATED_COLOR_PATTERN =
  /(#[0-9a-fA-F]{6}|rgb\([^)]+\));\s*\/\*\s*([a-z]+-\d{2,3})(?:\s+at\s+[\d.]+%\s+alpha)?\s*\*\//g;

function hexToRgb(hex: string): readonly [number, number, number] {
  const value = Number.parseInt(hex.slice(1), 16);
  return [(value >> 16) & 0xff, (value >> 8) & 0xff, value & 0xff];
}

function rgbFunctionToRgb(value: string): readonly [number, number, number] {
  const [r, g, b] = value
    .slice(4, -1)
    .split("/")[0]
    .trim()
    .split(/[\s,]+/)
    .map(Number);
  return [r, g, b];
}

interface Annotation {
  readonly actual: readonly [number, number, number];
  readonly swatchName: string;
}

const annotations: Annotation[] = [...stylesCss.matchAll(ANNOTATED_COLOR_PATTERN)].map((match) => {
  const [, literal, swatchName] = match;
  const actual = literal.startsWith("#") ? hexToRgb(literal) : rgbFunctionToRgb(literal);
  return { actual, swatchName };
});

/** Every hex/`rgb()` literal in the file, annotated or not — the exhaustiveness check below
 * (not just the count-of-annotated-literals guard above) is what actually prevents a future
 * hand-added color from silently skipping this whole test by never getting a `/* swatch-name *\/`
 * comment in the first place. */
const ANY_COLOR_LITERAL_PATTERN = /#[0-9a-fA-F]{6}\b|rgb\([^)]+\)/g;
const allColorLiterals = [...stylesCss.matchAll(ANY_COLOR_LITERAL_PATTERN)].map(
  (match) => match[0]
);

test("every color literal in the file carries a swatch-name annotation", () => {
  expect(allColorLiterals.length).toBe(annotations.length);
});

describe("every annotated styles.css color literal matches its named palette swatch exactly", () => {
  test.each(
    annotations.map((annotation) => [annotation.swatchName, annotation] as const)
  )("%s", (swatchName, { actual }) => {
    const swatch = swatchByName.get(swatchName);
    if (swatch === undefined) {
      throw new Error(`styles.css names swatch "${swatchName}", which palette.ts doesn't export`);
    }
    expect(actual).toEqual(oklchToSrgb8(swatch.oklch));
  });
});
