/**
 * Draws each fenced `mermaid` code block of a rendered Markdown body (`MarkdownBody`'s block
 * form; a one-line preview and the inline variant write no `<pre>`), and each mermaid block of a
 * document, with one renderer: `renderMermaidMarkup` from `@legion/proof-editor/mermaid-render`
 * (upstream's validation and renderer), run in a Web Worker, its markup turned into an element by
 * `mermaidSvgElement` from `@legion/proof-editor/mermaid` (a stricter sanitizer and the editor's
 * accessible name). A body's block becomes a `figure` holding the diagram, then (only for a source
 * the renderer refuses) a status line naming the reason, then the original code block inside a
 * `Mermaid source` disclosure, closed once the diagram drew and open on a refusal so the source is
 * readable at once.
 *
 * A document's blocks draw here too, through the editor's drawer: `features/doc/editor.ts` gives
 * `createProofEditor` `requestMermaidDrawing` as its `drawMermaid`, so the live editor, a
 * read-only document and a past version share this queue, its time limit and its cache with every
 * body on the page. The editor's code-block view asks once when a block opens and again 300 ms
 * after the last edit of its source, and keeps its last drawing until the answer replaces it.
 * `requestMermaidDrawing` draws a source at most once and never after its cancel runs: inside the
 * call when it is cached, later when the worker answers. When no worker can run (it failed to
 * start or load) the draw gets `unavailable` instead, inside the call for a source asked for while
 * the worker is failed and at the failure for a source waiting on it, so a body's block stays code
 * and an editor's block drops any drawing of an earlier source and shows its source.
 * `mermaidDrawsSettled` resolves once no source is waiting, so a focus in a document (a block
 * link, a comment link's quote, a reader's click to a block, a past version's highlighted mark)
 * can run again after the diagrams above its target have drawn and moved it.
 *
 * Both mount the SVG with `showMermaidSvg` in an open shadow root of the preview element, so the
 * diagram's own `<style>` styles that diagram alone rather than every SVG and `<text>` on the
 * page, and its marker ids (`url(#arrowhead…)`) resolve inside that root: two diagrams on a page,
 * or one body shown in two places at once, never point at each other's markers. The accessibility
 * tree and Playwright's locators reach into an open root; the disclosure stays in the light DOM.
 *
 * A mermaid block whose fence the parser still reads as open is still being written (an unclosed
 * fence runs to the end of the document), so it stays code: the live view re-parses a turn on
 * every frame while a model streams it, and drawing each frame would run the renderer and flash a
 * refusal (`Dangling Mermaid edge`) until the fence closes. Every other block of the same body
 * still draws. Open is the parser's answer, never a reading of the source text: the body is parsed
 * again with a probe line after it, and the fence is open exactly when the probe lands inside the
 * block (`endsInOpenMermaidFence`).
 *
 * The renderer (beautiful-mermaid and ELK) runs in one shared worker (`mermaid-render.worker.ts`),
 * created only once a body or a document holds a mermaid block not drawn before: ELK's layout time
 * grows faster than the diagram (a flowchart of 400 nodes takes seconds), any comment, ask or
 * message reaches readers who did not choose it, and a document is where diagrams get edited, so
 * no source may hold the page's main thread. The worker draws one source at a time, in the order
 * they were asked for; a source it has not answered `RENDER_TIME_LIMIT_MS` after it started on it
 * is refused (`Unable to render Mermaid diagram: it did not finish within 5 seconds`), and the
 * worker is terminated, which stops that layout, and a fresh one takes the sources still waiting.
 * A source no one waits for any more (every body that asked for it re-painted or unmounted, every
 * editor block that asked for it was edited or removed) is dropped and never sent; if the worker
 * is drawing it, the worker is terminated the same way, nothing is cached for it (a reader who
 * comes back asks again), and a fresh one takes the sources still waiting, so a reader who leaves
 * a page of slow diagrams does not wait for them on the next. That check runs on a microtask after
 * the cancel: React runs a layout effect's cleanup and the next run of it in one commit, so a body
 * re-painted with the same block (the live view, every frame while text after a finished diagram
 * streams) asks again first, and its source keeps its place and the draw in flight. A worker that
 * fails to start or load leaves every block as code (each waiting draw and each later request
 * gets `unavailable`), logs once, and none is started again until the window's next `online`
 * event.
 *
 * Drawn results are cached by source (the sanitized SVG, cloned per use, or the refusal), the
 * newest 32. A cached source draws in the layout effect that painted its body, so a body re-painted
 * while text after a finished diagram streams draws it again at once; an uncached one shows as
 * code and is replaced when the worker answers, unless the body re-painted or unmounted first, so
 * a diagram's first draw moves the content below it once.
 */
