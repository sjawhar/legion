/**
 * The Web Worker that draws a Mermaid source for `mermaid-diagrams.ts`, with the two renderers
 * Dispatch holds. `renderMermaidMarkup` from `@legion/proof-editor/mermaid-render` (upstream's
 * validation, beautiful-mermaid and ELK) draws the six families it has a layout for, and upstream
 * `mermaid` (`mermaid-upstream.ts`) draws every other type — a timeline, a mindmap, a pie — and
 * every source beautiful-mermaid refuses. Both layouts grow without bound with the diagram, so
 * both run here rather than on the reader's main thread. The page creates the worker with
 * `new Worker(new URL(…), { type: "module" })`, so Vite bundles this module and beautiful-mermaid
 * as the worker's own chunk; Mermaid is reached with `await import(…)`, so a reader whose page
 * holds only the families beautiful-mermaid draws fetches none of it.
 *
 * Which renderer a source goes to is read from the source's text alone (`drawsOnTheFastPath`), so
 * routing fetches nothing, and a source beautiful-mermaid refuses falls through to Mermaid.
 *
 * Once its imports have loaded it posts `{ ready: true }`, so the page starts a source's bound
 * when the worker can start on it, never while its chunk is still downloading. Then it answers
 * each `{ id, source }` request, in order, with `{ id, markup, renderer }` (the SVG markup, still
 * unsanitized, and the renderer that wrote it, which is the parse the page reads it with),
 * `{ id, error }` (the one-line reason Mermaid refused that source) or
 * `{ id, rendererUnavailable }` (no renderer could be loaded, which says nothing about the
 * source, so the page stores nothing and asks again). Under the same id it posts the phase it is
 * entering before each step of a job — `loading` before each download Mermaid needs, `drawing`
 * before the layout itself — so the page bounds a download and a layout differently
 * (`worker-queue.ts`, `mermaid-diagrams.ts`), and no job is unbounded. Everything a source needs
 * is downloaded under `loading`, the type's own definition chunk and its layout engine included
 * (`mermaid-upstream.ts`, `prepare`), so no part of a cold cache is spent on the render bound.
 */
import { renderMermaidMarkup } from "@legion/proof-editor/mermaid-render";

import { STATUS_LINE_MAX_CHARS, STATUS_PREFIX } from "./mermaid-status";
// Type-only, so it is erased: the page's chunk graph gains no edge to the renderer module.
import type { MermaidRenderer } from "./mermaid-upstream";
import type { JobPhase, WorkerReply } from "./worker-queue";

/** A source the page asks this worker to draw; `id` names the answer. */
export interface MermaidRequest {
  id: number;
  source: string;
}

/** Which renderer drew a source. The page reads the markup with the parse its renderer writes
 *  (`mermaid-diagrams.ts`): beautiful-mermaid's markup is XML, Mermaid's is HTML. */
export type MermaidRendererName = "beautiful-mermaid" | "mermaid";

/** What this worker has to say about one source: the SVG markup, still unsanitized, with the
 *  renderer that drew it; the reason of a refusal, which is one line of at most
 *  `REASON_MAX_CHARS` characters (an `error` of `null` is a refusal whose thrown value was no
 *  `Error`, or one whose message says nothing); or that no renderer could be loaded at all.
 *
 *  The last is not an answer about the source. A refusal is this source's and is remembered; a
 *  renderer that did not load is the worker's own trouble — a chunk a redeploy moved, a
 *  connection that dropped — so the page keeps nothing for it and the next body asks again
 *  (`mermaid-diagrams.ts`). */
export type DrawnSource =
  | { markup: string; renderer: MermaidRendererName }
  | { error: string | null }
  | { rendererUnavailable: true };

/** What the worker posts: that it is ready, the phase of the job it holds, or the answer to one
 *  request, under the id the request carried (`WorkerReply`). */
export type MermaidReply = WorkerReply<DrawnSource>;

/** Posts the phase a job has reached, under that job's id. The page arms a bound on each one
 *  (`worker-queue.ts`); the unit suite's fake worker posts its own, so it passes none. */
export type ReportPhase = (phase: JobPhase) => void;

/** The headers beautiful-mermaid has a layout for, lower-cased: a header that begins with one of
 *  these goes to it, and every other source to Mermaid. `statediagram` covers `stateDiagram-v2`
 *  and `xychart` covers `xychart-beta`, as each type's own header is written both ways. */
const FAST_PATH_HEADERS = [
  "graph",
  "flowchart",
  "statediagram",
  "sequencediagram",
  "classdiagram",
  "erdiagram",
  "xychart",
];

