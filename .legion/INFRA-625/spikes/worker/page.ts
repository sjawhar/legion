/* Spike page: compares a worker parse against the main-thread parse for INFRA-625. */
import {
  createHeadlessProof,
  type HeadlessProofEditor,
} from "/legion/workspaces/sjawhar/legion/infra-625/packages/proof-editor/src/lib-headless.ts";
import blockSchema from "/legion/workspaces/sjawhar/legion/infra-625/packages/envoy/internal/dispatch/pmdoc/schema/blocks.json";
import {
  DOMSerializer,
  type Node as ProseMirrorNode,
} from "/legion/workspaces/sjawhar/legion/infra-625/packages/dispatch/node_modules/prosemirror-model/dist/index.js";
import { shapes } from "./shapes.mjs";

/** What the worker posts once its imports and engine are up. */
interface WorkerReady {
  ready: true;
  probe: Record<string, boolean>;
  evalStartMs: number;
  evalMs: number;
  bootMs: number;
  bootError: string | null;
  timeOrigin: number;
}

/** What the worker posts for one parse request. */
interface WorkerAnswer {
  id: number;
  payload?: unknown;
  mode?: "obj" | "str";
  parseMs?: number;
  toJsonMs?: number;
  stringifyMs?: number;
  bytes?: number;
  sentAt?: number;
  error?: string;
  /** Stamped by the page's listener: the page clock when the answer arrived. */
  receivedAt?: number;
}

