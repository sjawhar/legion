/**
 * Draws each fenced `mermaid` code block of a rendered Markdown body (`MarkdownBody`'s block
 * form; a one-line preview and the inline variant write no `<pre>`) as the diagram the document
 * editor draws: `renderMermaidMarkup` from `@legion/proof-editor/mermaid-render` (upstream's
 * validation and renderer), run in a Web Worker, its markup turned into an element by
 * `mermaidSvgElement` from `@legion/proof-editor/mermaid` (a stricter sanitizer and the editor's
 * accessible name). The block becomes a `figure` holding the diagram, then (only for a source the
 * renderer refuses) a status line naming the reason, then the original code block inside a
 * `Mermaid source` disclosure, closed once the diagram drew and open on a refusal so the source is
 * readable at once.
 *
 * The SVG goes in an open shadow root of the preview element, so the diagram's own `<style>`
 * styles that diagram alone rather than every SVG and `<text>` on the page, and its marker ids
 * (`url(#arrowhead…)`) resolve inside that root: two diagrams on a page, or one body shown in two
 * places at once, never point at each other's markers. The accessibility tree and Playwright's
 * locators reach into an open root; the disclosure stays in the light DOM.
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
 * created only once a body holds a mermaid block not drawn before: ELK's layout time grows faster
 * than the diagram (a flowchart of 400 nodes takes seconds), and any comment, ask or message
 * reaches readers who did not choose it, so no source may hold the page's main thread. The worker
 * draws one source at a time, in the order bodies asked; a source it has not answered
 * `RENDER_TIME_LIMIT_MS` after it started on it is refused (`Unable to render Mermaid diagram: it
 * did not finish within 5 seconds`), and the worker is terminated, which stops that layout, and a
 * fresh one takes the sources still waiting. A worker that fails to start or load leaves every
 * block as code, logs once, and none is started again until the window's next `online` event.
 *
 * Drawn results are cached by source (the sanitized SVG, cloned per use, or the refusal), the
 * newest 32. A cached source draws in the layout effect that painted its body, so a body re-painted
 * while text after a finished diagram streams draws it again at once; an uncached one shows as
 * code and is replaced when the worker answers, unless the body re-painted or unmounted first, so
 * a diagram's first draw moves the content below it once.
 */
import { mermaidSvgElement } from "@legion/proof-editor/mermaid";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import { dangerText } from "../../theme/classes";
import type { MermaidReply, MermaidRequest } from "./mermaid-render.worker";

/** A drawn source: its sanitized SVG, or the status line a refusal shows. */
type Drawing = { svg: SVGSVGElement } | { error: string };
/** Draws one block once its source's answer arrives. */
type LateDraw = (drawn: Drawing) => void;
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

/** How long the worker may draw one source before it is refused and the worker terminated. In
 *  Chromium's worker a flowchart of 50 edges draws in under 0.4 s and one of 100 nodes with 2
 *  edges each in about 1.2 s; at 200 such nodes it takes about 3 s (5 s in WebKit and Firefox),
 *  and at 400 about 10 s. */
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

const drawings = new Map<string, Drawing>();
/** Each source asked for and not answered yet, oldest first, with the draws its answer feeds in
 *  paint order. The first is the one the worker is drawing or starts on next. */
const requests = new Map<string, LateDraw[]>();
let renderer: Renderer | undefined;
/** A worker failed to start or load; none is started until the window's next `online` event. */
let failed = false;
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
  const lastLine = source.slice(source.lastIndexOf("\n") + 1);
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

function refusal(reason: string | null): Drawing {
  return {
    error:
      reason === null
        ? "Unable to render Mermaid diagram."
        : `Unable to render Mermaid diagram: ${reason}`,
  };
}

/** Caches `drawn` for `source` and runs the draws that waited on it. */
function settle(source: string, drawn: Drawing): void {
  if (drawings.size >= CACHE_LIMIT) {
    const oldest = drawings.keys().next();
    if (oldest.done !== true) drawings.delete(oldest.value);
  }
  drawings.set(source, drawn);
  const draws = requests.get(source) ?? [];
  requests.delete(source);
  for (const draw of draws) draw(drawn);
}

/** Leaves every block waiting, and every later one, as code until the window's next `online`. */
function fail(error: unknown): void {
  failed = true;
  if (renderer !== undefined) {
    window.clearTimeout(renderer.current?.timer);
    renderer.worker.terminate();
    renderer = undefined;
  }
  requests.clear();
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
  let drawn: Drawing;
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

/** Refuses the source `from` has not answered in time, and terminates it, which stops its
 *  layout; the next request starts a fresh worker. */
function refuseLate(from: Renderer, source: string): void {
  from.worker.terminate();
  from.current = undefined;
  renderer = undefined;
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
  const from = renderer;
  const source = next.value;
  const id = ++lastRequestId;
  const timer = window.setTimeout(() => refuseLate(from, source), timeLimitMs);
  from.current = { id, source, timer };
  from.worker.postMessage({ id, source } satisfies MermaidRequest);
}

/** Replaces `pre` with its drawn figure, moving `pre` itself into the source disclosure. */
function drawBlock(pre: HTMLPreElement, drawn: Drawing): void {
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
    const style = document.createElement("style");
    style.textContent = PREVIEW_STYLE;
    preview.attachShadow({ mode: "open" }).append(style, document.importNode(drawn.svg, true));
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
 * Draws every mermaid code block `element` holds (painted from `parsed`, the parse of
 * `markdown`) but one whose fence is still open. `reparse` parses with the engine and soft-break
 * policy that produced `parsed`; the open-fence check runs it once, only when the body ends in a
 * mermaid block. A block whose source is cached draws now; every other one when the worker
 * answers, in the order the bodies were painted. Returns the cancel for those late draws: it
 * empties this call's own list of blocks, so a cancelled draw never runs and holds none of the
 * body's DOM while the worker draws.
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
  const late: HTMLPreElement[] = [];
  for (const pre of blocks) {
    const drawn = drawings.get(pre.textContent ?? "");
    if (drawn === undefined) late.push(pre);
    else drawBlock(pre, drawn);
  }
  if (late.length === 0 || failed) return noop;
  const target: { blocks: readonly HTMLPreElement[] | undefined } = { blocks: late };
  for (const [index, pre] of late.entries()) {
    const source = pre.textContent ?? "";
    const draw: LateDraw = (drawn) => {
      const block = target.blocks?.[index];
      if (block !== undefined) drawBlock(block, drawn);
    };
    const draws = requests.get(source);
    if (draws === undefined) requests.set(source, [draw]);
    else draws.push(draw);
  }
  sendNext();
  return () => {
    target.blocks = undefined;
  };
}

/**
 * Terminates the worker and forgets it, every cached drawing, every waiting draw and a failure,
 * so a test starts from a page that has drawn nothing. `timeLimitMs` replaces
 * `RENDER_TIME_LIMIT_MS` until the next reset, so a test of the time limit need not wait 5 s.
 */
export function resetMermaidDiagrams(limitMs = RENDER_TIME_LIMIT_MS): void {
  if (renderer !== undefined) {
    window.clearTimeout(renderer.current?.timer);
    renderer.worker.terminate();
    renderer = undefined;
  }
  failed = false;
  window.removeEventListener("online", recover);
  drawings.clear();
  requests.clear();
  timeLimitMs = limitMs;
}
