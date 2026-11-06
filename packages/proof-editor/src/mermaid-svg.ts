/**
 * `@legion/proof-editor/mermaid`: the DOM half of a Mermaid diagram, for Dispatch's Markdown bodies
 * and for the editor's mermaid code blocks alike. It sanitizes the markup either renderer returns,
 * mounts the result in a shadow root so its styles stay inside it, and declares the contract by
 * which the editor asks its host to draw a diagram. It imports nothing from upstream, so a page
 * imports it without beautiful-mermaid or ELK; the render itself runs in the host's Web Worker.
 *
 * One thing to know before writing a unit row of the `"html"` parse: happy-dom 20.14.0's HTML
 * parser compares a `</style>` end tag with the element's upper-case `tagName`, which an SVG
 * element carries in lower case, so an svg `<style>` loses its text and every element after it.
 * A Mermaid drawing opens with its `<style>`, so a row under happy-dom reads markup with that
 * element taken out, and the whole drawing is proven by a row in a browser.
 */

const SVG_NAMESPACE = "http://www.w3.org/2000/svg";
/** The namespace a browser lays a `foreignObject`'s content out in, whatever namespace the parse
 *  that read the markup assigned it. */
const XHTML_NAMESPACE = "http://www.w3.org/1999/xhtml";

/** Which parser reads the markup. There is no default, so every call site says which renderer's
 *  markup it holds and none inherits the wrong one.
 *  - `"xml"`: beautiful-mermaid's markup (`@legion/proof-editor/mermaid-render`), an XML
 *    document, read by the `image/svg+xml` parser and checked for a `parsererror`.
 *  - `"html"`: upstream Mermaid's markup, read the way the browser mounts it. Mermaid serializes
 *    a `foreignObject`'s content with an HTML serializer, so a label holding a void `<br>` or
 *    `<img>` is not well-formed XML and an XML parse of it fails outright. An HTML parse reports
 *    no `parsererror` node, so the root is tested by what the body holds instead. */
export type MermaidParse = "xml" | "html";

/** Which allowlist a node is matched against and which namespace it is kept in: the drawing, or
 *  the XHTML subtree a kept `foreignObject` opens. */
type Subtree = "svg" | "xhtml";

const NAMESPACES: Record<Subtree, string> = {
  svg: SVG_NAMESPACE,
  xhtml: XHTML_NAMESPACE,
};

/** The elements the two renderers emit, measured over one diagram of each type each of them
 *  draws — beautiful-mermaid's six (flowchart, state, sequence, class, ER, xychart) and every
 *  type Mermaid registers, drawn with `htmlLabels: false` — plus the text and description
 *  elements an SVG may carry. Anything else is removed with its subtree. An XML name is
 *  case-sensitive, so an element matches by its exact local name: `<RECT>` is no `<rect>`. An
 *  HTML parse has already folded a name to the one the browser gives it, so the same exact match
 *  reads `<FOREIGNOBJECT>` as `foreignObject` there, as a browser does.
 *
 *  Mermaid is what the last eight are for:
 *  - `linearGradient` and `stop`: the gradient 13 of its drawings paint a shape with, among them
 *    flowchart, class, ER, state and sankey, whose links are gradients and vanish without them.
 *  - `filter` and `feDropShadow`: the drop shadow 12 of them carry, a sequence diagram's among
 *    them.
 *  - `clipPath` (one drawing) and `switch` (one).
 *  - `symbol`: defined by c4 and sequence. No `<use>` of any measured drawing references one, so
 *    it is admitted for the Mermaid version that does; it costs nothing, because `<use>` and
 *    every `href` stay removed, so nothing a `symbol` holds can be drawn or fetched.
 *  - `foreignObject`: where journey and eventmodeling put their labels, which are HTML. */
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
  "linearGradient",
  "stop",
  "filter",
  "feDropShadow",
  "clipPath",
  "switch",
  "symbol",
  "foreignObject",
]);

/** The elements a kept `foreignObject` may hold: Mermaid's label markup, and nothing else. This
 *  list is load-bearing, because an author's own markup is not escaped on this path —
 *  eventmodeling inserts its label as HTML, so an author's `<p>` reaches the subtree and it is
 *  Mermaid's own DOMPurify, not an escape, that took a script or a handler out of it. So the
 *  list is what keeps an `<img>` or an `<a href>` out of a label.
 *
 *  A node here is matched by local name whatever namespace the parse gave it: Mermaid writes
 *  `xmlns="http://www.w3.org/1999/xhtml"` on a `foreignObject`'s own child only, and the
 *  attribute list below drops that attribute, while the same drawing made under another DOM
 *  carries no `xmlns` at all. Each kept node is then rebuilt in the XHTML namespace. */
const ALLOWED_XHTML_ELEMENTS: ReadonlySet<string> = new Set(["div", "span", "b", "br", "code"]);

/** The attributes a node inside a `foreignObject` may carry: the two Mermaid's labels use.
 *  Everything else goes, its `xmlns` and any `href` with it. The rules that remove what executes
 *  or fetches still apply to these two, so a `style` holding `url(https://…)` goes here as it
 *  goes anywhere else. */
