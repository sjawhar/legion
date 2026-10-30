import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useContext, useEffect, useRef, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import { issueQuery, whoAmIQuery } from "../../api/queries";
import type { AskApproval, InboxRow } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { quoteAccentBorder } from "../../theme/classes";
import { versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { CHANGED_BLOCK_CLASS } from "../doc/change-marks";
import { ProofDocument } from "../doc/ProofDocument";
import { DocumentRuntime } from "../doc/runtime";
import { loadBlockSchema } from "../doc/schema";
import { BLOCK_ID_ATTRIBUTE, scrollBlockToTop, scrollElementToTop } from "../doc/scroll-block";
import { VersionView } from "../doc/VersionView";
import { AskCard } from "./AskCard";
import { isDecisionAsk } from "./ask-decision";
import { ReadingPaneHead } from "./ReadingPaneHead";

/**
 * How long the column waits after the last change of the selected question before it opens
 * another document. Each document mount opens a websocket, a Y.Doc and an editor, so a reader
 * holding `j` down a list of thirty questions opens one room, not thirty.
 */
const DOCUMENT_SETTLE_MS = 200;

/** What the selected question is, which is what the column shows. */
type PaneSubject =
  | { kind: "approval"; approval: AskApproval }
  | { kind: "decision"; artifactId: string; blockId: string }
  | { kind: "card" };

/** An approval request is about its document's latest version; a question written as a decision
 *  block is about the document it sits in. An anchored question, and a question in no document,
 *  are the card the Inbox has always drawn. */
function subjectOf(ask: InboxRow): PaneSubject {
  if (ask.kind === "approval" && ask.approval !== undefined) {
    return { approval: ask.approval, kind: "approval" };
  }
  if (isDecisionAsk(ask)) {
    return { artifactId: ask.block_artifact.id, blockId: ask.block_id, kind: "decision" };
  }
  return { kind: "card" };
}

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

/** The artifact the column has opened: it follows the selection only once the selection has
 *  stood still for `DOCUMENT_SETTLE_MS`, and two questions in one document never change it. */
function useSettledArtifact(artifactId: string | undefined): string | undefined {
  const [settled, setSettled] = useState<string | undefined>(undefined);
  useEffect(() => {
    if (settled === artifactId) {
      return;
    }
    const timer = window.setTimeout(() => setSettled(artifactId), DOCUMENT_SETTLE_MS);
    return () => window.clearTimeout(timer);
  }, [artifactId, settled]);
  return settled;
}

export interface ReadingPaneProps {
  ask: InboxRow;
  /** The reader's own answer landed; the list selects the next row. */
  onAnswered: (askId: string) => void;
  /** The head's Snooze write is in flight or refused, or has stopped being either: the Inbox
   *  keeps a live row listed, which is what keeps this column on it long enough for a refusal
   *  to be read. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  viewer: string | undefined;
}

/**
 * The Inbox's reading column: the selected question's own document, where the answer is given.
 *
 * A question written as a decision block shows the live document, drawn by the document page's
 * own component in its pane surface, scrolled so that the heading of the question's section sits
 * at the top of the column, with a bar marking the section down to the decision block. An
 * approval request shows the document's latest version — the version Approve approves — with
 * every block changed since the last approval marked. An anchored question, and a question in no
 * document, show the card the Inbox has always drawn, and so does a question whose document
 * could not be opened: the column never shows the document page's rebuild or repair prompts.
 */
export function ReadingPane({
  ask,
  onAnswered,
  onSnoozeLive,
  viewer,
}: ReadingPaneProps): ReactNode {
  const askId = ask.id;
  const subject = subjectOf(ask);
  const artifactId =
    subject.kind === "approval"
      ? subject.approval.artifact_id
      : subject.kind === "decision"
        ? subject.artifactId
        : undefined;
  const blockId = subject.kind === "decision" ? subject.blockId : undefined;
  const approvedVersion =
    subject.kind === "approval" ? subject.approval.approved?.version : undefined;
  const settledArtifactId = useSettledArtifact(artifactId);
  const settled = artifactId !== undefined && settledArtifactId === artifactId;

  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const [sectionBar, setSectionBar] = useState<SectionBar | undefined>(undefined);
  const [sectionBlockId, setSectionBlockId] = useState<string | undefined>(undefined);
  /** Where "Next change" stands in the marked blocks, and the question it stands in: another
   *  question is read from its first marked block, not from where the last one was left. */
  const nextChangeAt = useRef({ askId, at: 0 });

  const { blockSchema: runtimeBlockSchema } = useContext(DocumentRuntime);
  const blockSchemaQuery = useQuery({
    queryFn: loadBlockSchema,
    queryKey: ["block-schema"],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const blockSchema = runtimeBlockSchema ?? blockSchemaQuery.data;
  const user = useQuery(whoAmIQuery()).data;
  const issue = useQuery({
    ...issueQuery(ask.issue_key ?? undefined),
    enabled: ask.issue_key !== null,
  });
  const artifactQuery = useQuery({
    enabled: settled,
    queryFn: () => {
      if (artifactId === undefined) throw new Error("A document read needs the document's id.");
      return api.getArtifact(artifactId);
    },
    queryKey: ["artifact", artifactId],
  });
  // The read the document page's own editor is admitted by: the server answers it `409
  // DOC_SCHEMA` for a tree outside the Proof schema, and the page offers a repair or a rebuild
  // for what it says. A column beside a list offers neither, so it reads the text itself, under
  // the page's own key, and falls back to the card rather than mounting a document that cannot
  // open.
  const textQuery = useQuery({
    enabled: settled && subject.kind === "decision",
    queryFn: () => {
      if (artifactId === undefined) throw new Error("A document read needs the document's id.");
      return api.getArtifactText(artifactId);
    },
    queryKey: ["artifact", artifactId, "text"],
  });
  // The version the request is marked against: the document's last approval by any person, read
  // under the document page's own version key, as today's card reads it (`EarlierApproval`).
  const baselineQuery = useQuery({
    enabled: settled && approvedVersion !== undefined,
    queryFn: () => {
      if (artifactId === undefined || approvedVersion === undefined) {
        throw new Error("A baseline read needs the document's id and the approved version.");
      }
      return api.getArtifactVersion(artifactId, approvedVersion);
    },
    queryKey: ["artifact", artifactId, "version", approvedVersion],
  });

  const artifact = artifactQuery.data;
  const changeBaseline = isText(baselineQuery.data) ? baselineQuery.data.markdown : undefined;
  const latestVersion =
    artifact === undefined ? undefined : versionsNewestFirst(artifact.versions)[0]?.number;
  // A document the column cannot read, and an approval request on a document that holds no
  // version to show, both fall back to the card. So does a document whose own admission failed
  // after this column's read succeeded: the page answers that with a repair or a rebuild prompt,
  // which a column beside a list never shows, so the document reports it here instead
  // (`onUnavailable`) and the card takes its place. The report names the document it is about,
  // so selecting another question is not answered with the last one's failure.
  const [unavailable, setUnavailable] = useState<string | undefined>(undefined);
  const failed =
    artifactQuery.isError ||
    textQuery.isError ||
    unavailable === artifactId ||
    (subject.kind === "approval" && artifact !== undefined && latestVersion === undefined);
  const showsDocument = subject.kind === "decision" && !failed && artifact !== undefined;

  // The editor draws its blocks after the socket syncs, and a diagram or a picture moves them
  // again afterwards, so the placement runs on every change of the drawn document: the scroll
  // once, when the question's block is first there, and the bar whenever the blocks move.
  useEffect(() => {
    setSectionBar(undefined);
    setSectionBlockId(undefined);
    const column = scroller.current;
    const drawn = content.current;
    if (column === null || drawn === null || blockId === undefined || !showsDocument) {
      return;
    }
    // The column is placed once, when the question's block is first drawn; the bar follows the
    // blocks for as long as they move.
    let placed = false;
    const place = () => {
      const block = blockElement(drawn, blockId);
      if (block === null) {
        return;
      }
      const headingId = sectionHeadingId(drawn, blockId);
      if (!placed) {
        if (headingId === undefined) {
          column.scrollTop = 0;
          placed = true;
        } else {
          placed = scrollBlockToTop(headingId, column);
        }
        if (placed) {
          setSectionBlockId(headingId);
        }
      }
      const bar = sectionBarOf(drawn, headingId, block);
      setSectionBar((current) => (sameBar(current, bar) ? current : bar));
    };
    place();
    const drawing = new MutationObserver(place);
    drawing.observe(drawn, { childList: true, subtree: true });
    const resizing = new ResizeObserver(place);
    resizing.observe(drawn);
    return () => {
      drawing.disconnect();
      resizing.disconnect();
    };
  }, [blockId, showsDocument]);

  const skipToAnswer = useCallback(() => {
    const column = scroller.current;
    if (column !== null && blockId !== undefined) {
      scrollBlockToTop(blockId, column);
    }
  }, [blockId]);

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
    const marked = drawn.querySelectorAll(`.${CHANGED_BLOCK_CLASS}`);
    const block = marked[nextChangeAt.current.at % marked.length];
    if (block === undefined) {
      return;
    }
    nextChangeAt.current = { askId, at: (nextChangeAt.current.at + 1) % marked.length };
    scrollElementToTop(block, column);
  }, [askId]);

  const body = (): ReactNode => {
    if (subject.kind === "card" || failed) {
      return (
        <AskCard
          ask={ask}
          initialThread={{ ask, ...ask.thread }}
          // One card per question: the card holds the answer the reader gave in it, so the next
          // question selected is a card of its own rather than this one showing that answer.
          key={ask.id}
          onAnswered={onAnswered}
          variant="full"
        />
      );
    }
    const name = ask.approval?.name ?? ask.document?.name ?? "the document";
    const skeleton = <LoadingSkeleton label={`Opening ${name}`} />;
    if (!settled || artifact === undefined || user === undefined) {
      return skeleton;
    }
    if (subject.kind === "approval") {
      if (
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
    }
    if (textQuery.data === undefined) {
      return skeleton;
    }
    return (
      <ProofDocument
        artifact={artifact}
        highlight={undefined}
        isClosed={issue.data !== undefined && issue.data.closed_at !== null}
        key={artifact.id}
        onAskAnswered={onAnswered}
        onUnavailable={() => setUnavailable(artifact.id)}
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
  };

  return (
    <section
      aria-label="Selected question"
      className="flex h-full min-h-0 flex-col gap-3"
      data-testid="inbox-pane"
    >
      <ReadingPaneHead
        artifact={artifact}
        ask={ask}
        onAnswered={onAnswered}
        onNextChange={
          subject.kind === "approval" && changeBaseline !== undefined ? nextChange : undefined
        }
        onSkipToAnswer={showsDocument ? skipToAnswer : undefined}
        onSnoozeLive={onSnoozeLive}
        sectionBlockId={sectionBlockId}
        viewer={viewer}
      />
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
            {body()}
          </div>
        </div>
      </div>
    </section>
  );
}

/** The column carries no version picker: which version it shows is the question's, never the
 *  reader's. */
function noVersionChange(): void {}
