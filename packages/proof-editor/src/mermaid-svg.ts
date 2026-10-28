const SVG_NAMESPACE = "http://www.w3.org/2000/svg";

/** The elements beautiful-mermaid emits, measured over one diagram of each type it draws
 *  (flowchart, state, sequence, class, ER, xychart), plus the text and description elements an
 *  SVG may carry. Anything else is removed with its subtree. XML names are case-sensitive, so an
 *  element matches only by its exact local name: `<RECT>` is no `<rect>`. */
const ALLOWED_ELEMENTS: ReadonlySet<string> = new Set([
  "svg",
  "defs",
  "g",
  "marker",
  "path",
  "line",
  "polyline",
  "polygon",
  "rect",
  "circle",
  "ellipse",
  "text",
  "tspan",
  "title",
  "desc",
  "style",
]);

/**
 * Turns the markup `renderMermaidMarkup` (`@legion/proof-editor/mermaid-render`) returns into an
 * `<svg>` of `document`, sanitized by `sanitizeMermaidSvg` rather than by upstream's private
 * `sanitizeSvg`, with the editor's accessible name. Markup that is not an SVG document throws
 * `Error("Invalid SVG output")`. This module imports nothing from upstream, so a page imports it
 * without beautiful-mermaid or ELK; the render runs in a Web Worker.
 */
export function mermaidSvgElement(markup: string, document: Document): SVGSVGElement {
  const svg = sanitizeMermaidSvg(markup, document);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "Rendered Mermaid diagram");
  return svg;
}

/**
 * Parses SVG markup and returns its `<svg>` imported into `document`; markup that is not an SVG
 * document — one that does not parse, or whose root is not the SVG namespace's `svg` by its exact,
 * case-sensitive name — throws `Error("Invalid SVG output")`. Stricter than upstream's private
 * `sanitizeSvg`, which blocks five tags, keeps every `href` but a `javascript:` one and keeps the
 * Google Fonts `@import`: this SVG comes from a comment, which any person or agent writes, and goes
 * into pages that hold no document. So only the elements beautiful-mermaid emits survive, matched
 * by exact local name; every `on*` and `href` attribute goes, matched case-insensitively, which is
 * stricter than XML asks; every attribute but a `data-*` one whose value holds a CSS escape (`\`)
 * or a function that fetches (`url(…)` other than a local `url(#id)`, `image-set(…)`, `image(…)`,
 * `cross-fade(…)`, `src(…)`, `element(…)`) goes, which covers an author's `classDef`/`style` paint
 * such as `fill:url(https://…)` or its escaped spelling `fill:\75rl(https://…)`; and each
 * `<style>` loses its `@import` rules. The parser and every node come from `document` and its
 * window, never from the global scope.
 */
export function sanitizeMermaidSvg(markup: string, document: Document): SVGSVGElement {
  const view = document.defaultView;
  if (view === null) throw new Error("sanitizeMermaidSvg needs a document that has a window");
  const parsed = new view.DOMParser().parseFromString(markup, "image/svg+xml");
  const svg: Element = parsed.documentElement;
  if (
    parsed.getElementsByTagName("parsererror").length > 0 ||
    svg.namespaceURI !== SVG_NAMESPACE ||
    svg.localName !== "svg"
  ) {
    throw new Error("Invalid SVG output");
  }
  sanitizeElement(svg);
  return document.adoptNode(svg) as SVGSVGElement;
}

/** A `url(#id)` reference to an element of the same SVG, unquoted or quoted. */
const LOCAL_URL = /url\s*\(\s*(["']?)#[^"'()\s]*\1\s*\)/gi;
/** A CSS function that loads a resource. A value holding a backslash is refused outright, since
 *  CSS decodes escapes in a function's name (`\75rl(` is `url(`) and no beautiful-mermaid output
 *  needs one outside a `data-*` attribute. */
const FETCHING_FUNCTION = /\\|(?:url|image-set|image|cross-fade|src|element)\s*\(/i;

/** An `@import` rule: its `url(…)` (unquoted, or quoted and possibly holding `;`) or quoted
 *  string target, any media query after it, and its terminating `;`. No author text reaches a
 *  `<style>` — a `classDef` or `style` line lands in presentation attributes, which the `url(`
 *  check filters — so a style's text is beautiful-mermaid's own sheet: fixed rules after an
 *  `@import` of Google Fonts whose quoted URL holds `;`. */
const IMPORT_RULE =
  /@import\s*(?:url\(\s*(?:"[^"]*"|'[^']*'|[^"')]*)\s*\)|"[^"]*"|'[^']*')[^;]*(?:;|$)/gi;

function sanitizeElement(element: Element): void {
  for (const attribute of Array.from(element.attributes)) {
    const name = attribute.name.toLowerCase();
    const localName = attribute.localName.toLowerCase();
    const fetches =
      !localName.startsWith("data-") &&
      FETCHING_FUNCTION.test(attribute.value.replace(LOCAL_URL, ""));
    if (name.startsWith("on") || localName.startsWith("on") || localName === "href" || fetches) {
      element.removeAttributeNode(attribute);
    }
  }
  if (element.localName === "style") {
    element.textContent = (element.textContent ?? "").replace(IMPORT_RULE, "");
    return;
  }
  for (const child of Array.from(element.children)) {
    const allowed = child.namespaceURI === SVG_NAMESPACE && ALLOWED_ELEMENTS.has(child.localName);
    if (allowed) sanitizeElement(child);
    else child.remove();
  }
}