/** The one header that begins with a fast-path word and is not one: ELK is a layout engine of
 *  Mermaid's, which beautiful-mermaid has nothing of. */
const ELK_FLOWCHART_HEADER = "flowchart-elk";

/** The source's header: its first line that is neither inside a `---` front-matter block nor a
 *  `%%` comment, trimmed and lower-cased, which is the line both renderers read the type from.
 *  Empty for a source that holds no such line.
 *
 *  Front matter opens on the source's very first line and nowhere else, which is where mermaid
 *  takes it: `matchFrontMatter` allows an indent on that line and nothing at all before it --
 *  not an empty line, not a `%%` comment -- and reads a source whose block never closes as
 *  holding no front matter. A `---` under a comment is the diagram's own text to both of them,
 *  so this and mermaid route such a source the same way. */
function headerOf(source: string): string {
  const lines = source.split("\n");
  let at = 0;
  if (lines[0]?.trim() === "---") {
    const closes = lines.findIndex((line, index) => index > 0 && line.trim() === "---");
    if (closes !== -1) at = closes + 1;
  }
  for (; at < lines.length; at += 1) {
    const line = (lines[at] ?? "").trim().toLowerCase();
    if (line === "" || line.startsWith("%%")) continue;
    return line;
  }
  return "";
}

/** Whether beautiful-mermaid is asked for this source first. Its refusal is not final: a source
 *  it refuses is drawn by Mermaid instead (`answerMermaidRequest`). */
function drawsOnTheFastPath(source: string): boolean {
  const header = headerOf(source);
  if (header.startsWith(ELK_FLOWCHART_HEADER)) return false;
  return FAST_PATH_HEADERS.some((fast) => header.startsWith(fast));
}

/**
 * The status line a refused source shows is one line of at most `STATUS_LINE_MAX_CHARS`
 * characters holding at most `SOURCE_CHARS_IN_REASON` characters of the source itself
 * (requirement R3), and the page writes it as `${STATUS_PREFIX}${reason}` (`mermaid-diagrams.ts`,
 * `refusal`), from the same two constants (`mermaid-status.ts`). The arithmetic this module adds
 * to them: the prefix is 34 characters, which leaves a reason 200 − 34 = 166; the one reason
 * that quotes the source spends 42 of those on its own words and the quotes around it and 120 on
 * the source (34 + 42 + 120 = 196, and 197 where the ellipsis of a cut source is on it), and
 * every other reason is cut to 166 (34 + 166 = 200).
 *
 * Mermaid's own messages hold to neither rule, which is why a reason is built here rather than
 * passed through: `UnknownDiagramError` repeats the whole source, and a parse error's message
 * runs to several lines, an excerpt of the source and a ruler pointing into it among them.
 */
const REASON_MAX_CHARS = STATUS_LINE_MAX_CHARS - STATUS_PREFIX.length;
const SOURCE_CHARS_IN_REASON = 120;

/** mermaid's error for a source whose first line names no type it draws, by the `name` its class
 *  sets, so this module reads it without importing mermaid. */
const UNKNOWN_DIAGRAM_ERROR = "UnknownDiagramError";

/** The words a reason for such a source is built from: the source itself says more than any
 *  wording could, and no renderer's own guess at a type is named, so the line never names a
 *  flowchart header. */
const UNDRAWN_SOURCE_REASON = "no renderer draws a diagram that begins";

/** The ruler a jison parse error draws under the token it failed on (`------^`), and nothing
 *  else: it means nothing once its message is one line, and the line above it is the excerpt it
 *  points into, which is source text from wherever the error is rather than the source's start. */
const PARSE_ERROR_RULER = /^[\s-]*\^[\s^-]*$/;

/** The source as a reason quotes it: one line, its first `SOURCE_CHARS_IN_REASON` characters,
 *  with an ellipsis where the rest was cut. */
function quotedSource(source: string): string {
  const oneLine = source.replace(/\s+/g, " ").trim();
  return oneLine.length > SOURCE_CHARS_IN_REASON
    ? `${oneLine.slice(0, SOURCE_CHARS_IN_REASON)}…`
    : oneLine;
}

/** A renderer's message as one line of at most `REASON_MAX_CHARS` characters: the ruler of a
 *  parse error and the excerpt it points into are dropped, every other line is joined by a
 *  space, and what is left is cut to fit the status line. */