import {
  type MermaidDrawing,
  mermaidSvgElement,
  showMermaidSvg,
} from "@legion/proof-editor/mermaid";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import { dangerText } from "../../theme/classes";
import type { MermaidReply, MermaidRequest } from "./mermaid-render.worker";

/** Draws one body block, or one editor block, once its source's answer arrives. */
type LateDraw = (drawn: MermaidDrawing) => void;
/** Parses a source with the engine and soft-break policy that parsed the body; `undefined` for a
 *  source the engine refuses. */
type Reparse = (source: string) => ProseMirrorNode | undefined;

/** A started worker. */
interface Renderer {
  worker: Worker;
  /** Its imports have loaded, so it starts on a request as soon as it gets one. */
  ready: boolean;
  /** The request it is drawing, and the timer that refuses it at the time limit. */
  current: { id: number; source: string; timer: number } | undefined;
}

/** How long the worker may draw one source before it is refused and the worker terminated. On a
 *  2-CPU issue pod, in Chromium's worker a flowchart of 50 edges draws in under 0.4 s and one of
 *  100 nodes with 2 edges each in about 1.2 s; at 200 such nodes it takes about 3 s (5 s in WebKit
 *  and Firefox), and at 400 about 10 s; a faster machine draws more within it. The browser rows
 *  of the limit hold the worker (`e2e/mermaid.e2e.ts`), so they depend on no machine's speed. */
const RENDER_TIME_LIMIT_MS = 5000;
/** How many drawn sources the cache keeps; the oldest insertion goes first. */
const CACHE_LIMIT = 32;
/** The line the open-fence check parses after a body. No author writes it: the words are wrapped
 *  in invisible separators (U+2063), so a code block that ends in it took in the probe, never an
 *  author's own text that happens to read the same. */
const OPEN_FENCE_PROBE = "\u2063dispatch-open-fence-probe\u2063";
/** The containers a line opens: any block quote markers and list markers, then its indent. */
const CONTAINER_LEAD = /^(?:[ \t]*(?:>|(?:[-+*]|\d{1,9}[.)])(?=[ \t]|$)))*[ \t]*/;
/** The one rule the diagram needs beside its own sheet: its natural width, up to the body's. */
const PREVIEW_STYLE = "svg{max-width:100%;height:auto;display:block}";

/** What the cache holds: `fail()` answers a waiting draw with `{ unavailable: true }` and caches
 *  nothing, so no source is ever remembered as unavailable and the next body asks again. */
type CachedDrawing = Exclude<MermaidDrawing, { unavailable: true }>;

const drawings = new Map<string, CachedDrawing>();
/** Each source asked for and not answered yet, oldest first, with the draws its answer feeds in
 *  the order they asked. The first is the one the worker is drawing or starts on next. A source
 *  whose draws were all cancelled stays, with none, until the cancel's microtask drops it. */
const requests = new Map<string, LateDraw[]>();
/** Resolves each pending `mermaidDrawsSettled` promise; run once `requests` is empty. */
const settledWaiters: (() => void)[] = [];
let renderer: Renderer | undefined;
/** A worker failed to start or load; none is started until the window's next `online` event. */
let failed = false;
/** A cancel has queued `dropAbandoned` and it has not run yet. */
let dropQueued = false;
let lastRequestId = 0;
/** `RENDER_TIME_LIMIT_MS`, unless a test shortened it through `resetMermaidDiagrams`. */
let timeLimitMs = RENDER_TIME_LIMIT_MS;

function noop(): void {}

function recover(): void {
  failed = false;
}

/** Upstream's `isMermaidCodeBlockLanguage` rule, restated so the page imports none of the
 *  renderer. */
function isMermaidLanguage(language: unknown): boolean {
  return typeof language === "string" && language.trim().toLowerCase() === "mermaid";
}

