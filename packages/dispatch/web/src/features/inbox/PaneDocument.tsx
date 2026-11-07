/**
 * The frame the Inbox's reading column draws in, and the live document it opens at one block.
 *
 * Every flow of the column - a question's (`ReadingPane.tsx`) and a thread's (`ThreadPane.tsx`)
 * - draws under `PaneFrame` with only its own reads and its own effects, and the two that show
 * a document beside the list are one `DocumentPane` over `useDocumentAtBlock`: a question
 * written as a decision block and a comment thread anchored in a project document are the same
 * placement problem, and the same pane.
 */

import { queryOptions, useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useCallback, useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { issueQuery, whoAmIQuery } from "../../api/queries";
import type { Artifact, AuthenticatedUser } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { focusVisibleRing, quoteAccentBorder } from "../../theme/classes";
import { liveTextQueryOptions, ProofDocument, type ProofDocumentProps } from "../doc/ProofDocument";
import { BLOCK_ID_ATTRIBUTE, scrollBlockToTop } from "../doc/scroll-block";
import { holdSection, type SectionHold } from "./pane-placement";

/**
 * How long the column waits after the last change of the selected question before it opens
 * another document. Each document mount opens a websocket, a Y.Doc and an editor, so a reader
 * holding `j` down a list of thirty questions opens one room, not thirty.
 */
const DOCUMENT_SETTLE_MS = 200;

/** The document the column shows, under the document page's own key. */
export const artifactQueryOptions = (artifactId: string) =>
  queryOptions({
    queryFn: () => api.getArtifact(artifactId),
    queryKey: ["artifact", artifactId],
  });

/** Where the section bar stands in the column's own scrolled content, in pixels. */
interface SectionBar {
  height: number;
  top: number;
}

function blockElement(root: ParentNode, blockId: string): HTMLElement | null {
  return root.querySelector<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`);
}

/**
 * The heading the question's section starts at: the nearest heading of any level before the
 * question's block. The plan reads it off the loaded ProseMirror document; a pane holds no
 * editor handle, so it reads the blocks the editor drew, which stand in the same order. A block
 * before any heading has no section, and the column then scrolls to the top and draws no bar.
 */
function sectionHeadingId(content: HTMLElement, blockId: string): string | undefined {
  const blocks = [...content.querySelectorAll<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}]`)];
  const at = blocks.findIndex((block) => block.dataset.blockId === blockId);
  for (let index = at - 1; index >= 0; index -= 1) {
    const block = blocks[index];
    if (/^H[1-6]$/.test(block.tagName)) {
      return block.dataset.blockId;
    }
  }
  return undefined;
}

/** The bar that marks the section down to the decision block: where the section's heading
 *  stands in the column's own scrolled content, and from there to the foot of that block. Both
 *  are read as single rects - the margin's `blockOffsets` measures every block in the document,
 *  and this runs on every batch of mutations a live document makes. */
function sectionBarOf(
  content: HTMLElement,
  headingId: string | undefined,
  block: HTMLElement
): SectionBar | undefined {
  if (headingId === undefined) {
    return undefined;
  }
  const heading = blockElement(content, headingId);
  if (heading === null) {
    return undefined;
  }
  const contentTop = content.getBoundingClientRect().top;
  const top = heading.getBoundingClientRect().top - contentTop;
  const blockRect = block.getBoundingClientRect();
  return { height: blockRect.top - contentTop - top + blockRect.height, top };
}

function sameBar(left: SectionBar | undefined, right: SectionBar | undefined): boolean {
  return left?.height === right?.height && left?.top === right?.top;
}

/** Whether the column has settled on this document: it follows the selection only once the
 *  selection has stood still for `DOCUMENT_SETTLE_MS`, and two questions in one document never
 *  change it. */
export function useSettled(artifactId: string): boolean {
  const [settled, setSettled] = useState<string | undefined>(undefined);
  useEffect(() => {
    if (settled === artifactId) {
      return;
    }
    const timer = window.setTimeout(() => setSettled(artifactId), DOCUMENT_SETTLE_MS);
    return () => window.clearTimeout(timer);
  }, [artifactId, settled]);
  return settled === artifactId;
}

/** The column's frame: the head above the one scroller every flow draws in, with the section
 *  bar beside the drawn content. The refs are the flow's, which measures and scrolls what it
 *  drew; the card flow measures nothing and passes none. */
