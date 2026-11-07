/**
 * The DOM upstream `mermaid` draws against inside Dispatch's diagram Web Worker.
 *
 * `installDomShim()` must run before `import("mermaid")`: mermaid and d3 read `document` and
 * `window` while their modules evaluate, so the globals have to be there before the first of
 * those modules runs. The call is idempotent -- the first builds the tree and defines the
 * globals, every later one hands back the same handle.
 *
 * linkedom supplies the tree (parsing, elements, attributes, selectors, serialization); it has no
 * layout, so everything mermaid asks about geometry -- `getBBox`, `getComputedTextLength`,
 * `getBoundingClientRect`, `getTotalLength`, `getPointAtLength` -- is answered here, with text
 * widths and heights from an `OffscreenCanvas`'s `measureText`.
 *
 * That geometry was measured in four engines -- Chromium 156, Firefox 157, WebKit 27.2 and the
 * iPhone's Chromium -- against what the same source draws on each engine's own main thread: 14
 * sources drew in all four, each within 1.2% of that engine's main-thread drawing. Every rule
 * below is one some drawing came out wrong without, and the comment on each says which.
 */
import { parseHTML } from "linkedom";

// ---------------------------------------------------------------------------------------------
// The nodes this module reads
// ---------------------------------------------------------------------------------------------

/** A node as the geometry below reads it. linkedom types its tree as the browser's, which
 *  neither carries the members this module defines on it nor tells an element from the text node
 *  beside it, so every node that crosses into this module is read through these shapes. */
interface ShimNode {
  readonly nodeType: number;
  /** A text node's characters. */
  readonly data?: string | null;
}

/** An element, with the members the cascade and the geometry read off one. The optional members
 *  are the ones a node handed to the shim from outside may not carry: mermaid, d3 and cytoscape
 *  call `getComputedStyle` and the box readers with whatever they hold. */
interface ShimElement extends ShimNode {
  readonly localName?: string | null;
  readonly childNodes: ArrayLike<ShimNode>;
  readonly children?: ArrayLike<ShimElement>;
  readonly parentElement: ShimElement | null;
  readonly ownerDocument: ShimDocument | null;
  readonly textContent: string | null;
  matches(selector: string): boolean;
  getAttribute?(name: string): string | null;
  hasAttribute?(name: string): boolean;
  querySelector?(selector: string): ShimElement | null;
  contains?(other: ShimNode): boolean;
}

/** The document the cascade reads its `<style>` elements from. */
interface ShimDocument {
  querySelectorAll(selector: string): ArrayLike<ShimElement>;
}

/** Whether a node is an element, which is what gives it the members above. */
function isElement(node: ShimNode): node is ShimElement {
  return node.nodeType === 1;
}

// ---------------------------------------------------------------------------------------------
// Text metrics, from the worker's `OffscreenCanvas`
// ---------------------------------------------------------------------------------------------

let metricsContext: OffscreenCanvasRenderingContext2D | null = null;

/** The one 2-D context every measurement goes through. All four engines were measured to have a
 *  working `OffscreenCanvas` 2-D context in a worker, so there is no second source of widths and
 *  no test on the way to one. */
function measuringContext(): OffscreenCanvasRenderingContext2D {
  if (!metricsContext) {
    const canvas = new OffscreenCanvas(8, 8);
    const got = canvas.getContext("2d");
    if (!got) throw new Error("OffscreenCanvas 2d context unavailable in this worker");
    metricsContext = got;
  }
  return metricsContext;
}

interface TextMetricsLite {
  width: number;
  ascent: number;
  descent: number;
}

/** One measurement per font and string: a diagram measures the same label many times over. This
 *  worker lives as long as the reader's page and draws every diagram on it, so the cache is
 *  bounded: past `METRICS_CACHE_LIMIT` entries the whole of it is dropped rather than an entry
 *  at a time, because a miss costs one `measureText` and nothing else, where an eviction order
 *  would cost a second structure on every hit. */
const metricsCache = new Map<string, TextMetricsLite>();
const METRICS_CACHE_LIMIT = 4000;

function measureText(text: string, font: string): TextMetricsLite {
  const key = `${font}\u0000${text}`;
  const hit = metricsCache.get(key);
  if (hit) return hit;
  const context = measuringContext();
  context.font = font;
  const measured = context.measureText(text);
  const ascent = measured.fontBoundingBoxAscent || measured.actualBoundingBoxAscent || 0;
  const descent = measured.fontBoundingBoxDescent || measured.actualBoundingBoxDescent || 0;
  const out: TextMetricsLite = { width: measured.width, ascent, descent };
  if (metricsCache.size >= METRICS_CACHE_LIMIT) metricsCache.clear();
  metricsCache.set(key, out);
  return out;
}

/** One x-height per font: `boxOfRuns` reads one for every run it baselines, and `lengthOf` one
 *  for every `ex` it resolves. Keyed by the font shorthand, as `metricsCache` is, and bounded by
 *  the fonts one drawing uses rather than by its text. */
const xHeightCache = new Map<string, number>();

/** x-height of the current font, for `dominant-baseline: middle`. */
function xHeight(font: string): number {
  const hit = xHeightCache.get(font);
  if (hit !== undefined) return hit;
  const context = measuringContext();
  context.font = font;
  const measured = context.measureText("x").actualBoundingBoxAscent || 0;
  xHeightCache.set(font, measured);
  return measured;
}

// ---------------------------------------------------------------------------------------------
// A minimal CSS cascade: `style` attribute > stylesheet rule > presentation attribute
// ---------------------------------------------------------------------------------------------

interface CssRule {
  selector: string;
  decls: Map<string, string>;
}

const sheetCache = new WeakMap<ShimElement, CssRule[]>();

function parseDeclarations(block: string): Map<string, string> {
  const decls = new Map<string, string>();
  for (const part of block.split(";")) {
    const i = part.indexOf(":");
    if (i < 0) continue;
    const prop = part.slice(0, i).trim().toLowerCase();
    const value = part.slice(i + 1).trim();
    if (prop) decls.set(prop, value.replace(/\s*!important$/i, ""));
  }
  return decls;
}