/** The document's last leaf block: its last block, followed down through containers (a quote, a
 *  list, a list item) to the block that holds text or nothing. */
function lastLeafBlock(parsed: ProseMirrorNode): ProseMirrorNode | null {
  let node = parsed.lastChild;
  while (node !== null && !node.isTextblock && node.lastChild !== null) {
    node = node.lastChild;
  }
  return node;
}

/**
 * Whether the document ends in a mermaid block whose fence is still open. Only the parser can
 * say, so the source text is never read for a closing fence: a link reference definition after a
 * closed fence is no block, so the diagram is the document's last block while the source's last
 * line is the definition; and CommonMark closes a fence only with a line of the opener's
 * character at least as long as the opener (three backticks under four, or tildes under
 * backticks, leave it open). So the body is parsed again with `OPEN_FENCE_PROBE` on a line after
 * it: an open fence runs to the end of the document and takes the probe in as code, a closed one
 * leaves it outside.
 *
 * The probe line sits in the containers the source's last non-blank line is in: that line's block
 * quote markers kept and its list markers turned into spaces (a list item's content indent), with
 * no blank line before it. A bare line, or a blank one, would end an open fence's quote or list
 * item, and the fence with it, so a quoted diagram still being written would read as closed.
 * Only a body whose last leaf block is a mermaid block is parsed again; a re-parse the engine
 * refuses counts as closed, so the diagram draws.
 */
function endsInOpenMermaidFence(
  parsed: ProseMirrorNode,
  markdown: string,
  reparse: Reparse
): boolean {
  const last = lastLeafBlock(parsed);
  if (last?.type.name !== "code_block" || !isMermaidLanguage(last.attrs.language)) return false;
  const source = markdown.trimEnd();
  // A line ends at LF, CR or CRLF, as micromark reads it; a CRLF's last LF follows its CR.
  const lastLine = source.slice(Math.max(source.lastIndexOf("\n"), source.lastIndexOf("\r")) + 1);
  const lead = (CONTAINER_LEAD.exec(lastLine)?.[0] ?? "").replace(/[^\s>]/g, " ");
  const newline = markdown.endsWith("\n") ? "" : "\n";
  const probed = reparse(`${markdown}${newline}${lead}${OPEN_FENCE_PROBE}`);
  const end = probed === undefined ? null : lastLeafBlock(probed);
  return (
    end?.type.name === "code_block" &&
    isMermaidLanguage(end.attrs.language) &&
    end.textContent.endsWith(OPEN_FENCE_PROBE)
  );
}

function refusal(reason: string | null): CachedDrawing {
  return {
    error:
      reason === null
        ? "Unable to render Mermaid diagram."
        : `Unable to render Mermaid diagram: ${reason}`,
  };
}

/** Resolves every `mermaidDrawsSettled` promise on the microtask after no source is waiting, so
 *  each draw that settled the last one has run first; a source asked for before that microtask
 *  keeps them waiting. Run after every change that can empty `requests`. */
function noteSettled(): void {
  if (requests.size > 0 || settledWaiters.length === 0) return;
  queueMicrotask(() => {
    if (requests.size > 0) return;
    for (const resolve of settledWaiters.splice(0)) resolve();
  });
}

/** Caches `drawn` for `source` and runs the draws that waited on it. */
function settle(source: string, drawn: CachedDrawing): void {
  if (drawings.size >= CACHE_LIMIT) {
    const oldest = drawings.keys().next();
    if (oldest.done !== true) drawings.delete(oldest.value);
  }
  drawings.set(source, drawn);
  const draws = requests.get(source) ?? [];
  requests.delete(source);
  for (const draw of draws) draw(drawn);
  noteSettled();
}

/** Terminates the worker, which stops a layout it is running, and forgets it and the request it
 *  was drawing, so an answer it posted before is dropped; the next request starts a fresh one. */
function stopRenderer(): void {
  if (renderer === undefined) return;
  window.clearTimeout(renderer.current?.timer);
  renderer.current = undefined;
  renderer.worker.terminate();
  renderer = undefined;
}

/** Shows every waiting block, and every later one, as code until the window's next `online`: each
 *  waiting draw runs with `unavailable`, so an editor block drops a drawing of an earlier source. */
