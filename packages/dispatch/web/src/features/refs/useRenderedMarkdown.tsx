import type { DOMSerializer, Node as ProseMirrorNode } from "prosemirror-model";
import {
  type ReactNode,
  type RefObject,
  useContext,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { type AskBlockHost, applyAskBlockShell, askShellSlots } from "../doc/typed-block-drawing";
import { collectPictureAnchors, DispatchPicture, type PictureAnchor } from "./DispatchPicture";
import { TextAskCard } from "./markdown-cards";
import { parsesInTheWorker, renderWithEngine, type SoftBreaks } from "./markdown-engine";
import { drawMermaidDiagrams } from "./mermaid-diagrams";
import {
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  RefLink,
} from "./RefLink";

/** The anchor list of a body with no references: one shared value, so re-rendering such a body
 *  leaves the state untouched instead of committing a fresh empty array each time. */
const NO_ANCHORS: readonly ReferenceAnchor[] = [];
/** The same, for a body with no Dispatch pictures. */
const NO_PICTURES: readonly PictureAnchor[] = [];
/** The same, for a body with no decision card to mount. */
const NO_ASK_HOSTS: readonly AskBlockHost[] = [];

/**
 * A host for each `ask` shell `paint` drew under `element` (`renderAskBlockShell`), paired in
 * document order with the parsed document's `ask` nodes. A surface that drew no shell for them
 * (`MarkdownPreview` paints one line of inline content) gets none: the shells must match the
 * nodes one for one, every one with its three slots (`askShellSlots`), or no card is mounted and
 * nothing is written. Once every shell has paired, it writes each through `applyAskBlockShell`,
 * and only there, because a card is mounted into it.
 */
function collectAskHosts(element: HTMLElement, parsed: ProseMirrorNode): readonly AskBlockHost[] {
  const shells = element.querySelectorAll<HTMLElement>('section[data-proof-block-type="ask"]');
  if (shells.length === 0) {
    return NO_ASK_HOSTS;
  }
  const nodes: ProseMirrorNode[] = [];
  parsed.descendants((node) => {
    if (node.type.name === "ask") {
      nodes.push(node);
    }
    return !node.isTextblock;
  });
  if (nodes.length !== shells.length) {
    return NO_ASK_HOSTS;
  }
  const paired: {
    readonly content: HTMLElement;
    readonly host: AskBlockHost;
    readonly shell: HTMLElement;
  }[] = [];
  for (const [index, node] of nodes.entries()) {
    const slots = askShellSlots(shells[index]);
    if (slots === undefined) {
      return NO_ASK_HOSTS;
    }
    const { content, footer, header } = slots;
    paired.push({ content, host: { footer, header, key: index, node }, shell: shells[index] });
  }
  for (const { content, host, shell } of paired) {
    applyAskBlockShell(shell, content, host.node);
  }
  return paired.map(({ host }) => host);
}

/** How a surface fills its element from the parsed document: `MarkdownBody` serializes the whole
 *  tree, `MarkdownPreview` one line of inline content. The serializer is the engine's: the
 *  schema's own, except that it writes a Dispatch picture as the placeholder a `DispatchPicture`
 *  portals into (`markdown-engine.ts`). */
export type PaintMarkdown = (
  element: HTMLElement,
  parsed: ProseMirrorNode,
  serializer: DOMSerializer
) => void;

export interface RenderedMarkdown {
  /** The engine could not load or parse the source, and the element holds it as literal text.
   *  It is the one final failure a body has: a parse still owed is `isPending`. */
  isFallback: boolean;
  /** The parse worker still owes this body's formatting (`markdown-engine.ts`). The element
   *  holds the body's own text, or the paint it had before the source changed: readable and
   *  selectable, under nothing, either way. */
  isPending: boolean;
  /** A `RefLink` portal into every reference anchor of the element, a `DispatchPicture` portal
   *  into every Dispatch picture's placeholder, and the provided decision card (`TextAskCard`)
   *  for every `ask` shell, which portals into the shell's slots itself; rendered inside it. */
  portals: ReactNode;
}

/** A surface's last pass over its rendered element, given the reference anchors the link pass
 *  found (each already emptied for its portal). It answers the anchors to portal into: the same
 *  ones, or whatever element it put in each one's place. */
export type DecorateMarkdown = (
  element: HTMLElement,
  anchors: readonly ReferenceAnchor[]
) => readonly ReferenceAnchor[];

/**
 * A `paint` run a piece at a time: it fills the element as `paint` does, yielding wherever the
 * work can be cut, and the element holds the whole body once it is done.
 * `useRenderedMarkdown` drives it across tasks for a body the parse worker parsed, so a list of
 * thousands of items is built without holding the reader's thread for one long task; everything
 * a surface reads from a painted element - the ask hosts, the anchors, the pictures, the
 * diagrams, `onRendered` - waits for the last piece. A painter builds off the page and puts the
 * body in the element at the end (`MarkdownBody`), so no portal and no pass ever meets half a
 * body.
 */
export type PaintMarkdownInSlices = (
  element: HTMLElement,
  parsed: ProseMirrorNode,
  serializer: DOMSerializer
) => Generator<void>;

/** How long one slice may hold the page's thread before it hands it back. */
const SLICE_MS = 50;

/** What a surface tells `useRenderedMarkdown` beyond its element, source and paint; each has a
 *  default, so a surface names only what it changes. */
export interface RenderedMarkdownOptions {
  /** The shape each Dispatch picture's `DispatchPicture` takes: `block` (the default) at the
   *  column's width, `inline` as a thumbnail beside its caption. */
  pictures?: "block" | "inline";
  /** The surface's last pass over its rendered element (`DecorateMarkdown`); none by default. */
  decorate?: DecorateMarkdown;
  /** Called after each render lands; none by default. */
  onRendered?: () => void;
  /** The parse policy (`SoftBreaks`): `space`, the default, or `line`. */
  softBreaks?: SoftBreaks;
  /** `paint`, a piece at a time (`PaintMarkdownInSlices`), for a body the parse worker parsed;
   *  without one such a body is painted in one go, as every other body is. */
  paintInSlices?: PaintMarkdownInSlices;
}

/** Every anchor a paint leaves for a portal: the reference anchors the link pass found, as
 *  `decorate` answered them, and each Dispatch picture's placeholder after it. */
interface PortalAnchors {
  readonly pictures: readonly PictureAnchor[];
  readonly references: readonly ReferenceAnchor[];
}

/** Runs the link pass, `decorate` and the picture pass over a freshly painted element, in that
 *  order, and answers what the portals mount into. */
function portalAnchors(
  element: HTMLElement,
  decorate: DecorateMarkdown | undefined
): PortalAnchors {
  linkifyDispatchRefs(element);
  const found = collectReferenceAnchors(element);
  const anchors = decorate === undefined ? found : decorate(element, found);
  const placeholders = collectPictureAnchors(element);
  return {
    pictures: placeholders.length === 0 ? NO_PICTURES : placeholders,
    references: anchors.length === 0 ? NO_ANCHORS : anchors,
  };
}

/** Told of an element the parse worker has just formatted. */
type LatePaintListener = (element: HTMLElement) => void;

const latePaintListeners = new Set<LatePaintListener>();

/**
 * Calls `listener` with each element a body the reader was shown as pending has just been painted
 * formatted into, in the task that painted it, before the browser paints. `use-follow-latest.ts`
 * listens: the newest turn, formatting after Jump to latest placed the reader at it, is placed
 * again. Answers the unsubscribe.
 */
export function onMarkdownLatePaint(listener: LatePaintListener): () => void {
  latePaintListeners.add(listener);
  return () => {
    latePaintListeners.delete(listener);
  };
}

/**
 * Where the content under `element` starts, as the reader sees it now, for `keepTheReadersPlace`
 * to put back once the body is painted. A body the parse worker formats lands after the commit
 * that drew it, and nothing else measures a second time (`ViewportAnchor` measures once per
 * commit, `use-follow-latest.ts` places once).
 */
function whereTheContentUnderItWas(element: HTMLElement): number {
  return element.getBoundingClientRect().bottom;
}

/**
 * Puts the reader back where they were before a paint that grew `element`, which `before`
 * (`whereTheContentUnderItWas`) measured.
 *
 * A body wholly above the top of the page pushes everything the reader is looking at down by what
 * its formatting added, so the page scrolls back by exactly that. What is measured is the
 * element's own bottom edge in the viewport, which is where the content under it starts: a shift
 * the browser's own scroll anchoring already made is in that measurement, so the page is never
 * moved twice. A body the reader can see is left alone - its top does not move, and that is where
 * they are reading from - and it is not measured again either: reading a rectangle lays the whole
 * body out there and then, which for a list of thousands of items is a tenth of a second on the
 * reader's thread for a page that was never going to move. One measurement covers a whole paint,
 * its slices included, for the same reason: measuring between slices lays the list out again for
 * every one of them, which is seconds of the reader's thread, and leaves the reader where this
 * puts them anyway.
 */
function keepTheReadersPlace(element: HTMLElement, before: number): void {
  if (before > 0) {
    return;
  }
  const moved = element.getBoundingClientRect().bottom - before;
  if (moved !== 0) {
    window.scrollBy(0, moved);
  }
}

/**
 * What every Markdown-bearing element does once mounted, in the layout phase of the commit
 * (`renderWithEngine`, which says why that phase): the source is parsed and handed to
 * `paint`, or, without an engine or a parse, written into the element as literal text with
 * `isFallback` set. A dispatch:// reference stays literal text to Markdown (it isn't a scheme GFM
 * autolinks), so bare refs then get wrapped into real links; a same-origin dashboard URL is
 * already a real link by then (GFM autolinked it, or the author wrote it as Markdown link
 * syntax). Either way, every reference anchor gets its href rewritten to the SPA route and its
 * text handed to a portal-mounted `RefLink` for the resolved title. `decorate` runs last, on the
 * fallback text as on a painted element, so a `<mark>` it adds never splits a reference before
 * the link pass sees it, and a plain lead it prepends is never read as a reference; it answers the
 * anchors the portals mount into, so it may replace an anchor's element. Each Dispatch picture's
 * placeholder is found after it, wherever it left one (`MarkdownPreview` puts an inert span in a
 * link's place), and gets a portal-mounted `DispatchPicture` in the shape `pictures` names.
 * Where the app provides a decision card (`TextAskCard`), each `ask` shell a parsed `paint` drew
 * gets one, mounted in the same commit (`collectAskHosts`); the card is a dependency, so a new
 * provider value renders again. On a painted body, each fenced `mermaid` block then draws as its
 * diagram (`drawMermaidDiagrams`, which draws a source it has not drawn before late, once its
 * worker answers; the effect's cleanup cancels that). It runs after every other pass, so a
 * `dispatch://` in a diagram's label is never wrapped as a link or collected as a reference.
 * `onRendered` fires after each render lands; the newest callback is the one called, and a new
 * callback alone re-renders nothing. `paint` and `decorate` are dependencies: a caller passes a
 * module-level function or memoises one on what it reads. The options object itself is not one,
 * so a caller builds it inline on every render.
 *
 * A body longer than `markdownWorkerThreshold` is parsed off the page's own thread
 * (`markdown-engine.ts`), so it lands a frame or more after the commit that rendered it. Until
 * it does, `isPending` is set and the element holds the body's own literal text - selectable,
 * under nothing - unless it already holds a paint of an earlier source, which it keeps instead,
 * so a streaming turn never flips back to its source between frames. Every paint, the pending
 * one included, recomputes the ask hosts, the reference anchors and the picture anchors in the
 * same callback, so no portal is left pointing into a page that has been replaced; a kept paint
 * replaces nothing and keeps the portals it has. Only the newest source's answer paints: the
 * effect's cleanup drops the answer to a source the element has moved on from. `onRendered`
 * fires on the paint that formats the body, never on the pending one.
 *
 * A body that formats after its commit must not move the reader, and nothing else measures a
 * second time: `ViewportAnchor` measures once per commit and `use-follow-latest.ts` places the
 * reader once. So the late paint keeps the reader's place itself
 * (`keepTheReadersPlace`), and tells `use-follow-latest.ts` about itself
 * (`onMarkdownLatePaint`), which places the reader again where Jump to latest had placed them at
 * the turn that just formatted.
 *
 * A body the parse worker parsed, where the surface gave a `paintInSlices`, is built a piece at
 * a time: each task does at most `SLICE_MS` of the work and hands the thread back, so the 16,000
 * elements of an 8,000-item list are never built in one task. The painter builds off the page
 * and puts the body in the element with the last piece, so the reader never sees half a list and
 * the page lays the body out once. The element stays pending until that last piece, and the ask
 * hosts, the anchors, the pictures, the diagrams and `onRendered` all wait for it, so no portal
 * is mounted into a body still being built. A paint the element has moved on from is stopped
 * where it stands, and what the reader is reading stays until the newer source's own paint
 * lands. Only the paint that first formats a body here is built that way: a repaint of a body
 * the element already shows - a new decision card, a new `decorate` pass - lands in the commit
 * that asked for it, so everything mounted into the paint the reader is reading moves to the
 * new one with it.
 */
export function useRenderedMarkdown(
  root: RefObject<HTMLElement | null>,
  markdown: string,
  paint: PaintMarkdown,
  {
    pictures = "block",
    decorate,
    onRendered,
    paintInSlices,
    softBreaks = "space",
  }: RenderedMarkdownOptions = {}
): RenderedMarkdown {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const Card = useContext(TextAskCard);
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [pictureAnchors, setPictureAnchors] = useState<readonly PictureAnchor[]>(NO_PICTURES);
  const [askHosts, setAskHosts] = useState<readonly AskBlockHost[]>(NO_ASK_HOSTS);
  const [isFallback, setIsFallback] = useState(false);
  const [isPending, setIsPending] = useState(false);
  /** Whether the element holds a formatted paint, which a pending source keeps rather than
   *  replacing with its text. */
  const formatted = useRef(false);
  /** Whether the reader has been shown a pending element, so the paint that formats it replaces
   *  something they are already reading. */
  const wasPending = useRef(false);

  useLayoutEffect(() => {
    const element = root.current;
    if (element === null) {
      throw new Error("The Markdown element is unavailable.");
    }
    setIsFallback(false);
    let cancelDraw: (() => void) | undefined;
    let sliceTimer: number | undefined;
    let slices: Generator<void> | undefined;
    const cancelRender = renderWithEngine(
      markdown,
      softBreaks,
      ({ engine, openMermaidFence, parsed, pending }) => {
        if (pending) {
          setIsPending(true);
          wasPending.current = true;
          if (formatted.current) {
            // The paint the reader is reading stays until the answer to this source lands, and
            // with it every portal mounted into it.
            return;
          }
          element.replaceChildren(document.createTextNode(markdown));
          setAskHosts(NO_ASK_HOSTS);
          const { pictures: placeholders, references } = portalAnchors(element, decorate);
          setReferenceAnchors(references);
          setPictureAnchors(placeholders);
          return;
        }
        const replacesPending = wasPending.current;
        wasPending.current = false;
        // Whether this is the paint that first formats the body here. A body the element does
        // not hold formatted yet is the one that may be built across tasks: the reader has its
        // text and nothing is mounted into it. A repaint of a body the element already shows -
        // a new decision card, a new `decorate` pass - lands in this commit instead, so the
        // cards, the links and the pictures mounted into the paint the reader is reading move
        // to the new one with it rather than a few tasks later.
        const first = !formatted.current;
        // Where the reader is, read once for the whole paint, slices and all, and only for the
        // paint that replaces one the reader was shown as pending: that is the one that lands
        // after its own commit and grows the element under them. A paint that replaces nothing
        // pending - every body parsed on the page's own thread - measures nothing, so a short
        // body never lays the page out in this effect.
        const before = replacesPending ? whereTheContentUnderItWas(element) : undefined;
        // Everything a surface reads from the element, once the whole body is in it.
        const finish = (): void => {
          if (before !== undefined) keepTheReadersPlace(element, before);
          setIsPending(false);
          let hosts = NO_ASK_HOSTS;
          if (engine !== undefined && parsed !== undefined && Card !== undefined) {
            hosts = collectAskHosts(element, parsed);
          }
          setAskHosts(hosts);
          const { pictures: placeholders, references } = portalAnchors(element, decorate);
          setReferenceAnchors(references);
          setPictureAnchors(placeholders);
          if (engine !== undefined && parsed !== undefined) {
            cancelDraw = drawMermaidDiagrams(
              element,
              parsed,
              markdown,
              (source) => engine.parse(source, softBreaks),
              openMermaidFence
            );
          }
          if (replacesPending) {
            for (const listener of [...latePaintListeners]) {
              listener(element);
            }
          }
          onRenderedRef.current?.();
        };
        if (engine === undefined || parsed === undefined) {
          formatted.current = false;
          setIsFallback(true);
          element.replaceChildren(document.createTextNode(markdown));
          finish();
          return;
        }
        const serializer = engine.serializer;
        if (paintInSlices === undefined || !parsesInTheWorker(markdown) || !first) {
          paint(element, parsed, serializer);
          formatted.current = true;
          finish();
          return;
        }
        slices = paintInSlices(element, parsed, serializer);
        const nextSlice = (): void => {
          const until = performance.now() + SLICE_MS;
          let done = false;
          do {
            done = slices?.next().done === true;
          } while (!done && performance.now() < until);
          if (done) {
            slices = undefined;
            formatted.current = true;
            finish();
            return;
          }
          sliceTimer = window.setTimeout(nextSlice, 0);
        };
        nextSlice();
      }
    );
    return () => {
      cancelRender();
      cancelDraw?.();
      // A source the element has moved on from stops mid-build: what the reader has stays until
      // the newer source's answer paints over it, rather than flipping back to text.
      window.clearTimeout(sliceTimer);
      slices?.return(undefined);
      slices = undefined;
    };
  }, [Card, decorate, markdown, paint, paintInSlices, root, softBreaks]);

  return {
    isFallback,
    isPending,
    portals: [
      ...referenceAnchors.map(({ anchor, key, route }) =>
        createPortal(<RefLink anchor={anchor} route={route} />, anchor, key)
      ),
      ...pictureAnchors.map(({ anchor, caption, key, route }) =>
        createPortal(
          <DispatchPicture caption={caption} route={route} variant={pictures} />,
          anchor,
          key
        )
      ),
      ...(Card === undefined
        ? []
        : askHosts.map((host) => <Card host={host} key={`ask-${host.key}`} />)),
    ],
  };
}