/** The flattened rule list of one document, kept for the duration of one of the shim's public
 *  calls (`inOneCall`). `documentRules` is read once per property, per element, per step up the
 *  tree, and linkedom's `querySelectorAll` walks the whole document for each of those, which was
 *  most of what a drawing's geometry cost. The document cannot change inside one synchronous
 *  call, so one walk answers them all. */
let callDepth = 0;
let callDocument: ShimDocument | null = null;
let callRules: CssRule[] | null = null;

/** Runs one of the shim's public calls -- a box, a text length, a computed style -- holding the
 *  flattened rule list for it. Re-entrant: a call the shim makes inside another (`getClientRects`
 *  through `getBoundingClientRect`) keeps the outermost frame's list, and only the outermost
 *  frame drops it. */
function inOneCall<Answer>(answer: () => Answer): Answer {
  callDepth += 1;
  try {
    return answer();
  } finally {
    callDepth -= 1;
    if (callDepth === 0) {
      callDocument = null;
      callRules = null;
    }
  }
}

/** Flat rule list of every `<style>` in the element's document, in document order. */
function documentRules(el: ShimElement): CssRule[] {
  const doc = el.ownerDocument;
  if (!doc) return [];
  if (callRules !== null && callDocument === doc) return callRules;
  const styles = Array.from(doc.querySelectorAll("style"));
  const rules: CssRule[] = [];
  for (const style of styles) {
    let parsed = sheetCache.get(style);
    if (!parsed) {
      parsed = [];
      // Strip comments and at-rule blocks we do not need, then split `sel { decls }`.
      const text = String(style.textContent ?? "").replace(/\/\*[\s\S]*?\*\//g, "");
      for (const match of text.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
        const selectors = match[1].trim();
        if (!selectors || selectors.startsWith("@")) continue;
        const decls = parseDeclarations(match[2]);
        for (const selector of selectors.split(",")) {
          parsed.push({ selector: selector.trim(), decls });
        }
      }
      sheetCache.set(style, parsed);
    }
    rules.push(...parsed);
  }
  if (callDepth > 0) {
    callDocument = doc;
    callRules = rules;
  }
  return rules;
}

function safeMatches(el: ShimElement, selector: string): boolean {
  try {
    return el.matches(selector);
  } catch {
    return false;
  }
}

/** The value a property is *declared* with on this element, by the cascade above. */
function declaredProperty(el: ShimElement, prop: string): string | undefined {
  const inline = el.getAttribute?.("style");
  if (inline) {
    const v = parseDeclarations(inline).get(prop);
    if (v) return v;
  }
  // The last rule that both carries the property and matches wins, so the list is read from its
  // end: the first match there is that rule, and the `safeMatches` of every rule before it is
  // work whose answer the cascade would have thrown away.
  const rules = documentRules(el);
  let fromSheet: string | undefined;
  for (let at = rules.length - 1; at >= 0; at -= 1) {
    const rule = rules[at];
    if (rule.decls.has(prop) && safeMatches(el, rule.selector)) {
      fromSheet = rule.decls.get(prop);
      break;
    }
  }
  if (fromSheet) return fromSheet;
  const attr = el.getAttribute?.(prop);
  return attr ?? undefined;
}

/** A custom property's value, looked up on the element then on any `:root`/`svg` rule. */
function customProperty(el: ShimElement, name: string): string | undefined {
  for (let n: ShimElement | null = el; n; n = n.parentElement) {
    const v = declaredProperty(n, name);
    if (v) return v;
  }
  for (const rule of documentRules(el)) {
    const v = rule.decls.get(name);
    if (v && (rule.selector === ":root" || rule.selector === "svg" || rule.selector === "html")) {
      return v;
    }
  }
  return undefined;
}

/** Resolves `var(--x, fallback)` occurrences in a declaration value. */
function resolveVars(el: ShimElement, value: string): string {
  let out = value;
  for (let i = 0; i < 4 && out.includes("var("); i += 1) {
    out = out.replace(
      /var\(\s*(--[\w-]+)\s*(?:,\s*([^()]*))?\)/g,
      (_all: string, name: string, fallback: string | undefined) =>
        customProperty(el, name) ?? (fallback ?? "").trim()
    );
  }
  return out;
}

/** An inherited property, walked up the tree. */
function resolvedProperty(el: ShimElement, prop: string): string | undefined {
  for (let n: ShimElement | null = el; n && n.nodeType === 1; n = n.parentElement) {
    const v = declaredProperty(n, prop);
    if (v && v !== "inherit") return resolveVars(n, v);
  }
  return undefined;
}

const ABSOLUTE_UNITS: Record<string, number> = {
  px: 1,
  pt: 4 / 3,
  pc: 16,
  in: 96,
  cm: 96 / 2.54,
  mm: 96 / 25.4,
};
const ROOT_FONT_SIZE = 16;
/** Properties whose initial computed value is a zero length. */
const ZERO_LENGTH = /^(padding|margin|border)(-|$)|-width$/;

const fontSizeCache = new WeakMap<ShimElement, number>();

/** The computed `font-size` in px. `em`, `ex` and `%` are relative to the parent's computed size,
 *  which is why this walks the tree rather than taking the nearest declaration at face value:
 *  timeline's title carries `font-size="4ex"`, and reading that as 4px put the title's box 27px
 *  below where Chrome puts it. */
function fontSizeOf(el: ShimElement | null): number {
  if (!el || el.nodeType !== 1) return ROOT_FONT_SIZE;
  const cached = fontSizeCache.get(el);
  if (cached !== undefined) return cached;
  const parent = el.parentElement;
  const parentSize = parent ? fontSizeOf(parent) : ROOT_FONT_SIZE;
  const declared = declaredProperty(el, "font-size");
  let size = parentSize;
  const raw = declared ? resolveVars(el, declared).trim() : "";
  const m = /^([\d.]+)\s*([a-z%]*)$/i.exec(raw);
  if (m) {
    const n = Number(m[1]);
    const unit = m[2].toLowerCase();
    if (!unit || unit === "px") size = n;
    else if (unit in ABSOLUTE_UNITS) size = n * ABSOLUTE_UNITS[unit];
    else if (unit === "em") size = n * parentSize;
    else if (unit === "rem") size = n * ROOT_FONT_SIZE;
    else if (unit === "%") size = (n / 100) * parentSize;
    else if (unit === "ex") {
      const family = resolvedProperty(el, "font-family") || "sans-serif";
      size = n * xHeight(`normal normal ${parentSize}px ${family}`);
    } else size = n;
  }
  fontSizeCache.set(el, size);
  return size;
}

/** A geometric attribute in px; `em`/`ex` resolve against the element's own font. */
function lengthOf(el: ShimElement, name: string, fallback = 0): number {
  const raw = el.getAttribute?.(name);
  if (raw === null || raw === undefined || raw === "") return fallback;
  const m = /^\s*([-+]?[\d.]+(?:e[-+]?\d+)?)\s*([a-z%]*)\s*$/i.exec(String(raw));
  if (!m) return fallback;
  const n = Number(m[1]);
  const unit = m[2].toLowerCase();
  if (!unit || unit === "px") return n;
  if (unit in ABSOLUTE_UNITS) return n * ABSOLUTE_UNITS[unit];
  if (unit === "em") return n * fontSizeOf(el);
  if (unit === "rem") return n * ROOT_FONT_SIZE;
  if (unit === "ex") return n * xHeight(fontOf(el));
  return n;
}

/** A canvas `font` shorthand for the element's resolved font. One per element: `boxOfRuns` reads
 *  it twice for every run of a `<text>` and `lengthOf` once for every `ex`, and each read
 *  resolves three properties up the tree, `font-family` and `font-size` among them -- both of
 *  which the page configures as custom properties that resolve to nothing in a worker, so each
 *  one also walks the cascade for a `var()`. Kept as long as `fontSizeCache`, the size half of
 *  the same answer. */
const fontCache = new WeakMap<ShimElement, string>();

function fontOf(el: ShimElement): string {
  const cached = fontCache.get(el);
  if (cached !== undefined) return cached;
  const size = fontSizeOf(el);
  const family = resolvedProperty(el, "font-family") || "sans-serif";
  const weight = resolvedProperty(el, "font-weight") || "normal";
  const style = resolvedProperty(el, "font-style") || "normal";
  const font = `${style} ${weight} ${size}px ${family}`;
  fontCache.set(el, font);
  return font;
}

// ---------------------------------------------------------------------------------------------
// Boxes and transforms
// ---------------------------------------------------------------------------------------------

interface Box {
  x: number;
  y: number;
  width: number;
  height: number;
}
type Matrix = [number, number, number, number, number, number]; // a b c d e f

const IDENTITY: Matrix = [1, 0, 0, 1, 0, 0];

function multiply(m: Matrix, n: Matrix): Matrix {
  return [
    m[0] * n[0] + m[2] * n[1],
    m[1] * n[0] + m[3] * n[1],
    m[0] * n[2] + m[2] * n[3],
    m[1] * n[2] + m[3] * n[3],
    m[0] * n[4] + m[2] * n[5] + m[4],
    m[1] * n[4] + m[3] * n[5] + m[5],
  ];
}

function parseTransform(value: string | null | undefined): Matrix {
  if (!value) return IDENTITY;
  let m = IDENTITY;
  for (const hit of value.matchAll(/(matrix|translate|scale|rotate|skewX|skewY)\s*\(([^)]*)\)/g)) {
    const args = hit[2]
      .split(/[\s,]+/)
      .filter(Boolean)
      .map(Number);
    switch (hit[1]) {
      case "matrix":
        m = multiply(m, args.slice(0, 6) as Matrix);
        break;
      case "translate":
        m = multiply(m, [1, 0, 0, 1, args[0] || 0, args[1] || 0]);
        break;
      case "scale": {
        const sx = args[0] ?? 1;
        m = multiply(m, [sx, 0, 0, args[1] ?? sx, 0, 0]);
        break;
      }
      case "rotate": {
        const a = ((args[0] || 0) * Math.PI) / 180;
        const cos = Math.cos(a);
        const sin = Math.sin(a);
        const r: Matrix = [cos, sin, -sin, cos, 0, 0];
        if (args.length >= 3) {
          m = multiply(multiply(multiply(m, [1, 0, 0, 1, args[1], args[2]]), r), [
            1,
            0,
            0,
            1,
            -args[1],
            -args[2],
          ]);
        } else {
          m = multiply(m, r);
        }
        break;
      }
      case "skewX":
        m = multiply(m, [1, 0, Math.tan(((args[0] || 0) * Math.PI) / 180), 1, 0, 0]);
        break;
      case "skewY":
        m = multiply(m, [1, Math.tan(((args[0] || 0) * Math.PI) / 180), 0, 1, 0, 0]);
        break;
    }
  }
  return m;
}

