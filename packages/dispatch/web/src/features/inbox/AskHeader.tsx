import type { ReactNode } from "react";

import type { Ask, AskUrgency, Comment } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import {
  askBlockPill,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  calloutSuccessTimestampText,
  surfaceBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { askResolutionBadge } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskAnchorHeader } from "./AskAnchorLink";
import { askTurnHolder } from "./ask-turn";
import { URGENCY_LABELS } from "./ask-urgency";

/**
 * Where a header is drawn. Each place keeps its own grammar and tone, and every one of them
 * reads the same six fields from the same ask: `card` is the Inbox card (both variants),
 * `answered` and `resolved` its records (`AskCompletionCard`), `block` the decision block's own
 * portalled header (`features/doc/AskBlockCard.tsx`).
 */
export type AskHeaderPlace = "answered" | "block" | "card" | "resolved";

/** What a decision block's own attributes say about its ask (`askBlockFacts`): what a block with
 *  no ask row - a past version, a block copied from another document - has to draw from. */
export interface AskHeaderBlock {
  /** The block cannot be answered as written, so its urgency mark gives way to `Malformed`. */
  readonly malformed: boolean;
  readonly state: string;
  readonly urgency: AskUrgency;
}

export interface AskHeaderProps {
  /** The indexed ask row. A decision block drawn from a past version, or copied from another
   *  document, has none and shows only the fields its own attributes hold. */
  ask?: Ask;
  /** What the hosting decision block's own attributes say, where the header is a block's. */
  block?: AskHeaderBlock;
  /** The decision block's footer draws a card of its own, and that card's header carries the
   *  provenance; the block's own header then draws neither it nor the state the card records. */
  carded?: boolean;
  /** What the host draws between its anchor and its provenance line: its question, and whatever
   *  else is that place's alone. A block's header has none - the editor holds the question. */
  children?: ReactNode;
  /** A control the host keeps around the asker's name, drawn for a session asker alone:
   *  `AskCard`'s session-handles disclosure. */
  control?: (name: ReactNode) => ReactNode;
  /** The card is hosted inside its own decision block, which already draws the frame, the
   *  urgency mark and the anchor. */
  inBlock?: boolean;
  /** The ask's newest reply, which decides whose turn it is. */
  lastReply?: Pick<Comment, "author">;
  /** The host's own parts at the head of the provenance line: the block's `Decision`. */
  leading?: ReactNode;
  /** The project document that owns an ask with no issue, so the header can name the ask's
   *  `dispatch://` reference (only Inbox rows carry `document` themselves). */
  owner?: { project: string; slug: string };
  place: AskHeaderPlace;
  /** The host's own end of the provenance line: the answered and resolved records' own
   *  sentences, `AskCard`'s session handles, the block's `Copied from`. */
  trailing?: ReactNode;
}

const PILL_CLASS = "rounded-full px-2 py-0.5 text-[10px] font-semibold tracking-wide uppercase";

/** The urgency notch's text colour, matching the urgency badge of the same level. */
const URGENCY_NOTCH_TEXT: Record<AskUrgency, string> = {
  blocking: badgeBlocking.text,
  high: badgeHigh.text,
  low: badgeLow.text,
  med: badgeMed.text,
};

/** The provenance line of each place, in that place's own tone and rhythm. */
const ROW_CLASS: Record<AskHeaderPlace, string> = {
  answered: `mt-2 text-xs ${calloutSuccessTimestampText}`,
  block: `mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurfaceMuted}`,
  card: `@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurface}`,
  resolved: `mt-2 text-xs ${textMutedOnSurface}`,
};

/** Whether the header draws its urgency notch, which hangs over the top edge of the card's own
 *  frame: that frame must be positioned and leave a row of room above it. A card inside a
 *  decision block draws none - the block's header has the urgency pill. */
export function hasUrgencyNotch(ask: Pick<Ask, "urgency">, inBlock: boolean): boolean {
  return !inBlock && (ask.urgency === "blocking" || ask.urgency === "high");
}

/** The asker's name, as the one naming rule spells it, and the tooltip the host puts on it. */
interface AskHeaderAsker {
  readonly label: string;
  readonly title: string;
}