function oneLineReason(message: string): string {
  const lines = message.split("\n");
  const reason = lines
    .filter(
      (line, at) => !PARSE_ERROR_RULER.test(line) && !PARSE_ERROR_RULER.test(lines[at + 1] ?? "")
    )
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
  return reason.length > REASON_MAX_CHARS ? `${reason.slice(0, REASON_MAX_CHARS - 1)}…` : reason;
}

/** Why `source` was refused, in the one line the page shows after `STATUS_PREFIX`; `null` where
 *  the thrown value was no `Error` or its message says nothing, which the page shows as the
 *  refusal alone. */
function reasonFor(error: unknown, source: string): string | null {
  if (!(error instanceof Error)) return null;
  if (error.name === UNKNOWN_DIAGRAM_ERROR) {
    return `${UNDRAWN_SOURCE_REASON} "${quotedSource(source)}"`;
  }
  const reason = oneLineReason(error.message);
  return reason === "" ? null : reason;
}

/** The global's own prototype, where a browser worker's `self` is shadowed for a
 *  beautiful-mermaid call; `null` outside a worker, where the prototype is shared and `self` is
 *  an own property of the global that beautiful-mermaid deletes for itself. */
const workerPrototype: object | null =
  "WorkerGlobalScope" in globalThis ? Object.getPrototypeOf(globalThis) : null;

// `self` reads as undefined while beautiful-mermaid draws. It builds ELK's bundled engine on its
// first flowchart, and that engine, finding `self` and no `document`, takes this worker for an
// ELK worker of its own: it claims the worker's `onmessage` and leaves its in-thread constructor
// unexported, so every flowchart is refused (`En is not a constructor`). beautiful-mermaid
// deletes `globalThis.self` around that build, which hides it under Bun and Node but not in a
// browser, where `self` is an accessor of `WorkerGlobalScope.prototype` that a delete on the
// global does not reach. So an accessor reading undefined shadows it on the global's own
// prototype (`DedicatedWorkerGlobalScope.prototype`, which has none) for the call.
const hiddenSelf: PropertyDescriptor = {
  configurable: true,
  get: () => undefined,
  set: () => {
    // beautiful-mermaid writes the value it read back once ELK is built (`if (hadSelf) g.self =
    // origSelf;`, beautiful-mermaid 1.1.3 `dist/index.js:5951-5957`), and that value is the
    // `undefined` the getter above returned, so there is nothing to keep. The setter must
    // still exist: the bundle is a module, so strict code, where an assignment to an accessor
    // with no setter throws a `TypeError`, which would refuse every flowchart.
  },
};

/** The globals the DOM shim defined, once Mermaid has been loaded in this worker; empty before
 *  that, when the worker has no DOM to take away. */
let shimGlobals: readonly string[] = [];

/**
 * Runs one synchronous beautiful-mermaid call in the environment beautiful-mermaid was proven
 * in: no `self` (see `hiddenSelf`), and none of the globals the DOM shim defines for Mermaid
 * (`mermaidShimGlobals`, `document` and `window` among them; the list is read from the shim
 * rather than counted here, because which names linkedom hands it differs by host). The
 * worker is shared, so a page holding a timeline above a flowchart would otherwise run
 * beautiful-mermaid in a worker that has a `document` — a combination this repository records as
 * failing (`packages/proof-editor/tests/mermaid-svg.test.ts:6-8`: "ELK […] fails to build while a
 * global `document` exists", and the lines 30-31 of
 * `docs/solutions/testing/a-library-that-sniffs-self-behaves-differently-in-a-browser-worker-than-under-bun-pr2311.md`)
 * and that nobody has measured the other way round.
 *
 * The bracket never spans an `await`: Mermaid's render is a promise, and holding these away
 * across it would draw against no DOM at all. Only this call is bracketed; the dynamic imports
 * and the Mermaid render run outside it.
 */
function withoutADom<Drawn>(draw: () => Drawn): Drawn {
  if (workerPrototype !== null) Object.defineProperty(workerPrototype, "self", hiddenSelf);
  const taken = new Map<string, PropertyDescriptor>();
  for (const name of shimGlobals) {
    const held = Object.getOwnPropertyDescriptor(globalThis, name);
    if (held === undefined) continue;
    taken.set(name, held);
    Reflect.deleteProperty(globalThis, name);
  }
  try {
    return draw();
  } finally {
    if (workerPrototype !== null) Reflect.deleteProperty(workerPrototype, "self");
    for (const [name, held] of taken) Object.defineProperty(globalThis, name, held);
  }
}