function applyMatrix(m: Matrix, box: Box | null): Box | null {
  if (!box) return null;
  if (m === IDENTITY) return box;
  const xs: number[] = [];
  const ys: number[] = [];
  for (const [px, py] of [
    [box.x, box.y],
    [box.x + box.width, box.y],
    [box.x, box.y + box.height],
    [box.x + box.width, box.y + box.height],
  ]) {
    xs.push(m[0] * px + m[2] * py + m[4]);
    ys.push(m[1] * px + m[3] * py + m[5]);
  }
  const x = Math.min(...xs);
  const y = Math.min(...ys);
  return { x, y, width: Math.max(...xs) - x, height: Math.max(...ys) - y };
}

function unite(a: Box | null, b: Box | null): Box | null {
  if (!a) return b;
  if (!b) return a;
  const x = Math.min(a.x, b.x);
  const y = Math.min(a.y, b.y);
  const right = Math.max(a.x + a.width, b.x + b.width);
  const bottom = Math.max(a.y + a.height, b.y + b.height);
  return { x, y, width: right - x, height: bottom - y };
}

function num(el: ShimElement, name: string, fallback = 0): number {
  const raw = el.getAttribute?.(name);
  if (raw === null || raw === undefined || raw === "") return fallback;
  const n = Number.parseFloat(raw);
  return Number.isFinite(n) ? n : fallback;
}

