import { INBOX_DISMISSED_PAGE } from "@legion/contracts";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useQuery } from "@tanstack/react-query";
import {
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow, MyReplyRow } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { capSearchQuery } from "../../lib/query-param";
import {
  borderDefault,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { searchWords } from "../agents/agent-search";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { COPY_REF_SELECTOR, CopyRefButton } from "../refs/CopyRefButton";
import {
  buildInboxPath,
  INBOX_FILTER_FIELDS,
  type InboxRoute,
  type InboxShow,
  parseInboxSearch,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type KeymapScope, useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { useThreadOrder } from "../threads/thread-order";
import { AskCard } from "./AskCard";
import { InboxOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";
import { AssignToMe } from "./AssignToMe";
import { formatAskAge } from "./ask-age";
import { outsideAskCard } from "./ask-card";
import { type AskOrdinal, askOrdinals, controlName } from "./ask-name";
import { askUrgency } from "./ask-urgency";
import { BlockedOnYou, waitingOnHuman } from "./BlockedOnYou";
import { BulkBar } from "./BulkBar";
import { DismissControl, RowDismissContext } from "./DismissControl";
import { ALL_GROUP_KEY, group, inFieldFilter, LATER_GROUP_KEY, sortRows } from "./grouping";
import { InboxBar, type InboxFilters, type InboxPopover } from "./InboxBar";
import { GROUP_PAGE, groupItems } from "./InboxGroups";
import { ROW_ATTRIBUTE, ROW_SELECTOR } from "./inbox-row";
import { matchesInboxSearch } from "./inbox-search";
import {
  type InboxListRow,
  rowId,
  rowLastActivityAt,
  rowPriority,
  rowType,
  rowUrgency,
  threadReferenceRoute,
} from "./list-row";
import { useMarkAllRead } from "./mark-all";
import { useMotionHold } from "./motion-hold";
import { PaneDivider, usePaneWidth } from "./PaneDivider";
import { type PlacedRow, usePaneSelection } from "./pane-selection";
import { useFoldedGroups } from "./preferences";
import { questionLine } from "./question-line";
import { ReadingPane } from "./ReadingPane";
import { RowKindIcon, RowPriorityTag, RowUrgencyMark } from "./RowMarks";
import { SnoozeControl } from "./SnoozeControl";
import {
  type InboxSection,
  inAssigneeFilter,
  isOwnList,
  isUnassigned,
  sectionOf,
  sectionTitles,
} from "./sections";
import { ThreadRowCard } from "./ThreadRowCard";
import { ThreadTitle } from "./ThreadTitle";
import { dismissRow, type InboxDismissRow } from "./useInboxDismiss";
import { useInboxSelection } from "./useInboxSelection";
import { useInboxThreadRows } from "./useInboxThreadRows";

/** When an agent replied last on a row that still needs the viewer, that reply is surfaced so
 *  the viewer knows to look before answering. Whose turn it is ("Waiting on you" / "Waiting on
 *  <agent>") is the card's own status line. */
function InboxRowChip({ ask }: { ask: InboxRow }): ReactNode {
  if (ask.waiting_on === "human" && ask.last_reply?.author.kind === "session") {
    // One span, so the pill's flex box keeps the space between the name and "replied".
    return (
      <LabelPill>
        <span>
          <AgentName actor={ask.last_reply.author} inline /> replied
        </span>
      </LabelPill>
    );
  }
  return null;
}

/** The bulk bar's snooze picker: `h` hands it focus and Escape takes it back to the list. */
const BULK_PICKER_SELECTOR = "[data-inbox-bulk-snooze]";

/** The reading column's own scroller: `l` hands it focus and Escape takes it back to the list. */
const PANE_SCROLLER_SELECTOR = '[data-testid="inbox-pane-scroller"]';

/** The "Add a note or answer in your own words" disclosure an `AskCard` folds its answer box
 *  behind in its compact variant - the card a decision block hosts, which is what the reading
 *  column shows for a question written as one - while it is still shut. The card carries the
 *  handle (`AskCard.tsx`), as it carries `data-ask-answer` and `data-ask-option`. */
const NOTE_DISCLOSURE_SELECTOR = '[data-ask-note-disclosure][aria-expanded="false"]';

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  return focusedMatching(ROW_SELECTOR);
}

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

/** Everything inside a short row that answers a click itself: its links, its checkbox, and
 *  whatever control a row gains later. A click on one of those is that control's - the link
 *  opens the ask inside its issue or document, the checkbox marks the row - so the row's own
 *  selection stands down for it, the rule the margin's cards hold to (`margin/ThreadCard.tsx`).
 *  Everywhere else in the row, its title and its time included, the click is the row's and
 *  selects it. */
const ROW_CONTROL_SELECTOR = "a, button, input, label, select, textarea";

/** What every row layout is given: the row, where it sits, and the reader's marks on it. */
interface InboxRowProps {
  /** The band this row is drawn under, for the fold and the held-row rule. */
  group: string;
  /** Whether the reader has marked this row for a bulk action. */
  marked: boolean;
  /** Marks or unmarks this row, by its own `rowId`; `x` and the row's own checkbox are the two
   *  ways in. */
  onMark: (id: string) => void;
  /** Set on the held row - one the server dropped or moved to another band that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  row: InboxListRow;
  section: InboxSection;
}

/** The row's own checkbox, the pointer's way to the mark `x` makes. Every row takes one: with
 *  rows marked the bulk bar offers **Snooze** for the asks among them and **Dismiss** for the
 *  threads. */
function MarkCheckbox({
  id,
  marked,
  name,
  onMark,
}: {
  id: string;
  marked: boolean;
  name: string;
  onMark: (id: string) => void;
}): ReactNode {
  return (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      onChange={() => onMark(id)}
      type="checkbox"
    />
  );
}

/** The `<li>` both layouts sit in: where the row is, what the reader's hand does to it, and the
 *  marks the selected row carries. Only the short row is selectable, so only it is ever drawn
 *  as the selected one. */
function InboxRowShell({
  children,
  group: groupKey,
  onRelease,
  onSelect,
  row,
  section,
  selected,
}: {
  children: ReactNode;
  group: string;
  onRelease: (() => void) | undefined;
  /** Selects this row, which is what the reading column shows; the short row alone has one. */
  onSelect: ((id: string) => void) | undefined;
  row: InboxListRow;
  section: InboxSection;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}): ReactNode {
  const id = rowId(row);
  const releaseOnFocusOut =
    onRelease === undefined
      ? undefined
      : (event: FocusEvent<HTMLLIElement>) => {
          if (!event.currentTarget.contains(event.relatedTarget)) onRelease();
        };
  return (
    /* The keyboard selects a row through the Inbox's own registry (`j`, `k` and the arrows),
       which needs no handler on the row itself; the click is the pointer's way to that same
       selection.
       biome-ignore lint/a11y/useKeyWithClickEvents: explained just above. */
    <li
      aria-current={selected ? "true" : undefined}
      className={`group rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing} ${
        selected ? surfaceMutedBg : ""
      }`}
      data-inbox-group={groupKey}
      data-inbox-row={id}
      data-inbox-section={section}
      data-inbox-selected={selected ? "true" : undefined}
      onBlur={releaseOnFocusOut}
      onClick={
        onSelect === undefined
          ? undefined
          : (event) => {
              if (
                event.target instanceof Element &&
                closestMatching(event.target, ROW_CONTROL_SELECTOR) !== null
              ) {
                return;
              }
              onSelect(id);
            }
      }
      onPointerLeave={onRelease}
      tabIndex={-1}
    >
      {children}
    </li>
  );
}

/** What a short row adds to every row layout's props: beside the reading column the row is
 *  selectable, and the one the column shows is drawn as selected. */
interface ShortRowProps {
  onSelect: (id: string) => void;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}

/**
 * The frame both short rows draw in, two lines deep: the kind's icon at the left edge, then
 * what the row is about and when it was last written in; under them the priority tag, an ask's
 * urgency, and what the row says about its owner. Every mark is `shrink-0`, so the title is
 * what shortens and neither the marks nor the key are ever cut.
 */
function ShortRowFrame({
  age,
  check,
  group: groupKey,
  onRelease,
  onSelect,
  owner,
  row,
  section,
  selected,
  title,
}: ShortRowProps & {
  /** When the row was last written in, as its own kind reads a time. */
  age: ReactNode;
  /** The mark checkbox, or the space a row that takes no mark leaves where it would be. */
  check: ReactNode;
  group: string;
  onRelease: (() => void) | undefined;
  /** The second line below the marks: the row's owner, and whatever it carries beside it. */
  owner: ReactNode;
  row: InboxListRow;
  section: InboxSection;
  title: ReactNode;
}): ReactNode {
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      row={row}
      section={section}
      selected={selected}
    >
      <div className="flex flex-col gap-0.5 py-1.5">
        <div className="flex items-center gap-2">
          {check}
          <RowKindIcon type={rowType(row)} />
          {title}
          <time
            className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`}
            dateTime={rowLastActivityAt(row)}
          >
            {age}
          </time>
        </div>
        <div className="flex min-w-0 items-baseline gap-2 pl-6">
          <RowPriorityTag priority={rowPriority(row)} />
          <RowUrgencyMark urgency={rowUrgency(row)} />
          {owner}
        </div>
      </div>
    </InboxRowShell>
  );
}

/** An open question beside the reading column: its first line, how long it has waited, and the
 *  issue or document it belongs to. */
function AskShortRow({
  ask,
  group: groupKey,
  marked,
  onMark,
  onRelease,
  onSelect,
  ordinal,
  row,
  section,
  selected,
}: InboxRowProps & ShortRowProps & { ask: InboxRow }): ReactNode {
  return (
    <ShortRowFrame
      age={formatAskAge(rowLastActivityAt(row))}
      check={
        <MarkCheckbox
          id={ask.id}
          marked={marked}
          name={controlName(ask, ordinal)}
          onMark={onMark}
        />
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={<InboxOwnerLink ask={ask} rowOwner />}
      row={row}
      section={section}
      selected={selected}
      title={<span className="min-w-0 grow truncate text-sm">{questionLine(ask.question)}</span>}
    />
  );
}

/** A Reply or an Agent row beside the reading column: the thread's subject as a link to the
 *  thread itself, when it was last written in, whose it is, and the **Dismiss** that takes it out
 *  of the list - drawn while the reader's hand is on the row, and always on the selected one. */
function ThreadShortRow({
  group: groupKey,
  marked,
  onMark,
  onRelease,
  onSelect,
  row,
  section,
  selected,
  thread,
}: InboxRowProps & ShortRowProps & { thread: MyReplyRow }): ReactNode {
  const reference = threadReferenceRoute(thread);
  return (
    <ShortRowFrame
      age={<Timestamp at={rowLastActivityAt(row)} />}
      check={
        <MarkCheckbox
          id={rowId(row)}
          marked={marked}
          name={questionLine(thread.subject)}
          onMark={onMark}
        />
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={
        <>
          <ThreadOwnerLine className={`truncate text-sm ${textMutedOnCanvas}`} thread={thread} />
          {reference === undefined ? null : <CopyRefButton route={reference} />}
          <DismissControl shown={selected} thread={thread} />
        </>
      }
      row={row}
      section={section}
      selected={selected}
      title={
        <ThreadTitle className="min-w-0 grow truncate text-sm" href={thread.ref} thread={thread} />
      }
    />
  );
}

/** The row without a reading column beside it: the question's whole card, under the controls
 *  that defer or reroute it. */
function InboxCardRow({
  ask,
  assignLive,
  group: groupKey,
  marked,
  onAnswered,
  onAssignLive,
  onMark,
  onRelease,
  onSnoozeLive,
  ordinal,
  row,
  section,
  threadUpdatedAt,
  viewer,
}: InboxRowProps & {
  ask: InboxRow;
  /** True while this row's "Assign to me" write is in flight or failed, whatever band the
   *  optimistic update put the row in. */
  assignLive: boolean;
  onAnswered: (id: string) => void;
  onAssignLive: (askId: string, live: boolean) => void;
  /** The same registration for this row's snooze write; the Inbox keeps a live row rendered
   *  even when its band is folded, so `Snoozing…` and a refusal survive the fold. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** Timestamp of the Inbox snapshot that supplied this row's initial thread. */
  threadUpdatedAt: number;
  /** The signed-in person's lowercase email; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const name = controlName(ask, ordinal);
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={undefined}
      row={row}
      section={section}
      selected={false}
    >
      {/* One line: the issue this question belongs to on the left, the controls that defer or
          reroute it on the right. It wraps rather than overlapping - a refusal from Snooze or
          Assign to me is a full-width row inside the right group, so it never draws over the
          issue key. */}
      <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
        <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />
        <RowKindIcon type={rowType(row)} />
        <div className="flex min-w-0 grow basis-48 items-baseline gap-2">
          <InboxOwnerLink ask={ask} rowOwner />
          {/* The reply chip truncates rather than squeezing the title away. */}
          <span className="min-w-0 max-w-[40%] shrink truncate">
            <InboxRowChip ask={ask} />
          </span>
        </div>
        {/* `max-w-full` bounds the group to the row: `shrink-0` alone lets a server's own
            refusal reason - `snoozed_until must be in the future`, or anything longer - set
            the group's width and widen the whole document past the viewport on a phone. */}
        <div className="flex max-w-full shrink-0 flex-wrap items-center gap-1">
          <RowUrgencyMark urgency={askUrgency(ask.urgency)} />
          {ask.issue_key === null ? null : (
            <PriorityControl issueKey={ask.issue_key} priority={issuePriority(ask.priority)} />
          )}
          {ask.issue_key !== null && (isUnassigned(row) || assignLive) ? (
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
    </InboxRowShell>
  );
}

/** A Reply or an Agent row where the question rows are cards: the row's mark over the card's
 *  shell, whose thread body - and whose **Dismiss** - is written elsewhere. */
function ThreadCardRow({
  group: groupKey,
  marked,
  onMark,
  onRelease,
  row,
  section,
  thread,
}: InboxRowProps & { thread: MyReplyRow }): ReactNode {
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={undefined}
      row={row}
      section={section}
      selected={false}
    >
      {/* The mark sits where a question card's does, on a line of its own above the card. */}
      <div className="mb-1.5 flex items-center gap-2">
        <MarkCheckbox
          id={rowId(row)}
          marked={marked}
          name={questionLine(thread.subject)}
          onMark={onMark}
        />
      </div>
      <ThreadRowCard thread={thread} />
    </InboxRowShell>
  );
}

/** The rows of one band, with the held row - one the server no longer lists here but the
 *  reader is still on - kept after the nearest row above it that is still listed, so it does not
 *  move while they finish. */
function withHeld(
  rows: readonly InboxListRow[],
  held: InboxListRow | undefined,
  previous: readonly PlacedRow[]
): readonly InboxListRow[] {
  if (held === undefined) return rows;
  const heldId = rowId(held);
  const ids = rows.map((row) => rowId(row));
  for (
    let index = previous.findIndex((placed) => rowId(placed.row) === heldId) - 1;
    index >= 0;
    index -= 1
  ) {
    const above = previous[index];
    const at = above === undefined ? -1 : ids.indexOf(rowId(above.row));
    if (at !== -1) return [...rows.slice(0, at + 1), held, ...rows.slice(at + 1)];
  }
  return [held, ...rows];
}

/** The row to keep where the reader last saw it, if any. Dropped from the list: kept as last
 *  seen. Moved to another band: what the list says about it changed under the reader's hand, so
 *  the listed row is kept in the band they saw it in. A move into or out of Later is a snooze -
 *  the reader's own click, shown at once. */
function heldRow(
  seen: PlacedRow | undefined,
  listed: InboxListRow | undefined,
  now: string | undefined
): PlacedRow | undefined {
  if (seen === undefined) return undefined;
  if (listed === undefined) return seen;
  const movedBand =
    now !== undefined &&
    now !== seen.group &&
    now !== LATER_GROUP_KEY &&
    seen.group !== LATER_GROUP_KEY;
  return movedBand ? { ...seen, row: listed } : undefined;
}

/** What the Inbox says when no band has a row. Typed text comes first: a reader who has narrowed
 *  the list is told why it is empty before anything else is, whatever view, filter or agent the
 *  address also carries. It answers on the words the box searches by rather than on a non-empty
 *  `q`, so an address carrying only spaces narrows nothing and says nothing about it. The
 *  Dismissed view is about one thing only, so it says that and nothing else. An agent filter's
 *  line is about that agent's asks alone, naming it in plain text (`useAgentText`): the empty
 *  state is a message, not a byline. Otherwise the credential requests above the bands wait on
 *  the viewer as much as an ask whose turn is theirs, so it says nothing until their list has
 *  come back empty: while the list loads, or after it fails, the Inbox cannot say that nothing
 *  needs them. The three filtered lines differ because only the reader's own list is about them:
 *  a cleared filter is about everyone, and any other selection is about whoever it names. */
function emptyStateMessage({
  agent,
  agentTitle,
  credentials,
  selection,
  show,
  viewer,
  words,
}: {
  agent: string | undefined;
  agentTitle: string | undefined;
  credentials: CredentialRequests;
  /** Who the list is filtered to, as `?assignee=` reads: empty is the cleared filter. */
  selection: readonly string[];
  show: InboxShow;
  viewer: string;
  /** What the search box narrows by, as the list reads it: `searchWords` of the capped `?q=`. */
  words: readonly string[];
}): string | undefined {
  if (words.length > 0) return "Nothing matches this text";
  if (show === "dismissed") return "Nothing dismissed";
  if (agent !== undefined) return `No open asks from ${agentTitle}`;
  if (credentials.status !== "listed" || credentials.requests.length > 0) return undefined;
  if (isOwnList(selection, viewer)) return "Nothing needs you";
  return selection.length === 0 ? "Nothing needs anyone" : "Nothing matches this filter";
}

/**
 * `filter`/`onFilterChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its filters or the text
 * typed in its search box (either would navigate the page it sits over), and its
 * Escape/j/k/etc. must resolve under the drawer's own `dialog` scope - the modal scope a dialog
 * anywhere on the stack exclusively consults - rather than the standalone page's `inbox` scope,
 * which a dialog's keydown handling never reaches. `pane` is the third: the drawer is a peek at
 * any width, never a two-column reader. `InboxPage`'s unmodified `<Inbox/>` call keeps today's
 * URL-backed behaviour untouched.
 */
export interface InboxProps {
  filter?: InboxRoute;
  keymapScope?: KeymapScope;
  /** A host that owns the filter itself: every change this list makes - a filter, the sort, the
   *  grouping, the view, the text typed in the search box - is reported here instead of being
   *  written to the address. */
  onFilterChange?: (next: InboxRoute) => void;
  /** Whether this Inbox may show its reading column beside the list, which it does from
   *  `READING_PANE_QUERY` (1280 px). False keeps the card in the row, no selection and nothing
   *  written to `?item=`, at every width, which is what the drawer is. */
  pane?: boolean;
  /** The box the host scrolls these rows in, where that is neither the window nor the list's
   *  own column: the drawer scrolls them in its `aside`, so the viewport anchor must read and
   *  move that element's `scrollTop` - `window.scrollBy` moves nothing there, and the row the
   *  reader's hand is on jumps by the height of a row removed above it. Ignored while the
   *  reading pane shows, where the list column is the box that scrolls. */
  scroller?: RefObject<HTMLElement | null>;
}

export function Inbox({
  filter: filterOverride,
  keymapScope = "inbox",
  onFilterChange,
  pane = true,
  scroller,
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
  /** Which of the reader's two lists is showing: their Inbox, or what they have dismissed out of
   *  it. A thread stays in the Inbox, read or not, until it is dismissed. */
  const show: InboxShow = filter.show ?? "inbox";
  const showDismissed = show === "dismissed";
  const listRef = useRef<HTMLElement>(null);
  /** The list column, which scrolls in itself beside the reading column. */
  const listScroller = useRef<HTMLDivElement>(null);
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. Written by
   *  `holdPane` below rather than handed to the wrapper, so the column's unmount is seen. */
  const paneRef = useRef<HTMLDivElement | null>(null);
  /** The grid the list, the divider and the reading column share, whose width a drag of the
   *  divider is measured against. */
  const columnsRef = useRef<HTMLDivElement>(null);
  /** How much of that grid the reading column takes, kept for the reader's browser. */
  const [paneWidth, setPaneWidth] = usePaneWidth();
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
  const sort = filter.sort ?? "newest";
  const groupBy = filter.group ?? "priority";
  const filters: InboxFilters = filter.filters ?? {};
  const [threadOrder, setThreadOrder] = useThreadOrder();
  const [folded, toggleFold] = useFoldedGroups(groupBy);
  // How many pages of rows each band is drawing; a band starts on its first.
  const [pages, setPages] = useState<ReadonlyMap<string, number>>(() => new Map());
  const showMore = useCallback((key: string) => {
    setPages((current) => new Map(current).set(key, (current.get(key) ?? 1) + 1));
  }, []);
  // Which of the bar's popovers is open, so Escape is that popover's rather than the selection's.
  const [openPopover, setOpenPopover] = useState<InboxPopover | undefined>(undefined);
  const barOpen = openPopover !== undefined;
  const changeFilter = (next: InboxRoute) => {
    if (onFilterChange) {
      onFilterChange(next);
    } else {
      // Replace rather than push: a reader trying filters out would otherwise have to walk Back
      // through every pick, the rule every URL-backed filter here holds to
      // (`lib/url-array-params.ts`).
      navigate(buildInboxPath(next), { replace: true });
    }
  };
  /** The search box, so `f` can land the reader's focus in it from anywhere on the page. */
  const searchRef = useRef<HTMLInputElement>(null);
  // What the box narrows by: the address's text, cut as the box itself cuts what it writes, and
  // split into words once per render rather than once per row. An address that arrived with a
  // longer `?q=` is left as it is: the list is filtered by the cut text, while the box shows
  // what the address holds.
  const words = searchWords(capSearchQuery(filter.q ?? ""));
  const typeQuery = (typed: string) => {
    // One cut, here rather than a `maxLength` on the box as well: what the box writes and what
    // the list filters by are then the one rule, and the cut stays provable from a browser.
    const next = capSearchQuery(typed);
    // A keypress on a box already at the cap produces the text the filter holds, and writing it
    // would replace the history entry and re-render every row for nothing. React restores a
    // controlled input's DOM value when the state does not move, so the cap still holds in the
    // box.
    if (next === (filter.q ?? "")) return;
    // The typed text is a field of the route like any other, so it goes back the one way every
    // other narrowing does: to the host that owns the filter, or to the address, replacing
    // rather than pushing - a reader typing a word would otherwise have to walk Back through
    // every letter of it.
    changeFilter({ ...filter, q: next === "" ? undefined : next });
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
  // The row the reader's hand is on (focus or pointer), read from the DOM as last committed. When
  // the server has dropped it (answered or resolved elsewhere) or handed its turn the other way
  // (their own ask-back, an agent's note or reply), it is kept - `held` - where the reader last
  // saw it, until their focus or pointer leaves it; `release` re-renders so the row is chosen
  // afresh without it and takes its new place then.
  const viewport = useRef<ViewportAnchor>(null);
  const anchor = viewport.current?.interacted() ?? null;
  /** The rows the reader can see, in the order they see them, as this list last rendered them:
   *  what the motion hold is read against, and what the selection steps through. */
  const presented = useRef<readonly PlacedRow[]>([]);
  const answered = useRef<string | null>(null);
  const [, setReleased] = useState(0);
  const release = () => setReleased((count) => count + 1);
  const openFoldedBand = useCallback(
    (key: string) => {
      if (folded.has(key)) toggleFold(key);
    },
    [folded, toggleFold]
  );
  // What this list holds, and computed here rather than after the early returns so the bindings
  // below speak for exactly the rows on the page.
  const agent = filter.agent;
  // An agent filter and `?section=needs-you` both narrow the list to asks, so neither lists a
  // thread: one is about what an agent asked, the other about what waits on the reader now. The
  // Dismissed view is the other way round - an ask is never dismissed, so it holds threads alone.
  const asksOnly = agent !== undefined || filter.section === "needs-you";
  const fromAgent =
    agent === undefined
      ? (adoptedInbox ?? [])
      : (adoptedInbox ?? []).filter(
          (ask) => ask.author.kind === "session" && ask.author.id === agent
        );
  const askRows: InboxListRow[] = showDismissed
    ? []
    : (filter.section === "needs-you" ? waitingOnHuman(fromAgent) : fromAgent).map((ask) => ({
        ask,
        kind: "ask",
      }));
  // The thread side of this list, in one hook: the reader's Inbox or what they dismissed out of
  // it, the read mark a write over that list carries, and the page behind **Show more**.
  const threads = useInboxThreadRows({ asksOnly, listRef, show });
  /** Every row this list could hold before its five fields narrow it: what the bar counts. */
  const candidates = [...askRows, ...threads.rows];
  // The filters and the typed text both decide what is listed, with one exception that outranks
  // them all: a row whose "Assign to me" write is live stays. Taking a question under the
  // Unassigned filter is exactly that case - the row stops matching the moment the optimistic
  // assignment lands - and unmounting the control would take `Assigning…`, a refusal and its
  // Retry off the screen with it.
  const narrowed = candidates.filter(
    (row) =>
      (row.kind === "ask" && assigning.has(row.ask.id)) ||
      (inAssigneeFilter(row, selection, viewer ?? "") &&
        matchesInboxSearch(row, words) &&
        INBOX_FILTER_FIELDS.every((field) =>
          inFieldFilter(row, field, filters[field], viewer ?? "")
        ))
  );
  // The Dismissed view keeps the server's own order, newest dismissal first, and bands nothing:
  // it is one list of what the reader has already dealt with.
  const shown = showDismissed ? narrowed : sortRows(narrowed, sort, viewer ?? "");
  const byId = new Map(shown.map((row) => [rowId(row), row]));
  // What a press that names the list's own narrowing rather than ids carries: the read mark of
  // the list read the reader is looking at, and undefined where no such press may be made at
  // all - the server knows the list's five fields and nothing of the text typed in the search
  // box, so a press that sent the narrowing under typed text would act on rows the text hid.
  // **Mark all read** and a **Select all** dismiss read this one name.
  const narrowingMark = words.length === 0 ? threads.asOf : undefined;
  // The bar's **Mark all read**, over exactly this list: its filters, the read mark of the list
  // read it is showing, and the rows it is showing. The Dismissed view offers none, so it passes
  // no read mark either: nothing there is left to mark.
  const markAll = useMarkAllRead({
    asOf: showDismissed ? undefined : narrowingMark,
    filters,
    rows: shown,
  });
  // Where each row sits, judged against the clock at render: a snoozed row rejoins its band on
  // the first render after its moment passes. Computed here, with the selection, rather than
  // after the early returns, because the bar's count and the bindings both read it.
  const place = viewer === undefined ? undefined : Date.now();
  // The bands as this list heads them: another person's question is not "Waiting on you".
  const titles = sectionTitles(selection, viewer ?? "");
  const bands =
    place === undefined
      ? []
      : showDismissed
        ? [{ key: ALL_GROUP_KEY, label: undefined, rows: shown }]
        : group(shown, groupBy, { now: place, titles, viewer: viewer ?? "" });
  // A row in a band the reader has not opened is not on the page: the band renders its heading
  // and count and none of its rows, so there is no checkbox to untick, and the reader may well
  // have set that row's own moment since marking it - a pick that wrote it would overwrite what
  // they chose. A row whose own snooze write is still in flight stays rendered inside the fold,
  // so it still counts. One rule, read by the selection here and by the render below.
  const drawnIn = (bandKey: string, rows: readonly InboxListRow[]): readonly InboxListRow[] => {
    if (folded.has(bandKey)) {
      return rows.filter((row) => row.kind === "ask" && snoozing.has(row.ask.id));
    }
    // The Dismissed view pages at the server: every row a page answered is drawn, and **Show
    // more** reads the next 200 rather than revealing rows already here.
    if (showDismissed) return rows;
    return rows.slice(0, (pages.get(bandKey) ?? 1) * GROUP_PAGE);
  };
  const drawnIds = new Set(
    bands.flatMap(({ key, rows }) => drawnIn(key, rows).map((row) => rowId(row)))
  );
  // What the reader marked, and the two presses the bar makes on it: the state and both writes
  // live in one hook of their own (`useInboxSelection`).
  const {
    allRows,
    barRef: bulkBarRef,
    barShown: bulkBarShown,
    clearSelection,
    dismissedHere,
    dismissPick,
    dismissRows,
    dismissWrite,
    marked,
    onMark,
    selectAll,
    selectedAsks,
    selectedThreads,
    snoozePick,
    snoozeRefusal,
    snoozing: bulkSnoozing,
  } = useInboxSelection({
    asOf: narrowingMark,
    candidates,
    drawnIds,
    filters,
    rows: byId,
    showDismissed,
  });
  const paneSelection = usePaneSelection({
    bulkSnoozing,
    filter,
    listRef,
    openFoldedBand: useCallback(() => openFoldedBand(LATER_GROUP_KEY), [openFoldedBand]),
    paneShown,
    presented,
    snoozing,
  });
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly. The reader's own answer
  // is one of the ways the selection is handed on, and only while it is still the selected
  // question: the column holds the question here until the server decides their answer, but a
  // selection that moved on meanwhile - an approval reviewed from the head, a row they walked
  // off - must not be pulled back off the row that took its place.
  const recordAnswered = (id: string) => {
    answered.current = id;
    if (paneShown && paneSelection.selectedNow.current === id) {
      paneSelection.select(paneSelection.nextAfterSelected());
    }
    release();
  };
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  const step = (delta: 1 | -1) => roveFocus(rows(), rowAround(document.activeElement), delta);
  const focusInPane = () => paneRef.current?.contains(document.activeElement) === true;
  const focusInList = () => listRef.current?.contains(document.activeElement) === true;
  /** The row a recorded origin names: null where nothing was recorded, and null where the row
   *  it names has since left the list. One lookup, for the two ways back into the list below. */
  const originRow = (origin: string | null): HTMLElement | null =>
    origin === null
      ? null
      : (listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${origin}"]`) ?? null);
  /** The row `l` was pressed on: the row Escape returns the focus to from inside the column,
   *  which is another row than the selected one whenever `j` moved the selection while the
   *  focus stayed in the list. Every other way into the column leaves it null, or names the row
   *  of the question the column shows (below), and either way Escape returns to that row, as it
   *  always has.
   *
   *  It is written by one key - `read-pane`'s `run` - and only where no record stands: a second
   *  `l`, and the `l` a reader comes back in on after an unmount inside the column left the
   *  record standing, both leave it alone, where either would otherwise write the row the
   *  column is showing over the row the reader came from. So re-entry by key and re-entry by
   *  click, which writes nothing at all, leave a standing record alike. The test is the record
   *  alone, with no check of where the focus is, because none is needed: a press from inside
   *  the column with no record standing finds no focused row there (`focusedRow()` is null in
   *  the column), so it writes the row of the question the column shows, or null where that
   *  question has no listed row, and either sends Escape exactly where `paneWayBack`'s fallback
   *  already would - the stored value differs from a click's null, the row Escape reaches does
   *  not. It is read by one accessor, `paneWayBack`, which both halves of Escape ask, so the
   *  `when` and the `run` cannot disagree about where the key goes.
   *
   *  The record stands exactly while two facts hold of the reader, each with its own drops;
   *  between them no route can outlive the record.
   *
   *  (a) The column still shows the question the record was made on. The layout effect below
   *      drops it wherever `paneSelection.id` has changed under a standing column: a step of
   *      `j` or `k`, the reader's own answer. Where the commit has nothing shown at all the
   *      column itself goes and `holdPane` drops the record first - a snooze of theirs and a
   *      question the server withdraws both take the selected row off the list before the next
   *      one is handed the selection - and that is also the only drop for the Inbox falling
   *      below the column's width, where the selection stands and the column goes.
   *  (b) The reader's focus has not left the column. The document's `focusin` below drops the
   *      record wherever the focus lands outside the wrapper, which is every route out at once:
   *      `f` to the search box, Escape to the row, a Tab onto a row's checkbox, a click into
   *      the list, the row a step out of the column focuses on its way, the dialog a picture
   *      opens full size. `forgetPaneReturn`, the wrapper's `onBlur`, covers the one exit that
   *      lands nowhere and so fires no `focusin`: a focus leaving the column for no node at all
   *      - a `blur()` of the control holding it, a click on the page outside its focusable
   *      `main` - which leaves `document.body` holding the focus. A click on non-focusable
   *      content inside `main` focuses `main` itself, which is a `focusin` outside the wrapper.
   *
   *  Two cases look like a route out and keep the record, because neither breaks that pair. A
   *  window or tab blur moves no focus at all: nothing is focused anew, so no `focusin` fires,
   *  and the `onBlur` that does names no target while the scroller still holds the focus - the
   *  reader is still in the column, and back on the tab Escape owes them the row they came
   *  from. A focused node React removes from inside the column - one body component swapped
   *  for another - while the column still shows the question the record was made on leaves
   *  the reader on that question, so the record is still theirs: React fires no blur for a
   *  node it removes, and the `document.body` the focus falls to is focused by nothing, so
   *  neither drop of (b) reads it. Their next focus does. A control that disables itself is
   *  not that case: Chromium blurs it as it is disabled, before React removes it - the head's
   *  `Assign to me` while its write is in flight - so the wrapper's `onBlur` reads the focus
   *  leaving for the body and drops the record, and Escape then goes to the row of the
   *  question the column shows. */
  const paneReturn = useRef<string | null>(null);
  /** The question the column was showing when this component last looked, which is how the
   *  layout effect below tells a commit that changed it from one that did not. Nothing else
   *  writes it, so the record itself still names a row and nothing more. */
  const shownQuestion = useRef<string | undefined>(undefined);
  // The column shows another question, so the record is spent: the row it names belongs to a
  // question the reader has been carried off, whichever route carried them. Read after every
  // commit, as the selection's own settling is (`pane-selection.ts`), and dropped here rather
  // than tested against the question at Escape, because a record kept past its question would
  // come back the moment the reader selected that question again. On the layout lane, so that
  // the drop lands in the commit that changed the question: React flushes a passive effect
  // before the next key only for the changes it commits synchronously, and the question can
  // change on another lane - a transition, a `?item=` the router settles - which would leave the
  // reader one Escape with a record its question has already left.
  useLayoutEffect(() => {
    if (shownQuestion.current === paneSelection.id) return;
    shownQuestion.current = paneSelection.id;
    paneReturn.current = null;
  });
  /** The column wrapper: the node the keys above act in, and the drop its unmount makes. React
   *  calls a ref callback with null whenever the callback's identity changes, so this one is
   *  stable - an arrow written into the JSX would be a new callback every render, called with
   *  null each time, and would drop a record the reader is still owed. */
  const holdPane = useCallback((node: HTMLDivElement | null) => {
    paneRef.current = node;
    // The column has gone, taking the question the record was made on off the screen and the
    // focus the scroller held to `document.body` with it. Nothing else reports that: no blur
    // fires for a node React removes and `body` takes no `focusin`. So this is the drop that
    // covers the commit a snooze or a withdrawal of the selected question leaves with nothing
    // shown, before the next question is handed the selection - it is first there, ahead of the
    // layout effect above - and the only one that covers the Inbox falling below the column's
    // width, where the selection stands and the column goes.
    if (node === null) paneReturn.current = null;
  }, []);
  // Fact (b), stated as itself: wherever the focus lands, the record stands only while that
  // place is inside the column. A focus the column itself takes - the head's Snooze under `h`,
  // a link in the drawn document, the scroller `l` focuses as it writes the record - lands
  // inside the wrapper and changes nothing, so the key's own `focusin` cannot undo it, and
  // Escape's `run` reads the record before it focuses the row, so the drop that press makes
  // takes nothing it still needs.
  //
  // One listener for this component's life, not one put up and taken down around each record:
  // the record is a ref precisely so that `l` commits nothing, so there is no render to hang a
  // record-shaped subscription on, and making one would mean rendering the Inbox twice per `l`.
  // The handler reads refs alone and nothing of this render, so it never goes stale and the
  // effect has nothing to depend on: it binds once on mount, unbinds on unmount, and no render
  // in between churns it.
  useEffect(() => {
    const dropWhereFocusLands = () => {
      // `focusin` fires once the focus has landed, so the document already names where.
      if (paneRef.current?.contains(document.activeElement) !== true) paneReturn.current = null;
    };
    document.addEventListener("focusin", dropWhereFocusLands);
    return () => document.removeEventListener("focusin", dropWhereFocusLands);
  }, []);
  /** The drop a focus that lands nowhere makes, which is the half of fact (b) no `focusin`
   *  reports: React's `onBlur` bubbles from the column's descendants, so a reader whose control
   *  blurs itself, or who clicks the page outside its focusable `main`, has left the column with
   *  `document.body` holding the focus, and nothing is focused for the listener above to read.
   *
   *  Where the focus went is `relatedTarget`, and `document.activeElement` where the event names
   *  none, because those two nulls mean opposite things: a window or tab blur moves no focus and
   *  keeps the record, while a focus that left for the body drops it. The routes out that land
   *  on a node are this handler's too, an event before the listener above reads them, so what
   *  needs it is only the focus that lands nowhere. */
  const forgetPaneReturn = (event: FocusEvent<HTMLDivElement>) => {
    if (!event.currentTarget.contains(event.relatedTarget ?? document.activeElement)) {
      paneReturn.current = null;
    }
  };
  /** The row Escape returns the focus to from inside the column: the row `l` recorded, else the
   *  row the column is showing, and null where the list holds neither. The one reader of the
   *  record - the binding's `when` tests it against null, its `run` focuses it, and there is no
   *  second spelling of the way back for the two of them to disagree over. */
  const paneWayBack = (): HTMLElement | null =>
    originRow(paneReturn.current) ?? paneSelection.selectedRow();
  /** One step of the reader's, in whichever mode the Inbox is in: the selection the column
   *  follows, or the focus roving the rows. Chosen once, so no binding below asks again. */
  const stepInMode = paneShown ? paneSelection.step : step;
  /** That step, taken from wherever the reader is standing. From inside the column the focus
   *  comes back to the selected row first: `j` fires in there, a `tabindex=0` section being no
   *  editable target, and a step from an approval to a question the column draws as a card
   *  swaps `ApprovalBody` for `CardBody`, which unmounts the focused scroller and drops the
   *  focus to `document.body` - where neither of Escape's branches holds and the key would do
   *  nothing. The reader then stands exactly where a click and then `j` leaves them today. */
  const move = (delta: 1 | -1) => {
    if (paneShown && focusInPane()) {
      paneSelection.selectedRow()?.focus();
    }
    stepInMode(delta);
  };
  /** What the keys act on, for the bindings' labels: the row the column is showing, or the row
   *  holding the focus. */
  const acted = paneShown ? "selected" : "focused";
  /** When the arrows move, and what their label says of it: in the reading column the arrows
   *  are the column's - a document scrolls, an option list moves - so they move the selection
   *  only while the focus is in the list beside it. */
  const arrowsMove = paneShown ? focusInList : () => focusedRow() !== null;
  const arrowsWhen = paneShown ? "the focus is in the list" : "a row is focused";
  /** The row the keys act on: the one the column is showing, or the one holding the focus. */
  const handRow = (): InboxListRow | undefined => {
    const id = paneShown ? paneSelection.id : focusedRow()?.dataset.inboxRow;
    return id === undefined ? undefined : byId.get(id);
  };
  /** The row `x` marks, by its own `rowId`: asks and threads alike, since the bulk bar now offers
   *  each kind its own press. */
  const markableRow = (): string | undefined => {
    const row = handRow();
    return row === undefined ? undefined : rowId(row);
  };
  /** The row `e` dismisses: the thread row in hand, as the route names it - its id and what the
   *  page drew in it. An Ask or an Approval row has no Dismiss at all (`dismissRow`). */
  const dismissableRow = (): InboxDismissRow | undefined => {
    const row = handRow();
    return row === undefined ? undefined : dismissRow(row);
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
  /** The column's scroller, the focus stop `l` moves the reader into, or null where no column
   *  is rendered. */
  const paneScroller = () =>
    paneRef.current?.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR) ?? null;
  /** Where the answer keys - Enter and 1-9 - act: the selected question's own card, not the
   *  whole column. The column of a question written as a decision block draws the whole live
   *  document, and `ProofDocument` hosts an answerable card for every open block of it
   *  (`ProofDocument.tsx`), so the first `[data-ask-option]` under the column is the first open
   *  block's, which is another question than the one the reader selected. Null where the
   *  selected row has no card drawn for it: an approval, whose column is the document's
   *  version and whose answer is the head's own Approve, a document whose block has not
   *  mounted its card yet, and every thread row, which asks nothing. Outside the column the
   *  keys act on the focused row, as all of them do. */
  const answerHand = () =>
    paneShown
      ? paneSelection.id === undefined
        ? null
        : (paneRef.current?.querySelector<HTMLElement>(
            `[data-ask-card="${CSS.escape(paneSelection.id)}"]`
          ) ?? null)
      : focusedRow();
  const inAnswerHand = (selector: string) =>
    answerHand()?.querySelector<HTMLElement>(selector) ?? null;
  // The bar is reached through its own ref, which holds wherever the header draws it.
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
      label: "Next row",
      palette: false,
      run: () => move(1),
      when: () => rows().length > 0,
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous row",
      palette: false,
      run: () => move(-1),
      when: () => rows().length > 0,
    },
    {
      id: "arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: `Next / previous row while ${arrowsWhen}`,
      run: (event) => move(event.key === "ArrowDown" ? 1 : -1),
      when: arrowsMove,
    },
    {
      id: "answer",
      keys: "Enter",
      label: `Answer the ${acted} ask`,
      run: () => {
        // A decision block's compact card folds its answer box behind "Add a note or answer in
        // your own words"; Enter presses that disclosure, and the card lands the focus in the
        // box its own render reveals (`AskCard.tsx`) - the box does not exist in this tick, so
        // nothing here could focus it. Only the column draws that compact card: from a row, and
        // from a disclosure already open, the key goes straight to the box.
        const box = inAnswerHand("[data-ask-answer]");
        if (box === null && paneShown) {
          inAnswerHand(NOTE_DISCLOSURE_SELECTOR)?.click();
          return;
        }
        box?.focus();
      },
      // Enter is the list's key: it carries the reader from the row they are on into the
      // column's answer box. Everywhere else the key belongs to the control that holds the
      // focus - the card's own `Ask back instead` or an option inside the column, a row's owner
      // link, a band's fold, the bulk-snooze bar, a filter control - and the registry's handler
      // calls `preventDefault` for every binding that fires, so a binding that claimed the key
      // there would press nothing. It fires from a row itself, and with the focus on nothing
      // (`document.body`), where a reader who has pressed no key yet stands. `h` stands down
      // inside an ask card for the same reason (`outsideAskCard`).
      when: () =>
        paneShown
          ? (focusedRow() !== null || document.activeElement === document.body) &&
            (inAnswerHand("[data-ask-answer]") !== null ||
              inAnswerHand(NOTE_DISCLOSURE_SELECTOR) !== null)
          : inAnswerHand("[data-ask-answer]") !== null,
    },
    {
      id: "option",
      keys: ["1", "2", "3", "4", "5", "6", "7", "8", "9"],
      label: `Select option 1–9 of the ${acted} ask`,
      run: (event) => {
        const options = answerHand()?.querySelectorAll<HTMLElement>("[data-ask-option]");
        options?.[Number(event.key) - 1]?.click();
      },
      when: () => inAnswerHand("[data-ask-option]") !== null,
    },
    {
      id: "open",
      keys: "o",
      label: "Open the row's issue, document or thread",
      run: () => inHand("[data-inbox-owner]")?.click(),
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      id: "copy-ref",
      keys: "y",
      // An Agent row has no reference of its own - its thread is a message to a session - so
      // it draws no copy control and this stands down on it.
      label: `Copy the ${acted} row's reference`,
      run: () => inHand(COPY_REF_SELECTOR)?.click(),
      when: () => inHand(COPY_REF_SELECTOR) !== null,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the row, then clear the focused row",
      run: () => {
        // From the reading column, Escape is the way back to the row the reader came from: `l`
        // recorded that row, which is not the selected one whenever `j` moved the selection
        // without moving the focus. Every other way in records nothing, or the row the column
        // is showing, and returns, as it always has, to that row. `paneWayBack` is both of
        // those, and the `when` below asks it the same question, so the key never fires on a way
        // back the run then cannot find.
        if (paneShown && focusInPane()) {
          paneWayBack()?.focus();
          return;
        }
        const row = rowAround(document.activeElement);
        if (row === focusedRow()) {
          row?.blur();
        } else {
          row?.focus();
        }
      },
      // Both ways back are tested, because either can be the only one left. The column keeps a
      // question whose row the typed text hides while the reader has an unsent answer in it
      // (`pane-selection.ts`'s `paneAsk`), and there the row `l` was pressed on is the way out,
      // the selected row having none; every other way into the column records no row, and there
      // the selected row is the way out. With only the selected row tested, Escape in a column
      // held open over a hidden row would match no binding and do nothing.
      when: () =>
        rowAround(document.activeElement) !== null ||
        (paneShown && focusInPane() && paneWayBack() !== null),
    },
    {
      // The column is the other half of the page, and `l` is the step into it: the scroller
      // takes the focus, and the browser scrolls it on the arrow keys, Page Up, Page Down,
      // Space, Home and End from there.
      id: "read-pane",
      keys: "l",
      label: "Move the focus into the reading pane",
      run: () => {
        // The record Escape reads, written only where no record stands. It names the row the
        // reader came from - the one holding the focus, or the selected one where the focus is
        // on nothing (`document.body`, where a reader who has pressed no key yet stands) or on
        // something that is not a row, which includes a press from inside the column itself.
        //
        // A press with a record standing writes nothing, whether the reader is in the column
        // already or on the body an unmount inside the column dropped their focus to: both
        // would write the row the column is showing, or null, over the row the reader actually
        // came from, which is the row Escape owes them. So the key leaves a standing record
        // exactly as a click back into the column does, and the two ways back in agree.
        if (paneReturn.current === null) {
          paneReturn.current =
            focusedRow()?.dataset.inboxRow ?? paneSelection.selectedRow()?.dataset.inboxRow ?? null;
        }
        paneScroller()?.focus();
      },
      // One test for all three cases the key does nothing in: below 1280 px and in the drawer
      // `paneShown` is false (`pane && wideEnoughForPane`), and with no row selected no column
      // is rendered at all, so `paneRef.current` is null and there is no scroller under it.
      when: () => paneShown && paneScroller() !== null,
    },
    {
      // Marking walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
      id: "select",
      keys: "x",
      label: `Select or deselect the ${acted} row`,
      palette: false,
      run: () => {
        const id = markableRow();
        if (id !== undefined) onMark(id);
      },
      when: () => markableRow() !== undefined,
    },
    {
      id: "dismiss",
      keys: "e",
      // The row goes out of the Inbox, and in the Dismissed view back into it; the selection then
      // moves to the next row, as it does after a snooze. An Ask or an Approval row has no
      // Dismiss, so the key does nothing on one.
      label: showDismissed
        ? `Move the ${acted} thread back to the Inbox`
        : `Dismiss the ${acted} thread`,
      run: () => {
        const named = dismissableRow();
        if (named !== undefined) dismissRows([named]);
      },
      // Not from inside an ask the reader is answering, the rule `h` holds to: the card's own
      // controls are not the list's, and a typed `e` belongs to whatever holds the focus.
      when: () => dismissableRow() !== undefined && (!paneShown || outsideAskCard()),
    },
    {
      id: "snooze",
      keys: "h",
      // One key for both: with rows marked it is the whole selection, otherwise the row in hand.
      label: "Snooze the focused ask, or every selected ask",
      run: () => {
        if (selectedAsks.length === 0) {
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
        (selectedAsks.length > 0 && outsideAskCard()) ||
        (inHand("[data-inbox-snooze]") !== null && (!paneShown || outsideAskCard())),
    },
    {
      id: "back-from-bulk",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the bulk snooze picker",
      run: () => {
        // With every marked row gone there is no row to return to, so the bar itself is the
        // level out: it holds the count and the way to clear it.
        (originRow(pickerOrigin.current) ?? rows()[0] ?? bulkBarRef.current)?.focus();
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
      // A popover of the bar's takes Escape first: one press closes it, and dropping what the
      // reader marked at the same time would be two undos for one key.
      when: () => focusedRow() === null && bulkBarShown && !barOpen,
    },
    {
      // The box is one key away from anywhere on the page, and `f` is not `inEditable`: inside
      // the box itself a `search` input takes typed text (`keymap.ts`'s `TEXT_INPUT_TYPES`), so
      // the registry passes the key through and the reader types an `f` rather than re-focusing
      // what they are already in.
      id: "search",
      keys: "f",
      label: "Search asks",
      run: () => searchRef.current?.focus(),
      when: () => searchRef.current !== null,
    },
    {
      // Escape is one level out, and out of the box is the list: the row the column is showing
      // on the page, and the first row in the drawer, where nothing is selected. It is offered
      // only while the focus is in the box, so the three Escapes above are untouched - `back`
      // and `back-from-bulk` both answer false there, and `clear-selection` is not `inEditable`
      // at all - and the drawer's own Close waits its turn, the Inbox's registration being the
      // first of the scope to match (`keymap.ts`'s `resolve`).
      id: "leave-search",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the search box",
      run: () => (paneSelection.selectedRow() ?? rows()[0])?.focus(),
      when: () => searchRef.current !== null && document.activeElement === searchRef.current,
    },
  ]);

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  // Which row the list holds in place while the server moves the list under it: the row the
  // reader's hand is on (focus or pointer), with or without the reading column. A row the
  // column is merely showing is not held - the Inbox selects the first row for the reader the
  // moment it opens, and a question the server withdraws, resolves or answers elsewhere must
  // not stay in the list on the strength of a selection nobody made. What the column does with
  // a selected question the server takes away is `paneRow` (`pane-selection.ts`). A row the
  // reader dismissed under their own hand is not held either: they asked for it to go, as they
  // do by answering one, so it goes at once and the next row takes its place.
  //
  // The hold is told which rows the typed text excludes rather than being handed the narrowed
  // list: a row the reader's own typing drops must go at once, even with the pointer resting on
  // it, so it is not "seen" here at all and there is nothing to hold. A row the server stopped
  // sending is held exactly as it is today.
  const handId = anchor?.id;
  const seen =
    handId === undefined || handId === answered.current || dismissedHere(handId)
      ? undefined
      : presented.current.find(
          (placed) => rowId(placed.row) === handId && matchesInboxSearch(placed.row, words)
        );
  const listed = seen === undefined ? undefined : byId.get(rowId(seen.row));
  const nowBand =
    listed === undefined
      ? undefined
      : bands.find(({ rows: banded }) => banded.includes(listed))?.key;
  const held = heldRow(seen, listed, nowBand);

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
  const agentChip =
    agentSubject === undefined ? undefined : (
      <AgentName
        {...agentSubject}
        className="flex items-center whitespace-pre"
        control={(name) => (
          <Link
            aria-label="Clear agent filter"
            className="inline-flex min-h-11 items-center rounded-full"
            to={buildInboxPath({ assignees: filter.assignees, item: filter.item, q: filter.q })}
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
    <BulkBar
      all={allRows}
      askCount={selectedAsks.length}
      barRef={bulkBarRef}
      onClear={clearSelection}
      onDismiss={dismissPick}
      onPick={snoozePick}
      onPickerFocus={(from) => {
        pickerOrigin.current = rowAround(from)?.dataset.inboxRow ?? null;
      }}
      onSelectAll={selectAll}
      pending={bulkSnoozing}
      refusal={snoozeRefusal}
      show={show}
      threadCount={selectedThreads.length}
    />
  ) : null;
  // The box above the bar's own fields, on a line of its own so the fields it searches stay
  // readable in its placeholder at a phone's width. Its classes are the Agents page's own field,
  // the 44 px tap target included, plus the focus ring `inputClasses` does not carry: `f` lands
  // the focus here, and a reader who arrived by key must see where they are. The cap is said in
  // the title rather than enforced by a `maxLength`, so `capSearchQuery` in the change handler
  // is the one rule that cuts what is written and what is filtered by.
  const searchBox = (
    <input
      aria-label="Search asks"
      className={`min-h-11 min-w-0 flex-1 basis-64 rounded-lg border px-3 py-2 text-sm outline-none focus-visible:ring-2 ${inputClasses(true)} ${focusVisibleRing}`}
      onChange={(event) => typeQuery(event.target.value)}
      placeholder="Issue, title, question or author"
      ref={searchRef}
      title={`At most ${SEARCH_QUERY_MAX} characters`}
      type="search"
      value={filter.q ?? ""}
    />
  );
  // Everything above the list, the same whether or not any band has a row, and a sibling of the
  // scrolling list rather than an item in it: the bar stays put while the rows move.
  const header = (
    <div className="space-y-4">
      <CredentialRequestsSection credentials={credentials} />
      {/* The box narrows the same list the bar's fields narrow, so it stands with them, above
          the chrome that opens them. */}
      <div className="flex flex-wrap items-center gap-2">{searchBox}</div>
      <InboxBar
        agentToken={agentChip}
        assignees={filter.assignees}
        filters={filters}
        markAll={markAll}
        onAssigneesChange={(next) => changeFilter({ ...filter, assignees: next })}
        onClearAgent={() => changeFilter({ ...filter, agent: undefined, section: undefined })}
        onFiltersChange={(next) =>
          changeFilter({ ...filter, filters: Object.keys(next).length === 0 ? undefined : next })
        }
        onPopoverOpenChange={setOpenPopover}
        rows={candidates}
        selection={selection}
        view={{
          group: groupBy,
          onGroupChange: (next) => changeFilter({ ...filter, group: next }),
          onShowChange: (next) =>
            changeFilter({ ...filter, show: next === "dismissed" ? "dismissed" : undefined }),
          onSortChange: (next) => changeFilter({ ...filter, sort: next }),
          onThreadOrderChange: setThreadOrder,
          show,
          sort,
          threadOrder,
        }}
        viewer={viewer}
      />
      {agent === undefined && !showDismissed ? (
        <BlockedOnYou
          asks={adoptedInbox ?? []}
          credentialRequests={credentials.requests}
          viewer={viewer}
        />
      ) : null}
      {/* A dismiss the server refused: the rows are back where they were, and the page says so
          here, above the list, because the control that sent it was on a row this write had
          already taken off the page. */}
      {dismissWrite.refusal === undefined ? null : (
        <p className={`text-sm ${dangerText}`} data-inbox-dismiss-refused="" role="alert">
          {dismissWrite.refusal}
        </p>
      )}
      {bulkBar}
    </div>
  );

  // Two columns, the reading column to the right of the list, each scrolling in itself inside
  // the height the page gives them (`app.tsx` makes the Inbox page fill the viewport here).
  //
  // One root, selection or none: the column is what appears and disappears beside the list,
  // never the list itself. A selection empties for a commit whenever the selected row leaves
  // the rendered bands - the reader's own bulk snooze of that row does it, before the effect
  // above hands the selection on - and a root that changed shape there would have React
  // unmount the whole list, taking the keyboard's focus, the bulk bar and every row's state
  // with it and mounting a fresh one a commit later.
  const page = (listColumn: ReactNode, subject: InboxListRow | undefined): ReactNode => (
    // Every thread this list draws - in the column, and in a card below its width - draws its own
    // Dismiss from here (`DismissControl`).
    <RowDismissContext
      value={{
        dismiss: (named) => dismissRows([named]),
        pending: dismissWrite.pending,
        show,
      }}
    >
      <div className={paneShown ? "flex min-h-0 flex-1 flex-col gap-4" : "space-y-4"}>
        {header}
        {paneShown ? (
          // The two columns are a grid of two fractions, so the share the reader dragged the
          // divider to is exactly the share the pane takes, at any width; the divider is the
          // column between them and takes its own.
          //
          // One row, `minmax(0, 1fr)`: an implicit `auto` row is as tall as the taller column,
          // so a long document in the pane would make the row - and with it both columns'
          // boxes - taller than the page, and neither column would scroll in itself. The page
          // would scroll instead, which is how the pane came to show a document's top however
          // far down it the reader's section sat.
          <div
            className="grid min-h-0 flex-1"
            ref={columnsRef}
            style={{
              gridTemplateColumns:
                subject === undefined
                  ? "minmax(0, 1fr)"
                  : `minmax(0, ${1 - paneWidth}fr) auto minmax(0, ${paneWidth}fr)`,
              gridTemplateRows: "minmax(0, 1fr)",
            }}
          >
            {listColumn}
            {subject === undefined ? null : (
              <>
                <PaneDivider columns={columnsRef} onWidthChange={setPaneWidth} width={paneWidth} />
                {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only reads where the focus has already gone, so the column needs no role of its own. */}
                <div className="min-h-0 min-w-0" onBlur={forgetPaneReturn} ref={holdPane}>
                  <ReadingPane
                    onAnswered={recordAnswered}
                    onSnoozeLive={onSnoozeLive}
                    row={subject}
                    viewer={viewer}
                  />
                </div>
              </>
            )}
          </div>
        ) : (
          listColumn
        )}
      </div>
    </RowDismissContext>
  );

  if (shown.length === 0 && held === undefined) {
    const emptyMessage = emptyStateMessage({
      agent,
      agentTitle,
      credentials,
      selection,
      show,
      viewer,
      words,
    });
    // Nothing to read, so nothing to select and no column beside the list. The one question that
    // outlives every row is the one the column is keeping for an unsent answer, and it keeps
    // its column here too: the reader's text is in it.
    presented.current = [];
    const empty = (
      <div data-testid="inbox-list">
        {emptyMessage === undefined ? null : (
          <EmptyState label="Inbox empty state" message={emptyMessage} />
        )}
      </div>
    );
    return page(empty, paneSelection.showing([]));
  }

  // The held row is drawn in the band the reader saw it in, after the row it then followed.
  // A band the held row has left is empty, and an empty band has nothing to head.
  const drawn = bands
    .map((band) => {
      const banded = withHeld(
        band.rows.filter((row) => rowId(row) !== (held === undefined ? "" : rowId(held.row))),
        held?.group === band.key ? held.row : undefined,
        presented.current
      );
      return { band: { ...band, rows: banded }, rows: drawnIn(band.key, banded) };
    })
    .filter(({ band }) => band.rows.length > 0);
  /** Which turn band a row is drawn in: the held row's is the one the reader saw it in. */
  const sectionDrawn = (row: InboxListRow): InboxSection =>
    held !== undefined && rowId(held.row) === rowId(row)
      ? held.section
      : (sectionOf(row, place) ?? "human");
  presented.current = drawn.flatMap(({ band, rows: visible }) =>
    visible.map((row) => ({ group: band.key, row, section: sectionDrawn(row) }))
  );
  // Every row the bands hold, a folded one's included: `?item=` naming a row the reader deferred
  // opens that band rather than being read as an address this list cannot answer.
  const listedRows: PlacedRow[] = drawn.flatMap(({ band }) =>
    band.rows.map((row) => ({ group: band.key, row, section: sectionDrawn(row) }))
  );
  // Names are read off the rows as they render - band by band, top to bottom - so "ask 2 of 2"
  // never sits above "ask 1 of 2", and a twin folded away leaves the row on screen with the
  // short name rather than a count of something the reader cannot see.
  const ordinals = askOrdinals(
    presented.current.flatMap(({ row }) => (row.kind === "ask" ? [row.ask] : []))
  );
  const selectedRow = paneSelection.showing(listedRows);

  const renderRow = (bandKey: string, row: InboxListRow): ReactNode => {
    const id = rowId(row);
    const section = sectionDrawn(row);
    const onRelease = held !== undefined && rowId(held.row) === id ? release : undefined;
    const shared = {
      group: bandKey,
      marked: marked.has(id),
      onMark,
      onRelease,
      ordinal: row.kind === "ask" ? ordinals.get(row.ask.id) : undefined,
      row,
      section,
    };
    if (paneShown) {
      return row.kind === "ask" ? (
        <AskShortRow
          {...shared}
          ask={row.ask}
          key={id}
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
        />
      ) : (
        <ThreadShortRow
          {...shared}
          key={id}
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
          thread={row.thread}
        />
      );
    }
    return row.kind === "ask" ? (
      <InboxCardRow
        {...shared}
        ask={row.ask}
        assignLive={assigning.has(row.ask.id)}
        key={id}
        onAnswered={recordAnswered}
        onAssignLive={onAssignLive}
        onSnoozeLive={onSnoozeLive}
        threadUpdatedAt={inbox.dataUpdatedAt}
        viewer={viewer}
      />
    ) : (
      <ThreadCardRow {...shared} key={id} thread={row.thread} />
    );
  };

  // One list, keyed by row id, with the band headings as items between the rows: a row that
  // changes band moves within the same parent, so React moves its node instead of remounting
  // it - its draft, selection, disclosures, and focus stay, and the viewport anchor can find it.
  //
  // One header, rows or none: what appears and disappears beside it is the list, never the
  // header itself, which `page` draws above the bands and above the empty state alike. That is
  // the rule the comment above `page` states for the columns root, and the header is under it
  // for the same reason - drawing the header inside each of those two trees would have React
  // unmount it on the letter that leaves nothing listed and mount a fresh one, taking the
  // search box out from under the reader's hand and their focus to `document.body` with it, so
  // the rest of the word, and the Backspace that would correct it, would reach the Inbox's own
  // single-key bindings instead of the box (INFRA-963).
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it. Without the pane the rows scroll in whatever
    // the host gives (the drawer's own `aside`), and in the window when it gives nothing.
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
        scroller={paneShown ? listScroller : scroller}
      >
        <ul className="space-y-3">
          {drawn.flatMap(({ band, rows: visible }) =>
            groupItems({
              folded: folded.has(band.key),
              group: band,
              held: folded.has(band.key) ? 0 : band.rows.length - visible.length,
              onShowMore: showMore,
              onToggleFold: toggleFold,
              renderRow: (row) => renderRow(band.key, row),
              rows: visible,
            })
          )}
        </ul>
        {/* The Dismissed view's own **Show more**: the next page of dismissals, read from the
            cursor the last page named, rather than rows this page is holding back. */}
        {threads.showMore === undefined ? null : (
          <button
            className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
            data-inbox-show-more="dismissed"
            disabled={threads.showingMore}
            onClick={threads.showMore}
            type="button"
          >
            {threads.showingMore ? "Loading…" : `Show ${INBOX_DISMISSED_PAGE} more`}
          </button>
        )}
      </ViewportAnchor>
    </div>
  );

  // Nothing to read is nothing to select, so an empty list is the whole Inbox above, as it is
  // below the reading column's width. The one question that outlives every row is the one the
  // column is keeping for an unsent answer, and `showing` hands that one back from an empty
  // list too, so it keeps its column there: the reader's text is in it.
  return page(list, selectedRow);
}
