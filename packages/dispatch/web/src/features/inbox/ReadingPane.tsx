import { queryOptions, useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import { askThreadQuery, issueQuery, whoAmIQuery } from "../../api/queries";
import type {
  Artifact,
  AskApproval,
  AuthenticatedUser,
  InboxRow,
  MyReplyRow,
} from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { quoteAccentBorder, textPrimaryOnCanvas } from "../../theme/classes";
import { versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { CHANGED_BLOCK_CLASS } from "../doc/change-marks";
import { liveTextQueryOptions, ProofDocument, type ProofDocumentProps } from "../doc/ProofDocument";
import { DocumentRuntime } from "../doc/runtime";
import { loadBlockSchema } from "../doc/schema";
import { BLOCK_ID_ATTRIBUTE, scrollBlockToTop, scrollElementToTop } from "../doc/scroll-block";
import { VersionView } from "../doc/VersionView";
import { AskCard } from "./AskCard";
import { isDecisionAsk } from "./ask-decision";
import type { InboxListRow } from "./list-row";
import { holdSection, type SectionHold } from "./pane-placement";
import { questionLine } from "./question-line";
import { ReadingPaneHead, ThreadPaneHead } from "./ReadingPaneHead";
import { ThreadBody } from "./ThreadBody";

/**
 * How long the column waits after the last change of the selected question before it opens
 * another document. Each document mount opens a websocket, a Y.Doc and an editor, so a reader
 * holding `j` down a list of thirty questions opens one room, not thirty.
 */
const DOCUMENT_SETTLE_MS = 200;

/** What the selected question is, which is what the column shows. */
type PaneSubject =
  | { kind: "approval"; approval: AskApproval; artifactId: string }
  | { kind: "decision"; artifactId: string; blockId: string }
  | { kind: "card" };

/** An approval request is about its document's latest version; a question written as a decision
 *  block is about the document it sits in. An anchored question, and a question in no document,
 *  are the card the Inbox has always drawn. */
function subjectOf(ask: InboxRow): PaneSubject {
  if (ask.kind === "approval" && ask.approval !== undefined) {
    return { approval: ask.approval, artifactId: ask.approval.artifact_id, kind: "approval" };
  }
  if (isDecisionAsk(ask)) {
    return { artifactId: ask.block_artifact.id, blockId: ask.block_id, kind: "decision" };
  }
  return { kind: "card" };
}

/** The document the column shows, under the document page's own key. */
const artifactQueryOptions = (artifactId: string) =>
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
function useSettled(artifactId: string): boolean {
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

export interface ReadingPaneProps {
  /** The reader's own answer landed; the list selects the next row. */
  onAnswered: (askId: string) => void;
  /** The head's Snooze write is in flight or refused, or has stopped being either: the Inbox
   *  keeps a live row listed, which is what keeps this column on it long enough for a refusal
   *  to be read. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** The selected row: an open ask, or a thread of the reader's own. */
  row: InboxListRow;
  viewer: string | undefined;
}

/** What every flow's head is given; each adds the controls that are its own. */
interface PaneFlowProps {
  ask: InboxRow;
  onAnswered: (askId: string) => void;
  onSnoozeLive: (askId: string, live: boolean) => void;
  viewer: string | undefined;
}

/** The column's frame: the head above the one scroller every flow draws in, with the section
 *  bar beside the drawn content. The refs are the flow's, which measures and scrolls what it
 *  drew; the card flow measures nothing and passes none. */
function PaneFrame({
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
  scroller?: RefObject<HTMLDivElement | null>;
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
      <div className="min-h-0 flex-1 overflow-y-auto" ref={scroller}>
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
      </div>
    </section>
  );
}

/** The question's own card, which the column draws for a question in no document and for one
 *  whose document it could not open. One card per question: the card holds the answer the
 *  reader gave in it, so the next question selected is a card of its own rather than this one
 *  showing that answer. */
function QuestionCard({ ask, onAnswered }: Pick<PaneFlowProps, "ask" | "onAnswered">): ReactNode {
  return (
    <AskCard
      ask={ask}
      initialThread={{ ask, ...ask.thread }}
      key={ask.id}
      onAnswered={onAnswered}
      variant="full"
    />
  );
}

/** An anchored question, and a question in no document: the card the Inbox has always drawn,
 *  under the head's own controls. */
function CardBody({ ask, onAnswered, onSnoozeLive, viewer }: PaneFlowProps): ReactNode {
  return (
    <PaneFrame
      head={
        <ReadingPaneHead
          artifact={undefined}
          ask={ask}
          onAnswered={onAnswered}
          onNextChange={undefined}
          onSkipToAnswer={undefined}
          onSnoozeLive={onSnoozeLive}
          sectionBlockId={undefined}
          viewer={viewer}
        />
      }
    >
      <QuestionCard ask={ask} onAnswered={onAnswered} />
    </PaneFrame>
  );
}

/** Whether the version drawn in `content` carries blocks marked as changed. The marks are drawn
 *  into the editor after the headless engine warms, and `changeMarksAgainst` marks nothing for
 *  an engine that never warmed or refused the baseline, so "Next change" is offered for what is
 *  drawn rather than for a baseline that merely loaded. The column keeps one watch on the frame
 *  it draws into: another question replaces the blocks under it, which is a change like any
 *  other and is read the same way. */
function useMarkedBlocks(content: RefObject<HTMLDivElement | null>): boolean {
  const [marked, setMarked] = useState(false);
  useEffect(() => {
    const drawn = content.current;
    if (drawn === null) {
      return;
    }
    const read = () => setMarked(drawn.querySelector(`.${CHANGED_BLOCK_CLASS}`) !== null);
    read();
    // A decoration lands either as a class on a block already drawn or with the blocks the
    // editor draws when it mounts, so both are watched.
    const watching = new MutationObserver(read);
    watching.observe(drawn, {
      attributeFilter: ["class"],
      attributes: true,
      childList: true,
      subtree: true,
    });
    return () => watching.disconnect();
  }, [content]);
  return marked;
}

/** An approval request: the document's latest version - the version Approve approves - with
 *  every word changed since the last approval marked, and "Next change" stepping the marks. */
function ApprovalBody({
  approval,
  artifactId,
  ask,
  onAnswered,
  onSnoozeLive,
  viewer,
}: PaneFlowProps & { approval: AskApproval; artifactId: string }): ReactNode {
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  /** Where "Next change" stands in the marked blocks, and the question it stands in: another
   *  question is read from its first marked block, not from where the last one was left. */
  const nextChangeAt = useRef({ askId: ask.id, at: 0 });
  const settled = useSettled(artifactId);
  const approvedVersion = approval.approved?.version;

  const { blockSchema: runtimeBlockSchema } = useContext(DocumentRuntime);
  const blockSchemaQuery = useQuery({
    queryFn: loadBlockSchema,
    queryKey: ["block-schema"],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const blockSchema = runtimeBlockSchema ?? blockSchemaQuery.data;
  const user = useQuery(whoAmIQuery()).data;
  const artifactQuery = useQuery({ ...artifactQueryOptions(artifactId), enabled: settled });
  // The version the request is marked against: the document's last approval by any person, read
  // under the document page's own version key, as today's card reads it (`EarlierApproval`).
  const baselineQuery = useQuery({
    enabled: settled && approvedVersion !== undefined,
    queryFn: () => {
      if (approvedVersion === undefined) {
        throw new Error("A baseline read needs the approved version.");
      }
      return api.getArtifactVersion(artifactId, approvedVersion);
    },
    queryKey: ["artifact", artifactId, "version", approvedVersion],
  });

  const artifact = artifactQuery.data;
  const changeBaseline = isText(baselineQuery.data) ? baselineQuery.data.markdown : undefined;
  const latestVersion =
    artifact === undefined ? undefined : versionsNewestFirst(artifact.versions)[0]?.number;
  // A document the column cannot read, and a request on a document that holds no version to
  // show, both fall back to the card.
  const failed = artifactQuery.isError || (artifact !== undefined && latestVersion === undefined);
  const marked = useMarkedBlocks(content);

  const askId = ask.id;
  /** The next block the version marks as changed, wrapping at the end of the document. */
  const nextChange = useCallback(() => {
    const column = scroller.current;
    const drawn = content.current;
    if (column === null || drawn === null) {
      return;
    }
    if (nextChangeAt.current.askId !== askId) {
      nextChangeAt.current = { askId, at: 0 };
    }
    const found = drawn.querySelectorAll(`.${CHANGED_BLOCK_CLASS}`);
    const block = found[nextChangeAt.current.at % found.length];
    if (block === undefined) {
      return;
    }
    nextChangeAt.current = { askId, at: (nextChangeAt.current.at + 1) % found.length };
    scrollElementToTop(block, column);
  }, [askId]);

  const body = (): ReactNode => {
    if (failed) {
      return <QuestionCard ask={ask} onAnswered={onAnswered} />;
    }
    const skeleton = <LoadingSkeleton label={`Opening ${approval.name}`} />;
    if (
      !settled ||
      artifact === undefined ||
      user === undefined ||
      latestVersion === undefined ||
      (approvedVersion !== undefined && baselineQuery.isPending)
    ) {
      return skeleton;
    }
    return (
      <VersionView
        artifactId={artifact.id}
        // The pane draws no ask row over a stored version: the blocks of the version carry what
        // was decided in them, and a column the reader flips through with `j` reads no list of
        // the document's asks to name their askers.
        asks={[]}
        blockSchema={blockSchema}
        changeBaseline={changeBaseline}
        createdAt={
          artifact.versions.find((version) => version.number === latestVersion)?.created_at
        }
        highlight={undefined}
        owner={
          artifact.issue_key === null
            ? { project: artifact.project, slug: artifact.slug }
            : undefined
        }
        version={latestVersion}
      />
    );
  };

  return (
    <PaneFrame
      content={content}
      head={
        <ReadingPaneHead
          artifact={artifact}
          ask={ask}
          onAnswered={onAnswered}
          onNextChange={marked ? nextChange : undefined}
          onSkipToAnswer={undefined}
          onSnoozeLive={onSnoozeLive}
          sectionBlockId={undefined}
          viewer={viewer}
        />
      }
      scroller={scroller}
    >
      {body()}
    </PaneFrame>
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

/** A question written as a decision block: the live document, drawn by the document page's own
 *  component in its pane surface, scrolled so that the heading of the question's section sits
 *  at the top of the column, with a bar marking the section down to the block. */
function DecisionBody({
  artifactId,
  ask,
  blockId,
  onAnswered,
  onSnoozeLive,
  viewer,
}: PaneFlowProps & { artifactId: string; blockId: string }): ReactNode {
  const document = useDocumentAtBlock({ artifactId, blockId, issueKey: ask.issue_key });
  const { artifact, failed, showsDocument, user } = document;

  const body = (): ReactNode => {
    if (failed) {
      return <QuestionCard ask={ask} onAnswered={onAnswered} />;
    }
    const name = ask.approval?.name ?? ask.document?.name ?? "the document";
    if (!document.ready || artifact === undefined || user === undefined) {
      return <LoadingSkeleton label={`Opening ${name}`} />;
    }
    return (
      <PaneDocument
        anchoredThread={undefined}
        artifact={artifact}
        document={document}
        onAskAnswered={onAnswered}
        user={user}
      />
    );
  };

  return (
    <PaneFrame
      content={document.content}
      head={
        <ReadingPaneHead
          artifact={artifact}
          ask={ask}
          onAnswered={onAnswered}
          onNextChange={undefined}
          onSkipToAnswer={showsDocument ? document.skipToBlock : undefined}
          onSnoozeLive={onSnoozeLive}
          sectionBlockId={document.sectionBlockId}
          viewer={viewer}
        />
      }
      scroller={document.scroller}
      sectionBar={document.sectionBar}
    >
      {body()}
    </PaneFrame>
  );
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

/** The subject line a thread body opens with: what the thread is about, in one line. */
function ThreadSubject({ thread }: { thread: MyReplyRow }): ReactNode {
  return (
    <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`} data-inbox-thread-subject="">
      {questionLine(thread.subject)}
    </h2>
  );
}

/** Which of the column's four thread bodies this is, as the pane marks it. */
type ThreadPaneBody = "agent" | "ask" | "document" | "issue";

/** A thread that needs no document: a comment thread on an issue, an issue message
 *  conversation, an ask that is not a decision block, and a thread whose document the column
 *  could not open, which shows the quote the thread is anchored to in place of it. */
function ThreadBodyPane({
  body,
  fallback = false,
  quote,
  thread,
}: {
  body: ThreadPaneBody;
  /** Whether this is the document body falling back to the thread alone. */
  fallback?: boolean;
  quote: boolean;
  thread: MyReplyRow;
}): ReactNode {
  return (
    <PaneFrame head={<ThreadPaneHead thread={thread} />} label="Selected thread">
      <div data-inbox-pane-body={body} data-inbox-pane-fallback={fallback ? "" : undefined}>
        <ThreadSubject thread={thread} />
        <ThreadBody openAtFirstUnread quote={quote} thread={thread} />
      </div>
    </PaneFrame>
  );
}

/**
 * A comment thread anchored in a project document: the live document, opened at the section
 * that holds the anchored block, with the quote highlighted and the thread drawn inside the
 * document column directly below that block, through the node view's own slot.
 *
 * It falls back to the thread alone, with its quote, when the document cannot be opened and when
 * the document holds no such block - an orphaned anchor, or one naming a block that has gone.
 */
function DocumentThreadBody({
  artifactId,
  blockId,
  thread,
}: {
  artifactId: string;
  blockId: string;
  thread: MyReplyRow;
}): ReactNode {
  const document = useDocumentAtBlock({ artifactId, blockId, issueKey: null });
  const { artifact, blockFound, failed, user } = document;
  if (failed || blockFound === false) {
    return <ThreadBodyPane body="document" fallback quote thread={thread} />;
  }
  const body = (): ReactNode => {
    if (!document.ready || artifact === undefined || user === undefined) {
      return <LoadingSkeleton label={`Opening ${thread.document?.name ?? "the document"}`} />;
    }
    return (
      <PaneDocument
        anchoredThread={{
          blockId,
          children: <ThreadBody openAtFirstUnread thread={thread} />,
          markId: thread.anchor?.mark_id ?? null,
        }}
        artifact={artifact}
        document={document}
        onAskAnswered={noAnswer}
        user={user}
      />
    );
  };
  return (
    <PaneFrame
      content={document.content}
      head={<ThreadPaneHead sectionBlockId={document.sectionBlockId} thread={thread} />}
      label="Selected thread"
      scroller={document.scroller}
      sectionBar={document.sectionBar}
    >
      <div data-inbox-pane-body="document">{body()}</div>
    </PaneFrame>
  );
}

/** The thread of an ask. A decision block's ask opens its document at its section, where the
 *  block hosts its own answer form and exchange, as an open question's does; every other ask
 *  shows its card, with its answer and its thread. */
function AskThreadPane({ thread }: { thread: MyReplyRow }): ReactNode {
  // The card's own key, so the card below reads no second copy of the same thread.
  const read = useQuery(askThreadQuery(thread.thread_id));
  const ask = read.data?.ask;
  if (ask !== undefined && isDecisionAsk(ask)) {
    return (
      <AskDocumentBody
        artifactId={ask.block_artifact.id}
        blockId={ask.block_id}
        issueKey={ask.issue_key}
        thread={thread}
      />
    );
  }
  return <ThreadBodyPane body="ask" quote={false} thread={thread} />;
}

/** The document of a decision block's ask, opened at its section: the block draws the question,
 *  its answer and its thread itself (`AskBlockCard`). */
function AskDocumentBody({
  artifactId,
  blockId,
  issueKey,
  thread,
}: {
  artifactId: string;
  blockId: string;
  issueKey: string | null;
  thread: MyReplyRow;
}): ReactNode {
  const document = useDocumentAtBlock({ artifactId, blockId, issueKey });
  const { artifact, failed, user } = document;
  if (failed) {
    return <ThreadBodyPane body="ask" fallback quote thread={thread} />;
  }
  return (
    <PaneFrame
      content={document.content}
      head={<ThreadPaneHead sectionBlockId={document.sectionBlockId} thread={thread} />}
      label="Selected thread"
      scroller={document.scroller}
      sectionBar={document.sectionBar}
    >
      <div data-inbox-pane-body="ask">
        {!document.ready || artifact === undefined || user === undefined ? (
          <LoadingSkeleton label={`Opening ${thread.document?.name ?? "the document"}`} />
        ) : (
          <PaneDocument
            anchoredThread={undefined}
            artifact={artifact}
            document={document}
            onAskAnswered={noAnswer}
            user={user}
          />
        )}
      </div>
    </PaneFrame>
  );
}

/** The reading column for a Reply or an Agent row: one body per shape the thread comes in. */
function ThreadPane({ thread }: { thread: MyReplyRow }): ReactNode {
  if (thread.kind === "ask") {
    return <AskThreadPane thread={thread} />;
  }
  const anchor = thread.anchor;
  const blockId = anchor?.block_id;
  if (
    thread.kind === "comment" &&
    thread.document !== undefined &&
    anchor !== undefined &&
    !anchor.orphaned &&
    blockId !== null &&
    blockId !== undefined
  ) {
    return <DocumentThreadBody artifactId={anchor.artifact_id} blockId={blockId} thread={thread} />;
  }
  // A thread in a document whose anchor cannot be placed - orphaned, or naming no block - is the
  // document body falling back to the thread alone, with its quote.
  const inDocument = thread.kind === "comment" && thread.document !== undefined;
  return (
    <ThreadBodyPane
      body={thread.kind === "agent" ? "agent" : inDocument ? "document" : "issue"}
      fallback={inDocument}
      quote={inDocument}
      thread={thread}
    />
  );
}

/**
 * The Inbox's reading column: what the selected row is, where it is answered.
 *
 * A question is one of three flows, each with only its own reads and its own effects under the
 * one head: a question written as a decision block (`DecisionBody`), an approval request
 * (`ApprovalBody`), and a question in no document (`CardBody`), which is what a question whose
 * document could not be opened falls back to as well — the column never shows the document
 * page's rebuild or repair prompts. A Reply or an Agent row is one of four thread bodies
 * (`ThreadPane`), which fall back the same way.
 */
export function ReadingPane({
  onAnswered,
  onSnoozeLive,
  row,
  viewer,
}: ReadingPaneProps): ReactNode {
  if (row.kind === "thread") {
    return <ThreadPane thread={row.thread} />;
  }
  const ask = row.ask;
  const subject = subjectOf(ask);
  const flow = { ask, onAnswered, onSnoozeLive, viewer };
  if (subject.kind === "approval") {
    return <ApprovalBody {...flow} approval={subject.approval} artifactId={subject.artifactId} />;
  }
  if (subject.kind === "decision") {
    return <DecisionBody {...flow} artifactId={subject.artifactId} blockId={subject.blockId} />;
  }
  return <CardBody {...flow} />;
}

/** A thread's document answers nothing of the column's: an ask answered in one of its blocks is
 *  the block's own business, and the list reads its rows again on the event. */
function noAnswer(): void {}

/** The column carries no version picker: which version it shows is the question's, never the
 *  reader's. */
function noVersionChange(): void {}
