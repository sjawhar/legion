import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { oklchToSrgb8 } from "./contrast";
import { type ColorNotation, type ColorUse, findColors } from "./css-colors";
import * as P from "./palette";

/**
 * `styles.css` only uses hand-written color literals for the pre-hydration `:root` fallback,
 * outside components' `dark:`-className mechanism. Each literal is annotated with the matching
 * palette swatch so a manual fallback change cannot silently drift from the rendered theme.
 * `css-colors.ts` finds every colour a human can write there, in any notation; the pin accepts
 * one of two notations, and only annotated, so a colour added in any other way fails here
 * rather than slipping past a pattern that never looked for it.
 */

const STYLES_CSS_PATH = join(import.meta.dir, "..", "styles.css");
const stylesCss = readFileSync(STYLES_CSS_PATH, "utf8");

const swatchByName = new Map<string, P.Swatch>();
for (const value of Object.values(P)) {
  if (typeof value === "object" && value !== null && "name" in value && "oklch" in value) {
    swatchByName.set((value as P.Swatch).name, value as P.Swatch);
  }
}

/** The notations the pin can check against a swatch. Every other notation fails, however it is
 * annotated: the fallback is read by a human against `palette.ts`, and a second notation for the
 * same colour is a second thing to keep in sync. */
const PINNED_NOTATIONS: readonly ColorNotation[] = ["hex6", "rgb"];

/** The colours that fail the pin: an unannotated one, or one in a notation it cannot check. */
function unpinned(uses: readonly ColorUse[]): readonly ColorUse[] {
  return uses.filter((use) => use.annotation === null || !PINNED_NOTATIONS.includes(use.notation));
}

interface ReaderCase {
  readonly name: string;
  readonly css: string;
  /** Not `readonly`: `toEqual`'s expected value is a mutable array. */
  readonly found: { literal: string; notation: ColorNotation }[];
}

/** An SVG data URI, written as `styles.css` writes one (`%3C`-escaped, single-quoted
 * attributes), with the paint attributes a case needs. */
function dataUri(attributes: string): string {
  return `url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg'%3E%3Crect ${attributes}/%3E%3C/svg%3E")`;
}

/** Every notation R13 names, each in a declaration the reader must read as setting a colour. */
const NOTATION_CASES: readonly ReaderCase[] = [
  {
    name: "a 3-digit hex",
    css: "a { color: #abc; }",
    found: [{ literal: "#abc", notation: "hex3" }],
  },
  {
    name: "a 4-digit hex",
    css: "a { background-color: #abcd; }",
    found: [{ literal: "#abcd", notation: "hex4" }],
  },
  {
    name: "a 6-digit hex",
    css: "a { color: #0f172b; }",
    found: [{ literal: "#0f172b", notation: "hex6" }],
  },
  {
    name: "an 8-digit hex",
    css: "a { border-bottom-color: #0f172b80; }",
    found: [{ literal: "#0f172b80", notation: "hex8" }],
  },
  {
    name: "a %23-escaped hex in a data URI",
    css: `a { background-image: ${dataUri("fill='%230f172b'")}; }`,
    found: [{ literal: "%230f172b", notation: "escaped-hex" }],
  },
  {
    name: "rgb()",
    css: "a { color: rgb(15 23 43); }",
    found: [{ literal: "rgb(15 23 43)", notation: "rgb" }],
  },
  {
    name: "rgba()",
    css: "a { background: rgba(15, 23, 43, 0.5); }",
    found: [{ literal: "rgba(15, 23, 43, 0.5)", notation: "rgba" }],
  },
  {
    name: "hsl()",
    css: "a { outline-color: hsl(222 47% 11%); }",
    found: [{ literal: "hsl(222 47% 11%)", notation: "hsl" }],
  },
  {
    name: "hsla()",
    css: "a { text-decoration-color: hsla(222, 47%, 11%, 0.5); }",
    found: [{ literal: "hsla(222, 47%, 11%, 0.5)", notation: "hsla" }],
  },
  {
    name: "oklch()",
    css: "a { --pre-bg: oklch(0.208 0.042 265.755); }",
    found: [{ literal: "oklch(0.208 0.042 265.755)", notation: "oklch" }],
  },
  {
    name: "oklab()",
    css: "a { box-shadow: inset 0 -2px 0 oklab(0.21 -0.01 -0.06); }",
    found: [{ literal: "oklab(0.21 -0.01 -0.06)", notation: "oklab" }],
  },
  {
    name: "a named colour",
    css: "a { caret-color: rebeccapurple; }",
    found: [{ literal: "rebeccapurple", notation: "named" }],
  },
  {
    name: "a named colour in a custom property",
    css: "a { --blockquote-border: tan; }",
    found: [{ literal: "tan", notation: "named" }],
  },
  {
    name: "a named colour in an SVG fill in a data URI",
    css: `a { background-image: ${dataUri("fill='white' fill-opacity='0.5'")}; }`,
    found: [{ literal: "white", notation: "named" }],
  },
  {
    name: "a named colour in an SVG stroke in a data URI",
    css: `a { background: ${dataUri("stroke='black' stroke-width='2.25'")} center / 100%; }`,
    found: [{ literal: "black", notation: "named" }],
  },
];

