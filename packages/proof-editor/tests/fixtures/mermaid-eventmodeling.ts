/**
 * Upstream Mermaid 12.3.0's drawing of an `eventmodeling` frame carrying inline data, for the
 * sanitizer rows of `tests/mermaid-svg.test.ts`. The frame's label is HTML inside a
 * `<foreignObject>`: a `<div>`, a `<span>`, the frame's name in `<b>`, two `<br>` line breaks
 * and the inline data in `<code>`, which is every element a label may hold.
 *
 * Source: `eventmodeling\n  tf 1 cmd Dispatch {item: apple, count: 2}\n`, which is the
 * `eventmodeling` entry of `packages/dispatch/e2e/mermaid-sources.ts` with the inline data
 * `{item: apple, count: 2}` on its frame.
 *
 * Recorded by rendering that source through
 * `packages/dispatch/web/src/features/refs/mermaid-upstream.ts` under Bun -- so under the
 * configuration that module's `initialize` sets (theme `base` with Dispatch's palette,
 * `securityLevel: "strict"`, `htmlLabels: false`, `deterministicIds: true`,
 * `suppressErrorRendering: true`) and against that module's own DOM shim -- with one stub on
 * `globalThis` the shim needs and Bun has not:
 *
 *     class OffscreenCanvas {
 *       constructor(width, height) {}
 *       getContext(kind) {
 *         return kind === "2d"
 *           ? { font: "", measureText: (text) => ({ width: text.length * 8 }) }
 *           : null;
 *       }
 *     }
 *
 * Every length below was measured through that `measureText`, so a different width per character
 * records a different drawing. One source per process, so the render id is `dispatch-mermaid-0`.
 *
 * The drawing's own `<style>` element is taken out of what is committed, and nothing else is:
 * happy-dom 20.14.0's HTML parser compares a `</style>` end tag with the element's upper-case
 * `tagName`, which an SVG element carries in lower case, so an svg `<style>` loses its text and
 * every element after it (`src/mermaid-svg.ts`'s module comment). A row under happy-dom would
 * therefore read an empty drawing: the whole drawing, its `<style>` included, is read by a
 * browser row instead (`packages/dispatch/e2e/mermaid.e2e.ts`).
 *
 * One spelling in this recording is not the one a browser writes. mermaid rewrites every void
 * `<br>` of a label to `<br/>` (`cleanUpSvgCode`) and then hands the markup to DOMPurify, whose
 * serializer writes it back as a void `<br>` in a browser and leaves it self-closed under
 * linkedom, which is the DOM the shim builds. Measured in Google Chrome for Testing 158.0.8117.2
 * against mermaid 12.3.0, with this source and `securityLevel: "strict"`, the label reads
 * `<b>Dispatch</b><br><br><code ...>`. The row that needs the void form writes it back itself,
 * and says why.
 */
export const MERMAID_EVENTMODELING_DRAWING =
  '<svg aria-roledescription="eventmodeling" role="graphics-document document" viewBox="-30 -30 639 190" style="max-width: 639px;" xmlns="http://www.w3.org/2000/svg" width="100%" id="dispatch-mermaid-0"><g /><g class="em-swimlane"><rect stroke="rgb(240,240,240)" fill="rgb(250,250,250)" height="130" width="579" rx="3" y="0" x="0" /><text y="30" x="30" font-weight="bold">Command/Read Model</text></g><g class="em-box"><rect fill="#bcd6fe" stroke="#679ac3" height="100" width="304" rx="3" y="15" x="250" /><foreignObject height="80" width="284" y="25" x="260"><div xmlns="http://www.w3.org/1999/xhtml" style="display:table;height:100%;width:100%"><span style="display:table-cell;text-align:center;vertical-align:middle"><b>Dispatch</b><br/><br/><code style="text-align: left; display: block;max-width:430px">item:&#160;apple,&#160;count:</code></span></div></foreignObject></g><defs><marker orient="auto" refY="3.5" refX="10" markerHeight="7" markerWidth="10" id="em-arrowhead-dispatch-mermaid-0"><polygon fill="#62748e" points="0 0, 10 3.5, 0 7" /></marker></defs></svg>';