export function PaneFrame({
  children,
  content,
  head,
  label = "Selected question",
  scroller,
  sectionBar,
}: {
  children: ReactNode;
  content?: RefObject<HTMLDivElement | null>;
  head: ReactNode;
  /** What the column is of: a question, or a thread of the reader's own. */
  label?: string;
  scroller?: RefObject<HTMLElement | null>;
  sectionBar?: SectionBar;
}): ReactNode {
  return (
    <section
      aria-label={label}
      className="flex h-full min-h-0 flex-col gap-3"
      data-testid="inbox-pane"
      id="inbox-pane"
    >
      {head}
      {/* The one scroller every flow draws in is a focus stop of its own, so a reader who has
          read the head can hand the keyboard to the document. Nothing here scrolls it: the
          browser scrolls whichever focusable element holds the focus on the arrow keys, Page
          Up, Page Down, Space, Home and End, so this writes no scrolling code. A labelled
          `<section>` is `role="region"`, and its name is neither a superset nor a subset of
          the enclosing section's name, since Playwright matches an accessible name by
          substring: each of the two names matches only itself. */}
      <section
        aria-label="Question body"
        className={`min-h-0 flex-1 overflow-y-auto outline-none focus-visible:ring-2 ${focusVisibleRing}`}
        data-testid="inbox-pane-scroller"
        ref={scroller}
        // biome-ignore lint/a11y/noNoninteractiveTabindex: A region is not interactive, and this one is deliberately in the tab order: it is the column's one focus stop, and the keyboard reaches the document through it.
        tabIndex={0}
      >
        {/* The bar is drawn beside the editor rather than inside it: a class Dispatch writes into
            editor DOM is read back as a document edit. */}
        <div className="relative">
          {sectionBar === undefined ? null : (
            <div
              className={`pointer-events-none absolute left-0 border-l-2 ${quoteAccentBorder}`}
              data-testid="pane-section-bar"
              style={{ height: `${sectionBar.height}px`, top: `${sectionBar.top}px` }}
            />
          )}
          <div className="pl-3" ref={content}>
            {children}
          </div>
        </div>
      </section>
    </section>
  );
}

/** What a column showing a document at one block holds: the refs it measures and scrolls, the
 *  document itself, where the section bar stands, and whether it could be placed at all. */
interface DocumentAtBlock {
  /** The document, once the read lands. */
  readonly artifact: Artifact | undefined;
  /** Whether the editor has drawn the block the column is opened at; undefined while it has
   *  drawn no block at all, and false once it has drawn a document holding no such block. */
  readonly blockFound: boolean | undefined;
  readonly content: RefObject<HTMLDivElement | null>;
  /** The document cannot be opened beside the list: the read failed, or its own admission did. */
  readonly failed: boolean;
  readonly isClosed: boolean;
  readonly onUnavailable: () => void;
  /** Every read the document needs has landed, so it can be drawn. */
  readonly ready: boolean;
  readonly scroller: RefObject<HTMLDivElement | null>;
  readonly sectionBar: SectionBar | undefined;
  /** The heading of the section the column is opened at, once the block has drawn. */
  readonly sectionBlockId: string | undefined;
  readonly showsDocument: boolean;
  /** Puts the column back at the block it was opened for. */
  readonly skipToBlock: () => void;
  readonly user: AuthenticatedUser | undefined;
}

/**
 * The live document opened at one block: the read, the placement, the section bar and the
 * fall-back every body that shows a document beside the list shares.
 *
 * The column follows the selection only once it has settled (`useSettled`), reads the document
 * under the document page's own keys, and reports whether the editor has drawn the block it was
 * opened at: a question written as a decision block, and a comment thread anchored in a project
 * document, are the same placement problem, and this is the one answer to it.
 */