/** beautiful-mermaid's markup for `source`, or `null` where it refuses it, whatever the refusal:
 *  its grammar is narrower than its headers suggest (it draws a flowchart only for
 *  `graph|flowchart TD|TB|LR|BT|RL` on a line of its own, so `graph TD;` is refused), and its
 *  validator reads the source's raw first line, so a `%%` comment above `sequenceDiagram` sends
 *  that source into its flowchart parser. Each of those is an ordinary source Mermaid draws, so
 *  a refusal here is not the reader's answer: it is the fall-through to Mermaid, whose own
 *  refusal, where it comes to one, names no flowchart header. */
function beautifulMermaidMarkup(source: string): string | null {
  return withoutADom(() => {
    try {
      return renderMermaidMarkup(source);
    } catch {
      return null;
    }
  });
}

/** Mermaid's renderer, loaded on the first source that needs it: 31 chunks and 4,367,973 bytes
 *  cold, which is why the page gives a job that is loading a bound of its own
 *  (`RENDERER_LOAD_LIMIT_MS`). The renderer module owns the DOM shim and answers the globals it
 *  defined, which is what the bracket above takes away. */
async function mermaidRenderer(report?: ReportPhase): Promise<MermaidRenderer> {
  report?.("loading");
  // Dynamic, not static: a static import would put Mermaid, d3 and cytoscape in the worker's own
  // chunk, which every reader of a flowchart would then download.
  const { loadMermaidRenderer, mermaidShimGlobals } = await import("./mermaid-upstream");
  shimGlobals = mermaidShimGlobals();
  // Not the same report twice over: the chunks above were one download and Mermaid's own are
  // another, so this re-arms the load bound before the renderer module's `import("mermaid")`
  // rather than leaving that download to run out what is left of the first one's.
  report?.("loading");
  return loadMermaidRenderer();
}

/**
 * Draws one request's source: beautiful-mermaid where it has a layout for that source's header,
 * Mermaid for every other source and for every source beautiful-mermaid refuses. It answers
 * every source, a refusal included, and `report` tells the page which phase of that work it has
 * reached. Exported for the unit tests, whose stand-in `Worker` answers through it on the test's
 * own thread.
 *
 * Three answers, which the page keeps apart (`mermaid-diagrams.ts`):
 *
 * - the markup and the renderer that drew it;
 * - `{ error }`, a refusal of this source that this source will get again -- a parse error, a
 *   type no renderer draws, a source past the size cap -- which the page shows as its status
 *   line and remembers;
 * - `{ rendererUnavailable }`, a renderer that could not be loaded at all, which says nothing
 *   about the source: the page stores nothing for it and asks again.
 *
 * Everything Mermaid downloads happens while the job is under the load bound: the renderer
 * module, Mermaid itself, then `prepare`, which fetches this source's type definition chunk and
 * its layout engine (`mermaid-upstream.ts`). `drawing` is posted when what is left is the
 * layout, which is what `RENDER_TIME_LIMIT_MS` is a bound on.
 */
export async function answerMermaidRequest(
  { id, source }: MermaidRequest,
  report?: ReportPhase
): Promise<MermaidReply> {
  if (drawsOnTheFastPath(source)) {
    report?.("drawing");
    const markup = beautifulMermaidMarkup(source);
    if (markup !== null) return { id, markup, renderer: "beautiful-mermaid" };
  }
  let renderer: MermaidRenderer;
  try {
    renderer = await mermaidRenderer(report);
  } catch (error) {
    // Not a refusal of this source: nothing was read of it. The page leaves the block as its
    // own code and remembers nothing, so the next body asks again.
    console.error("The Mermaid renderer did not load in this worker", error);
    return { id, rendererUnavailable: true };
  }
  try {
    await renderer.prepare(source, () => report?.("loading"));
    report?.("drawing");
    return { id, markup: await renderer.draw(source), renderer: "mermaid" };
  } catch (error) {
    return { id, error: reasonFor(error, source) };
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
  // One job at a time, in the order they arrived: drawing is asynchronous now (Mermaid's render
  // is a promise, and its chunks load inside a job), so each job waits on the one before it
  // rather than racing it, and the answers keep the order the requests came in. The chain never
  // rejects: `answerMermaidRequest` answers every source, a refusal included.
  let answering: Promise<void> = Promise.resolve();
  scope.addEventListener("message", (event) => {
    const request = event.data;
    answering = answering.then(async () => {
      const reply = await answerMermaidRequest(request, (phase) => {
        scope.postMessage({ id: request.id, phase });
      });
      scope.postMessage(reply);
    });
  });
  scope.postMessage({ ready: true });
}