function boxOfPoints(points: Array<[number, number]>): Box | null {
  if (points.length === 0) return null;
  let minX = Number.POSITIVE_INFINITY;
  let minY = Number.POSITIVE_INFINITY;
  let maxX = Number.NEGATIVE_INFINITY;
  let maxY = Number.NEGATIVE_INFINITY;
  for (const [x, y] of points) {
    if (x < minX) minX = x;
    if (y < minY) minY = y;
    if (x > maxX) maxX = x;
    if (y > maxY) maxY = y;
  }
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY };
}

/** Flattens a path's `d` to points; curves are sampled, which is tight enough for a layout box. */
function pathPoints(d: string): Array<[number, number]> {
  const out: Array<[number, number]> = [];
  const tokens = d.match(/[astvzqmhlcASTVZQMHLC]|[-+]?(?:\d*\.\d+|\d+)(?:[eE][-+]?\d+)?/g);
  if (!tokens) return out;
  let i = 0;
  let cx = 0;
  let cy = 0;
  let startX = 0;
  let startY = 0;
  let cmd = "";
  const n = () => Number(tokens[i++]);
  const sample = (p: Array<[number, number]>) => {
    for (let t = 0; t <= 1.0001; t += 1 / 12) {
      const u = 1 - t;
      if (p.length === 4) {
        out.push([
          u * u * u * p[0][0] +
            3 * u * u * t * p[1][0] +
            3 * u * t * t * p[2][0] +
            t * t * t * p[3][0],
          u * u * u * p[0][1] +
            3 * u * u * t * p[1][1] +
            3 * u * t * t * p[2][1] +
            t * t * t * p[3][1],
        ]);
      } else {
        out.push([
          u * u * p[0][0] + 2 * u * t * p[1][0] + t * t * p[2][0],
          u * u * p[0][1] + 2 * u * t * p[1][1] + t * t * p[2][1],
        ]);
      }
    }
  };
  while (i < tokens.length) {
    if (/[a-zA-Z]/.test(tokens[i])) cmd = tokens[i++];
    const rel = cmd === cmd.toLowerCase();
    const base = rel ? [cx, cy] : [0, 0];
    switch (cmd.toLowerCase()) {
      case "m": {
        cx = base[0] + n();
        cy = base[1] + n();
        startX = cx;
        startY = cy;
        out.push([cx, cy]);
        cmd = rel ? "l" : "L";
        break;
      }
      case "l": {
        cx = base[0] + n();
        cy = base[1] + n();
        out.push([cx, cy]);
        break;
      }
      case "h": {
        cx = base[0] + n();
        out.push([cx, cy]);
        break;
      }
      case "v": {
        cy = base[1] + n();
        out.push([cx, cy]);
        break;
      }
      case "c": {
        const p1: [number, number] = [base[0] + n(), base[1] + n()];
        const p2: [number, number] = [base[0] + n(), base[1] + n()];
        const p3: [number, number] = [base[0] + n(), base[1] + n()];
        sample([[cx, cy], p1, p2, p3]);
        cx = p3[0];
        cy = p3[1];
        break;
      }
      case "s":
      case "q": {
        const p1: [number, number] = [base[0] + n(), base[1] + n()];
        const p2: [number, number] = [base[0] + n(), base[1] + n()];
        sample([[cx, cy], p1, p2]);
        cx = p2[0];
        cy = p2[1];
        break;
      }
      case "t": {
        const p: [number, number] = [base[0] + n(), base[1] + n()];
        out.push([cx, cy], p);
        cx = p[0];
        cy = p[1];
        break;
      }
      case "a": {
        n();
        n();
        n();
        n();
        n();
        const ex = base[0] + n();
        const ey = base[1] + n();
        out.push([cx, cy], [ex, ey]);
        cx = ex;
        cy = ey;
        break;
      }
      case "z": {
        cx = startX;
        cy = startY;
        out.push([cx, cy]);
        break;
      }
      default:
        i += 1;
    }
  }
  return out;
}

/** Collapses whitespace the way SVG's default `xml:space` does. */
function collapse(text: string): string {
  return text
    .replace(/[\n\r]/g, "")
    .replace(/\t/g, " ")
    .replace(/ +/g, " ");
}

interface Run {
  text: string;
  el: ShimElement;
  x: number;
  baseline: number;
  /** Which positioned chunk the run belongs to. `text-anchor` applies to a whole chunk -- every
   *  character from one absolute `x` to the next -- not to each run, so the chunk's total width
   *  is what gets centred. */
  chunk: number;
}

/** Lays a `<text>` out into runs: one per text node, positioned by x/y/dx/dy on it or an
 *  ancestor. A tspan's own dx/dy *replaces* the ancestor's rather than adding to it, which is
 *  what timeline relies on (`<text dy="1em"><tspan dy="1em" …>`). */
function textRuns(text: ShimElement): Run[] {
  const runs: Run[] = [];
  let penX = lengthOf(text, "x");
  let penY = lengthOf(text, "y");
  let chunk = 0;

  const walk = (node: ShimElement, dx: number, dy: number) => {
    let pendingDx = dx;
    let pendingDy = dy;
    for (const child of Array.from(node.childNodes)) {
      if (child.nodeType === 3) {
        const raw = collapse(String(child.data ?? ""));
        if (raw.trim() === "") continue;
        penX += pendingDx;
        penY += pendingDy;
        pendingDx = 0;
        pendingDy = 0;
        runs.push({ text: raw, el: node, x: penX, baseline: penY, chunk });
        penX += measureText(raw, fontOf(node)).width;
      } else if (isElement(child)) {
        const tag = String(child.localName ?? "").toLowerCase();
        if (tag !== "tspan" && tag !== "textpath" && tag !== "a") continue;
        if (child.hasAttribute?.("x")) {
          penX = lengthOf(child, "x");
          chunk += 1;
        }
        if (child.hasAttribute?.("y")) penY = lengthOf(child, "y");
        walk(
          child,
          child.hasAttribute?.("dx") ? lengthOf(child, "dx") : pendingDx,
          child.hasAttribute?.("dy") ? lengthOf(child, "dy") : pendingDy
        );
        pendingDx = 0;
        pendingDy = 0;
      }
    }
  };
  walk(text, lengthOf(text, "dx"), lengthOf(text, "dy"));
  if (runs.length === 0) {
    // A `<text>` with no characters counts as nothing unless it holds a tspan, which counts as a
    // point.
    if (text.querySelector?.("tspan")) {
      runs.push({ text: "", el: text, x: 0, baseline: 0, chunk: 0 });
    }
    return runs;
  }
  // The whole `<text>`'s leading and trailing space is trimmed.
  runs[0].text = runs[0].text.replace(/^ /, "");
  runs[runs.length - 1].text = runs[runs.length - 1].text.replace(/ $/, "");
  return runs;
}