/** A promise that settles after `ms`, without an executor callback. */
function delay(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/** A running record of the longest gap between two `setTimeout(0)` ticks. */
class Heartbeat {
  private last = 0;
  private worst = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;

  start(): void {
    this.last = performance.now();
    this.worst = 0;
    const tick = (): void => {
      const now = performance.now();
      this.worst = Math.max(this.worst, now - this.last);
      this.last = now;
      this.timer = setTimeout(tick, 0);
    };
    this.timer = setTimeout(tick, 0);
  }

  /** The longest gap seen, after letting one last tick land. */
  async stop(): Promise<number> {
    await delay(4);
    clearTimeout(this.timer);
    return Math.round(this.worst * 10) / 10;
  }
}

const round = (value: number): number => Math.round(value * 10) / 10;

interface PageGlobals {
  ready?: boolean;
  bootInfo?: unknown;
  runWorker?: (shape: string, mode: "obj" | "str") => Promise<unknown>;
  runMain?: (shape: string) => Promise<unknown>;
  compare?: (shape: string) => Promise<unknown>;
  mainBoot?: unknown;
}
const page: PageGlobals = globalThis as unknown as PageGlobals;

const mainBootStart = performance.now();
const engine: HeadlessProofEditor = await createHeadlessProof({
  blockSchema: blockSchema as never,
  readOnly: true,
});
const mainBootMs = performance.now() - mainBootStart;
const serializer = DOMSerializer.fromSchema(engine.schema);
page.mainBoot = { mainBootMs: round(mainBootMs) };

const host = document.createElement("div");
host.style.cssText = "width:800px;position:absolute;left:-10000px;top:0";
document.body.appendChild(host);

/** Warms the parser, the serializer and layout so no measurement pays a first-call cost. */
function warm(): void {
  const doc = engine.parseMarkdown("- a\n> b\n\nx [y](z) *w*\n".repeat(200));
  const el = document.createElement("div");
  el.appendChild(serializer.serializeFragment(doc.content));
  host.appendChild(el);
  el.getBoundingClientRect();
  el.remove();
  engine.schema.nodeFromJSON(doc.toJSON());
}

/** Serializes a parsed document into the page and measures the layout it forces. */
function paint(doc: ProseMirrorNode): { serMs: number; layoutMs: number; error?: string } {
  const el = document.createElement("div");
  const t0 = performance.now();
  try {
    el.appendChild(serializer.serializeFragment(doc.content));
  } catch (error) {
    return { serMs: round(performance.now() - t0), layoutMs: -1, error: String(error).slice(0, 80) };
  }
  const t1 = performance.now();
  host.appendChild(el);
  el.getBoundingClientRect();
  const t2 = performance.now();
  el.remove();
  return { serMs: round(t1 - t0), layoutMs: round(t2 - t1) };
}

const worker = new Worker(new URL("./parse.worker.js", import.meta.url), { type: "module" });
worker.addEventListener("error", (event) => {
  console.error(
    `WORKER ERROR ${event.message} @ ${event.filename}:${event.lineno}:${event.colno}`,
    event.error === null || event.error === undefined ? "" : String(event.error.stack ?? "")
  );
});
worker.addEventListener("messageerror", () => console.error("WORKER MESSAGEERROR"));
let nextId = 1;
const pending = new Map<number, (answer: WorkerAnswer) => void>();
let workerReady: WorkerReady | undefined;
const readyGate = Promise.withResolvers<WorkerReady>();
worker.addEventListener("message", (event: MessageEvent<WorkerReady | WorkerAnswer>) => {
  const received = performance.now();
  const data = event.data;
  if ("ready" in data) {
    workerReady = data;
    readyGate.resolve(data);
    return;
  }
  const settle = pending.get(data.id);
  pending.delete(data.id);
  settle?.({ ...data, receivedAt: received });
});

const workerCreatedAt = performance.now();
const ready = await readyGate.promise;
page.bootInfo = {
  ...ready.probe,
  /** `new Worker()` until the worker's global scope existed (fetch start + thread spin-up). */
  spawnMs: round(ready.timeOrigin - performance.timeOrigin - workerCreatedAt),
  /** Scope creation until the bundle's whole import graph had evaluated (fetch + compile + eval). */
  loadMs: round(ready.evalStartMs),
  bootMs: round(ready.bootMs),
  bootError: ready.bootError,
  readyAfterCreateMs: round(performance.now() - workerCreatedAt),
};
warm();

/** The worker's `performance.now()` plus this is the page's `performance.now()`. */
const clockOffset = ready.timeOrigin - performance.timeOrigin;

page.runWorker = async (shape, mode) => {
  const body = shapes[shape]();
  const beat = new Heartbeat();
  beat.start();
  const id = nextId++;
  const settled = Promise.withResolvers<WorkerAnswer>();
  pending.set(id, settled.resolve);
  worker.postMessage({ id, body, mode });
  const answer = await settled.promise;
  if (answer.error !== undefined) {
    return { shape, len: body.length, mode, error: answer.error, gapMs: await beat.stop() };
  }
  const sentInPageClock = (answer.sentAt ?? 0) + clockOffset;
  const cloneMs = round((answer.receivedAt ?? 0) - sentInPageClock);
  const t0 = performance.now();
  const json = mode === "str" ? JSON.parse(answer.payload as string) : answer.payload;
  const t1 = performance.now();
  const doc = engine.schema.nodeFromJSON(json);
  const t2 = performance.now();
  const painted = paint(doc);
  const gapMs = await beat.stop();
  return {
    shape,
    len: body.length,
    mode,
    jsonBytes: answer.bytes,
    workerParseMs: round(answer.parseMs ?? 0),
    workerToJsonMs: round(answer.toJsonMs ?? 0),
    workerStringifyMs: round(answer.stringifyMs ?? 0),
    cloneMs,
    jsonParseMs: round(t1 - t0),
    nodeFromJsonMs: round(t2 - t1),
    ...painted,
    mainWorkMs: round(t1 - t0 + (t2 - t1) + painted.serMs + painted.layoutMs),
    gapMs,
  };
};

page.runMain = async (shape) => {
  const body = shapes[shape]();
  const beat = new Heartbeat();
  beat.start();
  await delay(8);
  const t0 = performance.now();
  let doc: ProseMirrorNode;
  try {
    doc = engine.parseMarkdown(body);
  } catch (error) {
    return {
      shape,
      len: body.length,
      parseMs: round(performance.now() - t0),
      error: String(error).slice(0, 80),
      gapMs: await beat.stop(),
    };
  }
  const parseMs = round(performance.now() - t0);
  const painted = paint(doc);
  return {
    shape,
    len: body.length,
    parseMs,
    ...painted,
    mainWorkMs: round(parseMs + painted.serMs + painted.layoutMs),
    gapMs: await beat.stop(),
  };
};

/** Block ids are freshly minted uuids on each parse, so parity compares the tree without them,
 *  and separately checks that each document minted the same number of them. */
function stripBlockIds(value: unknown, ids: string[]): unknown {
  if (Array.isArray(value)) {
    return value.map((item) => stripBlockIds(item, ids));
  }
  if (value === null || typeof value !== "object") {
    return value;
  }
  const out: Record<string, unknown> = {};
  for (const [key, inner] of Object.entries(value)) {
    if (key === "blockId" && typeof inner === "string") {
      ids.push(inner);
      continue;
    }
    out[key] = stripBlockIds(inner, ids);
  }
  return out;
}

/** The HTML a parsed document serializes to: what the reader actually sees. */
const htmlOf = (doc: ProseMirrorNode): string => {
  const el = document.createElement("div");
  el.appendChild(serializer.serializeFragment(doc.content));
  return el.innerHTML;
};

page.compare = async (shape) => {
  const body = shapes[shape]();
  const id = nextId++;
  const settled = Promise.withResolvers<WorkerAnswer>();
  pending.set(id, settled.resolve);
  worker.postMessage({ id, body, mode: "str" });
  const answer = await settled.promise;
  if (answer.error !== undefined) {
    return { shape, workerError: answer.error };
  }
  const workerIds: string[] = [];
  const mainIds: string[] = [];
  const workerJson = stripBlockIds(JSON.parse(answer.payload as string), workerIds);
  const workerDoc = engine.schema.nodeFromJSON(JSON.parse(answer.payload as string));
  const mainDoc = engine.parseMarkdown(body);
  const mainJson = stripBlockIds(mainDoc.toJSON(), mainIds);
  const sameJson = JSON.stringify(workerJson) === JSON.stringify(mainJson);
  const workerHtml = htmlOf(workerDoc);
  const mainHtml = htmlOf(mainDoc);
  return {
    shape,
    len: body.length,
    sameJson,
    sameIdCount: workerIds.length === mainIds.length,
    idCount: mainIds.length,
    uniqueIds: new Set([...workerIds, ...mainIds]).size === workerIds.length + mainIds.length,
    sameHtml: workerHtml.replaceAll(/[0-9a-f]{8}-[0-9a-f-]{27}/gu, "·") ===
      mainHtml.replaceAll(/[0-9a-f]{8}-[0-9a-f-]{27}/gu, "·"),
    htmlBytes: mainHtml.length,
  };
};

page.ready = true;
