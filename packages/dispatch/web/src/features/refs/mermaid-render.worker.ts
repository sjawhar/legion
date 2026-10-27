/**
 * The Web Worker that draws a Mermaid source for `mermaid-diagrams.ts`: `renderMermaidMarkup` from
 * `@legion/proof-editor/mermaid-render` (upstream's validation, beautiful-mermaid and ELK), whose
 * layout cost grows without bound with the diagram, runs here rather than on the reader's main
 * thread. The page creates it with `new Worker(new URL(…), { type: "module" })`, so Vite bundles
 * this module and the renderer as the worker's own chunk.
 *
 * Once its imports have loaded it posts `{ ready: true }`, so the page starts a source's time
 * limit when the worker can start on it, never while its chunk is still downloading. Then it
 * answers each `{ id, source }` request, in order, with `{ id, markup }` (the SVG markup, still
 * unsanitized) or `{ id, error }` (the message of the renderer's refusal).
 */
import { renderMermaidMarkup } from "@legion/proof-editor/mermaid-render";

/** A source the page asks this worker to draw; `id` names the answer. */
export interface MermaidRequest {
  id: number;
  source: string;
}

/** What the worker posts: that it is ready, or the answer to one request. An `error` of `null`
 *  is a refusal whose thrown value was no `Error`. */
export type MermaidReply =
  | { ready: true }
  | { id: number; markup: string }
  | { id: number; error: string | null };

/** Draws one request's source. Exported for the unit tests, whose stand-in `Worker` answers
 *  through it on the test's own thread. */
export function answerMermaidRequest({ id, source }: MermaidRequest): MermaidReply {
  try {
    return { id, markup: renderMermaidMarkup(source) };
  } catch (error) {
    return { id, error: error instanceof Error ? error.message : null };
  }
}

/** The dedicated worker's own scope: `self`, typed as far as this module uses it (the SPA's
 *  TypeScript program carries the DOM library, not the worker one). */
interface WorkerScope {
  addEventListener(type: "message", listener: (event: MessageEvent<MermaidRequest>) => void): void;
  postMessage(message: MermaidReply): void;
}

// A dedicated worker's global is a `WorkerGlobalScope`, which a page does not expose; a test that
// imports this module for `answerMermaidRequest` gets no listener.
if ("WorkerGlobalScope" in globalThis) {
  const scope = globalThis as unknown as WorkerScope;
  // `self` reads as undefined while a request is drawn. beautiful-mermaid builds ELK's bundled
  // engine on its first flowchart, and that engine, finding `self` and no `document`, takes this
  // worker for an ELK worker of its own: it claims the worker's `onmessage` and leaves its
  // in-thread constructor unexported, so every flowchart is refused (`En is not a constructor`).
  // beautiful-mermaid deletes `globalThis.self` around that build, which hides it under Bun and
  // Node but not in a browser, where `self` is an accessor of `WorkerGlobalScope.prototype` that
  // a delete on the global does not reach. So an accessor reading undefined shadows it on the
  // global's own prototype (`DedicatedWorkerGlobalScope.prototype`, which has none) for the call.
  const ownPrototype: object = Object.getPrototypeOf(globalThis);
  const hiddenSelf: PropertyDescriptor = {
    configurable: true,
    get: () => undefined,
    set: () => {
      // beautiful-mermaid writes back the value it read; there is nothing to keep.
    },
  };
  scope.addEventListener("message", (event) => {
    Object.defineProperty(ownPrototype, "self", hiddenSelf);
    let reply: MermaidReply;
    try {
      reply = answerMermaidRequest(event.data);
    } finally {
      Reflect.deleteProperty(ownPrototype, "self");
    }
    scope.postMessage(reply);
  });
  scope.postMessage({ ready: true });
}
