/**
 * The editor's view of every code block, legion's own in place of upstream's
 * `mermaidDiagramsPlugin` (INFRA-532). A plain code block is upstream's
 * `div.proof-code-block > pre.proof-code-block-pre > code`. A mermaid block is drawn by the host's
 * `MermaidDrawer` (Dispatch's Web Worker), never on the page's thread, and the drawing is mounted
 * in the preview's shadow root, so the diagram's styles stay inside it; when no renderer can run,
 * the block shows its source as code. This module imports nothing from upstream, so
 * beautiful-mermaid and ELK stay out of the editor's bundle.
 */

import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, ViewMutationRecord } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";
import { type MermaidDrawer, type MermaidDrawing, showMermaidSvg } from "./mermaid-svg";

/** How long after the last edit of a mermaid block's source the view asks for a new drawing. */
export const EDIT_REDRAW_DELAY_MS = 300;

/** The diagram's sizing, in its shadow root's own sheet, since no page rule (lib.css) reaches
 *  inside the root. */
const EDITOR_SHEET =
  "svg{display:block;width:100%;min-width:640px;height:auto}@media (max-width:720px){svg{min-width:520px}}";

const MERMAID_LANGUAGE = "mermaid";

const mermaidCodeBlockKey = new PluginKey("mermaid-code-block");

/**
 * A code block's node view. The layout of a mermaid block (the preview, the status line and the
 * open source disclosure holding the `pre`) is set only in `update`, ProseMirror's own update,
 * and never when a drawing arrives: an answer comes back at any time, often while the reader is
 * typing in the block, and moving the `pre` then would move the contentDOM under the caret. An
 * answer only fills or clears the preview's shadow root and the status line. Until the first
 * answer, and for good when there is no drawer or it never answers, the block shows its source;
 * an `unavailable` answer (no renderer can draw the source) clears the preview and the status, so
 * the block shows its source again rather than a drawing of an earlier source. The drawer never
 * calls `draw` after its cancel, and each request cancels the one before it, so only the latest
 * request's answer ever reaches the block.
 */
class MermaidCodeBlockView implements NodeView {
  readonly dom: HTMLDivElement;
  readonly contentDOM: HTMLElement;

  private readonly preview: HTMLDivElement;
  private readonly status: HTMLDivElement;
  private readonly sourceDetails: HTMLDetailsElement;
  private readonly sourceSummary: HTMLElement;
  private readonly pre: HTMLPreElement;
  private currentLanguage = "";
  private currentSource = "";
  private cancelRequest: (() => void) | undefined;
  private redrawTimer: number | undefined;

  constructor(
    node: ProseMirrorNode,
    view: EditorView,
    private readonly drawMermaid: MermaidDrawer | undefined
  ) {
    const document = view.dom.ownerDocument;
    this.dom = document.createElement("div");
    this.dom.className = "proof-code-block";

    this.preview = document.createElement("div");
    this.preview.className = "proof-mermaid-preview";
    this.preview.hidden = true;
    this.preview.setAttribute("contenteditable", "false");

    this.status = document.createElement("div");
    this.status.className = "proof-mermaid-status";
    this.status.hidden = true;
    this.status.setAttribute("contenteditable", "false");

    this.sourceDetails = document.createElement("details");
    this.sourceDetails.className = "proof-mermaid-source";
    this.sourceDetails.open = true;

    this.sourceSummary = document.createElement("summary");
    this.sourceSummary.className = "proof-mermaid-source-summary";
    this.sourceSummary.textContent = "Mermaid source";
    this.sourceSummary.setAttribute("contenteditable", "false");

    this.pre = document.createElement("pre");
    this.pre.className = "proof-code-block-pre";

    this.contentDOM = document.createElement("code");

    this.pre.appendChild(this.contentDOM);
    this.sourceDetails.append(this.sourceSummary);
    this.dom.append(this.pre);

    this.update(node);
  }