const ALLOWED_XHTML_ATTRIBUTES: ReadonlySet<string> = new Set(["class", "style"]);

/**
 * Turns the markup a renderer returns into an `<svg>` of `document`, sanitized by
 * `sanitizeMermaidSvg` rather than by upstream's private `sanitizeSvg`, with the editor's
 * accessible name. `parse` names the renderer's markup, as it does there. Markup that is not an
 * SVG document throws `Error("Invalid SVG output")`.
 */
export function mermaidSvgElement(
  markup: string,
  document: Document,
  parse: MermaidParse
): SVGSVGElement {
  const svg = sanitizeMermaidSvg(markup, document, parse);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "Rendered Mermaid diagram");
  return svg;
}

/** A drawn source: the sanitized diagram (shared; a consumer inserts a clone, as `showMermaidSvg`
 *  does), the full status text of a refusal, or `unavailable` when no renderer can draw the source
 *  (the host's worker failed to start or load), so the block shows its source as code, with no
 *  status line. */
export type MermaidDrawing = { svg: SVGSVGElement } | { error: string } | { unavailable: true };

/** Draws `source` for the editor: calls `draw` at most once and never after the returned cancel
 *  runs — synchronously inside the call when the drawing is cached or no renderer can run
 *  (`unavailable`), later when the renderer answers or fails. Returns the cancel. */
export type MermaidDrawer = (source: string, draw: (drawing: MermaidDrawing) => void) => () => void;

/**
 * Mounts `svg` (cloned into `host`'s document) with a `<style>` holding `sheet` in `host`'s open
 * shadow root, replacing whatever the root held, so neither the diagram's own `<style>` nor
 * `sheet` reaches the page and the page's rules do not reach the diagram. The root is created
 * once and reused: `attachShadow` throws on a second call, and a block redraws into the same
 * element on every edit.
 */
export function showMermaidSvg(host: HTMLElement, svg: SVGSVGElement, sheet: string): void {
  const document = host.ownerDocument;
  const root = host.shadowRoot ?? host.attachShadow({ mode: "open" });
  const style = document.createElement("style");
  style.textContent = sheet;
  root.replaceChildren(style, document.importNode(svg, true));
}

/**
 * Parses SVG markup with the parser `parse` names and returns its `<svg>` imported into
 * `document`, in the SVG namespace and with a kept `foreignObject`'s content in the XHTML
 * namespace. Markup that is not an SVG document — one that does not parse as XML, or whose root
 * is not the SVG namespace's `svg` by its exact, case-sensitive name, or, under `"html"`, whose
 * body does not hold exactly one element named `svg` — throws `Error("Invalid SVG output")`.
 *
 * Stricter than upstream's private `sanitizeSvg`, which blocks five tags, keeps every `href` but
 * a `javascript:` one and keeps the Google Fonts `@import`: this SVG comes from a comment or a
 * document, which any person or agent writes. So only the elements the renderers emit survive,
 * matched by exact local name; every `on*` and `href` attribute goes, matched case-insensitively,
 * which is stricter than XML asks; every attribute but a `data-*` one whose value holds a CSS
 * escape (`\`) or a function that fetches (`url(…)` other than a local `url(#id)`,
 * `image-set(…)`, `image(…)`, `cross-fade(…)`, `src(…)`, `element(…)`) goes, which covers an
 * author's `classDef`/`style` paint such as `fill:url(https://…)` or its escaped spelling
 * `fill:\75rl(https://…)`; a kept `foreignObject` holds nothing but `ALLOWED_XHTML_ELEMENTS`
 * carrying `ALLOWED_XHTML_ATTRIBUTES`, so no `<img>`, `<a>` or `href` survives in a label; and
 * each `<style>` loses its `@import` rules. The parser and every node come from `document` and
 * its window, never from the global scope.
 */
export function sanitizeMermaidSvg(
  markup: string,
  document: Document,
  parse: MermaidParse
): SVGSVGElement {
  const view = document.defaultView;
  if (view === null) throw new Error("sanitizeMermaidSvg needs a document that has a window");
  const root = parse === "xml" ? xmlRoot(view, markup) : htmlRoot(view, markup);
  return document.adoptNode(sanitizeElement(root, parse, "svg")) as SVGSVGElement;
}

/** The root of an XML parse: the markup must parse, and its root must be the SVG namespace's
 *  `svg` by its exact, case-sensitive name. */
function xmlRoot(view: Window & typeof globalThis, markup: string): Element {
  const parsed = new view.DOMParser().parseFromString(markup, "image/svg+xml");
  const root: Element = parsed.documentElement;
  if (
    parsed.getElementsByTagName("parsererror").length > 0 ||
    root.namespaceURI !== SVG_NAMESPACE ||
    root.localName !== "svg"
  ) {
    throw new Error("Invalid SVG output");
  }
  return root;
}