/** The box of `keep`'s runs, anchored and baselined by `styleFrom`'s resolved properties. `all`
 *  is every run of the `<text>`, which the anchor needs: it shifts a whole positioned chunk by
 *  that chunk's total width, not each run by its own. */
function boxOfRuns(all: Run[], styleFrom: ShimElement, keep: Run[] = all): Box | null {
  if (keep.length === 0) return null;
  const anchor = (resolvedProperty(styleFrom, "text-anchor") || "start").trim();
  const baselineProp = (resolvedProperty(styleFrom, "dominant-baseline") || "auto").trim();
  // Only the chunks a kept run sits in are totalled: a caller that keeps one `<tspan>`
  // (`tspanBox`) reads no other chunk's width, and measuring them all made a `<text>` of n
  // tspans cost n measurements of each of its runs.
  const keptChunks = keep === all ? null : new Set(keep.map((run) => run.chunk));
  const chunkWidth = new Map<number, number>();
  for (const run of all) {
    if (keptChunks !== null && !keptChunks.has(run.chunk)) continue;
    const width = run.text === "" ? 0 : measureText(run.text, fontOf(run.el)).width;
    chunkWidth.set(run.chunk, (chunkWidth.get(run.chunk) ?? 0) + width);
  }
  let box: Box | null = null;
  for (const run of keep) {
    const font = fontOf(run.el);
    const m = run.text === "" ? { width: 0, ascent: 0, descent: 0 } : measureText(run.text, font);
    const total = chunkWidth.get(run.chunk) ?? m.width;
    let x = run.x;
    if (anchor === "middle") x -= total / 2;
    else if (anchor === "end") x -= total;
    let baseline = run.baseline;
    if (baselineProp === "middle") baseline += xHeight(font) / 2;
    else if (baselineProp === "central") baseline += (m.ascent - m.descent) / 2;
    else if (baselineProp === "hanging" || baselineProp === "text-before-edge") {
      baseline += m.ascent;
    } else if (baselineProp === "text-after-edge" || baselineProp === "ideographic") {
      baseline -= m.descent;
    }
    box = unite(box, { x, y: baseline - m.ascent, width: m.width, height: m.ascent + m.descent });
  }
  return box;
}

/** A `<tspan>`'s own characters. xychart measures the tspan, not the `<text>`, so without this
 *  every axis label measured 0 wide and the axis was drawn off the left edge of the viewBox. */
function tspanBox(tspan: ShimElement): Box | null {
  let text: ShimElement | null = tspan.parentElement;
  while (text && String(text.localName ?? "").toLowerCase() !== "text") text = text.parentElement;
  if (!text) return null;
  const all = textRuns(text);
  return boxOfRuns(
    all,
    tspan,
    all.filter((run) => run.el === tspan || tspan.contains?.(run.el))
  );
}

const CONTAINERS = new Set(["g", "svg", "a", "switch", "marker", "symbol", "defs", "clippath"]);

/** Elements the renderer never draws in place, so they add nothing to an ancestor's box. mermaid
 *  puts its arrowhead `<marker>`s directly under the root `<g>`, not in `<defs>`; counting them
 *  pulled every flowchart's box back to the origin. */
const NEVER_DRAWN = new Set([
  "defs",
  "style",
  "title",
  "desc",
  "metadata",
  "marker",
  "symbol",
  "clippath",
  "mask",
  "pattern",
  "lineargradient",
  "radialgradient",
  "filter",
  "script",
]);

/** The element's own geometry plus its children's, in the element's user space. */
function bboxOf(el: ShimElement): Box | null {
  const tag = String(el.localName ?? "").toLowerCase();
  switch (tag) {
    case "rect": {
      const w = num(el, "width");
      const h = num(el, "height");
      if (w <= 0 || h <= 0) return null;
      return { x: num(el, "x"), y: num(el, "y"), width: w, height: h };
    }
    case "circle": {
      const r = num(el, "r");
      if (r <= 0) return null;
      return { x: num(el, "cx") - r, y: num(el, "cy") - r, width: 2 * r, height: 2 * r };
    }
    case "ellipse": {
      const rx = num(el, "rx");
      const ry = num(el, "ry");
      if (rx <= 0 || ry <= 0) return null;
      return { x: num(el, "cx") - rx, y: num(el, "cy") - ry, width: 2 * rx, height: 2 * ry };
    }
    case "line":
      return boxOfPoints([
        [num(el, "x1"), num(el, "y1")],
        [num(el, "x2"), num(el, "y2")],
      ]);
    case "polygon":
    case "polyline": {
      const nums = (el.getAttribute?.("points") || "")
        .split(/[\s,]+/)
        .filter(Boolean)
        .map(Number);
      const pts: Array<[number, number]> = [];
      for (let i = 0; i + 1 < nums.length; i += 2) pts.push([nums[i], nums[i + 1]]);
      return boxOfPoints(pts);
    }
    case "path":
      return boxOfPoints(pointsOf(el));
    case "text":
      return boxOfRuns(textRuns(el), el);
    case "tspan":
      return tspanBox(el);
    case "image":
    case "foreignobject":
    case "use": {
      const w = num(el, "width");
      const h = num(el, "height");
      if (w <= 0 && h <= 0) return null;
      return { x: num(el, "x"), y: num(el, "y"), width: w, height: h };
    }
    default: {
      if (!CONTAINERS.has(tag)) return null;
      let box: Box | null = null;
      for (const child of Array.from(el.children ?? [])) {
        const childTag = String(child.localName ?? "").toLowerCase();
        if (NEVER_DRAWN.has(childTag)) continue;
        if (child.getAttribute?.("display") === "none") continue;
        const inner = bboxOf(child);
        box = unite(box, applyMatrix(parseTransform(child.getAttribute?.("transform")), inner));
      }
      return box;
    }
  }
}

