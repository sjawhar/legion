/**
 * The Web Worker that parses a long Markdown body for `markdown-engine.ts`: the same read-only
 * headless Proof engine the page parses with, off the reader's own thread, so no body under the
 * server's cap holds the tab while it is read (sjawhar/legion#2519). The page creates it with
 * `new Worker(new URL(…), { type: "module" })` once a body longer than `markdownWorkerThreshold`
 * is rendered, so Vite bundles this module, the engine and the schema manifest as the worker's
 * own chunk, fetched by a page that holds such a body and by no other.
 *
 * The engine is built from the manifest this build compiled in (`compiledBlockSchema`), the one
 * the page itself parses with, so a long body reads the same above and below the threshold and
 * no schema has to be posted here. It is built read-only and with no `renderBlock`: a typed
 * block's host drawing is read only by `toDOM` (`block-schema.ts`), and the answer this worker
 * posts is the parsed document as `toJSON` gives it, which the page turns back into a node of
 * its own schema - the one carrying the page's `renderBlock` - with `documentFromJSON`
 * (`markdown-engine.ts`), node by node. `Schema.nodeFromJSON` is not that path and cannot be: it
 * checks each attribute against the type its schema gives it, and this page's own parser writes
 * `title: null` for a picture with no title where `image.title` is a string, so every body
 * holding one would be refused.
 *
 * Once its imports have loaded it posts `{ ready: true }`, so the page starts a body's deadline
 * when the worker can start on it, never while its chunk is still downloading. The page then
 * posts each body as `{ id, markdown, softBreaks }`; this worker answers each with
 * `{ id, doc, openMermaidFence }` or, for a body the parser refuses, `{ id }` with no `doc`.
 * `openMermaidFence` is the open-fence check `mermaid-diagrams.ts` would otherwise run on the
 * page with a second parse of the same body, so a streaming turn ending in an unclosed
 * ```` ```mermaid ```` fence costs one round trip per frame rather than two.
 *
 * A failure the page must hear about - a manifest the engine cannot be built from - is thrown
 * rather than answered, so the worker's `error` reaches the page and it parses the body itself;
 * a refusal answered as `{ id }` means only that the parser refused the body, which is a body's
 * one final failure.
 */
import {
  createHeadlessProof,
  type HeadlessProofEditor,
  type SoftBreaks,
} from "@legion/proof-editor/headless";

import type { Node as ProseMirrorNode } from "prosemirror-model";

import { compiledBlockSchema } from "../doc/schema";
import { endsInOpenMermaidFence } from "./mermaid-diagrams";
import type { WorkerReply } from "./worker-queue";

/** One body the page asks this worker to parse; `id` names the answer. */
export interface ParseRequest {
  id: number;
  markdown: string;
  softBreaks: SoftBreaks;
}

/** A document, or one node of it, as `ProseMirrorNode.toJSON()` writes it: what this worker
 *  answers with, and what `documentFromJSON` (`markdown-engine.ts`) turns back into a node of
 *  the page's own schema. */
export interface DocumentJSON {
  readonly type: string;
  readonly attrs?: Record<string, unknown>;
  readonly content?: readonly DocumentJSON[];
  readonly marks?: readonly { readonly type: string; readonly attrs?: Record<string, unknown> }[];
  readonly text?: string;
}

/** What this worker has to say about one body: the parsed document, with the open-fence check
 *  made with the parse beside it. No `doc` is a body the parser refused. */
export interface ParsedBody {
  doc?: DocumentJSON;
  openMermaidFence?: boolean;
}

/** What the worker posts: that it is ready, or the answer to one request, under the id the
 *  request carried (`WorkerReply`). */
export type ParseReply = WorkerReply<ParsedBody>;

/** The dedicated worker's own scope: `self`, typed as far as this module uses it (the SPA's
 *  TypeScript program carries the DOM library, not the worker one). */
interface WorkerScope {
  addEventListener(type: "message", listener: (event: MessageEvent<ParseRequest>) => void): void;
  postMessage(message: ParseReply): void;
}

/** Answers one body: the parsed document as plain JSON with the open-fence check beside it, or a
 *  refusal for a body the parser throws on (a raw-HTML span, a mark span this schema has no node
 *  for), which the page shows as its literal text. */
function answerParseRequest(
  { id, markdown, softBreaks }: ParseRequest,
  proof: HeadlessProofEditor
): ParseReply {
  let parsed: ProseMirrorNode;
  try {
    parsed = proof.parseMarkdown(markdown, softBreaks);
  } catch {
    return { id };
  }
  return {
    doc: parsed.toJSON() as DocumentJSON,
    id,
    openMermaidFence: endsInOpenMermaidFence(parsed, markdown, (source) => {
      try {
        return proof.parseMarkdown(source, softBreaks);
      } catch {
        return undefined;
      }
    }),
  };
}

// A dedicated worker's global is a `WorkerGlobalScope`, which a page does not expose, so nothing
// listens where this module is imported for its types alone.
if ("WorkerGlobalScope" in globalThis) {
  const scope = globalThis as unknown as WorkerScope;
  let engine: Promise<HeadlessProofEditor> | undefined;
  scope.addEventListener("message", (event) => {
    // Built on the first body rather than on import, so a worker a page creates and never sends
    // anything to costs nothing beyond its chunk. A manifest the engine cannot be built from
    // throws here, where the page hears it as the worker's `error`.
    engine ??= createHeadlessProof({ blockSchema: compiledBlockSchema, readOnly: true });
    const request = event.data;
    void engine.then(
      (proof) => {
        scope.postMessage(answerParseRequest(request, proof));
      },
      (error: unknown) => {
        // A rejected promise is the worker's own business; the page hears a failure only as an
        // uncaught error, which is what makes it parse the body itself.
        setTimeout(() => {
          throw error;
        });
      }
    );
  });
  scope.postMessage({ ready: true });
}
