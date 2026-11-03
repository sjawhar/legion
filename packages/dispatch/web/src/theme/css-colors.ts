/**
 * Finds the colours a stylesheet writes by hand, for `styles-css-pin.test.ts`.
 *
 * One regular expression over the whole file cannot do this: it reads the `#1434` of an issue
 * number in a comment as a hex colour, the `%23tick` of a fragment reference as an escaped one,
 * and the `white` of an SVG mask as a named colour. So this walks the stylesheet's declarations
 * instead, with comments dropped, and looks for colour notations only where a colour can be
 * set: in the value of a declaration whose property sets a colour (or is a custom property),
 * and in an SVG `fill`/`stroke` attribute inside a data URI, which paints a colour whatever
 * property carries it. A data URI under `mask`/`mask-image` paints mask alpha, not colour, so
 * its attributes are skipped.
 */

export type ColorNotation =
  | "hex3"
  | "hex4"
  | "hex6"
  | "hex8"
  | "escaped-hex"
  | "rgb"
  | "rgba"
  | "hsl"
  | "hsla"
  | "oklch"
  | "oklab"
  | "named";

export interface ColorUse {
  /** The colour exactly as written. */
  readonly literal: string;
  readonly notation: ColorNotation;
  /** The lowercased property of the declaration the colour sits in. */
  readonly property: string;
  /** 1-based line the declaration starts on. */
  readonly line: number;
  /**
   * The swatch name of the `literal; /* swatch-name *\/` comment following the declaration, or
   * `null` where there is none. A colour only takes the annotation when it ends its
   * declaration's value, so a colour buried in a longer value (an SVG attribute inside a data
   * URI included) is always unannotated and can never satisfy the pin.
   */
  readonly annotation: string | null;
}

/**
 * Properties that set a colour: CSS Color Module Level 4's `color`, the backgrounds, the border,
 * outline, text-decoration, column-rule, caret and accent colour properties (longhand and the
 * shorthands that can carry a colour), SVG's `fill`/`stroke`, and the two shadows.
 */
const COLOR_PROPERTY_PATTERN =
  /^(?:color|background(?:-color)?|border(?:-(?:top|right|bottom|left|block|inline|block-start|block-end|inline-start|inline-end))?(?:-color)?|outline(?:-color)?|text-decoration(?:-color)?|column-rule(?:-color)?|caret-color|accent-color|fill|stroke|box-shadow|text-shadow)$/;

/** A data URI under one of these paints a mask's alpha, not a colour (`styles.css`'s
 * task-checkbox tick). */
const MASK_PROPERTY_PATTERN = /^mask(?:-image)?$/;

const NOTATION_BY_FUNCTION: Record<string, ColorNotation | undefined> = {
  rgb: "rgb",
  rgba: "rgba",
  hsl: "hsl",
  hsla: "hsla",
  oklch: "oklch",
  oklab: "oklab",
};

const NOTATION_BY_HEX_DIGITS: Record<number, ColorNotation | undefined> = {
  3: "hex3",
  4: "hex4",
  6: "hex6",
  8: "hex8",
};

/** CSS Color Module Level 4's named colours, less `transparent` and `currentColor`, which name
 * no colour of the palette's own. */
const NAMED_COLORS: ReadonlySet<string> = new Set([
  "aliceblue",
  "antiquewhite",
  "aqua",
  "aquamarine",
  "azure",
  "beige",
  "bisque",
  "black",
  "blanchedalmond",
  "blue",
  "blueviolet",
  "brown",
  "burlywood",
  "cadetblue",
  "chartreuse",
  "chocolate",
  "coral",
  "cornflowerblue",
  "cornsilk",
  "crimson",
  "cyan",
  "darkblue",
  "darkcyan",
  "darkgoldenrod",
  "darkgray",
  "darkgreen",
  "darkgrey",
  "darkkhaki",
  "darkmagenta",
  "darkolivegreen",
  "darkorange",
  "darkorchid",
  "darkred",
  "darksalmon",
  "darkseagreen",
  "darkslateblue",
  "darkslategray",
  "darkslategrey",
  "darkturquoise",
  "darkviolet",
  "deeppink",
  "deepskyblue",
  "dimgray",
  "dimgrey",
  "dodgerblue",
  "firebrick",
  "floralwhite",
  "forestgreen",
  "fuchsia",
  "gainsboro",
  "ghostwhite",
  "gold",
  "goldenrod",
  "gray",
  "green",
  "greenyellow",
  "grey",
  "honeydew",
  "hotpink",
  "indianred",
  "indigo",
  "ivory",
  "khaki",
  "lavender",
  "lavenderblush",
  "lawngreen",
  "lemonchiffon",
  "lightblue",
  "lightcoral",
  "lightcyan",
  "lightgoldenrodyellow",
  "lightgray",
  "lightgreen",
  "lightgrey",
  "lightpink",
  "lightsalmon",
  "lightseagreen",
  "lightskyblue",
  "lightslategray",
  "lightslategrey",
  "lightsteelblue",
  "lightyellow",
  "lime",
  "limegreen",
  "linen",
  "magenta",
  "maroon",
  "mediumaquamarine",
  "mediumblue",
  "mediumorchid",
  "mediumpurple",
  "mediumseagreen",
  "mediumslateblue",
  "mediumspringgreen",
  "mediumturquoise",
  "mediumvioletred",
  "midnightblue",
  "mintcream",
  "mistyrose",
  "moccasin",
  "navajowhite",
  "navy",
  "oldlace",
  "olive",
  "olivedrab",
  "orange",
  "orangered",
  "orchid",
  "palegoldenrod",
  "palegreen",
  "paleturquoise",
  "palevioletred",
  "papayawhip",
  "peachpuff",
  "peru",
  "pink",
  "plum",
  "powderblue",
  "purple",
  "rebeccapurple",
  "red",
  "rosybrown",
  "royalblue",
  "saddlebrown",
  "salmon",
  "sandybrown",
  "seagreen",
  "seashell",
  "sienna",
  "silver",
  "skyblue",
  "slateblue",
  "slategray",
  "slategrey",
  "snow",
  "springgreen",
  "steelblue",
  "tan",
  "teal",
  "thistle",
  "tomato",
  "turquoise",
  "violet",
  "wheat",
  "white",
  "whitesmoke",
  "yellow",
  "yellowgreen",
]);

