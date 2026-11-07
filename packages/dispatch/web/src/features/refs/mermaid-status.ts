/**
 * The one line a reader is shown for a mermaid source no renderer drew, as far as both ends of
 * the diagram worker agree on it: the page writes the line (`mermaid-diagrams.ts`) and the worker
 * cuts the reason to what is left of the line's budget (`mermaid-render.worker.ts`), so the words
 * and the budget are one module's rather than each module's own.
 *
 * It imports nothing, so the page reads the worker's arithmetic without pulling the worker's own
 * module -- and beautiful-mermaid with it -- into the page's chunk.
 */

/** What every refusal that names a reason begins with. */
export const STATUS_PREFIX = "Unable to render Mermaid diagram: ";

/** The whole status line's budget: the prefix and the reason after it (requirement R3). */
export const STATUS_LINE_MAX_CHARS = 200;