function useDocumentAtBlock({
  artifactId,
  blockId,
  issueKey,
}: {
  artifactId: string;
  blockId: string;
  issueKey: string | null;
}): DocumentAtBlock {
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const [sectionBar, setSectionBar] = useState<SectionBar | undefined>(undefined);
  const [sectionBlockId, setSectionBlockId] = useState<string | undefined>(undefined);
  // Whether the editor has drawn the block the column is opened at; undefined while it has drawn
  // no blocks at all. A document that holds no such block is an anchor the column cannot place,
  // which is what its host falls back from.
  const [blockFound, setBlockFound] = useState<boolean | undefined>(undefined);
  // This column's hold on its own placement, while one is on: the effect below takes it, and
  // "Skip to the answer" - a placement of the reader's own, pressed in the head rather than in
  // the column - lets it go.
  const hold = useRef<SectionHold | undefined>(undefined);
  const settled = useSettled(artifactId);

  const user = useQuery(whoAmIQuery()).data;
  const issue = useQuery({
    ...issueQuery(issueKey ?? undefined),
    enabled: issueKey !== null,
  });
  const artifactQuery = useQuery({ ...artifactQueryOptions(artifactId), enabled: settled });
  // The read the document page's own editor is admitted by: the server answers it `409
  // DOC_SCHEMA` for a tree outside the Proof schema, and the page offers a repair or a rebuild
  // for what it says. A column beside a list offers neither, so it reads the text itself, under
  // the page's own key, and falls back to the card rather than mounting a document that cannot
  // open.
  const textQuery = useQuery({ ...liveTextQueryOptions(artifactId), enabled: settled });

  const artifact = artifactQuery.data;
  // A document whose own admission failed after this column's read succeeded falls back too: the
  // page answers that with a repair or a rebuild prompt, which a column beside a list never
  // shows, so the document reports it here instead (`onUnavailable`). The report names the
  // document it is about, so selecting another row is not answered with the last one's failure.
  const [unavailable, setUnavailable] = useState<string | undefined>(undefined);
  const failed = artifactQuery.isError || textQuery.isError || unavailable === artifactId;
  const showsDocument = !failed && artifact !== undefined;

  // The editor draws its blocks after the socket syncs, and a diagram or a picture moves them
  // again afterwards, so the placement runs on every change of the drawn document: the scroll
  // for as long as the column is the reader's to place (`holdSection`), and the bar whenever
  // the blocks move.
  useEffect(() => {
    setSectionBar(undefined);
    setSectionBlockId(undefined);
    setBlockFound(undefined);
    const column = scroller.current;
    const drawn = content.current;
    if (column === null || drawn === null || !showsDocument) {
      return;
    }
    // One scroll is never enough for a section deep in a document: the editor draws its blocks
    // in pieces, so the first scroll is clamped by a column still shorter than the document,
    // and every block that lands above the heading afterwards pushes it down again. So the
    // column is put back at its section on every change until the reader moves it, which is
    // the only thing that ends the placement (`pane-placement.ts`).
    const held = holdSection(column);
    hold.current = held;
    const place = () => {
      const block = blockElement(drawn, blockId);
      if (block === null) {
        // Nothing drawn yet says nothing about the block; a drawn document without it does.
        setBlockFound((current) =>
          drawn.querySelector(`[${BLOCK_ID_ATTRIBUTE}]`) === null ? current : false
        );
        return;
      }
      setBlockFound(true);
      const headingId = sectionHeadingId(drawn, blockId);
      // Which section the column is opened at is known as soon as the block is drawn, whether
      // or not the scroll has got there yet: it is what the head's "Open page" opens at.
      setSectionBlockId(headingId);
      if (held.held()) {
        if (headingId === undefined) {
          column.scrollTop = 0;
        } else {
          scrollBlockToTop(headingId, column);
        }
      }
      const bar = sectionBarOf(drawn, headingId, block);
      setSectionBar((current) => (sameBar(current, bar) ? current : bar));
    };
    place();
    // The editor moves its blocks by more than it adds them: a picture or a diagram that draws
    // where a node view already stood changes no child of `drawn`, so attributes and text are
    // watched beside them.
    const drawing = new MutationObserver(place);
    drawing.observe(drawn, {
      attributes: true,
      characterData: true,
      childList: true,
      subtree: true,
    });
    const resizing = new ResizeObserver(place);
    resizing.observe(drawn);
    return () => {
      held.release();
      hold.current = undefined;
      drawing.disconnect();
      resizing.disconnect();
    };
  }, [blockId, showsDocument]);

  const skipToBlock = useCallback(() => {
    const column = scroller.current;
    if (column !== null) {
      // The reader's own placement: the column is theirs from here, so the section's is over.
      hold.current?.release();
      scrollBlockToTop(blockId, column);
    }
  }, [blockId]);

  return {
    artifact,
    blockFound,
    content,
    failed,
    isClosed: issue.data !== undefined && issue.data.closed_at !== null,
    onUnavailable: () => setUnavailable(artifactId),
    ready: settled && artifact !== undefined && user !== undefined && textQuery.data !== undefined,
    scroller,
    sectionBar,
    sectionBlockId,
    showsDocument,
    skipToBlock,
    user,
  };
}

