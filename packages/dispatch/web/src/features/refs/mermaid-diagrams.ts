/**
 * Draws each fenced `mermaid` code block of a rendered Markdown body (`MarkdownBody`'s block
 * form; a one-line preview and the inline variant write no `<pre>`) as the diagram the document
 * editor draws: `renderMermaidSvg` from `@legion/proof-editor/mermaid`, upstream's validation and
 * renderer with a stricter sanitizer and the editor's accessible name. The block becomes a
 * `figure` holding the diagram, then (only for a source the renderer refuses) a status line
 * naming the reason, then the original code block inside a `Mermaid source` disclosure, closed
 * once the diagram drew and open on a refusal so the source is readable at once.
 *
 * The SVG goes in an open shadow root of the preview element, so the diagram's own `<style>`
 * styles that diagram alone rather than every SVG and `<text>` on the page, and its marker ids
 * (`url(#arrowhead…)`) resolve inside that root: two diagrams on a page, or one body shown in two
 * places at once, never point at each other's markers. The accessibility tree and Playwright's
 * locators reach into an open root; the disclosure stays in the light DOM.
 *
 * A mermaid block that is the document's last block while the source's last non-blank line is no
 * closing fence is still being written (an unclosed fence runs to the end of the document), so it
 * stays code: the live view re-parses a turn on every frame while a model streams it, and drawing
 * each frame would run the renderer and flash a refusal (`Dangling Mermaid edge`) until the fence
 * closes. Every other block of the same body still draws.
 *
 * The renderer (beautiful-mermaid and ELK) is a lazy chunk, loaded only once a body holds a
 * mermaid block. Once it has loaded, a body draws in the same layout effect that painted it;
 * before, its blocks show as code and are replaced when the chunk arrives (unless the body
 * re-painted or unmounted first), so the first diagram a page load draws moves the content below
 * it once. A chunk that fails to load leaves every block as code and is tried again by the next
 * body. Drawn results are cached by source (the sanitized SVG, cloned per use, or the refusal),
 * the newest 32, so a body re-painted while text after a finished diagram streams does not run the
 * renderer again.
 */
import type * as MermaidRenderer from "@legion/proof-editor/mermaid";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import { dangerText } from "../../theme/classes";
import { importWhenOnline } from "../shell/DeploymentResilience";

type MermaidModule = typeof MermaidRenderer;

/** A drawn source: its sanitized SVG, or the status line a refusal shows. */
type Drawing = { svg: SVGSVGElement } | { error: string };
/** A body's blocks waiting for the renderer to load. */
type PendingDraw = { element: HTMLElement; blocks: HTMLPreElement[] };

/** How many drawn sources the cache keeps; the oldest insertion goes first. */
const CACHE_LIMIT = 32;
/** A line that closes a fence: three or more backticks or tildes alone on it, after any
 *  blockquote markers (a diagram quoted at the end of a body closes inside its quote). */
const CLOSING_FENCE = /^(?:\s*>)*\s*(`{3,}|~{3,})\s*$/;
/** The one rule the diagram needs beside its own sheet: its natural width, up to the body's. */
const PREVIEW_STYLE = "svg{max-width:100%;height:auto;display:block}";

let loading: Promise<MermaidModule> | undefined;
let loaded: MermaidModule | undefined;
const drawings = new Map<string, Drawing>();
/** The late draws not yet cancelled, in paint order: drawn when the renderer loads and dropped
 *  when it fails, so a cancelled draw holds none of its body's DOM while the chunk is awaited. */
const pending = new Set<PendingDraw>();

function noop(): void {}

/** Upstream's `isMermaidCodeBlockLanguage` rule, restated so this module pulls none of the
 *  renderer's chunk until a body needs it. */
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

/** Whether the document ends in a mermaid block whose fence the source has not closed yet. */
function endsInOpenMermaidFence(parsed: ProseMirrorNode, markdown: string): boolean {
  const last = lastLeafBlock(parsed);
  if (last?.type.name !== "code_block" || !isMermaidLanguage(last.attrs.language)) return false;
  const source = markdown.trimEnd();
  return !CLOSING_FENCE.test(source.slice(source.lastIndexOf("\n") + 1));
}

function loadMermaid(): void {
  if (loading !== undefined) return;
  const attempt = importWhenOnline(() => import("@legion/proof-editor/mermaid"));
  loading = attempt;
  attempt.then(
    (module) => {
      if (loading !== attempt) return;
      loaded = module;
      const draws = Array.from(pending);
      pending.clear();
      for (const { element, blocks } of draws) drawBlocks(module, element, blocks);
    },
    (error: unknown) => {
      // The blocks stay code.
      if (loading === attempt) {
        loading = undefined;
        pending.clear();
      }
      console.error("Showing Mermaid source as code, the diagram renderer is unavailable", error);
    }
  );
}

function drawing(module: MermaidModule, source: string, document: Document): Drawing {
  const cached = drawings.get(source);
  if (cached !== undefined) return cached;
  let drawn: Drawing;
  try {
    drawn = { svg: module.renderMermaidSvg(source, document) };
  } catch (error) {
    drawn = {
      error:
        error instanceof Error
          ? `Unable to render Mermaid diagram: ${error.message}`
          : "Unable to render Mermaid diagram.",
    };
  }
  if (drawings.size >= CACHE_LIMIT) {
    const oldest = drawings.keys().next();
    if (oldest.done !== true) drawings.delete(oldest.value);
  }
  drawings.set(source, drawn);
  return drawn;
}

/** Replaces `pre` with its drawn figure, moving `pre` itself into the source disclosure. */
function drawBlock(module: MermaidModule, pre: HTMLPreElement): void {
  const document = pre.ownerDocument;
  const drawn = drawing(module, pre.textContent ?? "", document);
  const figure = document.createElement("figure");
  figure.setAttribute("data-mermaid-diagram", "");
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
    status.className = `dispatch-mermaid-status ${dangerText}`;
    status.textContent = drawn.error;
    figure.append(status);
    details.open = true;
  }
  pre.replaceWith(figure);
  details.append(summary, pre);
  figure.append(details);
}

function drawBlocks(module: MermaidModule, element: HTMLElement, blocks: HTMLPreElement[]): void {
  for (const pre of blocks) {
    if (element.contains(pre)) drawBlock(module, pre);
  }
}

/**
 * Draws every mermaid code block `element` holds (painted from `parsed`, the parse of
 * `markdown`) but one whose fence is still open, loading the renderer when none has loaded. With
 * the renderer loaded it draws now; otherwise when the renderer arrives. Returns the cancel for
 * that late draw; a cancelled draw never runs.
 */
export function drawMermaidDiagrams(
  element: HTMLElement,
  parsed: ProseMirrorNode,
  markdown: string
): () => void {
  const blocks = Array.from(element.querySelectorAll<HTMLPreElement>("pre[data-language]")).filter(
    (pre) => isMermaidLanguage(pre.getAttribute("data-language"))
  );
  if (blocks.length > 0 && endsInOpenMermaidFence(parsed, markdown)) blocks.pop();
  if (blocks.length === 0) return noop;
  if (loaded !== undefined) {
    drawBlocks(loaded, element, blocks);
    return noop;
  }
  const draw: PendingDraw = { element, blocks };
  pending.add(draw);
  loadMermaid();
  return () => {
    pending.delete(draw);
  };
}

/** Forgets the loaded renderer, every cached drawing and every pending draw, so a test starts
 *  from a page that has drawn nothing. */
export function resetMermaidDiagrams(): void {
  loading = undefined;
  loaded = undefined;
  drawings.clear();
  pending.clear();
}
