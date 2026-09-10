import { describe, expect, test } from "bun:test";

import { CONTRAST_CHECKS } from "./classes";
import { contrastRatio, oklchToSrgb8 } from "./contrast";
import { SLATE_50, SLATE_900, SLATE_950 } from "./palette";

describe("oklchToSrgb8", () => {
  // Cross-checks against Tailwind's well-known v3 hex constants for the (mostly unchanged)
  // slate scale, to catch a transposed OKLCH triple or a broken conversion formula.
  test.each([
    ["slate-50", SLATE_50, [248, 250, 252]],
    ["slate-900", SLATE_900, [15, 23, 43]],
    ["slate-950", SLATE_950, [2, 6, 24]],
  ] as const)("converts %s close to its known sRGB value", (_name, swatch, expected) => {
    const [r, g, b] = oklchToSrgb8(swatch.oklch);
    expect(Math.abs(r - expected[0])).toBeLessThanOrEqual(2);
    expect(Math.abs(g - expected[1])).toBeLessThanOrEqual(2);
    expect(Math.abs(b - expected[2])).toBeLessThanOrEqual(2);
  });
});

describe("dark-mode palette contrast", () => {
  // `CONTRAST_CHECKS` is populated by classes.ts itself, once per color role it exports — this
  // test is exhaustive over every semantic role the SPA can render, not a hand-picked sample:
  // adding a new role to classes.ts without registering its check is the only way to skip this
  // gate, and `no-raw-colors.test.ts` guarantees a component cannot introduce a color pairing
  // that bypasses classes.ts in the first place.
  test.each(
    CONTRAST_CHECKS.map((check) => [check.description, check] as const)
  )("%s meets its WCAG AA threshold in both color schemes", (_description, check) => {
    const lightRatio = contrastRatio(check.foreground.light.oklch, check.background.light.oklch);
    const darkRatio = contrastRatio(check.foreground.dark.oklch, check.background.dark.oklch);
    expect(lightRatio).toBeGreaterThanOrEqual(check.minRatio);
    expect(darkRatio).toBeGreaterThanOrEqual(check.minRatio);
  });

  test("registers at least one check per callout, badge, and text role", () => {
    // A regression guard for the registration mechanism itself: if `classes.ts` stopped
    // exporting roles through `registerText`/`badge`, this count would collapse to near-zero
    // without any single check failing.
    expect(CONTRAST_CHECKS.length).toBeGreaterThanOrEqual(20);
  });
});
