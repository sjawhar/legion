import { type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";

import { markdownClassName } from "../refs/markdown-engine";
import { markdownWorkerThreshold } from "../refs/markdown-worker-threshold";
import { paintBlock, paintBlockInSlices } from "../refs/paint-block";
import { useRenderedMarkdown } from "../refs/useRenderedMarkdown";
import { leadingBlocks } from "./document-blocks";
import { firstScreenAwaits, takeFirstScreen } from "./first-screen";

/** The class the preview's own element carries, which `styles.css` skips the off-screen blocks
 *  of exactly as it skips the editor's (`content-visibility`). */
const PREVIEW_CLASS = "dispatch-doc-preview";

/** The document's own draw inside `within`, while one is on the page: what the reader is
 *  reading before the editor exists, and what `ProofDocument` reads their place out of as it
 *  swaps the editor in. */
export function documentPreview(within: HTMLElement | null): HTMLElement | null {
  return within?.querySelector<HTMLElement>(`.${PREVIEW_CLASS}`) ?? null;
}

/**
 * The document as the server's markdown reads it, drawn before the editor exists.
 *
 * A large document used to show the reader nothing until its socket had opened, its whole Yjs
 * state had arrived and ProseMirror had built every one of its blocks in one task - 15.6 s on a
 * phone for a document of 406,000 characters, against a target of 5 s (dispatch://INFRA-341,
 * R11). None of that is needed to read it: `GET /artifacts/{id}/text` is the same document, and
 * the read-only headless engine that draws every comment and ask body on the page parses it off
 * the page's own thread and paints it a batch at a time (`refs/useRenderedMarkdown.tsx`,
 * `refs/paint-block.ts`). So the reader reads the document while the editor is still being
 * built, and `ProofDocument` swaps the editor in underneath them when it is.
 *
 * Its blocks carry `data-block-id`, because the headless engine stamps every block it parses
 * (`@legion/proof-editor`'s `block-ids.ts`) and serializes the id into the DOM, as the editor
 * does. Those ids are this parse's own, not the ones the server minted for the live tree: a
 * block link and the margin's placements are the editor's to land, and they still are
 * (`ProofDocument`'s `bindEditor`).
 *
 * It is named for what it is - `Document text`, under the `document` role - and never for the
 * editor: it takes no edits, and a reader whose screen reader announced the document editor
 * would be told they were in something they cannot type in. The page's one editable document
 * is the editor, and nothing reaching for that textbox finds this instead. What reads either
 * surface reads both by name (`e2e/large-documents-page.ts`, which measures the document's
 * first block whichever of the two is drawing it).
 *
 * It draws the document in two passes, because a reader needs the screen in front of them
 * before they need the rest. The first is the blocks the source opens with, cut at a boundary
 * the source itself gives (`leadingBlocks`) and no longer than the page parses in its own
 * commit (`markdownWorkerThreshold`): a screenful and more, parsed and painted in one short
 * task, with no worker in it. The second is the whole document, parsed off the page's thread
 * and painted a batch at a time, which replaces the first draw where it stands - the same
 * blocks in the same order, with the reader held at the one they are reading
 * (`useRenderedMarkdown`'s `keepTheReadersPlace`).
 *
 * Both passes are needed because the second one is slow and may never arrive. Measured on this
 * pod, `iphone` at 4x throttling, the whole 406,000-character document was posted to the parse
 * worker 3.4 s after this element mounted and answered 16 s after that, by which time the
 * editor had replaced the draw: a preview that waited for that parse painted nothing at all,
 * in either document, and the reader's first block was still the editor's at 15.6 s
 * (dispatch://INFRA-341, R11). The first pass costs a parse of 2,000 characters.
 *
 * Neither pass is ever paid for on the reader's own thread. This is the one surface that asks
 * the engine for the worker or nothing (`theWorkerOrNothing`): the queue's deadline is 10 s and
 * the measurement above puts the answer well past it, so the fallback every other body wants -
 * the page parsing it itself - would be a synchronous parse of the whole document in the middle
 * of the very wait this surface exists to shorten. It needs no fallback: the first pass is
 * already drawn in front of the reader and the editor is arriving behind it. For the same
 * reason it keeps no posted parse (`keepAPostedParse: false`): where the editor lands first
 * this element unmounts, and a kept place would have the queue build the whole parsed document
 * on the reader's thread for a paint thrown away in the next line, and hold it in the answer
 * cache afterwards.
 *
 * Until the first pass lands the element holds whatever it holds (`whilePending: "hold"`):
 * nothing, or - where the server drew the document's opening blocks into the page it sent - those
 * blocks, which this element adopts in the commit that mounts it rather than parsing and drawing
 * the same screenful again (`first-screen.ts`). A reader whose page carried a draw has read the
 * document since the page was parsed, which on a phone is seconds before this element exists, so
 * the first pass is skipped for them altogether and the only pass left is the whole document.
 * 2,000 characters of Markdown syntax is no document to read, and the pass that replaces it is a
 * frame away.
 *
 * It stops where the editor's build begins (`building`). The build fills the editor a batch of
 * blocks at a time and hands the thread back between batches
 * (`@legion/proof-editor`'s `progressive-build.ts`), and a second pass painting into those gaps
 * is work for a draw the editor is about to replace: it delays the editor the reader is waiting
 * for by the whole of its own cost. So from the task the build begins in, this element holds
 * what it has already drawn and paints no more - the whole document where that pass had
 * landed, the first screenful where it had not - and the parse it was waiting for is dropped
 * (a parse already posted is the worker's own to finish, and its answer is dropped where it
 * arrives). Nothing goes off the screen: a paint stopped part-way puts back what the element
 * held before it (`useRenderedMarkdown`), which is the pass the reader is reading. A build that
 * fails to produce an editor hands this element its own job back (`ProofDocument`'s
 * `reportLoadFailure`): there is nothing to replace the draw, so it paints the whole document.
 */
export function DocumentPreview({
  address,
  building,
  markdown,
}: {
  /** The address this document is open at, which the server's own draw names: blocks drawn for
   *  one document are never taken by another. */
  address: string;
  /** Whether the editor's build has begun, which is where this draw stops painting. */
  building: boolean;
  markdown: string;
}): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  /** Whether the page carries the blocks the server drew for this address.
   *
   *  Read here, taken in the commit that mounts this element. The reading leaves them on the
   *  screen: a render React yields in the middle of - the issue page's first render is a
   *  transition (`app.tsx`'s `IssuePageInATransition`) - would otherwise take the reader's
   *  document off the page frames before the element that puts it back is in it, and the
   *  reader would watch their first screen go and come back. The take and the append are one
   *  layout effect, so the blocks leave `#first-screen` and land here inside one commit,
   *  before the browser paints either. */
  const [serverDrewTheFirstScreen] = useState(() => firstScreenAwaits(address));
  useLayoutEffect(() => {
    const drawn = takeFirstScreen(address);
    if (drawn !== undefined && root.current !== null) {
      root.current.append(drawn);
    }
  }, [address]);
  const firstScreenful = useMemo(
    () => leadingBlocks(markdown, markdownWorkerThreshold),
    [markdown]
  );
  /** Whether a draw is behind this element, which is what puts the whole document in front of
   *  it: its own first pass, or the blocks the server drew, which are a first pass already made.
   *  A document whose every block fits in the first draw is its own first draw (`leadingBlocks`
   *  answers the source itself), so nothing is parsed twice for it. */
  const [drawnOnce, setDrawnOnce] = useState(serverDrewTheFirstScreen);
  /** The last source a pass of this element finished drawing, which is what it holds once the
   *  build has begun. Read only in the render the build's start causes, so it is never behind;
   *  `undefined` while nothing of this element's own has been drawn - before the first pass
   *  lands, and for a reader whose page carried the server's draw until the whole document
   *  replaces it - and the element then holds what it has rather than drawing anything. */
  const drawn = useRef<string | undefined>(serverDrewTheFirstScreen ? undefined : firstScreenful);
  const source = !drawnOnce ? firstScreenful : building ? drawn.current : markdown;
  /** Whether this element draws nothing for now and keeps what is in front of the reader: the
   *  server's blocks, until the document's text has arrived and been parsed, and whatever it
   *  holds from the task the editor's build begins in. */
  const holding = source === undefined || source === "";
  // The document's own parse policies, which are this hook's defaults: a single newline inside a
  // paragraph is a space (hard-wrapped Markdown is one paragraph), and a line of raw HTML leaves
  // the source to the literal-text fallback rather than drawing as text.
  const { isFallback, isPending, portals } = useRenderedMarkdown(root, source ?? "", paintBlock, {
    holding,
    keepAPostedParse: false,
    onRendered: () => {
      drawn.current = source;
      setDrawnOnce(true);
    },
    paintInSlices: paintBlockInSlices,
    theWorkerOrNothing: true,
    whilePending: "hold",
  });

  return (
    <div
      aria-label="Document text"
      className={`${markdownClassName} ${PREVIEW_CLASS} max-w-none`}
      role="document"
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      ref={root}
    >
      {portals}
    </div>
  );
}