function fail(error: unknown): void {
  failed = true;
  stopRenderer();
  const waiting = [...requests.values()];
  requests.clear();
  for (const draws of waiting) {
    for (const draw of draws) draw({ unavailable: true });
  }
  noteSettled();
  window.addEventListener("online", recover, { once: true });
  console.error("Showing Mermaid source as code, the diagram renderer is unavailable", error);
}

function answer(from: Renderer, reply: MermaidReply): void {
  if ("ready" in reply) {
    from.ready = true;
    sendNext();
    return;
  }
  const current = from.current;
  // An answer to a request the time limit already refused (it was posted before the worker was
  // terminated) is dropped.
  if (current?.id !== reply.id) return;
  window.clearTimeout(current.timer);
  from.current = undefined;
  let drawn: CachedDrawing;
  if ("markup" in reply) {
    try {
      drawn = { svg: mermaidSvgElement(reply.markup, document) };
    } catch (error) {
      drawn = refusal(error instanceof Error ? error.message : null);
    }
  } else {
    drawn = refusal(reply.error);
  }
  settle(current.source, drawn);
  sendNext();
}

/** Refuses `source`, which the worker has not answered within the time limit, and stops the
 *  worker, which stops its layout; the next request starts a fresh one. The timer that calls this
 *  was set by `sendNext` for the current renderer, and every path that ends that request or drops
 *  that renderer clears the timer first (`answer` on the reply, `stopRenderer` on every other), so
 *  the renderer it stops is always the one it timed. */
function refuseLate(source: string): void {
  stopRenderer();
  settle(source, refusal(`it did not finish within ${timeLimitMs / 1000} seconds`));
  sendNext();
}

function startRenderer(): void {
  let worker: Worker;
  try {
    worker = new Worker(new URL("./mermaid-render.worker.ts", import.meta.url), {
      type: "module",
    });
  } catch (error) {
    fail(error);
    return;
  }
  const started: Renderer = { worker, ready: false, current: undefined };
  renderer = started;
  worker.addEventListener("message", (event: MessageEvent<MermaidReply>) => {
    answer(started, event.data);
  });
  worker.addEventListener("error", fail);
}

/** Hands the oldest waiting source to the worker, starting one when none runs, unless the worker
 *  is still loading or busy. */
function sendNext(): void {
  const next = requests.keys().next();
  if (failed || next.done === true) return;
  if (renderer === undefined) {
    startRenderer();
    return;
  }
  if (!renderer.ready || renderer.current !== undefined) return;
  const source = next.value;
  const id = ++lastRequestId;
  const timer = window.setTimeout(() => refuseLate(source), timeLimitMs);
  renderer.current = { id, source, timer };
  renderer.worker.postMessage({ id, source } satisfies MermaidRequest);
}

/** Drops every source no draw waits for any more, so it is never sent; the worker drawing one is
 *  stopped and nothing is cached for it, and the sources still waiting go to a fresh worker. */
function dropAbandoned(): void {
  dropQueued = false;
  for (const [source, draws] of requests) {
    if (draws.length > 0) continue;
    requests.delete(source);
    if (renderer?.current?.source === source) stopRenderer();
  }
  noteSettled();
  sendNext();
}

/** Replaces `pre` with its drawn figure, moving `pre` itself into the source disclosure; with no
 *  renderer (`unavailable`) it leaves `pre` as code. */
function drawBlock(pre: HTMLPreElement, drawn: MermaidDrawing): void {
  if ("unavailable" in drawn) return;
  const document = pre.ownerDocument;
  const figure = document.createElement("figure");
  figure.className = "dispatch-mermaid";
  const details = document.createElement("details");
  details.className = "dispatch-mermaid-source";
  const summary = document.createElement("summary");
  summary.textContent = "Mermaid source";
  if ("svg" in drawn) {
    const preview = document.createElement("div");
    preview.className = "dispatch-mermaid-preview";
    showMermaidSvg(preview, drawn.svg, PREVIEW_STYLE);
    figure.append(preview);
  } else {
    const status = document.createElement("p");
    status.setAttribute("role", "status");
    status.className = dangerText;
    status.textContent = drawn.error;
    figure.append(status);
    details.open = true;
  }
  pre.replaceWith(figure);
  details.append(summary, pre);
  figure.append(details);
}