/**
 * One ask's header, wherever the ask is drawn: its urgency mark, its anchor, who asked, when,
 * the control that copies its `dispatch://` reference, and the label naming where it stands.
 * Every place reads those six fields from the same ask, so the Inbox card, the answered record,
 * the resolved record and a document's decision block cannot disagree about them.
 *
 * Each place keeps its own layout. The anchor belongs above the host's question and the
 * provenance line below it (chrome never sits between a reader and the primary content), so the
 * host passes what it draws between them as `children`; a block has no content of its own, and
 * its header is the one line it draws. The parts that are one place's alone - `AskCard`'s
 * session handles, the records' own sentences, the block's `Decision` and `Copied from` - come
 * in through `leading` and `trailing`.
 *
 * A state label is drawn where the place has one to draw: the resolved record's badge and a
 * decision block's pill. An open ask is in no state worth naming, and an answered record's whole
 * frame already says it. `data-ask-state` carries the value at every place all the same, beside
 * `data-ask-urgency`, `data-ask-asker`, `data-ask-at` and `data-ask-copy`: the five fields one
 * browser row reads to hold the four places to the same values. A field the place does not hold
 * carries no attribute at all.
 *
 * Naming an asker reads the agent registry, which is a query: a header with no asker to name -
 * a decision block in a body of text, which has no ask row at all - is drawn by a surface that
 * need not have one, so only the branch that names one reads it. A block ask the server indexed
 * with no pending author has an empty login and no name to draw, and still says when it was
 * asked.
 */
export function AskHeader(props: AskHeaderProps): ReactNode {
  const { ask, carded = false } = props;
  return ask !== undefined && !carded && (ask.author.kind === "session" || ask.author.id !== "") ? (
    <NamedAskHeader {...props} ask={ask} />
  ) : (
    <DrawnAskHeader {...props} asker={undefined} />
  );
}

function NamedAskHeader(props: AskHeaderProps & { ask: Ask }): ReactNode {
  const agentText = useAgentText();
  const { author } = props.ask;
  const label = agentText.label(author);
  return (
    <DrawnAskHeader
      {...props}
      asker={{
        label,
        // A session's tooltip also carries its full id, since the innermost title is the one a
        // browser shows, and an untitled session's name is the short form `session:<8>…` that
        // sessions started in the same minute share.
        title: author.kind === "session" ? `${label} · ${author.id}` : label,
      }}
    />
  );
}