/** A property name: `color`, `--custom-property`, `-webkit-anything`. */
const PROPERTY_NAME_PATTERN = /^-{0,2}[a-z][a-z0-9-]*$/;

/** The annotation a pinned literal carries, read from just past the declaration's `;`. */
const ANNOTATION_PATTERN = /^[ \t]*\/\*\s*([a-z]+-\d{2,3})(?:\s+at\s+[\d.]+%\s+alpha)?\s*\*\//;

/** An SVG paint attribute inside a data URI. `stroke-width` is not one: its name is followed by
 * `-`, not `=`. */
const SVG_PAINT_PATTERN = /\b(?:fill|stroke)=(["'])(.*?)\1/g;

const HEX_DIGIT_PATTERN = /[0-9a-fA-F]/;
/** The characters of a CSS identifier: `--color-red-500` is one token, not a `red` beside two
 * others, which is what keeps a named colour to a whole value token. */
const IDENT_CHAR_PATTERN = /[\w-]/;
const IDENT_START_PATTERN = /[a-zA-Z_-]/;

interface Declaration {
  readonly property: string;
  /** The value, comments dropped, trimmed. */
  readonly value: string;
  /** Offset of the declaration's first character in the stylesheet. */
  readonly offset: number;
  readonly annotation: string | null;
}

interface FoundColor {
  readonly literal: string;
  readonly notation: ColorNotation;
  /** Offset just past the colour in the text it was found in. */
  readonly end: number;
}

interface ValueContents {
  readonly colors: readonly FoundColor[];
  /** The text inside each `url(…)`, quotes included. */
  readonly urls: readonly string[];
}

/** Every colour the stylesheet writes by hand, in source order. */
export function findColors(css: string): readonly ColorUse[] {
  const uses: ColorUse[] = [];
  const lineAt = lineCounter(css);

  for (const declaration of readDeclarations(css)) {
    const { property, value } = declaration;
    const { colors, urls } = readValue(value);
    const found: { color: FoundColor; annotated: boolean }[] = [];

    if (COLOR_PROPERTY_PATTERN.test(property) || property.startsWith("--")) {
      for (const color of colors) {
        found.push({ color, annotated: value.slice(color.end).trim() === "" });
      }
    }
    if (!MASK_PROPERTY_PATTERN.test(property)) {
      for (const url of urls) {
        for (const paint of readSvgPaints(url)) {
          for (const color of readValue(paint).colors) {
            found.push({ color, annotated: false });
          }
        }
      }
    }
    if (found.length === 0) {
      continue;
    }

    const line = lineAt(declaration.offset);
    for (const { color, annotated } of found) {
      uses.push({
        literal: color.literal,
        notation: color.notation,
        property,
        line,
        annotation: annotated ? declaration.annotation : null,
      });
    }
  }

  return uses;
}

/**
 * The stylesheet's declarations, in source order. Text before a `{` is a selector or at-rule
 * prelude and is dropped, as is anything outside a block, so only what a browser would read as
 * a declaration is read as one.
 */
function readDeclarations(css: string): Declaration[] {
  const declarations: Declaration[] = [];
  let depth = 0;
  let parens = 0;
  let text = "";
  let start = -1;
  let index = 0;

  const append = (chunk: string, at: number): void => {
    if (start === -1 && chunk.trim() !== "") {
      start = at;
    }
    text += chunk;
  };

  while (index < css.length) {
    const char = css[index];

    if (char === "/" && css[index + 1] === "*") {
      const end = css.indexOf("*/", index + 2);
      index = end === -1 ? css.length : end + 2;
      continue;
    }

    if (char === '"' || char === "'") {
      const end = readString(css, index);
      append(css.slice(index, end), index);
      index = end;
      continue;
    }

    if (char === "(" || char === ")") {
      parens = char === "(" ? parens + 1 : Math.max(0, parens - 1);
      append(char, index);
      index += 1;
      continue;
    }

    if (parens === 0 && char === "{") {
      depth += 1;
      text = "";
      start = -1;
      index += 1;
      continue;
    }

    if (parens === 0 && (char === ";" || char === "}")) {
      if (depth > 0 && start !== -1) {
        const declaration = toDeclaration(css, text, start, char === ";" ? index + 1 : -1);
        if (declaration !== null) {
          declarations.push(declaration);
        }
      }
      if (char === "}") {
        depth = Math.max(0, depth - 1);
      }
      text = "";
      start = -1;
      index += 1;
      continue;
    }

    append(char, index);
    index += 1;
  }

  return declarations;
}

function toDeclaration(
  css: string,
  text: string,
  offset: number,
  afterSemicolon: number
): Declaration | null {
  const colon = text.indexOf(":");
  if (colon === -1) {
    return null;
  }
  const property = text.slice(0, colon).trim().toLowerCase();
  if (!PROPERTY_NAME_PATTERN.test(property)) {
    return null;
  }
  const value = text.slice(colon + 1).trim();
  if (value === "") {
    return null;
  }
  const annotation =
    afterSemicolon === -1
      ? null
      : (ANNOTATION_PATTERN.exec(css.slice(afterSemicolon))?.[1] ?? null);
  return { property, value, offset, annotation };
}

/** The colours and the `url(…)` contents of one declaration value. A colour inside a `url(…)`
 * is not a value of its own, so the scan steps over the whole function. */
function readValue(text: string): ValueContents {
  const colors: FoundColor[] = [];
  const urls: string[] = [];
  let index = 0;

  while (index < text.length) {
    const char = text[index];

    if (char === "#" || (char === "%" && text.startsWith("%23", index))) {
      const prefix = char === "#" ? 1 : 3;
      let end = index + prefix;
      while (end < text.length && HEX_DIGIT_PATTERN.test(text[end])) {
        end += 1;
      }
      const hex = NOTATION_BY_HEX_DIGITS[end - index - prefix];
      if (hex !== undefined && (end === text.length || !IDENT_CHAR_PATTERN.test(text[end]))) {
        colors.push({
          literal: text.slice(index, end),
          notation: char === "#" ? hex : "escaped-hex",
          end,
        });
      }
      index = end;
      continue;
    }

    if (IDENT_START_PATTERN.test(char)) {
      let end = index;
      while (end < text.length && IDENT_CHAR_PATTERN.test(text[end])) {
        end += 1;
      }
      const name = text.slice(index, end).toLowerCase();

      if (text[end] === "(") {
        const close = readBalanced(text, end);
        const notation = NOTATION_BY_FUNCTION[name];
        if (notation !== undefined) {
          colors.push({ literal: text.slice(index, close), notation, end: close });
          index = close;
        } else if (name === "url") {
          urls.push(text.slice(end + 1, Math.max(end + 1, close - 1)));
          index = close;
        } else {
          // Any other function (`var()`, `color-mix()`, `calc()`): its own name is no colour,
          // but a colour can sit among its arguments, so the scan walks into them.
          index = end + 1;
        }
        continue;
      }

      if (NAMED_COLORS.has(name)) {
        colors.push({ literal: text.slice(index, end), notation: "named", end });
      }
      index = end;
      continue;
    }

    index += 1;
  }

  return { colors, urls };
}

/** The value of every SVG `fill`/`stroke` attribute in a data URI. */
function readSvgPaints(url: string): string[] {
  const content = url.trim().replace(/^["']|["']$/g, "");
  if (!content.startsWith("data:")) {
    return [];
  }
  return [...content.matchAll(SVG_PAINT_PATTERN)].map((match) => match[2]);
}

/** Offset just past the string starting at `start`. */
function readString(text: string, start: number): number {
  const quote = text[start];
  let index = start + 1;
  while (index < text.length) {
    if (text[index] === "\\") {
      index += 2;
      continue;
    }
    if (text[index] === quote) {
      return index + 1;
    }
    index += 1;
  }
  return text.length;
}

/** Offset just past the `)` matching the `(` at `open`. */
function readBalanced(text: string, open: number): number {
  let depth = 0;
  let index = open;
  while (index < text.length) {
    const char = text[index];
    if (char === '"' || char === "'") {
      index = readString(text, index);
      continue;
    }
    if (char === "(") {
      depth += 1;
    } else if (char === ")") {
      depth -= 1;
      if (depth === 0) {
        return index + 1;
      }
    }
    index += 1;
  }
  return text.length;
}

/** Counts lines for offsets read in ascending order, which is how `findColors` reads them. */
function lineCounter(text: string): (offset: number) => number {
  let cursor = 0;
  let line = 1;
  return (offset) => {
    while (cursor < offset && cursor < text.length) {
      if (text[cursor] === "\n") {
        line += 1;
      }
      cursor += 1;
    }
    return line;
  };
}