/**
 * The editor's `MermaidDrawer` (`@legion/proof-editor/mermaid`), and the one way any block here
 * asks for a drawing. A cached source calls `draw` now, inside the call; so does an uncached one
 * while the worker is failed, with `unavailable`, and it is not queued. Any other joins the queue
 * and `draw` runs when the worker answers, or with `unavailable` if the worker fails first.
 * Returns the cancel: it takes `draw` off the source's list, so a cancelled draw never runs and
 * holds none of its caller's DOM while the worker draws, then queues `dropAbandoned` on a
 * microtask, after any re-paint in the same commit has asked again. A source drawn or refused
 * already, or never queued, has a cancel that does nothing.
 */
export function requestMermaidDrawing(source: string, draw: LateDraw): () => void {
  // A code block keeps its source's line ends, and the renderer splits lines at LF alone, so a
  // source written with lone CRs reaches it as LF; CRLF too, so one diagram is one cache entry
  // whoever asks for it.
  const normalised = source.replace(/\r\n?/g, "\n");
  const drawn = drawings.get(normalised);
  if (drawn !== undefined) {
    draw(drawn);
    return noop;
  }
  if (failed) {
    draw({ unavailable: true });
    return noop;
  }
  const draws = requests.get(normalised);
  if (draws === undefined) requests.set(normalised, [draw]);
  else draws.push(draw);
  sendNext();
  let cancelled = false;
  return () => {
    if (cancelled) return;
    cancelled = true;
    const waiting = requests.get(normalised);
    const at = waiting?.indexOf(draw) ?? -1;
    // A draw already run, refused or cleared left no list behind to empty, so nothing to drop.
    if (waiting === undefined || at < 0) return;
    waiting.splice(at, 1);
    if (dropQueued) return;
    dropQueued = true;
    queueMicrotask(dropAbandoned);
  };
}

/**
 * Draws every mermaid code block `element` holds (painted from `parsed`, the parse of
 * `markdown`) but one whose fence is still open, each through `requestMermaidDrawing`. `reparse`
 * parses with the engine and soft-break policy that produced `parsed`; the open-fence check runs
 * it once, only when the body ends in a mermaid block. A block whose source is cached draws now;
 * every other one when the worker answers, in the order the bodies were painted. Returns the
 * cancel for those late draws, which runs each block's own cancel.
 */
export function drawMermaidDiagrams(
  element: HTMLElement,
  parsed: ProseMirrorNode,
  markdown: string,
  reparse: Reparse
): () => void {
  const blocks = Array.from(element.querySelectorAll<HTMLPreElement>("pre[data-language]")).filter(
    (pre) => isMermaidLanguage(pre.getAttribute("data-language"))
  );
  if (blocks.length > 0 && endsInOpenMermaidFence(parsed, markdown, reparse)) blocks.pop();
  const cancels = blocks.map((pre) =>
    requestMermaidDrawing(pre.textContent ?? "", (drawn) => drawBlock(pre, drawn))
  );
  return () => {
    for (const cancel of cancels) cancel();
  };
}

/**
 * Resolves on the microtask after no source is waiting for the worker: every block that asked
 * has drawn, been refused or been cancelled, or a failure or a reset cleared the queue. With
 * none waiting it resolves at once. A focus in a document runs again then, after the diagrams
 * above its target have drawn and moved it (`features/doc/refocus-after-diagrams.ts`).
 */
export function mermaidDrawsSettled(): Promise<void> {
  if (requests.size === 0) return Promise.resolve();
  const { promise, resolve } = Promise.withResolvers<void>();
  settledWaiters.push(resolve);
  return promise;
}

/** Whether any source is waiting for the worker, so a diagram may still draw and move what is
 *  below it. A focus in a document runs again only then. */
export function mermaidDrawsWaiting(): boolean {
  return requests.size > 0;
}

/**
 * Terminates the worker and forgets it, every cached drawing, every waiting draw and a failure,
 * so a test starts from a page that has drawn nothing. `timeLimitMs` replaces
 * `RENDER_TIME_LIMIT_MS` until the next reset, so a test of the time limit need not wait 5 s.
 */
export function resetMermaidDiagrams(limitMs = RENDER_TIME_LIMIT_MS): void {
  stopRenderer();
  failed = false;
  window.removeEventListener("online", recover);
  drawings.clear();
  requests.clear();
  noteSettled();
  timeLimitMs = limitMs;
}