/** The element's flattened `d`, kept until that element's `d` changes. mermaid's own edge
 *  sampler reads one path's `getTotalLength` and then 27 `getPointAtLength` of it, and each of
 *  those re-parsed and re-sampled the whole path. */
const pathCache = new WeakMap<ShimElement, { d: string; points: Array<[number, number]> }>();

function pointsOf(el: ShimElement): Array<[number, number]> {
  const d = el.getAttribute?.("d") || "";
  const held = pathCache.get(el);
  if (held !== undefined && held.d === d) return held.points;
  const points = pathPoints(d);
  pathCache.set(el, { d, points });
  return points;
}

function pathLength(el: ShimElement): number {
  const pts = pointsOf(el);
  let total = 0;
  for (let i = 1; i < pts.length; i += 1) {
    total += Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1]);
  }
  return total;
}

function pointAtLength(el: ShimElement, at: number): { x: number; y: number } {
  const pts = pointsOf(el);
  if (pts.length === 0) return { x: 0, y: 0 };
  let walked = 0;
  for (let i = 1; i < pts.length; i += 1) {
    const seg = Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1]);
    if (walked + seg >= at) {
      const t = seg === 0 ? 0 : (at - walked) / seg;
      return {
        x: pts[i - 1][0] + (pts[i][0] - pts[i - 1][0]) * t,
        y: pts[i - 1][1] + (pts[i][1] - pts[i - 1][1]) * t,
      };
    }
    walked += seg;
  }
  return { x: pts[pts.length - 1][0], y: pts[pts.length - 1][1] };
}

/** The matrix from the element's user space up to its root `<svg>`. */
function ctmOf(el: ShimElement): Matrix {
  let m = IDENTITY;
  for (let n: ShimElement | null = el; n && n.nodeType === 1; n = n.parentElement) {
    m = multiply(parseTransform(n.getAttribute?.("transform")), m);
  }
  return m;
}

// ---------------------------------------------------------------------------------------------
// A constructable-stylesheet stand-in (`createCssStyles` builds mermaid's theme CSS through it)
// ---------------------------------------------------------------------------------------------

/** A rule's declarations, as much of `CSSStyleDeclaration` as mermaid reads: c4's
 *  `elementFontStyles` inserts `.c4-shape.c4-person .label {}` and writes its font through
 *  `rule.style.setProperty`, then keeps the rules whose `rule.style.length` is above zero and
 *  serializes each one's `cssText`, so the write, the count and the serialization have to be the
 *  same declarations. A value is kept verbatim, `!important` and all, because `cssImportantStyles`
 *  writes one of those for every `classDef` a source declares. */
class ShimCSSStyleDeclaration {
  private readonly declarations = new Map<string, string>();
  get length(): number {
    return this.declarations.size;
  }
  get cssText(): string {
    return Array.from(this.declarations, ([prop, value]) => `${prop}: ${value};`).join(" ");
  }
  getPropertyValue(prop: string): string {
    return this.declarations.get(prop) ?? "";
  }
  setProperty(prop: string, value: string): void {
    this.declarations.set(prop, value);
  }
  removeProperty(prop: string): string {
    const had = this.getPropertyValue(prop);
    this.declarations.delete(prop);
    return had;
  }
  item(index: number): string {
    return Array.from(this.declarations.keys())[index] ?? "";
  }
  /** The declarations of a `sel { ... }` block, as the sheet's own parser reads one. A `;` inside
   *  a quoted value splits the block here, as it does in the cascade's `parseDeclarations`. */
  load(block: string): void {
    for (const part of block.split(";")) {
      const colon = part.indexOf(":");
      if (colon < 0) continue;
      const prop = part.slice(0, colon).trim();
      if (prop) this.declarations.set(prop, part.slice(colon + 1).trim());
    }
  }
}

/** One rule of the sheet. `cssText` is rebuilt from the declarations rather than kept as the text
 *  the rule was inserted with, so a rule c4 wrote through `style` serializes with what it wrote. */
class ShimCSSStyleRule {
  readonly selectorText: string;
  readonly style = new ShimCSSStyleDeclaration();
  constructor(text: string) {
    const open = text.indexOf("{");
    this.selectorText = (open < 0 ? text : text.slice(0, open)).trim();
    if (open >= 0) this.style.load(text.slice(open + 1, text.lastIndexOf("}")));
  }
  get cssText(): string {
    return `${this.selectorText} { ${this.style.cssText} }`;
  }
}

class ShimCSSStyleSheet {
  cssRules: ShimCSSStyleRule[] = [];
  insertRule(rule: string, index = this.cssRules.length): number {
    this.cssRules.splice(index, 0, new ShimCSSStyleRule(rule));
    return index;
  }
  deleteRule(index: number): void {
    this.cssRules.splice(index, 1);
  }
  replaceSync(text: string): void {
    this.cssRules = [];
    for (const match of text.matchAll(/([^{}]+\{[^{}]*\})/g)) {
      this.cssRules.push(new ShimCSSStyleRule(match[1].trim()));
    }
  }
  async replace(text: string): Promise<this> {
    this.replaceSync(text);
    return this;
  }
}

/** mermaid serializes the drawing it built through one of these. */
class ShimXMLSerializer {
  serializeToString(node: { outerHTML?: string | null }): string {
    return node.outerHTML ?? String(node);
  }
}

