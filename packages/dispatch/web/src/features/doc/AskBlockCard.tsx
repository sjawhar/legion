import type { ReactNode } from "react";
import { createPortal } from "react-dom";

import type { Ask } from "../../api/types";
import {
  askBlockPill,
  badgeBlocking,
  badgeLow,
  dangerText,
  surfaceBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { AskOptionList } from "../inbox/AskOptionList";
import { URGENCY_LABELS } from "../inbox/ask-urgency";
import { actorLabel } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type AskBlockFacts, type AskBlockHost, askBlockFacts } from "./ask-block";

const PILL_CLASS = "rounded-full px-2 py-0.5 text-[10px] font-semibold tracking-wide uppercase";

export interface AskBlockCardProps {
  /** The indexed ask row for this block, once the server has created it. */
  ask: Ask | undefined;
  host: AskBlockHost;
  /** Called once the server has recorded the reader's answer from this block. */
  onAnswered?: (id: string) => void;
  /** The project document holding the block, for an ask that has no issue to be referenced
   *  under; an issue document's asks carry their `issue_key`. */
  owner: { project: string; slug: string } | undefined;
  readOnly: boolean;
}

/** The decision block's host for the one ask card every surface shares. It portals into the
 * node view's two slots (see `AskBlockView`): the header names the block type and urgency; the
 * footer is the Inbox's `AskCard` — who asked and when, the exchange, and the composer or the
 * recorded outcome — once the server has indexed the block into an ask row and the surface is
 * live. Until then, and on a read-only surface (closed issue, version view), the block reads its
 * own attributes: the options with the chosen ones ticked and the note, and the header names who
 * answered. The question itself is the editor's content, so no part of this card repeats it. */
export function AskBlockCard({
  ask,
  host,
  onAnswered,
  owner,
  readOnly,
}: AskBlockCardProps): ReactNode {
  const facts = askBlockFacts(host.node);
  const malformed = facts.malformedReason !== undefined;
  const open = facts.state === "open";
  const answered = facts.state === "answered";
  const hosted = !malformed && !readOnly && ask !== undefined;
  const pill = askBlockPill[facts.urgency];
  // A block ask indexed without an actor (the server settled it with no pending author) still
  // shows when it was asked; "asked by" waits for a name.
  const asker = ask === undefined ? undefined : actorLabel(ask.author);
  const reference = ask === undefined ? undefined : itemRoute("ask", ask, owner);
  return (
    <>
      {createPortal(
        <>
          <header
            className={`mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurfaceMuted}`}
          >
            {malformed ? (
              <span className={`${PILL_CLASS} ${badgeBlocking.bg} ${badgeBlocking.text}`}>
                Malformed
              </span>
            ) : (
              <span
                className={`${PILL_CLASS} ${pill.bg} ${pill.text}`}
                data-dispatch-ask-pill={facts.urgency}
              >
                {URGENCY_LABELS[facts.urgency]}
              </span>
            )}
            <strong className={textPrimaryOnSurface}>Decision</strong>
            {hosted || ask === undefined ? null : (
              <span className="inline-flex items-center gap-x-2" data-dispatch-ask-asked="">
                <span aria-hidden="true">·</span>
                <span>{asker === "" ? "asked" : `asked by ${asker}`}</span>
                {asker === "" ? null : <span aria-hidden="true">·</span>}
                <Timestamp at={ask.created_at} />
              </span>
            )}
            {/* The open card's metadata line carries the reference itself; the record it shows
                once answered or resolved does not, so the header keeps it then. */}
            {(hosted && ask.state === "open") || reference === undefined ? null : (
              <CopyRefButton route={reference} />
            )}
            {!hosted && answered ? (
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
            {!hosted && !malformed && !open && !answered ? (
              <span className={`${PILL_CLASS} ${badgeLow.bg} ${badgeLow.text}`}>
                {facts.state === "resolved" ? "Resolved" : "Closed"}
              </span>
            ) : null}
          </header>
          {malformed ? (
            <p className={`mb-2 text-sm font-medium ${dangerText}`}>{facts.malformedReason}</p>
          ) : null}
        </>,
        host.header
      )}
      {createPortal(
        malformed ? (
          <p className={`mt-3 text-sm ${textMutedOnSurfaceMuted}`}>
            Fix the block text; the decision re-activates once it parses.
          </p>
        ) : hosted ? (
          <AskCard
            ask={ask}
            frame="block"
            onAnswered={onAnswered}
            owner={owner}
            thread="collapsed"
            variant="compact"
          />
        ) : (
          <AttributeRecord facts={facts} />
        ),
        host.footer
      )}
    </>
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