/** The live document in the column, with whatever its host draws under one of its blocks. */
function PaneDocument({
  anchoredThread,
  artifact,
  document,
  onAskAnswered,
  user,
}: {
  anchoredThread: ProofDocumentProps["anchoredThread"];
  artifact: Artifact;
  document: DocumentAtBlock;
  onAskAnswered: (askId: string) => void;
  user: AuthenticatedUser;
}): ReactNode {
  return (
    <ProofDocument
      anchoredThread={anchoredThread}
      artifact={artifact}
      highlight={undefined}
      isClosed={document.isClosed}
      key={artifact.id}
      onAskAnswered={onAskAnswered}
      onUnavailable={document.onUnavailable}
      onVersionChange={noVersionChange}
      owner={
        artifact.issue_key === null
          ? {
              artifactId: artifact.id,
              kind: "document",
              project: artifact.project,
              slug: artifact.slug,
            }
          : { kind: "issue", key: artifact.issue_key }
      }
      showDiff={false}
      surface="pane"
      user={user}
    />
  );
}

/** The column carries no version picker: which version it shows is the question's, never the
 *  reader's. */
function noVersionChange(): void {}

/**
 * The body the column draws in a document: the live document opened at one block, under the
 * host's own head, with what it falls back to where the document cannot be opened or the block
 * cannot be placed.
 *
 * The two hosts draw the same pane - a question written as a decision block
 * (`ReadingPane.tsx`) and a thread in its own document (`ThreadPane.tsx`) - and differ only in
 * what they put in it: the head, what the frame is of, what it falls back to, the name the
 * skeleton opens, and whose slot the document carries, the ask's answer or the thread.
 */
export function DocumentPane({
  anchoredThread,
  artifactId,
  blockId,
  fallback,
  head,
  issueKey,
  label,
  loadingName,
  onAskAnswered,
  paneBody,
}: {
  /** The thread drawn under its own block, for a comment thread anchored in the document; a
   *  decision block's ask needs none, because the block draws its own question and thread. A
   *  host that passes one falls back where the document holds no such block: an anchor the
   *  column cannot place leaves the thread nowhere to be drawn. */
  anchoredThread: ProofDocumentProps["anchoredThread"];
  artifactId: string;
  blockId: string;
  /** What the column draws in this frame instead of the document: the question's own card, or
   *  the thread alone with the text it is anchored to, marked as the host marks it. */
  fallback: ReactNode;
  /** The head above the document, given what the column found in it. */
  head: (document: DocumentAtBlock) => ReactNode;
  issueKey: string | null;
  /** What the frame is of, where it is not a question (`PaneFrame`). */
  label?: string;
  /** The document the skeleton says it is opening. */
  loadingName: string;
  onAskAnswered: (askId: string) => void;
  /** The `data-inbox-pane-body` mark the thread column puts on what it drew
   *  (`ThreadPane.tsx`); a question's body carries none. */
  paneBody?: "ask" | "document";
}): ReactNode {
  const document = useDocumentAtBlock({ artifactId, blockId, issueKey });
  const { artifact, blockFound, failed, user } = document;

  const body = (): ReactNode => {
    if (failed || (anchoredThread !== undefined && blockFound === false)) {
      return fallback;
    }
    const drawn =
      !document.ready || artifact === undefined || user === undefined ? (
        <LoadingSkeleton label={`Opening ${loadingName}`} />
      ) : (
        <PaneDocument
          anchoredThread={anchoredThread}
          artifact={artifact}
          document={document}
          onAskAnswered={onAskAnswered}
          user={user}
        />
      );
    return paneBody === undefined ? drawn : <div data-inbox-pane-body={paneBody}>{drawn}</div>;
  };

  return (
    <PaneFrame
      content={document.content}
      head={head(document)}
      label={label}
      scroller={document.scroller}
      sectionBar={document.sectionBar}
    >
      {body()}
    </PaneFrame>
  );
}