// ---------------------------------------------------------------------------------------------
// Installation
// ---------------------------------------------------------------------------------------------

/** The width a self-sizing diagram is laid out at in this worker, which has no window to read
 *  one from: the `screen` c4 wraps a row of boundaries at, and `gantt.useWidth` of
 *  `mermaid-upstream.ts`. One value, so a drawing is the same for every reader rather than
 *  sized by the monitor it was drawn on. */
export const LAYOUT_WIDTH = 1200;

/** The box `getBoundingClientRect` and `getClientRects` answer with. */
interface ShimClientRect {
  x: number;
  y: number;
  left: number;
  top: number;
  right: number;
  bottom: number;
  width: number;
  height: number;
  toJSON(): Box;
}

interface ShimMatrixLike {
  a: number;
  b: number;
  c: number;
  d: number;
  e: number;
  f: number;
}

interface ShimScreenCTM extends ShimMatrixLike {
  inverse(): ShimMatrixLike;
}

interface ShimSvgPoint {
  x: number;
  y: number;
  matrixTransform(matrix: ShimMatrixLike): { x: number; y: number };
}

/** The geometry the shim defines on linkedom's `SVGElement` prototype. */
interface ShimSvgPrototype {
  getBBox(this: ShimElement): Box;
  getComputedTextLength(this: ShimElement): number;
  getSubStringLength(this: ShimElement, from: number, count: number): number;
  getTotalLength(this: ShimElement): number;
  getPointAtLength(this: ShimElement, at: number): { x: number; y: number };
  getScreenCTM(this: ShimElement): ShimScreenCTM;
  createSVGPoint(this: ShimElement): ShimSvgPoint;
}

/** The boxes the shim defines on linkedom's `Element` prototype. */
interface ShimElementPrototype {
  getBoundingClientRect(this: ShimElement): ShimClientRect;
  getClientRects(this: ShimElement): ShimClientRect[];
}

/** What `getComputedStyle` answers with, as far as mermaid, d3 and cytoscape read it. */
interface ShimComputedStyle {
  getPropertyValue(prop: string): string;
  fontSize: string;
  fontFamily: string;
}

/** The `<canvas>` the document answers with: a linkedom element carrying an `OffscreenCanvas`'s
 *  own members. */
interface ShimCanvasElement {
  width: number;
  height: number;
  getContext(type: string, settings?: unknown): unknown;
  toDataURL(): string;
  toBlob(callback: (blob: Blob | null) => void): void;
}

/** linkedom's element constructors take the owner document and the local name, where the
 *  browser's `HTMLElement`, which linkedom's types name, takes neither. */
interface ShimCanvasConstructor {
  new (ownerDocument: unknown, localName: string): ShimCanvasElement;
}

/** The document member the shim replaces. */
interface ShimDocumentHost {
  createElement(name: string, options?: unknown): object;
}

/** The window members the shim writes. */
interface ShimWindowHost {
  getComputedStyle: (el: ShimElement) => ShimComputedStyle;
  requestAnimationFrame: (callback: (time: number) => void) => unknown;
  cancelAnimationFrame: (handle: number) => void;
  CSSStyleSheet: typeof ShimCSSStyleSheet;
  XMLSerializer: typeof ShimXMLSerializer;
  location: { href: string; protocol: string; search: string } | undefined;
}

/** The element's box in the root `<svg>`'s space. */
function clientRectOf(this: ShimElement): ShimClientRect {
  return inOneCall(() => {
    const box = applyMatrix(ctmOf(this), bboxOf(this)) ?? { x: 0, y: 0, width: 0, height: 0 };
    return {
      x: box.x,
      y: box.y,
      left: box.x,
      top: box.y,
      right: box.x + box.width,
      bottom: box.y + box.height,
      width: box.width,
      height: box.height,
      toJSON: () => box,
    };
  });
}

/** The globals the shim defined. A caller that runs another renderer in this worker -- one that
 *  wants no `document` -- brackets its call by saving, deleting and restoring exactly the names
 *  in `globalsDefined`, so that list is every global `installDomShim` defined. */
export interface ShimHandle {
  globalsDefined: readonly string[];
}

let handle: ShimHandle | null = null;

