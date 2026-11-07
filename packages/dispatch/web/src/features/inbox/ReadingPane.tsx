import { useQuery } from "@tanstack/react-query";
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
import { whoAmIQuery } from "../../api/queries";
import type { AskApproval, InboxRow } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { borderDefault } from "../../theme/classes";
import { versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { CHANGED_BLOCK_CLASS } from "../doc/change-marks";
import { DocumentRuntime } from "../doc/runtime";
import { loadBlockSchema } from "../doc/schema";
import { scrollElementToTop } from "../doc/scroll-block";
import { VersionView } from "../doc/VersionView";
import { MarkdownBody } from "../refs/MarkdownBody";
import { AskCard } from "./AskCard";
import { isDecisionAsk } from "./ask-decision";
import type { InboxListRow } from "./list-row";
import { artifactQueryOptions, DocumentPane, PaneFrame, useSettled } from "./PaneDocument";
import { ReadingPaneHead } from "./ReadingPaneHead";
import { ThreadPane } from "./ThreadPane";

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
  const scroller = useRef<HTMLElement>(null);
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
    // The card the column falls back to draws the whole question itself, and the question
    // belongs on screen once: a block above that card would say it twice.
    if (failed) {
      return <QuestionCard ask={ask} onAnswered={onAnswered} />;
    }
    /** The question the request itself asks, which is the requester's own words about the
     *  document below it. `AskCard`'s renderer draws it, so a message with a list or a link
     *  reads as it was written, and a rule tells it from the document's first paragraph. */
    const question = (
      <div className={`mb-3 border-b pb-3 ${borderDefault}`} data-testid="pane-approval-question">
        <MarkdownBody markdown={ask.question} />
      </div>
    );
    /** Whether the document below the question is still opening: every read `VersionView` needs
     *  has to have landed before the column can draw it. */
    const opening =
      !settled ||
      artifact === undefined ||
      user === undefined ||
      latestVersion === undefined ||
      (approvedVersion !== undefined && baselineQuery.isPending);
    // The question stands while the skeleton does: it is the pane's alone to draw, and waits
    // on none of the document's reads.
    return (
      <>
        {question}
        {opening ? (
          <LoadingSkeleton label={`Opening ${approval.name}`} />
        ) : (
          <VersionView
            artifactId={artifact.id}
            // The pane draws no ask row over a stored version: the blocks of the version carry
            // what was decided in them, and a column the reader flips through with `j` reads no
            // list of the document's asks to name their askers.
            asks={[]}
            blockSchema={blockSchema}
            changeBaseline={changeBaseline}
            createdAt={
              (artifact.versions ?? []).find((version) => version.number === latestVersion)
                ?.created_at
            }
            highlight={undefined}
            owner={
              artifact.issue_key === null
                ? { project: artifact.project, slug: artifact.slug }
                : undefined
            }
            version={latestVersion}
          />
        )}
      </>
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
  return (
    <DocumentPane
      anchoredThread={undefined}
      artifactId={artifactId}
      blockId={blockId}
      fallback={<QuestionCard ask={ask} onAnswered={onAnswered} />}
      head={(document) => (
        <ReadingPaneHead
          artifact={document.artifact}
          ask={ask}
          onAnswered={onAnswered}
          onNextChange={undefined}
          onSkipToAnswer={document.showsDocument ? document.skipToBlock : undefined}
          onSnoozeLive={onSnoozeLive}
          sectionBlockId={document.sectionBlockId}
          viewer={viewer}
        />
      )}
      issueKey={ask.issue_key}
      loadingName={ask.approval?.name ?? ask.document?.name ?? "the document"}
      onAskAnswered={onAnswered}
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
 * (`ThreadPane.tsx`), which fall back the same way.
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