  /**
   * Sets the layout for the block's language. A block that becomes mermaid gets the mermaid
   * layout at once and asks for its drawing; an edit of a mermaid block's source only restarts
   * the redraw timer, so typing a word asks once, after the pause, rather than stopping and
   * restarting the host's worker on each key. Whatever is shown stays until the answer replaces it.
   */
  update(node: ProseMirrorNode): boolean {
    if (node.type.name !== "code_block") return false;

    // Upstream's rule: the language trimmed and lower-cased, "" when unset.
    const language: unknown = node.attrs.language;
    const nextLanguage = typeof language === "string" ? language.trim().toLowerCase() : "";
    const nextSource = node.textContent;
    if (nextLanguage === this.currentLanguage && nextSource === this.currentSource) return true;

    const wasMermaid = this.currentLanguage === MERMAID_LANGUAGE;
    this.currentLanguage = nextLanguage;
    this.currentSource = nextSource;
    this.pre.dataset.language = nextLanguage;

    if (nextLanguage !== MERMAID_LANGUAGE) {
      if (wasMermaid) this.showPlainLayout();
    } else if (!wasMermaid) {
      this.showMermaidLayout();
      this.requestDrawing();
    } else {
      window.clearTimeout(this.redrawTimer);
      this.redrawTimer = window.setTimeout(() => {
        this.redrawTimer = undefined;
        this.requestDrawing();
      }, EDIT_REDRAW_DELAY_MS);
    }
    return true;
  }

  stopEvent(event: Event): boolean {
    return event.target instanceof Node && this.sourceSummary.contains(event.target);
  }

  ignoreMutation(mutation: ViewMutationRecord): boolean {
    if (mutation.type === "selection") return false;
    return !this.contentDOM.contains(mutation.target);
  }

  /** Cancels the redraw timer and the request, so no answer reaches a removed view and no source
   *  stays queued in the host for it. */
  destroy(): void {
    this.stopDrawing();
  }

  /** Preview and status (both hidden until an answer fills them) above the `pre` inside the open
   *  source disclosure: the layout a drawing or a refusal later fills without moving the `pre`. */
  private showMermaidLayout(): void {
    this.dom.classList.add("proof-mermaid-block");
    this.sourceDetails.append(this.pre);
    this.dom.replaceChildren(this.preview, this.status, this.sourceDetails);
  }

  /** The block left mermaid: no drawing, no request, and the `pre` alone in the frame. */
  private showPlainLayout(): void {
    this.stopDrawing();
    this.dom.classList.remove("proof-mermaid-block");
    this.clearPreview();
    this.dom.replaceChildren(this.pre);
  }

  /** Empties the preview and the status line and hides both: what a block that has nothing to
   *  show above its source looks like, whether it left mermaid or no renderer could draw it. */
  private clearPreview(): void {
    this.preview.shadowRoot?.replaceChildren();
    this.preview.hidden = true;
    this.status.hidden = true;
    this.status.textContent = "";
  }

  /** Cancels the earlier request and asks for the current source; the drawer may answer inside
   *  the call (a cached drawing, or no renderer), which shows at once. */
  private requestDrawing(): void {
    this.cancelPendingRequest();
    if (this.drawMermaid === undefined) return;
    this.cancelRequest = this.drawMermaid(this.currentSource, (drawing) => this.show(drawing));
  }

  private cancelPendingRequest(): void {
    const cancel = this.cancelRequest;
    this.cancelRequest = undefined;
    cancel?.();
  }

  private stopDrawing(): void {
    window.clearTimeout(this.redrawTimer);
    this.redrawTimer = undefined;
    this.cancelPendingRequest();
  }

  /** Fills the preview's shadow root with a drawing, or clears it and shows a refusal's text, or
   *  clears both when no renderer can draw the source, leaving the source shown. */
  private show(drawing: MermaidDrawing): void {
    if ("svg" in drawing) {
      showMermaidSvg(this.preview, drawing.svg, EDITOR_SHEET);
      this.preview.hidden = false;
      this.status.hidden = true;
      this.status.textContent = "";
    } else if ("error" in drawing) {
      this.preview.shadowRoot?.replaceChildren();
      this.preview.hidden = true;
      this.status.textContent = drawing.error;
      this.status.hidden = false;
    } else {
      this.clearPreview();
    }
  }
}

/** The editor's code-block view, its mermaid blocks drawn by `drawMermaid`; without one they
 *  show their source. */
export function mermaidCodeBlockPlugin(drawMermaid: MermaidDrawer | undefined) {
  return $prose(
    () =>
      new Plugin({
        key: mermaidCodeBlockKey,
        props: {
          nodeViews: {
            code_block: (node, view) => new MermaidCodeBlockView(node, view, drawMermaid),
          },
        },
      })
  );
}
