import { useQuery } from "@tanstack/react-query";
import { type FocusEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import {
  borderDefault,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { buildInboxPath, type InboxRoute, parseInboxSearch } from "../refs/routes";
import { type KeymapScope, useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { AskCard } from "./AskCard";
import { InboxOwnerLink } from "./AskOwnerLink";
import { AssigneeFilter } from "./AssigneeFilter";
import { AssignToMe } from "./AssignToMe";
import { formatAskAge } from "./ask-age";
import { outsideAskCard } from "./ask-card";
import { isDecisionAsk } from "./ask-decision";
import { type AskOrdinal, askOrdinals, controlName } from "./ask-name";
import { BlockedOnYou, waitingOnHuman } from "./BlockedOnYou";
import { BulkSnoozeBar } from "./BulkSnoozeBar";
import { crossBandCounts, groupRows } from "./grouping";
import { bandGroupItems, useGroupJump } from "./InboxGroups";
import { ROW_ATTRIBUTE, ROW_SELECTOR } from "./inbox-row";
import { useMotionHold } from "./motion-hold";
import { questionLine } from "./question-line";
import { ReadingPane } from "./ReadingPane";
import { SnoozeControl } from "./SnoozeControl";
import {
  COLLAPSED_SECTIONS,
  INBOX_SECTIONS,
  type InboxSection,
  inAssigneeFilter,
  isOwnList,
  isTurnSection,
  isUnassigned,
  sectionOf,
  sectionTitles,
} from "./sections";
import { type AskSnoozeFailure, useAskSnoozeMany } from "./useAskSnooze";

function ReplyChip({ children }: { children: ReactNode }): ReactNode {
  return <LabelPill>{children}</LabelPill>;
}

/** When an agent replied last on a row that still needs the viewer, that reply is surfaced so
 *  the viewer knows to look before answering. Whose turn it is ("Waiting on you" / "Waiting on
 *  <agent>") is the card's own status line. */
function InboxRowChip({ ask }: { ask: InboxRow }): ReactNode {
  if (ask.waiting_on === "human" && ask.last_reply?.author.kind === "session") {
    // One span, so the pill's flex box keeps the space between the name and "replied".
    return (
      <ReplyChip>
        <span>
          <AgentName actor={ask.last_reply.author} inline /> replied
        </span>
      </ReplyChip>
    );
  }
  return null;
}

/** The bulk bar's snooze picker: `h` hands it focus and Escape takes it back to the list. */
const BULK_PICKER_SELECTOR = "[data-inbox-bulk-snooze]";

/** The "Add a note or answer in your own words" disclosure an `AskCard` folds its answer box
 *  behind in its compact variant - the card a decision block hosts, which is what the reading
 *  column shows for a question written as one. The card names the box it reveals in
 *  `aria-controls`, and no other disclosure on the page ends in `-disclosure`, so that is how
 *  Enter finds it without a handle of its own on the card. */
const NOTE_DISCLOSURE_SELECTOR = 'button[aria-expanded="false"][aria-controls$="-disclosure"]';

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  return focusedMatching(ROW_SELECTOR);
}

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

/** The kind a short row names: a question written as a decision block of a document, a request
 *  to approve a document version, or any other question. */
function askKind(ask: InboxRow): string {
  if (isDecisionAsk(ask)) {
    return "Decision";
  }
  return ask.kind === "approval" ? "Approval" : "Question";
}

function InboxItem({
  ask,
  assignLive,
  grouped,
  marked,
  ordinal,
  ownerKey,
  onAnswered,
  onAssignLive,
  onMark,
  onSelect,
  onSnoozeLive,
  onRelease,
  section,
  selected,
  threadUpdatedAt,
  viewer,
}: {
  ask: InboxRow;
  /** True while this row's "Assign to me" write is in flight or failed, whatever section the
   *  optimistic update put the row in. */
  assignLive: boolean;
  /** Whether this row sits under a multi-ask owner header. */
  grouped: boolean;
  /** Whether the reader has marked this row for a bulk action. */
  marked: boolean;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  /** The owner identity every row exposes for cross-band group jumps. */
  ownerKey: string;
  onAnswered: (id: string) => void;
  onAssignLive: (askId: string, live: boolean) => void;
  /** Marks or unmarks this row; `x` and the row's own checkbox are the two ways in. */
  onMark: (askId: string) => void;
  /** Selects this row, which is what the reading column shows. Set only while that column is
   *  on: without it a row is the card it has always been, and there is no selection.  */
  onSelect?: (askId: string) => void;
  /** The same registration for this row's snooze write; the Inbox keeps a live row rendered
   *  even when its band is folded, so `Snoozing…` and a refusal survive the fold. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** Set on the held row - one the server dropped or moved to another section that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  /** Timestamp of the Inbox snapshot that supplied this row's initial thread. */
  threadUpdatedAt: number;
  section: InboxSection;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
  /** The signed-in person's lowercase email; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const name = controlName(ask, ordinal);
  const short = onSelect !== undefined;
  const releaseOnFocusOut =
    onRelease === undefined
      ? undefined
      : (event: FocusEvent<HTMLLIElement>) => {
          if (!event.currentTarget.contains(event.relatedTarget)) onRelease();
        };
  const checkbox = (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      onChange={() => onMark(ask.id)}
      type="checkbox"
    />
  );
  return (
    /* The keyboard selects a row through the Inbox's own registry (`j`, `k` and the arrows),
       which needs no handler on the row itself; the click is the pointer's way to that same
       selection.
       biome-ignore lint/a11y/useKeyWithClickEvents: explained just above. */
    <li
      aria-current={short && selected ? "true" : undefined}
      className={`rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing} ${
        grouped ? `border-l-2 pl-3 ${borderDefault}` : ""
      } ${short && selected ? surfaceMutedBg : ""}`}
      data-inbox-group={grouped ? ownerKey : undefined}
      data-inbox-owner-key={ownerKey}
      data-inbox-row={ask.id}
      data-inbox-section={section}
      data-inbox-selected={short && selected ? "true" : undefined}
      onBlur={releaseOnFocusOut}
      onClick={onSelect === undefined ? undefined : () => onSelect(ask.id)}
      onPointerLeave={onRelease}
      tabIndex={-1}
    >
      {short ? (
        /* One line, read left to right: what the question belongs to, what kind of question it
           is, its first line, and how long it has been waiting. The question line takes what the
           owner and the badge leave, and truncates; the age never shrinks, since a row whose age
           is cut says nothing. */
        <div className="flex items-center gap-2 py-1.5">
          {checkbox}
          {/* A div, not a span: a row with neither an issue nor a document names itself with a
              paragraph (`InboxOwnerLink`), which no phrasing element may hold. */}
          <div className="flex min-w-0 basis-64 items-baseline">
            <InboxOwnerLink ask={ask} rowOwner />
          </div>
          <LabelPill>{askKind(ask)}</LabelPill>
          <span className="min-w-0 grow truncate text-sm">{questionLine(ask.question)}</span>
          <time className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`} dateTime={ask.created_at}>
            {formatAskAge(ask.created_at)}
          </time>
        </div>
      ) : (
        <>
          {/* One line: the issue this question belongs to on the left, the controls that defer or
              reroute it on the right. It wraps rather than overlapping - a refusal from Snooze or
              Assign to me is a full-width row inside the right group, so it never draws over the
              issue key - and the reply chip truncates rather than squeezing the title away. */}
          <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
            {checkbox}
            <div className="flex min-w-0 grow basis-48 items-baseline gap-2">
              <InboxOwnerLink ask={ask} rowOwner />
              <span className="min-w-0 max-w-[40%] shrink truncate">
                <InboxRowChip ask={ask} />
              </span>
            </div>
            {/* `max-w-full` bounds the group to the row: `shrink-0` alone lets a server's own
                refusal reason - `snoozed_until must be in the future`, or anything longer - set
                the group's width and widen the whole document past the viewport on a phone. */}
            <div className="flex max-w-full shrink-0 flex-wrap items-center gap-1">
              {ask.issue_key === null ? null : (
                <PriorityControl issueKey={ask.issue_key} priority={ask.priority} />
              )}
              {ask.issue_key !== null && (isUnassigned(ask) || assignLive) ? (
                <AssignToMe
                  askId={ask.id}
                  issueKey={ask.issue_key}
                  onLive={onAssignLive}
                  viewer={viewer}
                />
              ) : null}
              <SnoozeControl
                askId={ask.id}
                label={name}
                onLive={onSnoozeLive}
                snoozedUntil={section === "later" ? ask.snoozed_until : null}
              />
            </div>
          </div>
          <AskCard
            ask={ask}
            initialThread={{ ask, ...ask.thread }}
            initialThreadUpdatedAt={threadUpdatedAt}
            onAnswered={onAnswered}
          />
        </>
      )}
    </li>
  );
}

/** A row as the reader last saw it: the section it sat in, whatever the server says now. */
interface PlacedRow {
  ask: InboxRow;
  section: InboxSection;
}

/** The rows of one section, with the held row - one the server no longer lists here but the
 *  reader is still on - kept after the nearest row above it that is still listed, so it does not
 *  move while they finish. */
function withHeld(
  rows: readonly InboxRow[],
  held: InboxRow | undefined,
  previous: readonly PlacedRow[]
): readonly InboxRow[] {
  if (held === undefined) return rows;
  const ids = rows.map((row) => row.id);
  for (
    let index = previous.findIndex(({ ask }) => ask.id === held.id) - 1;
    index >= 0;
    index -= 1
  ) {
    const at = ids.indexOf(previous[index]?.ask.id ?? "");
    if (at !== -1) return [...rows.slice(0, at + 1), held, ...rows.slice(at + 1)];
  }
  return [held, ...rows];
}

/** The row to keep where the reader last saw it, if any. Dropped from the list: kept as last
 *  seen. Moved between the two turn sections: whose turn it is changed under the reader's hand,
 *  so the listed row is kept in the section they saw it in. A move into or out of Later is a
 *  snooze - the reader's own click, shown at once. */
function heldRow(
  seen: PlacedRow | undefined,
  listed: InboxRow | undefined,
  now: InboxSection | undefined
): PlacedRow | undefined {
  if (seen === undefined) return undefined;
  if (listed === undefined) return seen;
  const movedBetweenTurnSections =
    now !== undefined && now !== seen.section && isTurnSection(now) && isTurnSection(seen.section);
  return movedBetweenTurnSections ? { ask: listed, section: seen.section } : undefined;
}

/**
 * What the bar says after a pick the server refused in part. The count alone cannot tell a
 * sign-out from `snoozed_until must be in the future`, so the reason travels with it: one reason
 * when every refusal gave the same one, otherwise the first and how many others there are.
 */
function refusalText(failed: readonly AskSnoozeFailure[], of: number): string | undefined {
  const first = failed[0];
  if (first === undefined) return undefined;
  const reasons = new Set(failed.map(({ reason }) => reason));
  const head = `Could not snooze ${failed.length} of ${of}: ${first.reason}`;
  if (reasons.size === 1) return `${head}.`;
  const others = reasons.size - 1;
  return `${head}, and ${others} other reason${others === 1 ? "" : "s"}.`;
}

/** What the Inbox says when no band has a row. An agent filter's line is about that agent's asks
 *  alone, naming it in plain text (`useAgentText`): the empty state is a message, not a byline.
 *  Otherwise the credential requests above the bands wait on the viewer as much as an ask
 *  whose turn is theirs, so it says nothing until their list has come back empty: while the list
 *  loads, or after it fails, the Inbox cannot say that nothing needs them. The three filtered
 *  lines differ because only the reader's own list is about them: a cleared filter is about
 *  everyone, and any other selection is about whoever it names. */
function emptyStateMessage({
  agent,
  agentTitle,
  credentials,
  selection,
  viewer,
}: {
  agent: string | undefined;
  agentTitle: string | undefined;
  credentials: CredentialRequests;
  /** Who the list is filtered to, as `?assignee=` reads: empty is the cleared filter. */
  selection: readonly string[];
  viewer: string;
}): string | undefined {
  if (agent !== undefined) return `No open asks from ${agentTitle}`;
  if (credentials.status !== "listed" || credentials.requests.length > 0) return undefined;
  if (isOwnList(selection, viewer)) return "Nothing needs you";
  return selection.length === 0 ? "Nothing needs anyone" : "Nothing matches this filter";
}

/**
 * `filter`/`onAssigneesChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its assignee filter (that
 * would navigate the page it sits over), and its Escape/j/k/etc. must resolve under the drawer's
 * own `dialog` scope - the modal scope a dialog anywhere on the stack exclusively consults -
 * rather than the standalone page's `inbox` scope, which a dialog's keydown handling never
 * reaches. `pane` is the third: the drawer is a peek at any width, never a two-column reader.
 * `InboxPage`'s unmodified `<Inbox/>` call keeps today's URL-backed behaviour untouched.
 */
export interface InboxProps {
  filter?: InboxRoute;
  keymapScope?: KeymapScope;
  onAssigneesChange?: (assignees: readonly string[] | undefined) => void;
  /** Whether this Inbox may show its reading column beside the list, which it does from
   *  `READING_PANE_QUERY` (1280 px). False keeps today's Inbox - the card in the row, no
   *  selection and nothing written to `?item=` - at every width, which is what the drawer is. */
  pane?: boolean;
}

export function Inbox({
  filter: filterOverride,
  keymapScope = "inbox",
  onAssigneesChange,
  pane = true,
}: InboxProps = {}): ReactNode {
  const { search } = useLocation();
  const navigate = useNavigate();
  const filter = filterOverride ?? parseInboxSearch(search);
  // The reading column shows from 1280 px, and only where the host allows one: below that, and
  // in the drawer, the Inbox is the list of cards it has always been.
  const wideEnoughForPane = useMediaQuery(READING_PANE_QUERY);
  const paneShown = pane && wideEnoughForPane;
  // One inbox query, shared with the nav badge, the sidebar, the agents page and the margin; the
  // list below is a client-side partition of it, so an answered row leaves every surface at once.
  const inbox = useQuery(inboxQuery());
  const whoAmI = useQuery(whoAmIQuery());
  const listRef = useRef<HTMLElement>(null);
  /** The list column, which scrolls in itself beside the reading column. */
  const listScroller = useRef<HTMLDivElement>(null);
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. */
  const paneRef = useRef<HTMLDivElement>(null);
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adoptedInbox = useMotionHold(inbox.data, listRef);
  // The credential requests the Inbox lists above its asks: the section, the banner and the empty
  // state all read this one answer.
  const credentials = useCredentialRequests();
  const viewer = whoAmI.data?.login;
  // Who the list is for: what `?assignee=` carries, and the reader alone when it carries
  // nothing. The address is the whole memory - a filter the reader can link, and nothing kept
  // behind their back.
  const selection = filter.assignees ?? (viewer === undefined ? [] : [viewer]);
  // The popover's open state lives here because Escape has to know about it (see
  // `clear-selection` below).
  const [filterOpen, setFilterOpen] = useState(false);
  const selectAssignees = (next: readonly string[]) => {
    if (onAssigneesChange) {
      onAssigneesChange(next);
    } else {
      // Replace rather than push: a reader trying people out would otherwise have to walk Back
      // through every pick, the rule every URL-backed filter here holds to
      // (`lib/url-array-params.ts`).
      navigate(buildInboxPath({ ...filter, assignees: next }), { replace: true });
    }
  };
  const agentText = useAgentText();
  // Rows (by ask id) whose "Assign to me" write is in flight or failed: their control stays
  // mounted through the optimistic assignment and the rollback out of it (see `AssignToMe`).
  const [assigning, setAssigning] = useState<ReadonlySet<string>>(() => new Set());
  const onAssignLive = useCallback((askId: string, live: boolean) => {
    setAssigning((current) => {
      if (current.has(askId) === live) return current;
      const next = new Set(current);
      if (live) next.add(askId);
      else next.delete(askId);
      return next;
    });
  }, []);
  // The same registration for snooze writes: a row whose write is live stays rendered even
  // when Later is folded, so `Snoozing…` and a refusal are not unmounted by the fold the
  // optimistic update triggers (see `SnoozeControl`).
  const [snoozing, setSnoozing] = useState<ReadonlySet<string>>(() => new Set());
  const onSnoozeLive = useCallback((askId: string, live: boolean) => {
    setSnoozing((current) => {
      if (current.has(askId) === live) return current;
      const next = new Set(current);
      if (live) next.add(askId);
      else next.delete(askId);
      return next;
    });
  }, []);
  // The rows the reader has marked, by ask id: `x` and the row's own checkbox are the two ways
  // in. An id the server no longer lists drops out here, so a row that is listed again comes back
  // unmarked; a mark whose row this view does not list, or whose band is folded shut, drops out
  // of `selected` below, so the bar never counts - and `h` never writes - a row that is not on
  // the page. The mark itself outlives both: open the band, or switch back, and it is still made.
  const [marked, setMarked] = useState<ReadonlySet<string>>(() => new Set());
  // The bulk write lives here rather than in the bar, because the optimistic snooze can take
  // every marked row off this view at once and a bar that owned the write would be unmounted
  // before its own refusal arrived. The bar reports one pick downward and renders what it is
  // told. The pick's size is held with it, because that same move empties the selection the bar
  // would otherwise count: `0 selected` while two asks are being snoozed.
  const bulkWrite = useAskSnoozeMany();
  const [bulkSnoozing, setBulkSnoozing] = useState(0);
  const bulkBarRef = useRef<HTMLFieldSetElement>(null);
  const [bulkRefusal, setBulkRefusal] = useState<string | undefined>(undefined);
  const clearSelection = useCallback(() => {
    setMarked(new Set());
    setBulkRefusal(undefined);
  }, []);
  const onMark = useCallback((askId: string) => {
    setMarked((current) => {
      const next = new Set(current);
      if (!next.delete(askId)) next.add(askId);
      return next;
    });
  }, []);
  const listedIds = adoptedInbox?.map((row) => row.id).join(" ");
  useEffect(() => {
    if (listedIds === undefined) return;
    const listed = new Set(listedIds === "" ? [] : listedIds.split(" "));
    setMarked((current) =>
      [...current].every((id) => listed.has(id))
        ? current
        : new Set([...current].filter((id) => listed.has(id)))
    );
  }, [listedIds]);
  // What this list holds, and computed here rather than after the early returns so the bindings
  // below speak for exactly the rows on the page.
  const agent = filter.agent;
  const fromAgent =
    agent === undefined
      ? (adoptedInbox ?? [])
      : (adoptedInbox ?? []).filter(
          (ask) => ask.author.kind === "session" && ask.author.id === agent
        );
  // The filter decides what is listed, with one exception: a row whose "Assign to me" write is
  // live stays. Taking a question under the Unassigned filter is exactly that case - the row
  // stops matching the moment the optimistic assignment lands - and unmounting the control
  // would take `Assigning…`, a refusal and its Retry off the screen with it.
  const shown = (filter.section === "needs-you" ? waitingOnHuman(fromAgent) : fromAgent).filter(
    (row) => inAssigneeFilter(row, selection) || assigning.has(row.id)
  );
  // Later starts folded: its rows are the ones the reader has already dealt with by deferring.
  const [laterOpen, setLaterOpen] = useState(false);
  const jumpToGroup = useGroupJump(listRef, () => setLaterOpen(true));
  // Where each row sits, judged against the clock at render: a snoozed row rejoins its turn band
  // on the first render after its moment passes. Computed here, with the selection, rather than
  // after the early returns, because the bar's count and the bindings both read it.
  const place = viewer === undefined ? undefined : Date.now();
  // A row in a band the reader has not opened is not on the page: the band renders its heading
  // and count and none of its rows, so there is no checkbox to untick, and the reader may well
  // have set that row's own moment since marking it - a pick that wrote it would overwrite what
  // they chose. A row whose own snooze write is still in flight stays rendered inside the fold,
  // so it still counts. One rule, read by the selection here and by the render below.
  const inShutBand = (row: InboxRow, section: InboxSection | undefined): boolean =>
    section !== undefined &&
    COLLAPSED_SECTIONS[section] === true &&
    !laterOpen &&
    !snoozing.has(row.id);
  const selected = shown
    .filter(
      (row) =>
        marked.has(row.id) && (place === undefined || !inShutBand(row, sectionOf(row, place)))
    )
    .map((row) => row.id);
  // One pick, over the ids the bar is counting: the same optimistic move and rollback the per-row
  // control makes, once per ask. The ids the server took leave the selection; the ones it refused
  // are marked again under its own reason, even if Clear ran while the write was in flight, so a
  // second pick retries exactly those.
  const snoozePick = (until: string) => {
    const ids = [...selected];
    setBulkRefusal(undefined);
    setBulkSnoozing(ids.length);
    void bulkWrite.submit(ids, until).then(({ failed }) => {
      setBulkSnoozing(0);
      setBulkRefusal(refusalText(failed, ids.length));
      const refused = new Set(failed.map(({ askId }) => askId));
      const taken = new Set(ids.filter((id) => !refused.has(id)));
      setMarked((current) => {
        const next = new Set([...current].filter((id) => !taken.has(id)));
        for (const id of refused) next.add(id);
        return next;
      });
    });
  };
  // The bar, and the Clear it carries, shows while a mark is on the page, a pick is in the air or a
  // refusal is showing. Escape with no row focused is the keyboard's Clear and is offered on this
  // same predicate, so the two cannot drift: a pick whose optimistic move folds every marked row
  // into Later empties `selected`, not the marks behind it, and Clear still has work to do.
  const bulkBarShown = selected.length > 0 || bulkSnoozing !== 0 || bulkRefusal !== undefined;
  // The row the reader's hand is on (focus or pointer), read from the DOM as last committed. When
  // the server has dropped it (answered or resolved elsewhere) or handed its turn the other way
  // (their own ask-back, an agent's note or reply), it is kept - `held` - where the reader last
  // saw it, until their focus or pointer leaves it; `release` re-renders so the row is chosen
  // afresh without it and takes its new place then. That covers the reader's own answer while it
  // is in flight (the card shows Answering…, or the error if the server refuses it); once the
  // server has recorded it the row is not held - their click was the end of that interaction -
  // and it leaves with the refetch the success triggers. Assign to me has no such exclusion: the
  // one filter that offers it is `Unassigned`, which the settled assignment drops the row out of,
  // so the row is held where the reader last saw it until their focus or pointer leaves it, like
  // any other dropped row; `assigning` above keeps it listed only while the write is live.
  const viewport = useRef<ViewportAnchor>(null);
  const anchor = viewport.current?.interacted() ?? null;
  const presented = useRef<readonly PlacedRow[]>([]);
  /** Every row this list holds, a folded band's included: what `?item=` is answered against. */
  const listedRows = useRef<readonly PlacedRow[]>([]);
  const answered = useRef<string | null>(null);
  const [, setReleased] = useState(0);
  const release = () => setReleased((count) => count + 1);
  // The row the reading column is showing. The address holds it (`?item=`), so a reload or a
  // link keeps the reader's place; below the column's width, and in the drawer, there is no
  // selection and nothing is written. `shownItem` is the `?item=` this list has already
  // answered: without it, writing the selection to the address would read back as a reader's
  // own navigation and select the row again, one step behind every `j`.
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const shownItem = useRef<string | undefined>(undefined);
  const select = (askId: string | undefined) => {
    setSelectedId(askId);
    shownItem.current = askId;
    if (askId !== filter.item) {
      // Replace, never push: a reader walking the list with `j` would otherwise have to press
      // Back once per row to leave the page, the rule the assignee filter already holds to.
      // Selecting nothing clears `?item=` rather than leaving it on the row that has just
      // left: a reload reads the address, and an address naming a row the reader deferred
      // would open the folded band and put that question back in front of them.
      navigate(buildInboxPath({ ...filter, item: askId }), { replace: true });
    }
  };
  // Where the selection goes when the row it is on leaves the bands: the next row the reader
  // can see after it, else the nearest above it, so the column always shows a question they
  // can answer. One rule for both ways a row leaves by the reader's own hand - their answer,
  // which takes it at once below, and their snooze, which takes it once the write lands - and
  // read on every commit rather than when it is wanted, because a snooze folds its row into
  // `Later` the moment they pick: while that write is out the read is held, so where they are
  // sent is read off the list as it stood when they deferred.
  const nextAfterSelected = useRef<string | undefined>(undefined);
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly. The reader's own answer
  // is also one of the two things that move the selection for them.
  const recordAnswered = (id: string) => {
    answered.current = id;
    if (paneShown && selectedId === id) select(nextAfterSelected.current);
    release();
  };
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  const step = (delta: 1 | -1) => roveFocus(rows(), rowAround(document.activeElement), delta);
  /** Selects the row `delta` from the selected one, in the order the reader sees: what `j`,
   *  `k` and the arrows do while the reading column is on, in place of roving the focus. */
  const selectStep = (delta: 1 | -1) => {
    const visible = presented.current;
    const at = visible.findIndex((placed) => placed.ask.id === selectedId);
    const next = visible[at === -1 ? (delta === 1 ? 0 : visible.length - 1) : at + delta];
    if (next !== undefined) select(next.ask.id);
  };
  /** Where the Inbox's keys act: the reading column while it is on - its head's Snooze, owner
   *  link and Copy ref, and the answer form of whatever it is showing - else the row holding
   *  the focus. Null where there is neither, which is an Inbox with nothing selected (a wide
   *  but empty list, or the commit a snooze of the selected row leaves behind) and a list
   *  nothing in is focused. */
  const hand = () => (paneShown ? paneRef.current : focusedRow());
  /** The control `selector` names in whichever of the two that is, or null for none: every
   *  `when` below tests this against `null`, so the accessor never answers `undefined`. */
  const inHand = (selector: string) => hand()?.querySelector<HTMLElement>(selector) ?? null;
  const focusInPane = () => paneRef.current?.contains(document.activeElement) === true;
  const focusInList = () => listRef.current?.contains(document.activeElement) === true;
  /** The row the column is showing, or null while it is showing none. Held to the selection,
   *  because the effect keeping that row on screen is the selection's and nothing else's. */
  const selectedRow = useCallback(
    () =>
      selectedId === undefined
        ? null
        : (listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${selectedId}"]`) ??
          null),
    [selectedId]
  );
  // The bar is reached through its own ref: the empty state a pick can leave behind renders it
  // outside the list.
  const bulkPicker = () => bulkBarRef.current?.querySelector<HTMLElement>(BULK_PICKER_SELECTOR);
  // Which row `h` was pressed on, so Escape from the bulk picker - which sits above the bands and
  // has no row to fall back through - is one level out rather than a dead end.
  const pickerOrigin = useRef<string | null>(null);
  useKeymapScope(keymapScope);
  useKeymap(keymapScope, [
    // Movement is what the arrow keys are for; a palette row that moves the cursor helps nobody.
    // With the reading column on, movement is the selection's: the column follows it, and the
    // focus stays where the reader left it in the list.
    {
      id: "next",
      keys: "j",
      label: "Next ask",
      palette: false,
      run: () => (paneShown ? selectStep(1) : step(1)),
      when: () => rows().length > 0,
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous ask",
      palette: false,
      run: () => (paneShown ? selectStep(-1) : step(-1)),
      when: () => rows().length > 0,
    },
    {
      id: "arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: paneShown
        ? "Next / previous ask while the focus is in the list"
        : "Next / previous ask while a row is focused",
      run: (event) => {
        const delta = event.key === "ArrowDown" ? 1 : -1;
        if (paneShown) {
          selectStep(delta);
        } else {
          step(delta);
        }
      },
      // In the reading column the arrows are the column's - a document scrolls, an option list
      // moves - so they move the selection only while the focus is in the list beside it.
      when: () => (paneShown ? focusInList() : focusedRow() !== null),
    },
    {
      id: "answer",
      keys: "Enter",
      label: paneShown ? "Answer the selected ask" : "Answer the focused ask",
      run: () => {
        // A decision block's compact card folds its answer box behind "Add a note or answer in
        // your own words"; Enter opens that disclosure and lands in the box it reveals. Only
        // the column draws that compact card: from a row the key goes straight to the box.
        if (paneShown && inHand("[data-ask-answer]") === null) {
          inHand(NOTE_DISCLOSURE_SELECTOR)?.click();
        }
        inHand("[data-ask-answer]")?.focus();
      },
      // Enter is the list's key: it carries the reader from the row they are on into the
      // column's answer box. Inside the column the key is already somebody's - the card's own
      // `Ask back instead`, a link in the head, an option - and the registry's handler calls
      // `preventDefault`, so pressing it there would press nothing. `h` stands down inside an
      // ask card for the same reason (`outsideAskCard`); Enter's own is the whole column,
      // since the head's controls take it too.
      when: () =>
        paneShown
          ? !focusInPane() &&
            (inHand("[data-ask-answer]") !== null || inHand(NOTE_DISCLOSURE_SELECTOR) !== null)
          : inHand("[data-ask-answer]") !== null,
    },
    {
      id: "option",
      keys: ["1", "2", "3", "4", "5", "6", "7", "8", "9"],
      label: paneShown
        ? "Select option 1–9 of the selected ask"
        : "Select option 1–9 of the focused ask",
      run: (event) => {
        const options = hand()?.querySelectorAll<HTMLElement>("[data-ask-option]");
        options?.[Number(event.key) - 1]?.click();
      },
      when: () => inHand("[data-ask-option]") !== null,
    },
    {
      id: "open",
      keys: "o",
      label: "Open the ask's issue or document",
      run: () => inHand("[data-inbox-owner]")?.click(),
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      id: "copy-ref",
      keys: "y",
      label: paneShown ? "Copy the selected ask's reference" : "Copy the focused ask's reference",
      run: () => inHand(COPY_REF_SELECTOR)?.click(),
      when: () => inHand(COPY_REF_SELECTOR) !== null,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the ask row, then clear the focused row",
      run: () => {
        // From the reading column, Escape is the way back to the row the column is showing: the
        // focus belongs in the list, and the three ways into the column are deliberate.
        if (paneShown && focusInPane()) {
          selectedRow()?.focus();
          return;
        }
        const row = rowAround(document.activeElement);
        if (row === focusedRow()) {
          row?.blur();
        } else {
          row?.focus();
        }
      },
      when: () =>
        rowAround(document.activeElement) !== null ||
        (paneShown && focusInPane() && selectedRow() !== null),
    },
    {
      // Marking walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
      id: "select",
      keys: "x",
      label: paneShown
        ? "Select or deselect the selected ask"
        : "Select or deselect the focused ask",
      palette: false,
      run: () => {
        const askId = paneShown ? selectedId : focusedRow()?.dataset.inboxRow;
        if (askId !== undefined) onMark(askId);
      },
      when: () => (paneShown ? selectedId !== undefined : focusedRow() !== null),
    },
    {
      id: "snooze",
      keys: "h",
      // One key for both: with rows marked it is the whole selection, otherwise the row in hand.
      label: "Snooze the focused ask, or every selected ask",
      run: () => {
        if (selected.length === 0) {
          // The short row carries no Snooze of its own: the reading column's head does, for the
          // row it is showing.
          inHand("[data-inbox-snooze]")?.focus();
          return;
        }
        bulkPicker()?.focus();
      },
      // The selection's key works from wherever marking leaves the reader - a row, the checkbox
      // they just ticked, the bar - but not from inside an ask they are answering: an option
      // radio takes no typed text, so the registry passes the key through, and the card's own
      // controls are not the list's. The issue page withholds its writing keys the same way.
      // Inside the column that rule covers the lone Snooze too, the card there being the
      // column's own.
      when: () =>
        (selected.length > 0 && outsideAskCard()) ||
        (inHand("[data-inbox-snooze]") !== null && (!paneShown || outsideAskCard())),
    },
    {
      id: "back-from-bulk",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the bulk snooze picker",
      run: () => {
        const origin = pickerOrigin.current;
        const row =
          origin === null
            ? null
            : listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${origin}"]`);
        // With every marked row gone there is no row to return to, so the bar itself is the
        // level out: it holds the count and the way to clear it. It is reached through its own
        // ref, because the empty state a pick can leave behind renders it outside the list.
        (row ?? rows()[0] ?? bulkBarRef.current)?.focus();
      },
      when: () => document.activeElement?.matches(BULK_PICKER_SELECTOR) === true,
    },
    {
      id: "clear-selection",
      keys: "Escape",
      // Escape is one level out, and the selection is the outermost thing a row press made:
      // `back` takes the reader off the row first, and this clears what they marked. It is
      // offered exactly while the bar's Clear is, a pick in flight and a refusal the reader has
      // since unticked the rows of included - the keyboard that raised them dismisses them. Not a
      // palette row: in the palette Escape closes the palette, so the row would name a key that
      // does something else there.
      label: "Clear the selection",
      palette: false,
      run: clearSelection,
      // The popover takes Escape first: one press closes it, and dropping what the reader
      // marked at the same time would be two undos for one key.
      when: () => focusedRow() === null && bulkBarShown && !filterOpen,
    },
  ]);
  // What the selection is, settled after each commit against the rows this list holds.
  //
  // The address names it, and is read as the reader's own move - the first visit, a link, a
  // reload - exactly while it says something this list has not already answered: an ask it holds
  // is selected (its band opened first, if the reader linked into the folded one: following the
  // link is what they asked for), and one it does not hold leaves the first visible row
  // selected and the address rewritten. Otherwise the selection is the reader's, and the address
  // follows it.
  useEffect(() => {
    // A list that holds nothing - still loading, failed, or genuinely empty - answers no
    // address: `?item=` is left unread until there are rows to read it against.
    if (!paneShown || listedRows.current.length === 0) {
      return;
    }
    const visible = presented.current;
    if (filter.item !== shownItem.current) {
      shownItem.current = filter.item;
      const wanted = listedRows.current.find((placed) => placed.ask.id === filter.item);
      if (wanted !== undefined) {
        setSelectedId(wanted.ask.id);
        if (!visible.some((placed) => placed.ask.id === wanted.ask.id)) setLaterOpen(true);
        return;
      }
    }
    if (visible.some((placed) => placed.ask.id === selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    // The row the selection was on has left the bands - the reader deferred it, the server
    // took it away - so the selection goes where that row was heading and the address follows
    // it, down to naming no row at all when the list has none left to show.
    const next = nextAfterSelected.current;
    select(visible.some((placed) => placed.ask.id === next) ? next : visible[0]?.ask.id);
  });
  // The selected row is kept on screen as the reader walks the list: the column beside it shows
  // the row they cannot see otherwise.
  useEffect(() => {
    if (!paneShown || selectedId === undefined) {
      return;
    }
    selectedRow()?.scrollIntoView?.({ block: "nearest" });
  }, [paneShown, selectedId, selectedRow]);

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  // Which row the list holds in place while the server moves the list under it. Without the
  // reading column that is the row the reader's hand is on (focus or pointer); with it, it is
  // the row the column is showing - the reader is reading it, wherever their hand is - and the
  // hold lasts until they select another, rather than until they look away.
  const handId = paneShown ? selectedId : anchor?.id;
  const seen =
    handId === undefined || handId === answered.current
      ? undefined
      : presented.current.find(({ ask }) => ask.id === handId);
  const listed = seen === undefined ? undefined : shown.find((ask) => ask.id === seen.ask.id);
  const now = listed === undefined ? undefined : sectionOf(listed, place);
  const held = heldRow(seen, listed, now);
  // The bands as this list heads them: another person's question is not "Waiting on you".
  const titles = sectionTitles(selection, viewer);
  const assigneeFilter = (
    <AssigneeFilter
      onChange={selectAssignees}
      onOpenChange={setFilterOpen}
      open={filterOpen}
      selected={selection}
      viewer={viewer}
    />
  );
  // The filtered agent as its asks wrote it (the title it stamped, its attribution), or only its
  // id when none of its asks is listed; either way its registry title names it when listed.
  // Without the filter, `fromAgent` is every row, so it names no agent.
  const agentAuthor = agent === undefined ? undefined : fromAgent[0]?.author;
  const agentSubject =
    agent === undefined
      ? undefined
      : agentAuthor === undefined
        ? { sessionId: agent, storedTitle: undefined }
        : { actor: agentAuthor };
  const agentTitle = agentSubject === undefined ? undefined : agentText.part(agentSubject);
  // The clear-filter link keeps its own text and name; the agent's live link sits beside it.
  // `whitespace-pre` keeps the space before an attribution suffix, which a flex item would drop.
  const chip =
    agentSubject === undefined ? null : (
      <AgentName
        {...agentSubject}
        className="flex items-center whitespace-pre"
        control={(name) => (
          <Link
            aria-label="Clear agent filter"
            className="inline-flex min-h-11 items-center rounded-full"
            to={buildInboxPath({ assignees: filter.assignees, item: filter.item })}
          >
            <LabelPill selected>
              Asks from {name}
              {filter.section === "needs-you" ? " waiting on you" : ""} · clear
            </LabelPill>
          </Link>
        )}
        kind="control"
      />
    );

  // The bar outlives an empty list: a pick that empties this view leaves its write in the air, and
  // the refusal that comes back has to land somewhere the reader can see.
  const bulkBar = bulkBarShown ? (
    <BulkSnoozeBar
      barRef={bulkBarRef}
      count={selected.length}
      onClear={clearSelection}
      onPick={snoozePick}
      onPickerFocus={(from) => {
        pickerOrigin.current = rowAround(from)?.dataset.inboxRow ?? null;
      }}
      pending={bulkSnoozing}
      refusal={bulkRefusal}
    />
  ) : null;
  // Everything above the bands, the same whether or not any band has a row.
  const header = (
    <>
      <CredentialRequestsSection credentials={credentials} />
      {assigneeFilter}
      {chip}
      {agent === undefined ? (
        <BlockedOnYou
          asks={adoptedInbox ?? []}
          credentialRequests={credentials.requests}
          viewer={viewer}
        />
      ) : null}
      {bulkBar}
    </>
  );

  if (shown.length === 0 && held === undefined) {
    const emptyMessage = emptyStateMessage({ agent, agentTitle, credentials, selection, viewer });
    // Nothing to read, so nothing to select and no column beside the list: the list column is
    // the whole Inbox, as it is below the reading column's width.
    presented.current = [];
    listedRows.current = [];
    return (
      <div className="space-y-6" data-testid="inbox-list">
        {header}
        {emptyMessage === undefined ? null : (
          <EmptyState label="Inbox empty state" message={emptyMessage} />
        )}
      </div>
    );
  }

  const rowsIn = (section: InboxSection) =>
    withHeld(
      shown.filter((ask) => sectionOf(ask, place) === section && ask.id !== held?.ask.id),
      held?.section === section ? held.ask : undefined,
      presented.current
    );
  const sections = INBOX_SECTIONS.map((section) => {
    const rows = rowsIn(section);
    const shownRows = rows.filter((ask) => !inShutBand(ask, section));
    // The same rule the selection above reads, so keyboard roving, the viewport anchor,
    // `presented` and the bar all see exactly what the reader sees.
    return { groups: groupRows(shownRows), rows, section, shownRows };
  }).filter(({ rows }) => rows.length > 0);
  // Cross-band counts include folded rows: a Later target is opened before the jump focuses it.
  const notes = crossBandCounts(sections);
  presented.current = sections.flatMap(({ groups, section }) =>
    groups.flatMap((group) => group.rows.map((ask) => ({ ask, section })))
  );
  // Every row, a folded band's included: `?item=` naming a row the reader deferred opens that
  // band rather than being read as an address this list cannot answer.
  listedRows.current = sections.flatMap(({ rows, section }) =>
    rows.map((ask) => ({ ask, section }))
  );
  // Names are read off the rows as they render - band by band, top to bottom - so "ask 2 of 2"
  // never sits above "ask 1 of 2", and a twin folded away in `Later` leaves the row on screen
  // with the short name rather than a count of something the reader cannot see.
  const ordinals = askOrdinals(presented.current.map(({ ask }) => ask));
  const selectedAsk = paneShown
    ? presented.current.find(({ ask }) => ask.id === selectedId)?.ask
    : undefined;
  // Read here, where the rows stand as the reader sees them, and held while a snooze of the
  // selected row is out: that write folds its row into `Later` on the pick, so a read taken
  // after it would answer with the row above rather than the row that took its place.
  if (selectedId === undefined || (!snoozing.has(selectedId) && bulkSnoozing === 0)) {
    const at = presented.current.findIndex(({ ask }) => ask.id === selectedId);
    nextAfterSelected.current =
      at === -1 ? undefined : (presented.current[at + 1] ?? presented.current[at - 1])?.ask.id;
  }

  // One list, keyed by ask id, with the section headings as items between the rows: a row that
  // changes section moves within the same parent, so React moves its node instead of remounting
  // it - its draft, selection, disclosures, and focus stay, and the viewport anchor can find it.
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it.
    <div
      className={paneShown ? "min-w-0 flex-1 overflow-y-auto pr-1" : undefined}
      data-testid="inbox-list"
      ref={listScroller}
    >
      <ViewportAnchor
        className="space-y-4"
        group="data-inbox-section"
        item={ROW_ATTRIBUTE}
        ref={viewport}
        rootRef={listRef}
        scroller={paneShown ? listScroller : undefined}
      >
        {header}
        <ul className="space-y-3">
          {sections.flatMap(({ groups, rows, section }, index) => [
            <li
              className={index === 0 ? undefined : "pt-2"}
              key={`heading-${section}`}
              role="presentation"
            >
              {/* A band divider, not a heading that competes with the questions under it. */}
              <h2 className={`text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}>
                {COLLAPSED_SECTIONS[section] === true ? (
                  <DisclosureToggle
                    expanded={laterOpen}
                    label={`${titles[section]} (${rows.length})`}
                    onToggle={() => setLaterOpen((open) => !open)}
                    textClassName={textMutedOnCanvas}
                  />
                ) : (
                  titles[section]
                )}
              </h2>
            </li>,
            ...bandGroupItems({
              groups,
              notes: notes.get(section),
              onJump: jumpToGroup,
              renderRow: (ask, group) => (
                <InboxItem
                  ask={ask}
                  assignLive={assigning.has(ask.id)}
                  grouped={group.rows.length > 1}
                  key={ask.id}
                  marked={marked.has(ask.id)}
                  ordinal={ordinals.get(ask.id)}
                  ownerKey={group.owner.key}
                  onAnswered={recordAnswered}
                  onAssignLive={onAssignLive}
                  onMark={onMark}
                  onSelect={paneShown ? select : undefined}
                  onSnoozeLive={onSnoozeLive}
                  // The reader's hand releases a held row only where it is what holds it: with
                  // the reading column on, the selection holds the row, and moving it releases.
                  onRelease={!paneShown && ask.id === held?.ask.id ? release : undefined}
                  section={section}
                  selected={ask.id === selectedId}
                  threadUpdatedAt={inbox.dataUpdatedAt}
                  viewer={viewer}
                />
              ),
              section,
              titles,
            }),
          ])}
        </ul>
      </ViewportAnchor>
    </div>
  );

  if (!paneShown) {
    return list;
  }
  // Two columns, the reading column to the right of the list, each scrolling in itself inside
  // the height the page gives them (`app.tsx` makes the Inbox page fill the viewport here).
  //
  // One root, selection or none: the column is what appears and disappears beside the list,
  // never the list itself. A selection empties for a commit whenever the selected row leaves
  // the rendered bands - the reader's own bulk snooze of that row does it, before the effect
  // above hands the selection on - and a root that changed shape there would have React
  // unmount the whole list, taking the keyboard's focus, the bulk bar and every row's state
  // with it and mounting a fresh one a commit later.
  return (
    <div className="flex min-h-0 flex-1 gap-6">
      {list}
      {selectedAsk === undefined ? null : (
        <div className="min-w-0 flex-1" ref={paneRef}>
          <ReadingPane
            ask={selectedAsk}
            onAnswered={recordAnswered}
            onSnoozeLive={onSnoozeLive}
            viewer={viewer}
          />
        </div>
      )}
    </div>
  );
}
