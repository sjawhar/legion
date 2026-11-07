/**
 * Upstream Mermaid 12.3.0's drawing of a `journey`, for the sanitizer rows of
 * `tests/mermaid-svg.test.ts`. Its labels are HTML inside a `<foreignObject>`, which is what
 * these rows read.
 *
 * Source: `journey\n  title Dispatch\n  section Draw\n    Render: 5: Worker`, which is the
 * `journey` entry of `packages/dispatch/e2e/mermaid-sources.ts`.
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
 */
export const MERMAID_JOURNEY_DRAWING =
  '<svg aria-roledescription="journey" role="graphics-document document" height="565" preserveAspectRatio="xMinYMin meet" viewBox="0 -25 500 540" style="max-width: 500px;" xmlns="http://www.w3.org/2000/svg" width="100%" id="dispatch-mermaid-0"><g /><defs><marker orient="auto" markerHeight="4" markerWidth="6" refY="2" refX="5" id="dispatch-mermaid-0-arrowhead"><path d="M 0,0 V 4 L6,2 Z" /></marker></defs><circle r="7" stroke="#000" fill="#8FBC8F" class="actor-0" cy="60" cx="20" /><text class="legend" y="67" x="40"><tspan x="50">Worker</tspan></text><g><rect class="journey-section section-type-0" ry="3" rx="3" height="50" width="150" stroke="#666" fill="#191970" y="50" x="150" /><switch><foreignObject position="fixed" height="50" width="150" y="50" x="150"><div xmlns="http://www.w3.org/1999/xhtml" class="journey-section section-type-0" style="display:table;height:100%;width:100%"><div style="display:table-cell;text-align:center;vertical-align:middle" class="label">Draw</div></div></foreignObject><text class="journey-section section-type-0" alignment-baseline="central" dominant-baseline="central" style="text-anchor:middle;font-size:14;font-family:&quot;Open Sans&quot;, sans-serif" y="75" x="225"><tspan dy="0" x="225">Draw</tspan></text></switch></g><g><line stroke="#666" stroke-dasharray="4 2" stroke-width="1px" class="task-line" y2="450" x2="225" y1="110" x1="225" id="dispatch-mermaid-0-task0" /><circle overflow="visible" stroke-width="2" r="15" class="face" cy="300" cx="225" /><g><circle stroke="#666" fill="#666" stroke-width="2" r="1.5" cy="295" cx="220" /><circle stroke="#666" fill="#666" stroke-width="2" r="1.5" cy="295" cx="230" /><path transform="translate(225,302)" d="M7.5,0A7.5,7.5,0,1,1,-7.5,0L-6.818,0A6.818,6.818,0,1,0,6.818,0Z" class="mouth" /></g><rect class="task task-type-0" ry="3" rx="3" height="50" width="150" stroke="#666" fill="#191970" y="110" x="150" /><circle r="7" stroke="#000" fill="#8FBC8F" class="actor-0" cy="110" cx="164"><title>Worker</title></circle><switch><foreignObject position="fixed" height="50" width="150" y="110" x="150"><div xmlns="http://www.w3.org/1999/xhtml" class="task" style="display:table;height:100%;width:100%"><div style="display:table-cell;text-align:center;vertical-align:middle" class="label">Render</div></div></foreignObject><text class="task" alignment-baseline="central" dominant-baseline="central" style="text-anchor:middle;font-size:14;font-family:&quot;Open Sans&quot;, sans-serif" y="135" x="225"><tspan dy="0" x="225">Render</tspan></text></switch></g><text font-family="&quot;trebuchet ms&quot;, verdana, arial, sans-serif" fill="" y="25" font-weight="bold" font-size="4ex" x="150">Dispatch</text><line marker-end="url(#dispatch-mermaid-0-arrowhead)" stroke="black" stroke-width="4" y2="200" x2="346" y1="200" x1="150" /></svg>';
