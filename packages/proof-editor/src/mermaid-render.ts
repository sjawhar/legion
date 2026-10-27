import { renderProofMermaidSvg } from "proof-sdk-upstream/src/editor/plugins/mermaid-diagrams";

/**
 * Draws a Mermaid source as the document editor does — upstream's `renderProofMermaidSvg`, so the
 * same validation, renderer, colours and spacing — and returns the SVG markup unsanitized. A
 * source upstream refuses throws upstream's `Error` (for example `Dangling Mermaid edge: "A -->"`).
 *
 * It touches no DOM, so it runs in a Web Worker: Dispatch's Mermaid worker is its caller, which
 * keeps beautiful-mermaid and ELK, whose layout cost grows without bound with the diagram, off the
 * reader's main thread. The page turns the markup into an element with `mermaidSvgElement` from
 * `@legion/proof-editor/mermaid`, which sanitizes it.
 */
export function renderMermaidMarkup(source: string): string {
  return renderProofMermaidSvg(source);
}
