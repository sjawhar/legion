import type { ReactNode } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import {
  dangerText,
  surfaceBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { AskCompletionCard } from "../inbox/AskCompletionCard";
import { AskHeader } from "../inbox/AskHeader";
import { AskOptionList } from "../inbox/AskOptionList";
import { blockFragment } from "../refs/block-reference";
import { buildReferencePath, parseNodeReference, shortForm } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type AskBlockFacts, type AskBlockHost, askBlockFacts } from "./typed-block-drawing";

export interface AskBlockCardProps {
  /** The indexed ask row for this block, once the server has created it. */
  ask: Ask | undefined;
  /** The block stands in text no server reads - a comment, ask or message body - so nothing
   *  ever activates it: a malformed one is not told to fix its text to re-activate. */
  detached?: boolean;
  host: AskBlockHost;
  /** Called once the server has recorded the reader's answer from this block. */
  onAnswered?: (id: string) => void;
  /** The project document holding the block, for an ask that has no issue to be referenced
   *  under; an issue document's asks carry their `issue_key`. */
  owner: { project: string; slug: string } | undefined;
  readOnly: boolean;
  /** Whether a resolved block whose indexed ask row is resolved too shows the row's resolution
   *  record: the current document of a closed issue, whose close withdrew the ask. A saved
   *  version reads its own attributes only, so a later withdrawal never appears on it. */
  rowResolution: boolean;
}

/** The decision block's host for the one ask card every surface shares. It portals into the
 * node view's two slots (see `AskBlockView`): the header names the block type and urgency; the
 * footer is the Inbox's `AskCard` — who asked and when, the exchange, and the composer or the
 * recorded outcome — once the server has indexed the block into an ask row and the surface is
 * live. Until then, and on a read-only surface (closed issue, version view), the block reads its
 * own attributes: the options with the chosen ones ticked and the note, and the header names who
 * answered. The one exception is `rowResolution`: there a block whose attribute and ask row are
 * both resolved shows the row's resolution record (its badge and who resolved it), with no
 * composer. A block copied from another document's ask has no ask of its own: it shows its
 * source's state, and the header links to the document where that ask is answered. The question
 * itself is the editor's content, so no part of this card repeats it. */
export function AskBlockCard({
  ask,
  detached = false,
  host,
  onAnswered,
  owner,
  readOnly,
  rowResolution,
}: AskBlockCardProps): ReactNode {
  const facts = askBlockFacts(host.node);
  const malformed = facts.malformedReason !== undefined;
  const answered = facts.state === "answered";
  const hosted = !malformed && !readOnly && ask !== undefined;
  const resolvedRow =
    !malformed && rowResolution && facts.state === "resolved" && ask?.state === "resolved"
      ? ask
      : undefined;
  // Whether the footer shows a card naming who asked and how it ended, so the header does not:
  // that card draws its own header, which carries the provenance and the reference.
  const carded = hosted || resolvedRow !== undefined;
  return (
    <>
      {createPortal(
        <>
          <AskHeader
            ask={ask}
            block={{ malformed, state: facts.state, urgency: facts.urgency }}
            carded={carded}
            leading={<strong className={textPrimaryOnSurface}>Decision</strong>}
            owner={owner}
            place="block"
            trailing={
              <>
                {!carded && answered ? (
                  <span className="inline-flex items-center gap-x-2" data-dispatch-ask-answered="">
                    <span aria-hidden="true">·</span>
                    <span>Answered by {facts.answeredBy}</span>
                    {facts.answeredAt === undefined ? null : (
                      <>
                        <span aria-hidden="true">·</span>
                        <Timestamp at={facts.answeredAt} />
                      </>
                    )}
                  </span>
                ) : null}
                {ask === undefined && !malformed && facts.copiedFrom !== undefined ? (
                  <CopiedFromLink
                    blockId={facts.blockId}
                    copied={facts.copiedFrom}
                    state={facts.state}
                  />
                ) : null}
              </>
            }
          />
          {malformed ? (
            <p className={`mb-2 text-sm font-medium ${dangerText}`}>{facts.malformedReason}</p>
          ) : null}
        </>,
        host.header
      )}
      {createPortal(
        malformed ? (
          detached ? null : (
            <p className={`mt-3 text-sm ${textMutedOnSurfaceMuted}`}>
              Fix the block text; the decision re-activates once it parses.
            </p>
          )
        ) : hosted ? (
          <AskCard
            ask={ask}
            frame="block"
            markRead
            onAnswered={onAnswered}
            owner={owner}
            thread="collapsed"
            variant="compact"
          />
        ) : resolvedRow !== undefined ? (
          <AskCompletionCard
            artifactSlug={undefined}
            ask={resolvedRow}
            documentLink={false}
            edits={[]}
            frame="block"
            owner={owner}
          />
        ) : (
          <AttributeRecord facts={facts} />
        ),
        host.footer
      )}
    </>
  );
}

/** The read-only card a past version shows, for a decision block in a text body (a comment, ask
 *  or message): what the app provides as `TextAskCard`, so `useRenderedMarkdown` mounts it into
 *  each `ask` shell the body draws. The block reads its own attributes, never an ask row. */
export function TextAskBlockCard({ host }: { host: AskBlockHost }): ReactNode {
  return (
    <AskBlockCard
      ask={undefined}
      detached
      host={host}
      owner={undefined}
      readOnly
      rowResolution={false}
    />
  );
}

/** Where a copied block's source stands, by the state settlement wrote into the block. */
const COPY_STATE_LABELS: Record<string, string> = {
  answered: "answered there",
  open: "open there",
  resolved: "resolved there",
};

/** Where a copied block is answered: the document settlement names in the block's
 * `copied_from_document`, linked at the block, and the source's state there, which settlement
 * wrote into the block. Both come from the block, so the card reads no ask to show them; the
 * document's reference is the server's, read whatever the length of its name. */
function CopiedFromLink({
  blockId,
  copied,
  state,
}: {
  blockId: string;
  copied: NonNullable<AskBlockFacts["copiedFrom"]>;
  state: string;
}): ReactNode {
  const route = parseNodeReference(copied.document);
  const there = COPY_STATE_LABELS[state] ?? "closed there";
  return (
    <span className="inline-flex items-center gap-x-2" data-dispatch-ask-copied-from={copied.ask}>
      <span aria-hidden="true">·</span>
      <span>
        Copied from{" "}
        {route === undefined ? (
          "another document"
        ) : (
          <Link to={`${buildReferencePath(route)}${blockFragment(blockId)}`}>
            {route.kind === "spec" ? "the spec" : shortForm(route)}
          </Link>
        )}
        , {there}
      </span>
    </span>
  );
}

/** What the block's own attributes record: its options with the chosen ones ticked, and the
 * note. A still-open decision shows its options with nothing chosen. */
function AttributeRecord({ facts }: { facts: AskBlockFacts }): ReactNode {
  const otherText =
    facts.state === "answered" && facts.options.length > 0 && facts.selected.length === 0
      ? facts.answerText
      : "";
  return (
    <div className="mt-3 space-y-2" data-dispatch-ask-record="">
      <AskOptionList options={facts.options} rowClassName={surfaceBg} selected={facts.selected} />
      {otherText === "" ? null : (
        <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>Other</p>
      )}
      {facts.answerText === "" ? null : (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} data-dispatch-ask-answer="">
          {facts.answerText}
        </p>
      )}
    </div>
  );
}