/** The root of an HTML parse, which reports no `parsererror` node of its own and takes any markup
 *  it is given: the body must hold exactly one element, and its local name — already the one the
 *  browser's own parse gives it — must be `svg`. The namespace the parse assigned is not read
 *  here, nor trusted below: `sanitizeElement` puts the drawing in the SVG namespace. */
function htmlRoot(view: Window & typeof globalThis, markup: string): Element {
  const body = new view.DOMParser().parseFromString(markup, "text/html").body;
  const root = body.firstElementChild;
  if (body.children.length !== 1 || root === null || root.localName !== "svg") {
    throw new Error("Invalid SVG output");
  }
  return root;
}

/** A `url(#id)` reference to an element of the same SVG, unquoted or quoted. */
const LOCAL_URL = /url\s*\(\s*(["']?)#[^"'()\s]*\1\s*\)/gi;
/** A CSS function that loads a resource. A value holding a backslash is refused outright, since
 *  CSS decodes escapes in a function's name (`\75rl(` is `url(`) and no output of either renderer
 *  needs one outside a `data-*` attribute. */
const FETCHING_FUNCTION = /\\|(?:url|image-set|image|cross-fade|src|element)\s*\(/i;

/** An `@import` rule: its `url(…)` (unquoted, or quoted and possibly holding `;`) or quoted
 *  string target, any media query after it, and its terminating `;`. beautiful-mermaid's own
 *  sheet is fixed rules after an `@import` of Google Fonts whose quoted URL holds `;`, and
 *  Mermaid's is its theme's; an author's `classDef` or `style` line lands in presentation
 *  attributes, which the `url(` check filters. */
const IMPORT_RULE =
  /@import\s*(?:url\(\s*(?:"[^"]*"|'[^']*'|[^"')]*)\s*\)|"[^"]*"|'[^']*')[^;]*(?:;|$)/gi;

/** Sanitizes `element` and its subtree in place, and answers it in the namespace `subtree`
 *  belongs in — itself where the parse already put it there, else the rebuilt node that took its
 *  place in the tree. */
function sanitizeElement(element: Element, parse: MermaidParse, subtree: Subtree): Element {
  const kept = inNamespace(element, NAMESPACES[subtree]);
  for (const attribute of Array.from(kept.attributes)) {
    const name = attribute.name.toLowerCase();
    const localName = attribute.localName.toLowerCase();
    const fetches =
      !localName.startsWith("data-") &&
      FETCHING_FUNCTION.test(attribute.value.replace(LOCAL_URL, ""));
    if (
      name.startsWith("on") ||
      localName.startsWith("on") ||
      localName === "href" ||
      fetches ||
      (subtree === "xhtml" && !ALLOWED_XHTML_ATTRIBUTES.has(localName))
    ) {
      kept.removeAttributeNode(attribute);
    }
  }
  if (kept.localName === "style") {
    kept.textContent = (kept.textContent ?? "").replace(IMPORT_RULE, "");
    return kept;
  }
  const below: Subtree = kept.localName === "foreignObject" ? "xhtml" : subtree;
  for (const child of Array.from(kept.children)) {
    if (allows(child, parse, below)) sanitizeElement(child, parse, below);
    else child.remove();
  }
  return kept;
}

/** Whether a child stays where it was found. Inside a `foreignObject` the match is by local name
 *  alone, since the namespace a label's markup is parsed into differs between DOMs and parsers.
 *  In the drawing, an XML parse's namespaces are the document's own, so an element outside the
 *  SVG namespace is no part of the drawing and goes; an HTML parse assigns a namespace by its own
 *  rules, so there only the name is read. */
function allows(child: Element, parse: MermaidParse, subtree: Subtree): boolean {
  if (subtree === "xhtml") return ALLOWED_XHTML_ELEMENTS.has(child.localName);
  if (parse === "xml" && child.namespaceURI !== SVG_NAMESPACE) return false;
  return ALLOWED_ELEMENTS.has(child.localName);
}

/** `element` itself when the parse already put it in `namespace`, else a copy of it in that
 *  namespace, holding the same attributes and the same children and standing in its place. The
 *  parse is not trusted with this: a browser's HTML parse switches to XHTML inside a
 *  `foreignObject` where happy-dom's leaves the content in the SVG namespace, and an XML parse of
 *  a drawing made under a DOM that writes no `xmlns` on a label does the same. A namespace
 *  declaration is not copied over: the copy's namespace is its own, and a stale `xmlns` could
 *  only contradict it. */
function inNamespace(element: Element, namespace: string): Element {
  if (element.namespaceURI === namespace) return element;
  const copy = element.ownerDocument.createElementNS(namespace, element.localName);
  for (const attribute of Array.from(element.attributes)) {
    if (attribute.name === "xmlns" || attribute.name.startsWith("xmlns:")) continue;
    copy.setAttribute(attribute.name, attribute.value);
  }
  while (element.firstChild !== null) copy.appendChild(element.firstChild);
  element.parentNode?.replaceChild(copy, element);
  return copy;
}
