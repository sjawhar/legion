import { createHeadlessProof, type HeadlessProofEditor } from "@legion/proof-editor/headless";

import serverBlockSchema from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../../api/types";
import { setParseWorkerFactory } from "./markdown-engine";
// The worker's own protocol, which this fake answers; a type import, so nothing of the worker's
// module runs in the test's process.
import type { ParseReply, ParseRequest } from "./markdown-parse.worker";

/**
 * The page's parse worker, faked in process, for the rows that prove what a reader sees while a
 * body longer than `markdownWorkerThreshold` is parsed off the page's own thread
 * (sjawhar/legion#2403; `markdown-engine.ts` and `MarkdownBody.test.tsx` hold those rows).
 *
 * `markdown-engine.ts` creates its worker through one module-level factory, which
 * `setParseWorkerFactory` replaces, so a row installs this fake in its place and the engine's own
 * protocol runs unchanged:
 *
 * - the worker posts `{ ready: true }` once its imports have loaded, on a later task;
 * - the engine posts each body as `{ id, markdown, softBreaks }`;
 * - the worker answers `{ id, doc }`, where `doc` is the parsed document as the plain JSON
 *   `ProseMirrorNode.toJSON()` gives, and `{ id }` with no `doc` for a body the parser refused;
 * - a worker that crashed fires `error` and answers nothing;
 * - `terminate()` stops it: it posts nothing after that.
 *
 * Anything else the engine posts (a schema handshake, say) is kept in the worker's `posted` and
 * answered with nothing, so only bodies count as requests.
 *
 * Unlike the real worker nothing here answers on its own: a row calls `answer` or `crash`, so the
 * row decides when each answer lands, and can land a newer body's answer before an older one's.
 * Installing a factory leaves the engine holding no worker, so each row starts from a page that
 * has created none; `restoreParseWorker` puts the page's own factory back.
 */

type Listener = (event: unknown) => void;

const blockSchema = serverBlockSchema as unknown as BlockSchema;
/** The read-only engine the real worker builds, here on the test's own thread, built once. */
let headless: Promise<HeadlessProofEditor> | undefined;

/** Whether a message the engine posted is a body to parse, rather than anything else it sends. */
function isParseRequest(message: unknown): message is ParseRequest {
  return typeof (message as ParseRequest | undefined)?.markdown === "string";
}

function tick(): Promise<void> {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, 0);
  });
}

/** Stands in for one `Worker` the engine created: it records what it was posted and emits what a
 *  row tells it to, through both the `onmessage`/`onerror` properties and the listeners, so the
 *  engine is free to use either. */
export class FakeParseWorker {
  /** Every message the engine posted, bodies and anything else, oldest first. */
  readonly posted: unknown[] = [];
  terminated = false;
  onerror: Listener | null = null;
  onmessage: Listener | null = null;
  private readonly listeners: Record<string, Listener[]> = { error: [], message: [] };
  private readonly took: (request: ParseRequest, worker: FakeParseWorker) => void;

  constructor(took: (request: ParseRequest, worker: FakeParseWorker) => void) {
    this.took = took;
    setTimeout(() => {
      this.emit("message", { data: { ready: true } });
    }, 0);
  }

  addEventListener(type: string, listener: Listener): void {
    this.listeners[type]?.push(listener);
  }

  removeEventListener(type: string, listener: Listener): void {
    const listeners = this.listeners[type];
    const at = listeners?.indexOf(listener) ?? -1;
    if (listeners !== undefined && at !== -1) {
      listeners.splice(at, 1);
    }
  }

  postMessage(message: unknown): void {
    this.posted.push(message);
    if (isParseRequest(message)) {
      this.took(message, this);
    }
  }

  terminate(): void {
    this.terminated = true;
  }

  emit(type: "error" | "message", event: unknown): void {
    if (this.terminated) {
      return;
    }
    const own = type === "message" ? this.onmessage : this.onerror;
    own?.(event);
    for (const listener of [...(this.listeners[type] ?? [])]) {
      listener(event);
    }
  }
}

/** What a row drives the faked worker with. */
export interface InstalledParseWorker {
  /** How many times the engine asked the factory for a worker, a refused creation included. */
  readonly created: number;
  /** Every body the engine has sent to be parsed, oldest first, across workers. */
  readonly requests: readonly ParseRequest[];
  /** Every worker the factory made, oldest first. */
  readonly workers: readonly FakeParseWorker[];
  /** Answers the `index`th body the engine sent, once it has sent it: the document the read-only
   *  engine parses in process, or a refusal where that parser throws, delivered on a later task.
   *  Resolves once the answer has been delivered. */
  answer(index: number): Promise<void>;
  /** Fires `error` on the worker holding the `index`th body, as a worker that crashed mid-parse
   *  does, and answers nothing for it. */
  crash(index: number): Promise<void>;
  /** Resolves once the engine has sent `count` bodies, and throws naming what it sent instead. */
  waitForRequests(count: number): Promise<void>;
}

/** Installs the fake in place of the page's `new Worker(…)`. `refuseToCreate` makes the factory
 *  throw instead, as a page that forbids a worker does. */
export function installFakeParseWorker({
  refuseToCreate = false,
}: {
  refuseToCreate?: boolean;
} = {}): InstalledParseWorker {
  const workers: FakeParseWorker[] = [];
  const taken: { request: ParseRequest; worker: FakeParseWorker }[] = [];
  let asked = 0;

  setParseWorkerFactory(() => {
    asked += 1;
    if (refuseToCreate) {
      throw new Error("This page cannot create a parse worker.");
    }
    const worker = new FakeParseWorker((request, from) => {
      taken.push({ request, worker: from });
    });
    workers.push(worker);
    return worker as unknown as Worker;
  });

  async function waitForRequests(count: number): Promise<void> {
    for (let tries = 0; taken.length < count && tries < 500; tries += 1) {
      await tick();
    }
    if (taken.length < count) {
      throw new Error(
        `The engine sent the parse worker ${taken.length} bodies, not ${count}: a body longer ` +
          "than markdownWorkerThreshold is still parsed on the page's own thread."
      );
    }
  }

  async function sent(index: number): Promise<{ request: ParseRequest; worker: FakeParseWorker }> {
    await waitForRequests(index + 1);
    return taken[index];
  }

  return {
    get created() {
      return asked;
    },
    get requests() {
      return taken.map(({ request }) => request);
    },
    get workers() {
      return workers;
    },
    async answer(index) {
      const { request, worker } = await sent(index);
      headless ??= createHeadlessProof({ blockSchema, readOnly: true });
      const engine = await headless;
      let doc: object | undefined;
      try {
        doc = engine.parseMarkdown(request.markdown, request.softBreaks).toJSON();
      } catch {
        doc = undefined;
      }
      const reply: ParseReply = { doc, id: request.id };
      await tick();
      worker.emit("message", { data: reply });
    },
    async crash(index) {
      const { worker } = await sent(index);
      await tick();
      worker.emit("error", new Event("error"));
    },
    waitForRequests,
  };
}

/** Puts the page's own worker factory back, so the next page creates a real one. */
export function restoreParseWorker(): void {
  setParseWorkerFactory(undefined);
}

/** Lets the tasks already queued run, so a body the engine was about to send has been sent by the
 *  time a row counts what it sent. */
export async function flushTasks(): Promise<void> {
  for (let tries = 0; tries < 5; tries += 1) {
    await tick();
  }
}
