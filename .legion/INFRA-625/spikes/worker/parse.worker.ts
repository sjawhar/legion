/* Spike: the read-only Dispatch markdown engine inside a module Web Worker. */
const importStart = performance.now();
import {
  createHeadlessProof,
  type HeadlessProofEditor,
} from "/legion/workspaces/sjawhar/legion/infra-625/packages/proof-editor/src/lib-headless.ts";
import blockSchema from "/legion/workspaces/sjawhar/legion/infra-625/packages/envoy/internal/dispatch/pmdoc/schema/blocks.json";

/** The dedicated worker's own scope, as far as this spike uses it. */
interface WorkerScope {
  postMessage(message: unknown): void;
  addEventListener(type: "message", listener: (event: MessageEvent<ParseRequest>) => void): void;
}

/** One body the page asks this worker to parse. */
interface ParseRequest {
  id: number;
  body: string;
  /** `obj`: post the `toJSON()` object itself. `str`: post it as a JSON string. */
  mode: "obj" | "str";
}

// The worker global is a `DedicatedWorkerGlobalScope`; the spike's TypeScript program carries the
// DOM library, not the worker one, so name the shape this module uses.
const scope: WorkerScope = globalThis as unknown as WorkerScope;

const probe = {
  hasWindow: "window" in globalThis,
  hasDocument: "document" in globalThis,
  hasSelf: "self" in globalThis,
  hasCrypto: typeof crypto !== "undefined" && typeof crypto.getRandomValues === "function",
  isWorker: "WorkerGlobalScope" in globalThis,
};

const importDone = performance.now();

async function boot(): Promise<void> {
  const bootStart = performance.now();
  let bootError: string | null = null;
  let engine: HeadlessProofEditor | undefined;
  try {
    engine = await createHeadlessProof({ blockSchema: blockSchema as never, readOnly: true });
  } catch (error) {
    bootError =
      error instanceof Error
        ? `${error.name}: ${error.message}\n${(error.stack ?? "").split("\n").slice(0, 8).join("\n")}`
        : String(error);
  }
  const bootMs = performance.now() - bootStart;

  scope.addEventListener("message", (event) => {
    const { id, body, mode } = event.data;
    if (engine === undefined) {
      scope.postMessage({ id, error: bootError ?? "no engine" });
      return;
    }
    const t0 = performance.now();
    let json: unknown;
    let parseMs: number;
    try {
      const doc = engine.parseMarkdown(body);
      parseMs = performance.now() - t0;
      json = doc.toJSON();
    } catch (error) {
      scope.postMessage({ id, error: String(error).slice(0, 200), parseMs: performance.now() - t0 });
      return;
    }
    const t2 = performance.now();
    const payload = mode === "str" ? JSON.stringify(json) : json;
    const t3 = performance.now();
    scope.postMessage({
      id,
      payload,
      mode,
      parseMs,
      toJsonMs: t2 - t0 - parseMs,
      stringifyMs: t3 - t2,
      bytes: typeof payload === "string" ? payload.length : -1,
      sentAt: performance.now(),
    });
  });

  scope.postMessage({
    ready: true,
    probe,
    /** Worker clock (0 = worker scope creation) when this module started evaluating. */
    evalStartMs: importStart,
    /** How long the bundle's top-level evaluation took before `boot()` ran. */
    evalMs: bootStart - importStart,
    bootMs,
    bootError,
    timeOrigin: performance.timeOrigin,
  });
}

void boot();