/** What a colour notation looks like but is not a colour anyone wrote. */
const EXEMPTION_CASES: readonly ReaderCase[] = [
  {
    name: "a fragment reference",
    css: "a { fill: url(#tick); }",
    found: [],
  },
  {
    name: "a fragment reference inside a data URI",
    css: `a { background-image: ${dataUri("fill='none' mask='url(%23tick)'")}; }`,
    found: [],
  },
  {
    name: "a colour in a CSS comment",
    css: "a { /* was color: #0f172b, and tomato before that (#1434/#1452) */ color: inherit; }",
    found: [],
  },
  {
    name: "transparent",
    css: "a { --bg-color: transparent; }",
    found: [],
  },
  {
    name: "currentColor",
    css: "a { --text-color: currentColor; }",
    found: [],
  },
  {
    name: "a named colour inside a longer identifier",
    css: "a { background-color: var(--color-red-500); }",
    found: [],
  },
  {
    name: "a data URI as the value of mask",
    css: `a { mask: ${dataUri("fill='white' stroke='black'")} center / 100% no-repeat; }`,
    found: [],
  },
  {
    name: "a data URI as the value of mask-image",
    css: `a { mask-image: ${dataUri("fill='white' stroke='black'")}; }`,
    found: [],
  },
  {
    name: "the colour space of a color-mix()",
    css: "a { background-color: color-mix(in oklab, var(--color-amber-300) 30%, transparent); }",
    found: [],
  },
  {
    name: "the colour space of a color-mix() over currentColor",
    css: "a { border: 1px solid color-mix(in srgb, currentColor 55%, transparent); }",
    found: [],
  },
  {
    name: "a property that sets no colour",
    css: "a { font-family: Inter, ui-sans-serif, system-ui, sans-serif; }",
    found: [],
  },
];

describe("the reader finds every notation a colour can be written in", () => {
  test.each(
    NOTATION_CASES.map((readerCase) => [readerCase.name, readerCase] as const)
  )("%s", (_name, { css, found }) => {
    const uses = findColors(css);
    expect(uses.map(({ literal, notation }) => ({ literal, notation }))).toEqual(found);
    expect(unpinned(uses)).toHaveLength(uses.length);
  });
});

describe("the reader finds no colour where none is written", () => {
  test.each(
    EXEMPTION_CASES.map((readerCase) => [readerCase.name, readerCase] as const)
  )("%s", (_name, { css, found }) => {
    expect(findColors(css).map(({ literal, notation }) => ({ literal, notation }))).toEqual(found);
  });
});

test("the mask exemption covers only mask and mask-image", () => {
  const painted = dataUri("fill='white' stroke='black'");
  expect(findColors(`a { background-image: ${painted}; }`).map((use) => use.literal)).toEqual([
    "white",
    "black",
  ]);
  expect(findColors(`a { border-image-source: ${painted}; }`).map((use) => use.literal)).toEqual([
    "white",
    "black",
  ]);
});

describe("the pin accepts an annotated 6-digit hex or rgb() and nothing else", () => {
  test("an annotated literal passes", () => {
    const uses = findColors("a { box-shadow: inset 0 -2px 0 #0084d1; /* sky-600 */ }");
    expect(uses).toEqual([
      {
        literal: "#0084d1",
        notation: "hex6",
        property: "box-shadow",
        line: 1,
        annotation: "sky-600",
      },
    ]);
    expect(unpinned(uses)).toEqual([]);
  });

  test("the annotation belongs to the literal that ends the declaration", () => {
    const uses = findColors("a { box-shadow: 0 0 0 #0084d1, 0 0 0 #00bcff; /* sky-400 */ }");
    expect(uses.map((use) => [use.literal, use.annotation])).toEqual([
      ["#0084d1", null],
      ["#00bcff", "sky-400"],
    ]);
    expect(unpinned(uses).map((use) => use.literal)).toEqual(["#0084d1"]);
  });
});

const colorUses = findColors(stylesCss);

test("every color literal in the file carries a swatch-name annotation", () => {
  expect(unpinned(colorUses)).toEqual([]);
});

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

const annotations: Annotation[] = colorUses.flatMap((use) =>
  use.annotation === null
    ? []
    : [
        {
          actual: use.literal.startsWith("#")
            ? hexToRgb(use.literal)
            : rgbFunctionToRgb(use.literal),
          swatchName: use.annotation,
        },
      ]
);

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