function DrawnAskHeader({
  ask,
  asker,
  block,
  carded = false,
  children,
  control,
  inBlock = false,
  lastReply,
  leading,
  owner,
  place,
  trailing,
}: AskHeaderProps & { asker: AskHeaderAsker | undefined }): ReactNode {
  const author = ask?.author;
  const session = author?.kind === "session" ? author : undefined;
  const reference = ask === undefined ? undefined : itemRoute("ask", ask, ask.document ?? owner);
  const urgency = block?.urgency ?? ask?.urgency;
  const state = block?.state ?? ask?.state;
  // Who asked, when, and the reference: the card below a decision block draws all three, so the
  // block's own header draws none of them.
  const provenance = ask !== undefined && !carded;
  const copy = provenance && reference !== undefined ? <CopyRefButton route={reference} /> : null;

  const attributes: Record<string, string> = { "data-ask-header": "" };
  if (urgency !== undefined) attributes["data-ask-urgency"] = urgency;
  if (state !== undefined) attributes["data-ask-state"] = state;
  if (provenance && ask !== undefined) attributes["data-ask-at"] = ask.created_at;
  if (asker !== undefined) attributes["data-ask-asker"] = asker.label;
  if (copy !== null && reference !== undefined) {
    attributes["data-ask-copy"] = buildDispatchReference(reference);
  }

  const urgencyPill =
    block === undefined ? null : block.malformed ? (
      <span className={`${PILL_CLASS} ${badgeBlocking.bg} ${badgeBlocking.text}`}>Malformed</span>
    ) : (
      <span
        className={`${PILL_CLASS} ${askBlockPill[block.urgency].bg} ${askBlockPill[block.urgency].text}`}
        data-dispatch-ask-pill={block.urgency}
      >
        {URGENCY_LABELS[block.urgency]}
      </span>
    );
  const notch =
    ask === undefined || !hasUrgencyNotch(ask, inBlock) ? null : (
      <span
        aria-hidden="true"
        className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${surfaceBg} ${URGENCY_NOTCH_TEXT[ask.urgency]}`}
      >
        {URGENCY_LABELS[ask.urgency].toUpperCase()}
      </span>
    );

  const stateLabel =
    place === "resolved" && ask?.resolution !== undefined ? (
      <span
        className={`mr-1.5 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
        data-testid="ask-resolution-badge"
      >
        {askResolutionBadge(ask.resolution)}
      </span>
    ) : place === "block" &&
      block !== undefined &&
      !carded &&
      !block.malformed &&
      block.state !== "open" &&
      block.state !== "answered" ? (
      <span className={`${PILL_CLASS} ${badgeLow.bg} ${badgeLow.text}`}>
        {block.state === "resolved" ? "Resolved" : "Closed"}
      </span>
    ) : null;

  const askedAt = ask === undefined ? null : <Timestamp at={ask.created_at} />;
  const turnHolder = place === "card" && ask !== undefined ? askTurnHolder(ask, lastReply) : null;
  let provenanceNode: ReactNode = null;
  if (provenance && ask !== undefined && author !== undefined) {
    if (place === "block") {
      provenanceNode = (
        <span className="inline-flex items-center gap-x-2" data-dispatch-ask-asked="">
          <span aria-hidden="true">·</span>
          <span>
            {asker === undefined ? (
              "asked"
            ) : (
              <>
                asked by <AgentName actor={author} inline />
              </>
            )}
          </span>
          {asker === undefined ? null : <span aria-hidden="true">·</span>}
          {askedAt}
        </span>
      );
    } else if (place === "card") {
      provenanceNode = (
        // Turn, author and time are one group whose names truncate: a separator that wrapped
        // left a line opening - or ending - on a bare "·", and the agents are the only parts
        // that can give up width ("Waiting on you" never does). Below 24rem of line (a phone,
        // a margin) the turn takes a line of its own with no separator after it, and the
        // asker and time share the next: beside the turn and the live link's 44 px touch box
        // the asker's name kept only two letters.
        <span className="flex min-w-0 items-center gap-x-1.5 whitespace-nowrap @max-sm:flex-wrap">
          {turnHolder === null ? null : (
            <span
              className={`${turnHolder === "you" ? "shrink-0" : "min-w-0 truncate"} font-semibold @max-sm:basis-full ${textSecondaryOnSurface}`}
              data-testid={`turn-${ask.id}`}
            >
              Waiting on {turnHolder === "you" ? "you" : <AgentName actor={turnHolder} inline />}
            </span>
          )}
          {turnHolder === null ? null : (
            <span className="shrink-0 @max-sm:hidden" aria-hidden="true">
              ·
            </span>
          )}
          <span className="flex min-w-0 items-center gap-x-1.5">
            {session === undefined || control === undefined ? (
              <TruncatedText className="min-w-0" title={asker?.title ?? "asked"}>
                {asker === undefined ? "asked" : <AgentName actor={author} />}
              </TruncatedText>
            ) : (
              // The asker's control keeps its name and opens the session's copy handles; the
              // component's live link sits beside it, outside the control. `whitespace-pre`
              // keeps the space before an attribution suffix, which a flex item would drop.
              <AgentName
                actor={session}
                className="flex min-w-0 items-center whitespace-pre"
                control={(name) =>
                  control(
                    <TruncatedText className="min-w-0" title={asker?.title ?? "asked"}>
                      {name}
                    </TruncatedText>
                  )
                }
                kind="control"
              />
            )}
            <span className="shrink-0" aria-hidden="true">
              ·
            </span>
            {askedAt}
          </span>
        </span>
      );
    } else {
      provenanceNode = (
        <>
          {asker === undefined ? null : (
            <>
              <AgentName actor={author} inline />
              {" · "}
            </>
          )}
          Asked {askedAt}
        </>
      );
    }
  }

  const row = (
    <>
      {urgencyPill}
      {leading}
      {stateLabel}
      {provenanceNode}
      {copy}
      {trailing}
    </>
  );

  if (place === "block") {
    return (
      <header className={ROW_CLASS.block} {...attributes}>
        {row}
      </header>
    );
  }
  return (
    <div {...attributes}>
      {notch}
      {ask === undefined ? null : (
        <AskAnchorHeader
          ask={ask}
          inBlock={inBlock}
          quoteSpacing={place === "card" ? "roomy" : "compact"}
          tone={place === "answered" ? "success" : "surface"}
        />
      )}
      {children}
      <div className={ROW_CLASS[place]}>{row}</div>
    </div>
  );
}