/** Installs the shim on the worker's global scope. Idempotent. */
export function installDomShim(): ShimHandle {
  if (handle) return handle;
  const dom = parseHTML(
    '<!doctype html><html><head><meta charset="utf-8"></head><body></body></html>'
  );
  const win = dom.window;
  const doc = dom.document;
  // linkedom types its tree, its window and its element classes as the browser's, which carry
  // none of what the shim defines below. Each cast names the shape this module actually uses.
  const svgPrototype = dom.SVGElement.prototype as unknown as ShimSvgPrototype;
  const elementPrototype = dom.Element.prototype as unknown as ShimElementPrototype;
  const shimWindow = win as unknown as ShimWindowHost;
  const shimDocument = doc as unknown as ShimDocumentHost;
  const canvasConstructor = dom.HTMLElement as unknown as ShimCanvasConstructor;
  // The classes hang off the window by name, which its type does not index.
  const domClasses = dom as unknown as Record<string, unknown>;
  const windowClasses = win as unknown as Record<string, unknown>;

  // Geometry, answered here. linkedom gives every SVG element the one `SVGElement` class, so one
  // prototype carries all of it.
  svgPrototype.getBBox = function getBBox(this: ShimElement) {
    return inOneCall(() => bboxOf(this) ?? { x: 0, y: 0, width: 0, height: 0 });
  };
  svgPrototype.getComputedTextLength = function getComputedTextLength(this: ShimElement) {
    return inOneCall(() =>
      textRuns(this).reduce((sum, run) => sum + measureText(run.text, fontOf(run.el)).width, 0)
    );
  };
  svgPrototype.getSubStringLength = function getSubStringLength(
    this: ShimElement,
    from: number,
    count: number
  ) {
    return inOneCall(() => {
      const all = textRuns(this)
        .map((run) => run.text)
        .join("");
      return measureText(all.slice(from, from + count), fontOf(this)).width;
    });
  };
  svgPrototype.getTotalLength = function getTotalLength(this: ShimElement) {
    return pathLength(this);
  };
  svgPrototype.getPointAtLength = function getPointAtLength(this: ShimElement, at: number) {
    return pointAtLength(this, at);
  };
  svgPrototype.getScreenCTM = function getScreenCTM(this: ShimElement) {
    const m = ctmOf(this);
    return {
      a: m[0],
      b: m[1],
      c: m[2],
      d: m[3],
      e: m[4],
      f: m[5],
      inverse() {
        const det = m[0] * m[3] - m[1] * m[2] || 1;
        return {
          a: m[3] / det,
          b: -m[1] / det,
          c: -m[2] / det,
          d: m[0] / det,
          e: (m[2] * m[5] - m[3] * m[4]) / det,
          f: (m[1] * m[4] - m[0] * m[5]) / det,
        };
      },
    };
  };
  svgPrototype.createSVGPoint = function createSVGPoint(this: ShimElement) {
    return { x: 0, y: 0, matrixTransform: (m: ShimMatrixLike) => ({ x: m.e, y: m.f }) };
  };
  elementPrototype.getBoundingClientRect = clientRectOf;
  elementPrototype.getClientRects = function getClientRects(this: ShimElement) {
    return [clientRectOf.call(this)];
  };
  // `clientWidth`/`offsetWidth` are read for html labels, which the measured renders left off,
  // so each answers with the element's own drawn width rather than a laid-out box.
  for (const prop of ["clientWidth", "clientHeight", "offsetWidth", "offsetHeight"]) {
    if (!(prop in elementPrototype)) {
      Object.defineProperty(elementPrototype, prop, {
        configurable: true,
        get(this: ShimElement) {
          return Math.round(clientRectOf.call(this).width);
        },
      });
    }
  }

  // linkedom's `<canvas>` needs the native `canvas` npm package, which a browser worker neither
  // has nor needs; without this, mindmap dies in cytoscape's renderer with `sg is not a
  // function`. An `OffscreenCanvas` answers `getContext` instead.
  const createElement = shimDocument.createElement.bind(shimDocument);
  shimDocument.createElement = (name: string, options?: unknown) => {
    if (String(name).toLowerCase() !== "canvas") return createElement(name, options);
    const el = new canvasConstructor(shimDocument, "canvas");
    const off = new OffscreenCanvas(300, 150);
    Object.defineProperties(el, {
      width: {
        configurable: true,
        get: () => off.width,
        set: (v: number) => {
          off.width = v;
        },
      },
      height: {
        configurable: true,
        get: () => off.height,
        set: (v: number) => {
          off.height = v;
        },
      },
    });
    // An `OffscreenCanvas` takes the same context ids a `<canvas>` does; the cast only picks the
    // overload, which the browser's types write one of per id.
    el.getContext = (type: string, settings?: unknown) =>
      off.getContext(type as "2d", settings as CanvasRenderingContext2DSettings);
    el.toDataURL = () => "data:image/png;base64,";
    el.toBlob = (cb: (b: Blob | null) => void) => cb(null);
    return el;
  };

  const globalsDefined: string[] = [];
  const define = (name: string, value: unknown) => {
    if (value === undefined) return;
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    globalsDefined.push(name);
  };

  const computedStyleOf = (el: ShimElement): ShimComputedStyle =>
    inOneCall(() => ({
      // A browser computes an undeclared `padding-left` to "0px", not "". cytoscape's `size()`
      // subtracts `parseFloat(getPropertyValue("padding-left"))` from `clientWidth`, so "" made
      // the container NaN wide and every mindmap died in the grid layout.
      getPropertyValue: (prop: string) =>
        inOneCall(() => resolvedProperty(el, prop) ?? (ZERO_LENGTH.test(prop) ? "0px" : "")),
      fontSize: `${fontSizeOf(el)}px`,
      fontFamily: resolvedProperty(el, "font-family") ?? "",
    }));
  const requestFrame = (callback: (time: number) => void) =>
    setTimeout(() => callback(Date.now()), 0);
  const cancelFrame = (id: number) => clearTimeout(id);

  // What mermaid, d3 and DOMPurify read off the global scope.
  define("window", win);
  define("document", doc);
  for (const name of [
    "Node",
    "Element",
    "HTMLElement",
    "SVGElement",
    "DocumentFragment",
    "Document",
    "DOMParser",
    "MutationObserver",
    "Event",
    "CustomEvent",
    "NamedNodeMap",
    "Attr",
    "Text",
    "Comment",
    "HTMLTemplateElement",
    "HTMLFormElement",
    "HTMLCanvasElement",
    "Image",
    "customElements",
    "NodeList",
    "HTMLCollection",
  ]) {
    define(name, domClasses[name] ?? windowClasses[name]);
  }
  define("CSSStyleSheet", ShimCSSStyleSheet);
  define("XMLSerializer", ShimXMLSerializer);
  define("getComputedStyle", computedStyleOf);
  define("requestAnimationFrame", requestFrame);
  define("cancelAnimationFrame", cancelFrame);
  // c4 wraps a row of boundaries at `screen.availWidth`, which no worker has: without this every
  // C4 diagram dies with `screen is not defined`. The width is `LAYOUT_WIDTH`, which is also
  // `gantt.useWidth` of `mermaid-upstream.ts`, so a C4 drawing wraps the same way for every
  // reader rather than by the monitor it was drawn on.
  define("screen", {
    width: LAYOUT_WIDTH,
    height: 800,
    availWidth: LAYOUT_WIDTH,
    availHeight: 800,
  });

  // d3 and mermaid read these off `window` too.
  shimWindow.getComputedStyle = computedStyleOf;
  shimWindow.requestAnimationFrame = requestFrame;
  shimWindow.cancelAnimationFrame = cancelFrame;
  shimWindow.CSSStyleSheet = ShimCSSStyleSheet;
  shimWindow.XMLSerializer = ShimXMLSerializer;
  if (!shimWindow.location) {
    shimWindow.location = { href: "about:blank", protocol: "about:", search: "" };
  }

  handle = { globalsDefined };
  return handle;
}
