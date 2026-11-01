/**
 * Pure color math used to verify the dark-mode palette in `palette.ts`. No dependency on the
 * DOM or a browser: every Tailwind color in this app's default theme is defined in OKLCH
 * (see `tailwindcss/theme.css`), so contrast checks convert OKLCH to sRGB and apply the WCAG
 * relative-luminance formula directly, rather than trusting hand-copied hex approximations.
 */

export interface Oklch {
  /** Lightness, 0-1. */
  readonly l: number;
  /** Chroma. */
  readonly c: number;
  /** Hue, degrees. */
  readonly h: number;
}

export type Rgb8 = readonly [r: number, g: number, b: number];

/** Converts an OKLCH color to 8-bit sRGB, clamping out-of-gamut components. */
export function oklchToSrgb8({ l, c, h }: Oklch): Rgb8 {
  const hueRadians = (h * Math.PI) / 180;
  const a = c * Math.cos(hueRadians);
  const b = c * Math.sin(hueRadians);

  const lPrime = l + 0.3963377774 * a + 0.2158037573 * b;
  const mPrime = l - 0.1055613458 * a - 0.0638541728 * b;
  const sPrime = l - 0.0894841775 * a - 1.291485548 * b;

  const lCubed = lPrime ** 3;
  const mCubed = mPrime ** 3;
  const sCubed = sPrime ** 3;

  const rLinear = 4.0767416621 * lCubed - 3.3077115913 * mCubed + 0.2309699292 * sCubed;
  const gLinear = -1.2684380046 * lCubed + 2.6097574011 * mCubed - 0.3413193965 * sCubed;
  const bLinear = -0.0041960863 * lCubed - 0.7034186147 * mCubed + 1.707614701 * sCubed;

  return [gammaEncode(rLinear), gammaEncode(gLinear), gammaEncode(bLinear)];
}

function gammaEncode(linear: number): number {
  const clamped = Math.min(1, Math.max(0, linear));
  const encoded = clamped <= 0.0031308 ? 12.92 * clamped : 1.055 * clamped ** (1 / 2.4) - 0.055;
  return Math.round(encoded * 255);
}

/** WCAG relative luminance of an 8-bit sRGB color (https://www.w3.org/TR/WCAG21/#dfn-relative-luminance).
 * The published spec threshold (0.03928) is a known erratum: 0.03928/12.92 = 0.003040, which
 * does not match the encoding side's own breakpoint (0.0031308). The corrected constant,
 * 0.04045, makes the piecewise function continuous (0.04045/12.92 = 0.0031308 exactly) and is
 * what the W3C's own reference implementation and browsers use. */
function relativeLuminance([r, g, b]: Rgb8): number {
  const linearize = (channel8: number) => {
    const channel = channel8 / 255;
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * linearize(r) + 0.7152 * linearize(g) + 0.0722 * linearize(b);
}

/** WCAG contrast ratio between two OKLCH colors, in the range [1, 21]. */
export function contrastRatio(a: Oklch, b: Oklch): number {
  const luminanceA = relativeLuminance(oklchToSrgb8(a));
  const luminanceB = relativeLuminance(oklchToSrgb8(b));
  const lighter = Math.max(luminanceA, luminanceB);
  const darker = Math.min(luminanceA, luminanceB);
  return (lighter + 0.05) / (darker + 0.05);
}
