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

import type { DisplayTree } from "../../api/types";
import { type AskBlockHost, applyAskBlockShell, askShellSlots } from "../doc/typed-block-drawing";
import { collectPictureAnchors, DispatchPicture, type PictureAnchor } from "./DispatchPicture";
import { TextAskCard } from "./markdown-cards";
import { type MarkdownEngine, renderWithEngine, type SoftBreaks } from "./markdown-engine";
import { drawMermaidDiagrams } from "./mermaid-diagrams";
import {
  collectReferenceAnchors,
  linkifyDispatchRefs,
  type ReferenceAnchor,
  type ReferenceLinking,
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
 * What one piece of a sliced paint yields: `"page"` where the work it has just done reached the
 * page - a batch of the body put into the element - so the task ends there whatever is left of
 * `SLICE_MS`, and the engine styles and lays out that batch before the next one adds to it;
 * nothing where the work was off the page, where `SLICE_MS` alone decides when the thread goes
 * back. A painter yields `"page"` because the cost of attaching is not its own: it is paid by
 * the engine in the rendering update after the task, which no clock in the painter can measure.
 */
export type SliceYield = "page" | undefined;

/**
 * A `paint` run a piece at a time: it fills the element as `paint` does, yielding wherever the
 * work can be cut, and the element holds the whole body once it is done.
 * `useRenderedMarkdown` drives it across tasks for a body of more nodes than a batch, so a list
 * of thousands of items is built, and then put into the page, without holding the reader's thread
 * for one long task; everything a surface reads from a painted element - the ask hosts, the
 * anchors, the pictures, the diagrams, `onRendered` - waits for the last piece, so no portal and
 * no pass ever meets half a body.
 */
export type PaintMarkdownInSlices = (
  element: HTMLElement,
  parsed: ProseMirrorNode,
  engine: MarkdownEngine
) => Generator<SliceYield>;

/** How long one slice of off-page work may hold the page's thread before it hands it back. */
const SLICE_MS = 50;

/**
 * How many nodes of the built body one task puts into the page. What an engine spends on a batch
 * is the style and layout of what has just arrived, in the rendering update after that task, so
 * the batch is what one such pass costs. Measured on this pod against the dashboard's own
 * stylesheet (sjawhar/legion#2519), an 8,000-item list: put in with one `replaceChildren` the
 * thread runs no task for 918-971 ms in WebKit, 211 ms in Chromium and 173 ms in Firefox; put in
 * 500 nodes at a time, 72 ms, 124 ms and 39 ms, because each engine then lays out only the items
 * of the batch. WebKit is why the batch exists at all: it spends ~700 ms on a pass over 16,000
 * freshly attached nodes whether their boxes are laid out or skipped, so nothing but splitting
 * the attach brings it under the second a reader may wait.
 *
 * `MarkdownBody`'s sliced painter attaches a batch of this many nodes, and the decision to paint
 * in slices at all is read against the same number (`paintsAcrossTasks`), so what a task puts
 * into the page and what is worth splitting into tasks cannot drift apart.
 */
export const ATTACH_BATCH = 500;

/** How many nodes `node` holds, itself included, counted no further than `limit`: a document
 *  bigger than a batch answers `limit` or a little more, which is all the decision below needs
 *  to know. Shaped as `MarkdownBody`'s own count over a built node, since the two answer the
 *  same question about a tree neither may walk whole. */
function nodesInDocument(node: ProseMirrorNode, limit: number): number {
  let count = 1;
  for (let child = 0; child < node.childCount && count < limit; child += 1) {
    count += nodesInDocument(node.child(child), limit - count);
  }
  return count;
}

/**
 * Whether the paint of `parsed` is worth building and attaching across tasks rather than in one.
 *
 * This is not the parse worker's rule (`parsesInTheWorker` in `markdown-engine.ts`), and it is
 * read of the document rather than of the source for the reason that rule cannot answer it: a
 * stored text is drawn from the tree the server read it into and is parsed nowhere, in the
 * worker or on the page, whatever its length, so a body of 32,000 characters reaches this
 * callback with nothing having gone to the worker at all. Source length no longer tracks what a
 * paint weighs in either direction: 30,000 characters of one paragraph are two nodes in the
 * page, and 2,100 characters of a list are more than a thousand.
 *
 * What a paint costs is the nodes it attaches, and the style and layout the engine pays for them
 * afterwards, a batch at a time (`ATTACH_BATCH`). So the decision counts the document's own
 * nodes, which is the only measure of that this callback holds, and counts no further than one
 * batch: a document that fits in a batch reaches the page in a single task whether it is sliced
 * or not, and slicing it would only mark it pending and keep a reader's place across a paint
 * that never grows the page under them. Each node of the document draws at least one node in
 * the page, so the count is a floor under what the attach will put there.
 */
function paintsAcrossTasks(parsed: ProseMirrorNode): boolean {
  return nodesInDocument(parsed, ATTACH_BATCH + 1) > ATTACH_BATCH;
}

/** What a sliced paint tells its caller as it runs: `beforeBatch` opens each task, before the
 *  work of it reaches the element; `afterPageBatch` ends a task that stopped on a piece which
 *  reached the page; `done` runs once the last piece is in. */
interface SlicedPaintSteps {
  afterPageBatch(): void;
  beforeBatch(): void;
  done(): void;
}

/**
 * Runs `slices` across tasks: each task does at most `SLICE_MS` of off-page work and ends at
 * once on a piece that reached the page (`SliceYield`), so neither the building of the 16,000
 * elements of an 8,000-item list nor the engine's styling and layout of them ever falls into one
 * task.
 *
 * Returns the cancel: it drops the task that was queued and stops the generator where it stands,
 * which runs the painter's own clean-up (`Generator.return`). A painter that empties the element
 * before its first batch puts back what it held there (`MarkdownBody`'s `paintBlockInSlices`),
 * so a stopped paint leaves no part of a body on screen. Calling it once the last piece has
 * landed does nothing.
 */
function paintAcrossTasks(
  slices: Generator<SliceYield>,
  { afterPageBatch, beforeBatch, done }: SlicedPaintSteps
): () => void {
  let timer: number | undefined;
  const nextSlice = (): void => {
    beforeBatch();
    const until = performance.now() + SLICE_MS;
    let last = false;
    let reachedThePage = false;
    // A piece whose work reached the page ends the task where it stands: what it costs is the
    // engine's own styling and layout of it, which this thread's clock cannot see, so the budget
    // must not pack another batch in behind it.
    do {
      const piece = slices.next();
      last = piece.done === true;
      reachedThePage = piece.value === "page";
    } while (!last && !reachedThePage && performance.now() < until);
    if (reachedThePage) {
      afterPageBatch();
    }
    if (last) {
      done();
      return;
    }
    timer = window.setTimeout(nextSlice, 0);
  };
  nextSlice();
  return () => {
    window.clearTimeout(timer);
    slices.return(undefined);
  };
}

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
  /** `paint`, a piece at a time (`PaintMarkdownInSlices`), for a body of more nodes than one task
   *  puts into the page (`paintsAcrossTasks`); without one such a body is painted in one go, as
   *  every other body is. */
  paintInSlices?: PaintMarkdownInSlices;
  /** What the link pass may link in the painted text (`ReferenceLinking`): what the surface's
   *  own reading of its source cites and what is known about it (`useTextReferences`). */
  references: ReferenceLinking;
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
  decorate: DecorateMarkdown | undefined,
  references: ReferenceLinking
): PortalAnchors {
  linkifyDispatchRefs(element, references);
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
 * Where the content under `element` starts, as the reader sees it now: the element's own bottom
 * edge in the viewport. A shift the browser's own scroll anchoring already made is in that
 * measurement, so the page is never moved twice.
 */
function whereTheContentUnderItWas(element: HTMLElement): number {
  return element.getBoundingClientRect().bottom;
}

/** The reader's place across one late paint, kept while the paint grows `element` under them. */
interface ReadersPlace {
  /** Read before a batch of the paint goes in, so a page the reader has moved since the last
   *  correction is held to where they moved it rather than where it was. */
  hold(): void;
  /** Puts the reader back for whatever the batch just added. */
  keep(): void;
}

/**
 * Keeps the reader where they are across a paint that grows `element`, which lands after the
 * commit that drew it: nothing else measures a second time (`ViewportAnchor` measures once per
 * commit, `use-follow-latest.ts` places once).
 *
 * A body wholly above the top of the page pushes everything the reader is looking at down by
 * what its formatting added, so the page scrolls back by exactly that. A body the reader can see
 * is left alone - its top does not move, and that is where they are reading from. What is
 * measured is the element's own bottom edge in the viewport, which is where the content under it
 * starts.
 *
 * `keep` runs once per batch of a sliced paint rather than once for the whole of it, because the
 * reader is not still while 32 batches land: with one measurement taken before the first batch
 * and the correction applied after the last, a reader who scrolled by S in between was carried
 * back by exactly S (sjawhar/legion#2519). Each batch already reads the element's rectangle to
 * have its own layout done while it is all that is dirty (`useRenderedMarkdown`'s slice loop),
 * so a correction per batch costs no layout the paint was not paying anyway.
 *
 * `hold` is what keeps the reader's own scrolling: between one correction and the next batch the
 * page can move for two reasons, and the page's own scroll position tells them apart from where
 * the content under the element has got to. Where the reader scrolled, the content under the
 * element is no longer where the last correction left it, and the place to hold the rest of the
 * paint to is the one they chose. Where it was the browser's own scroll anchoring, the content
 * is exactly where the last correction left it - that is what anchoring does - so re-reading it
 * changes nothing and the page is never moved twice. Nothing is read at all while the page has
 * not moved, which is every batch of a reader sitting still.
 */
function keepTheReadersPlace(element: HTMLElement): ReadersPlace {
  let under = whereTheContentUnderItWas(element);
  let placedAt = window.scrollY;
  return {
    hold() {
      if (window.scrollY === placedAt) {
        return;
      }
      under = whereTheContentUnderItWas(element);
      placedAt = window.scrollY;
    },
    keep() {
      if (under <= 0) {
        const moved = whereTheContentUnderItWas(element) - under;
        if (moved !== 0) {
          window.scrollBy(0, moved);
        }
      }
      placedAt = window.scrollY;
    },
  };
}

/**
 * What every Markdown-bearing element does once mounted, in the layout phase of the commit
 * (`renderWithEngine`, which says why that phase): `tree`, the reading Go made of a stored
 * text, is drawn and handed to `paint`; a text Dispatch does not store, which carries no tree,
 * has `markdown` parsed here instead and handed to `paint` the same way; and without an engine,
 * a tree the schema refuses or a parse the engine refuses, `markdown` is written into the
 * element as literal text with `isFallback` set. A dispatch:// reference stays literal text to
 * Markdown (it isn't a scheme GFM autolinks), so bare refs then get wrapped into real links; a
 * same-origin dashboard URL is already a real link by then (GFM autolinked it, or the author
 * wrote it as Markdown link syntax). Either way, every reference anchor gets its href rewritten
 * to the SPA route and its text handed to a portal-mounted `RefLink` for the resolved title.
 * `decorate` runs last, on the fallback text as on a painted element, so a `<mark>` it adds
 * never splits a reference before the link pass sees it, and a plain lead it prepends is never
 * read as a reference; it answers the anchors the portals mount into, so it may replace an
 * anchor's element. Each Dispatch picture's placeholder is found after it, wherever it left one
 * (`MarkdownPreview` puts an inert span in a link's place), and gets a portal-mounted
 * `DispatchPicture` in the shape `pictures` names. Where the app provides a decision card
 * (`TextAskCard`), each `ask` shell a parsed `paint` drew gets one, mounted in the same commit
 * (`collectAskHosts`); the card is a dependency, so a new provider value renders again. On a
 * painted body, each fenced `mermaid` block then draws as its diagram (`drawMermaidDiagrams`,
 * which draws a source it has not drawn before late, once its worker answers; the paint that
 * replaces it, and the unmount, cancel that). It runs after every other pass, so a `dispatch://`
 * in a diagram's label is never wrapped as a link or collected as a reference.
 * `onRendered` fires after each render lands; the newest callback is the one called, and a new
 * callback alone re-renders nothing. `paint` and `decorate` are dependencies: a caller passes a
 * module-level function or memoises one on what it reads. The options object itself is not one,
 * so a caller builds it inline on every render.
 *
 * A body with no tree of its own and longer than `markdownWorkerThreshold` is parsed off the
 * page's own thread (`markdown-engine.ts`), so it lands a frame or more after the commit that
 * rendered it. A stored text never does, however long it is: its tree is drawn in the commit
 * that mounted it. Until a parse lands, `isPending` is set and the element holds the body's own
 * literal text - selectable, under nothing - unless it already holds a paint of an earlier
 * source, which it keeps instead, so a streaming turn never flips back to its source between
 * frames. Every paint, the pending one included, recomputes the ask hosts, the reference anchors
 * and the picture anchors in the same callback, so no portal is left pointing into a page that
 * has been replaced; a kept paint replaces nothing and keeps the portals it has. `onRendered`
 * fires on the paint that formats the body, never on the pending one.
 *
 * The wait for an answer belongs to this element, not to the render of one frame. A turn
 * streaming into the live view renders up to ten frames a second (`AgentThread.tsx`), and every
 * frame cancels the frame before it, so an element that took only the answer to the frame it is
 * on would be shown nothing at all for as long as the stream ran: each parse ends after the next
 * frame has replaced it. The frame the worker is holding is painted here when it lands instead
 * (`markdown-engine.ts`, `keepAPostedParse`), and the element is left marked pending, since the
 * newest frame is still owed. So a reader watches a long turn grow, one parse behind the stream.
 * What is dropped is an answer to an element that has gone - the ref the cancel leaves empty -
 * and one a newer frame has already painted over.
 *
 * A body that formats after its commit must not move the reader, and nothing else measures a
 * second time: `ViewportAnchor` measures once per commit and `use-follow-latest.ts` places the
 * reader once. So the late paint keeps the reader's place itself (`keepTheReadersPlace`), once
 * per batch it lands and holding to wherever the reader has scrolled to meanwhile, and tells
 * `use-follow-latest.ts` about itself (`onMarkdownLatePaint`), which places the reader again
 * where Jump to latest had placed them at the turn that just formatted.
 *
 * A body of more nodes than one task puts into the page (`paintsAcrossTasks`), where the surface
 * gave a `paintInSlices`, is built a piece at a time and put into the page a batch at a time:
 * each task does at most `SLICE_MS` of off-page work, and ends at once on a piece that reached
 * the page (`SliceYield`), so neither the building of the 16,000 elements of an 8,000-item list
 * nor the engine's styling and layout of them ever falls into one task. The reader sees the body
 * fill over those tasks - where their own text was for a body the worker parsed, and from
 * nothing for a stored one, which is drawn from its tree and was never shown as text. The
 * element is marked pending until the last piece - a body served from the engine's answer cache
 * on a fresh mount is sliced without ever having waited for the worker, as every stored body is,
 * and is marked all the same, since between the tasks it holds part of a body and nothing says
 * so otherwise - and the ask hosts, the anchors, the pictures, the diagrams and `onRendered`
 * all wait for it, so no portal is mounted into a body still being built. A frame arriving mid
 * paint stops nothing and writes nothing: the paint goes on to put its whole body in, which is
 * the newest the reader can be shown until that frame's own answer lands, and the element is
 * left to it rather than taking that frame's own text, which would stand in front of the
 * batches still arriving (sjawhar/legion#2519). What does stop a paint is the paint that
 * replaces it, and the unmount; a paint stopped part-built puts back the body the element held
 * before it, so the reader is never left with the prefix of one (`MarkdownBody`'s
 * `paintBlockInSlices`). Only the paint that first puts a given source here is built that way:
 * a repaint of the source the element already shows - a new decision card, a new `decorate`
 * pass - lands in the commit that asked for it, so everything mounted into the paint the reader
 * is reading moves to the new one with it. A source that differs from the one painted is sliced
 * however much the element already holds, so an edit of a long comment, and a streamed turn that
 * grew past a batch after a short paint, never land in one task.
 */
export function useRenderedMarkdown(
  root: RefObject<HTMLElement | null>,
  markdown: string,
  tree: DisplayTree | undefined,
  paint: PaintMarkdown,
  {
    pictures = "block",
    decorate,
    onRendered,
    paintInSlices,
    references,
    softBreaks = "space",
  }: RenderedMarkdownOptions
): RenderedMarkdown {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const Card = useContext(TextAskCard);
  const [referenceAnchors, setReferenceAnchors] = useState<readonly ReferenceAnchor[]>(NO_ANCHORS);
  const [pictureAnchors, setPictureAnchors] = useState<readonly PictureAnchor[]>(NO_PICTURES);
  const [askHosts, setAskHosts] = useState<readonly AskBlockHost[]>(NO_ASK_HOSTS);
  const [isFallback, setIsFallback] = useState(false);
  const [isPending, setIsPending] = useState(false);
  /** The source the element holds painted, which a pending source keeps rather than replacing
   *  with its text, and which says whether a paint is this source's first here. `undefined`
   *  where the element holds no paint: nothing yet, a body's literal text, or a paint of this
   *  source still landing, which `painting` is what says. */
  const painted = useRef<string | undefined>(undefined);
  /** Whether the reader has been shown a pending element, so the paint that formats it replaces
   *  something they are already reading. */
  const wasPending = useRef(false);

  /** How many renders this element has begun, and the newest of them a paint has landed from. An
   *  answer outlives the render that asked for it, so a streaming turn is shown the frame the
   *  worker was holding; what it must never do is paint over a newer frame's paint
   *  (sjawhar/legion#2519). */
  const renders = useRef(0);
  const paintedRender = useRef(0);
  /** Stops the paint filling the element across tasks, and the diagrams the paint before it
   *  asked for. Both outlive the render that started them - a paint goes on while the next frame
   *  is pending, and a diagram draws into the paint the reader is reading - so each is stopped
   *  by the paint that replaces it and by the unmount, never by a frame. `painting` is set only
   *  while that paint is filling the element: it is cleared where the paint lands, so a source
   *  still being painted here is as much a paint as a source painted whole
   *  (sjawhar/legion#2519). */
  const painting = useRef<(() => void) | undefined>(undefined);
  const drawing = useRef<(() => void) | undefined>(undefined);

  useLayoutEffect(
    () => () => {
      painting.current?.();
      drawing.current?.();
    },
    []
  );

  useLayoutEffect(() => {
    const element = root.current;
    if (element === null) {
      throw new Error("The Markdown element is unavailable.");
    }
    setIsFallback(false);
    renders.current += 1;
    const render = renders.current;
    /** Whether a frame newer than this render's source is still owed, which is what leaves the
     *  element marked pending through a paint of an older one. */
    const owed = (): boolean => render !== renders.current;
    const cancelRender = renderWithEngine(
      markdown,
      tree,
      softBreaks,
      ({ engine, openMermaidFence, parsed, pending }) => {
        // Nothing of an answer reaches an element that has gone, or one a newer frame has
        // already painted: those are the two ways a kept answer stops being worth having.
        if (root.current !== element || render < paintedRender.current) {
          return;
        }
        if (pending) {
          setIsPending(true);
          wasPending.current = true;
          if (painted.current !== undefined || painting.current !== undefined) {
            // The paint the reader is reading stays until the answer to this source lands, and
            // with it every portal mounted into it. A paint still filling the element across
            // tasks is one of those: nothing but the paint that replaces it, or the unmount,
            // stops it, so this source's text written here would stand in front of the batches
            // that paint goes on attaching (sjawhar/legion#2519).
            return;
          }
          element.replaceChildren(document.createTextNode(markdown));
          setAskHosts(NO_ASK_HOSTS);
          const { pictures: placeholders, references: anchors } = portalAnchors(
            element,
            decorate,
            references
          );
          setReferenceAnchors(anchors);
          setPictureAnchors(placeholders);
          return;
        }
        paintedRender.current = render;
        // The paint this element was being filled by, and the diagrams of the paint before it,
        // belong to what is about to be replaced.
        painting.current?.();
        painting.current = undefined;
        drawing.current?.();
        drawing.current = undefined;
        const replacesPending = wasPending.current;
        // A frame newer than this source may already be owed, and the element stays marked
        // pending until its answer lands, so that paint replaces a pending element too.
        wasPending.current = owed();
        // Whether this is the paint that first puts this source into the element, which is the
        // one that may be built across tasks. A repaint of the source the element already shows
        // - a new decision card, a new `decorate` pass - lands in this commit instead, so the
        // cards, the links and the pictures mounted into the paint the reader is reading move to
        // the new one with it rather than a few tasks later. It is the source that decides, not
        // merely whether anything is painted: an edit of a long comment, and a streamed turn
        // that grew past a batch after a short paint, are each a different source and each
        // 16,000 elements, which one `replaceChildren` of holds WebKit's thread for about a
        // second (`ATTACH_BATCH`) (sjawhar/legion#2519).
        const first = painted.current !== markdown;
        let place: ReadersPlace | undefined;
        // Everything a surface reads from the element, once the whole body is in it.
        const finish = (): void => {
          place?.keep();
          setIsPending(owed());
          let hosts = NO_ASK_HOSTS;
          if (engine !== undefined && parsed !== undefined && Card !== undefined) {
            hosts = collectAskHosts(element, parsed);
          }
          setAskHosts(hosts);
          const { pictures: placeholders, references: anchors } = portalAnchors(
            element,
            decorate,
            references
          );
          setReferenceAnchors(anchors);
          setPictureAnchors(placeholders);
          if (engine !== undefined && parsed !== undefined) {
            drawing.current = drawMermaidDiagrams(
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
          painted.current = undefined;
          setIsFallback(true);
          element.replaceChildren(document.createTextNode(markdown));
          finish();
          return;
        }
        // A paint of an older frame can land after a refused one, and what the element holds
        // then is formatted, whatever the frame before it was.
        setIsFallback(false);
        // Whether this paint is built and attached across tasks, which the document it will put
        // into the page decides (`paintsAcrossTasks`), not the source the parse worker's own
        // rule reads.
        const inSlices =
          paintInSlices !== undefined && first && paintsAcrossTasks(parsed)
            ? paintInSlices
            : undefined;
        // Where the reader is, watched for the whole paint, slices and all, and only for the
        // paint that lands after its own commit and grows the element under them: one that
        // replaces a pending element, and one built in slices, which a body served from the
        // engine's cache on a fresh mount is without ever having been pending. A paint that is
        // neither - every body that reaches the page in one task - measures nothing, so a short
        // body never lays the page out in this effect.
        if (replacesPending || inSlices !== undefined) {
          place = keepTheReadersPlace(element);
        }
        if (inSlices === undefined) {
          paint(element, parsed, engine.serializer);
          painted.current = markdown;
          finish();
          return;
        }
        // The element holds part of a body between the tasks of a sliced paint, so it carries
        // the pending marker until the last of them whether or not the reader was ever shown
        // pending text: a body the engine already had the answer for is painted in slices on a
        // fresh mount without the worker having been waited for at all.
        setIsPending(true);
        // `paintAcrossTasks` runs its first slice in this task, so a body that fits in one -
        // a long paragraph, whose whole tree is fewer nodes than a batch - is done before the
        // cancel it returns is in hand: `running` is what keeps `painting.current` from being
        // left holding a paint that is already over (sjawhar/legion#2519).
        let running = true;
        const stop = paintAcrossTasks(inSlices(element, parsed, engine), {
          afterPageBatch() {
            // The batch that has just arrived is laid out here, while it is all of the page that
            // is dirty. Left to the engine, the layout of every batch would fall into whichever
            // task first reads the page's geometry, and in WebKit a list of 8,000 items laid out
            // in one go is ~700 ms of that task (sjawhar/legion#2519). Laid out a batch at a
            // time it is a few milliseconds each. The reader is put back for what this batch
            // added from that same reading, so the correction never carries them off a place
            // they scrolled to while the rest of the body was still landing
            // (`keepTheReadersPlace`).
            element.getBoundingClientRect();
            place?.keep();
          },
          beforeBatch() {
            // What the reader has done to the page since the last batch, before this one grows
            // it under them.
            place?.hold();
          },
          done() {
            running = false;
            painting.current = undefined;
            painted.current = markdown;
            finish();
          },
        });
        if (running) {
          painting.current = stop;
        }
      },
      // The frame the worker is holding is the newest parse a streaming turn can be shown
      // before it pauses, so this element is still given that answer after it has asked for a
      // newer frame (`markdown-engine.ts`).
      { keepAPostedParse: true }
    );
    return cancelRender;
  }, [Card, decorate, markdown, paint, paintInSlices, references, root, softBreaks, tree]);

  return {
    isFallback,
    isPending,
    portals: [
      ...referenceAnchors.map(({ anchor, key, route, text }) =>
        createPortal(<RefLink anchor={anchor} route={route} text={text} />, anchor, key)
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
