import { useQuery } from "@tanstack/react-query";
import { type FocusEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import {
  inboxAllThreadCountQuery,
  inboxQuery,
  inboxThreadsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { InboxRow, MyReplyRow } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  unreadDotBg,
} from "../../theme/classes";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { COPY_REF_SELECTOR, CopyRefButton } from "../refs/CopyRefButton";
import {
  buildInboxPath,
  INBOX_FILTER_FIELDS,
  type InboxRoute,
  parseInboxSearch,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type KeymapScope, useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { AskCard } from "./AskCard";
import { AskOwnerLink, InboxOwnerLink } from "./AskOwnerLink";
import { AssignToMe } from "./AssignToMe";
import { formatAskAge } from "./ask-age";
import { outsideAskCard } from "./ask-card";
import { type AskOrdinal, askOrdinals, controlName } from "./ask-name";
import { BlockedOnYou, waitingOnHuman } from "./BlockedOnYou";
import { BulkSnoozeBar } from "./BulkSnoozeBar";
import { group, inFieldFilter, LATER_GROUP_KEY, sortRows } from "./grouping";
import { InboxBar, type InboxFilters } from "./InboxBar";
import { GROUP_PAGE, groupItems } from "./InboxGroups";
import { ROW_ATTRIBUTE, ROW_SELECTOR } from "./inbox-row";
import {
  type InboxListRow,
  rowId,
  rowLastActivityAt,
  rowPriority,
  rowType,
  rowUrgency,
} from "./list-row";
import { useMotionHold } from "./motion-hold";
import { type PlacedRow, usePaneSelection } from "./pane-selection";
import { useFoldedGroups, useThreadOrder } from "./preferences";
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
import { ThreadRowCard, threadOwner, threadReferenceRoute } from "./ThreadRowCard";
import { type AskSnoozeFailure, useAskSnoozeMany } from "./useAskSnooze";

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
  /** Whether the reader has marked this row for a bulk action; never a thread row. */
  marked: boolean;
  /** Marks or unmarks this ask; `x` and the row's own checkbox are the two ways in. */
  onMark: (askId: string) => void;
  /** Set on the held row - one the server dropped or moved to another band that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  row: InboxListRow;
  section: InboxSection;
}

/** The row's own checkbox, the pointer's way to the mark `x` makes - and, on a thread row, the
 *  space that checkbox would take, so every row's title starts at the same place. Snoozing,
 *  marking and the bulk bar are an open question's; a thread is read or it is not. */
function MarkCheckbox({
  marked,
  name,
  onMark,
  row,
}: {
  marked: boolean;
  name: string;
  onMark: (askId: string) => void;
  row: InboxListRow;
}): ReactNode {
  if (row.kind === "thread") {
    return <span aria-hidden="true" className="size-4 shrink-0" />;
  }
  const ask = row.ask;
  return (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      onChange={() => onMark(ask.id)}
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
      className={`rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing} ${
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

/** A thread row's title: a link to the thread's own page, bold with a dot while it holds a
 *  write the reader has not read. `o` follows it, which is why it wears `data-inbox-owner`. */
function ThreadTitle({ thread }: { thread: MyReplyRow }): ReactNode {
  const title = questionLine(thread.subject);
  return (
    <>
      <a
        className={`min-w-0 grow truncate text-sm ${
          thread.unread ? `font-semibold ${textPrimaryOnCanvas}` : textMutedOnCanvas
        }`}
        data-inbox-owner=""
        href={thread.ref}
        title={title}
      >
        {title}
      </a>
      {thread.unread ? (
        <span
          aria-label="Unread"
          className={`size-2 shrink-0 rounded-full ${unreadDotBg}`}
          role="img"
        />
      ) : null}
    </>
  );
}

/**
 * The row beside the reading column, two lines deep: the kind's icon at the left edge, then
 * what the row is about and when it was last written in; under them the priority tag, an ask's
 * urgency, and the whole issue key with its title. Every mark is `shrink-0`, so the title is
 * what shortens and neither the marks nor the key are ever cut.
 */
function InboxShortRow({
  group: groupKey,
  marked,
  onMark,
  onRelease,
  onSelect,
  ordinal,
  row,
  section,
  selected,
}: InboxRowProps & {
  onSelect: (id: string) => void;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}): ReactNode {
  const reference = row.kind === "thread" ? threadReferenceRoute(row.thread) : undefined;
  const owner = row.kind === "thread" ? threadOwner(row.thread) : null;
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
          <MarkCheckbox
            marked={marked}
            name={row.kind === "ask" ? controlName(row.ask, ordinal) : ""}
            onMark={onMark}
            row={row}
          />
          <RowKindIcon type={rowType(row)} />
          {row.kind === "ask" ? (
            <span className="min-w-0 grow truncate text-sm">{questionLine(row.ask.question)}</span>
          ) : (
            <ThreadTitle thread={row.thread} />
          )}
          <time
            className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`}
            dateTime={rowLastActivityAt(row)}
          >
            {row.kind === "ask" ? (
              formatAskAge(rowLastActivityAt(row))
            ) : (
              <Timestamp at={rowLastActivityAt(row)} />
            )}
          </time>
        </div>
        <div className="flex min-w-0 items-baseline gap-2 pl-6">
          <RowPriorityTag priority={rowPriority(row)} />
          <RowUrgencyMark urgency={rowUrgency(row)} />
          {row.kind === "ask" ? (
            <InboxOwnerLink ask={row.ask} rowOwner />
          ) : owner === null ? (
            <span className={`truncate text-sm ${textMutedOnCanvas}`}>
              {row.thread.session?.name ?? "Agent message"}
            </span>
          ) : (
            <AskOwnerLink className="grow" owner={owner} />
          )}
          {reference === undefined ? null : <CopyRefButton route={reference} />}
        </div>
      </div>
    </InboxRowShell>
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
        <MarkCheckbox marked={marked} name={name} onMark={onMark} row={row} />
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
          <RowUrgencyMark urgency={ask.urgency} />
          {ask.issue_key === null ? null : (
            <PriorityControl issueKey={ask.issue_key} priority={ask.priority} />
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

/** A Reply or an Agent row where the question rows are cards: the card's shell, whose thread
 *  body is written elsewhere. */
function ThreadCardRow({
  group: groupKey,
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

/** The second line of an empty list, under the message above: where the threads are. Under
 *  **Unread** it says that the ones already read are behind the switch, and only while there
 *  are any; with the switch on it says there is no thread at all. An agent filter and
 *  `?section=needs-you` narrow the list to asks, so neither sentence is true there and neither
 *  is drawn. */
function emptyThreadLine({
  asksOnly,
  readThreadsBehindSwitch,
  showReadThreads,
}: {
  asksOnly: boolean;
  /** Whether **Show read threads** would add a row this list is not showing. */
  readThreadsBehindSwitch: boolean;
  showReadThreads: boolean;
}): string | undefined {
  if (asksOnly) return undefined;
  if (showReadThreads) return "No threads either, read or unread.";
  return readThreadsBehindSwitch
    ? "No unread threads. Show read threads, under View, lists the ones you have read."
    : undefined;
}

/**
 * `filter`/`onFilterChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its filters (that would
 * navigate the page it sits over), and its Escape/j/k/etc. must resolve under the drawer's
 * own `dialog` scope - the modal scope a dialog anywhere on the stack exclusively consults -
 * rather than the standalone page's `inbox` scope, which a dialog's keydown handling never
 * reaches. `pane` is the third: the drawer is a peek at any width, never a two-column reader.
 * `InboxPage`'s unmodified `<Inbox/>` call keeps today's URL-backed behaviour untouched.
 */
export interface InboxProps {
  filter?: InboxRoute;
  keymapScope?: KeymapScope;
  /** A host that owns the filter itself: every change this list makes is reported here instead
   *  of being written to the address. */
  onFilterChange?: (next: InboxRoute) => void;
  /** Whether this Inbox may show its reading column beside the list, which it does from
   *  `READING_PANE_QUERY` (1280 px). False keeps the card in the row, no selection and nothing
   *  written to `?item=`, at every width, which is what the drawer is. */
  pane?: boolean;
}

export function Inbox({
  filter: filterOverride,
  keymapScope = "inbox",
  onFilterChange,
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
  // The reader's own threads, in one unpaged read: Unread by default, every one of them with
  // the switch on. Human-only, as the route is.
  const human = useIsHuman();
  const threadState = filter.threads === "all" ? "all" : "unread";
  const threads = useQuery({ ...inboxThreadsQuery(threadState), enabled: human });
  // The count the switch's other side carries. With the switch on, the unpaged read above
  // counts every thread and the unread ones are the rows that say so; with it off, that read
  // counts only the unread, so how many there are in all is a count-only read of its own.
  const allThreads = useQuery({
    ...inboxAllThreadCountQuery(),
    enabled: human && threadState !== "all",
  });
  const listRef = useRef<HTMLElement>(null);
  /** The list column, which scrolls in itself beside the reading column. */
  const listScroller = useRef<HTMLDivElement>(null);
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. */
  const paneRef = useRef<HTMLDivElement>(null);
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adoptedInbox = useMotionHold(inbox.data, listRef);
  const adoptedThreads = useMotionHold(threads.data?.rows, listRef);
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
  // A popover of the bar's is open, so Escape is that popover's rather than the selection's.
  const [barOpen, setBarOpen] = useState(false);
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
  // An agent filter and `?section=needs-you` both narrow the list to asks, so neither lists a
  // thread: one is about what an agent asked, the other about what waits on the reader now.
  const asksOnly = agent !== undefined || filter.section === "needs-you";
  const fromAgent =
    agent === undefined
      ? (adoptedInbox ?? [])
      : (adoptedInbox ?? []).filter(
          (ask) => ask.author.kind === "session" && ask.author.id === agent
        );
  const askRows: InboxListRow[] = (
    filter.section === "needs-you" ? waitingOnHuman(fromAgent) : fromAgent
  ).map((ask) => ({ ask, kind: "ask" }));
  const threadRows: InboxListRow[] = asksOnly
    ? []
    : (adoptedThreads ?? []).map((thread) => ({ kind: "thread", thread }));
  /** Every row this list could hold before its five fields narrow it: what the bar counts. */
  const candidates = [...askRows, ...threadRows];
  // The filters decide what is listed, with one exception: a row whose "Assign to me" write is
  // live stays. Taking a question under the Unassigned filter is exactly that case - the row
  // stops matching the moment the optimistic assignment lands - and unmounting the control
  // would take `Assigning…`, a refusal and its Retry off the screen with it.
  const narrowed = candidates.filter(
    (row) =>
      (row.kind === "ask" && assigning.has(row.ask.id)) ||
      (inAssigneeFilter(row, selection, viewer ?? "") &&
        INBOX_FILTER_FIELDS.every((field) =>
          inFieldFilter(row, field, filters[field], viewer ?? "")
        ))
  );
  const shown = sortRows(narrowed, sort, viewer ?? "");
  const byId = new Map(shown.map((row) => [rowId(row), row]));
  // Where each row sits, judged against the clock at render: a snoozed row rejoins its band on
  // the first render after its moment passes. Computed here, with the selection, rather than
  // after the early returns, because the bar's count and the bindings both read it.
  const place = viewer === undefined ? undefined : Date.now();
  // The bands as this list heads them: another person's question is not "Waiting on you".
  const titles = sectionTitles(selection, viewer ?? "");
  const bands =
    place === undefined ? [] : group(shown, groupBy, { now: place, titles, viewer: viewer ?? "" });
  // A row in a band the reader has not opened is not on the page: the band renders its heading
  // and count and none of its rows, so there is no checkbox to untick, and the reader may well
  // have set that row's own moment since marking it - a pick that wrote it would overwrite what
  // they chose. A row whose own snooze write is still in flight stays rendered inside the fold,
  // so it still counts. One rule, read by the selection here and by the render below.
  const drawnIn = (bandKey: string, rows: readonly InboxListRow[]): readonly InboxListRow[] => {
    if (folded.has(bandKey)) {
      return rows.filter((row) => row.kind === "ask" && snoozing.has(row.ask.id));
    }
    return rows.slice(0, (pages.get(bandKey) ?? 1) * GROUP_PAGE);
  };
  const drawnIds = new Set(
    bands.flatMap(({ key, rows }) => drawnIn(key, rows).map((row) => rowId(row)))
  );
  const selected = [...marked].filter((id) => drawnIds.has(id) && byId.get(id)?.kind === "ask");
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
  /** One step of the reader's, in whichever mode the Inbox is in: the selection the column
   *  follows, or the focus roving the rows. Chosen once, so no binding below asks again. */
  const move = paneShown ? paneSelection.step : step;
  /** What the keys act on, for the bindings' labels: the row the column is showing, or the row
   *  holding the focus. */
  const acted = paneShown ? "selected" : "focused";
  /** When the arrows move, and what their label says of it: in the reading column the arrows
   *  are the column's - a document scrolls, an option list moves - so they move the selection
   *  only while the focus is in the list beside it. */
  const arrowsMove = paneShown ? focusInList : () => focusedRow() !== null;
  const arrowsWhen = paneShown ? "the focus is in the list" : "a row is focused";
  /** The row `x` marks: the one the column is showing, or the one holding the focus - and only
   *  ever an ask. Marking, `h` and the bulk bar are an open question's; a thread row is read or
   *  it is not, and the bar's count never includes one. */
  const markableRow = (): string | undefined => {
    const id = paneShown ? paneSelection.id : focusedRow()?.dataset.inboxRow;
    return id !== undefined && byId.get(id)?.kind === "ask" ? id : undefined;
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
        // From the reading column, Escape is the way back to the row the column is showing: the
        // focus belongs in the list, and the three ways into the column are deliberate.
        if (paneShown && focusInPane()) {
          paneSelection.selectedRow()?.focus();
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
        (paneShown && focusInPane() && paneSelection.selectedRow() !== null),
    },
    {
      // Marking walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
      id: "select",
      keys: "x",
      label: `Select or deselect the ${acted} ask`,
      palette: false,
      run: () => {
        const askId = markableRow();
        if (askId !== undefined) onMark(askId);
      },
      when: () => markableRow() !== undefined,
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
      // A popover of the bar's takes Escape first: one press closes it, and dropping what the
      // reader marked at the same time would be two undos for one key.
      when: () => focusedRow() === null && bulkBarShown && !barOpen,
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
  // a selected question the server takes away is `paneRow` (`pane-selection.ts`).
  const handId = anchor?.id;
  const seen =
    handId === undefined || handId === answered.current
      ? undefined
      : presented.current.find((placed) => rowId(placed.row) === handId);
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
  // The two numbers the **Show read threads** switch carries, each read from the side of it
  // that knows: the unpaged read counts its own state's rows, and the count-only read above
  // answers for the other.
  const threadRowsRead = adoptedThreads ?? [];
  const unreadThreadCount =
    threadState === "all"
      ? threadRowsRead.filter((thread) => thread.unread).length
      : threadRowsRead.length;
  const allThreadCount = threadState === "all" ? threads.data?.total : allThreads.data;
  // Everything above the list, the same whether or not any band has a row, and a sibling of the
  // scrolling list rather than an item in it: the bar stays put while the rows move.
  const header = (
    <div className="space-y-4">
      <CredentialRequestsSection credentials={credentials} />
      <InboxBar
        agentToken={agentChip}
        allThreadCount={allThreadCount}
        assignees={filter.assignees}
        filters={filters}
        group={groupBy}
        onAssigneesChange={(next) => changeFilter({ ...filter, assignees: next })}
        onClearAgent={() => changeFilter({ ...filter, agent: undefined, section: undefined })}
        onFiltersChange={(next) =>
          changeFilter({ ...filter, filters: Object.keys(next).length === 0 ? undefined : next })
        }
        onGroupChange={(next) => changeFilter({ ...filter, group: next })}
        onPopoverOpenChange={setBarOpen}
        onShowReadThreadsChange={(next) =>
          changeFilter({ ...filter, threads: next ? "all" : undefined })
        }
        onSortChange={(next) => changeFilter({ ...filter, sort: next })}
        onThreadOrderChange={setThreadOrder}
        rows={candidates}
        selection={selection}
        showReadThreads={threadState === "all"}
        sort={sort}
        threadOrder={threadOrder}
        unreadThreadCount={unreadThreadCount}
        viewer={viewer}
      />
      {agent === undefined ? (
        <BlockedOnYou
          asks={adoptedInbox ?? []}
          credentialRequests={credentials.requests}
          viewer={viewer}
        />
      ) : null}
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
    <div className={paneShown ? "flex min-h-0 flex-1 flex-col gap-4" : "space-y-4"}>
      {header}
      {paneShown ? (
        <div className="flex min-h-0 flex-1 gap-6">
          {listColumn}
          {subject === undefined ? null : (
            <div className="min-w-0 flex-1" ref={paneRef}>
              {subject.kind === "ask" ? (
                <ReadingPane
                  ask={subject.ask}
                  onAnswered={recordAnswered}
                  onSnoozeLive={onSnoozeLive}
                  viewer={viewer}
                />
              ) : (
                <ThreadRowCard thread={subject.thread} />
              )}
            </div>
          )}
        </div>
      ) : (
        listColumn
      )}
    </div>
  );

  if (shown.length === 0 && held === undefined) {
    const emptyMessage = emptyStateMessage({
      agent,
      agentTitle,
      credentials,
      selection,
      viewer,
    });
    const threadLine = emptyThreadLine({
      asksOnly,
      readThreadsBehindSwitch: (allThreadCount ?? 0) > unreadThreadCount,
      showReadThreads: threadState === "all",
    });
    // Nothing to read, so nothing to select and no column beside the list. The one question that
    // outlives every row is the one the column is keeping for an unsent answer, and it keeps
    // its column here too: the reader's text is in it.
    presented.current = [];
    const empty = (
      <div data-testid="inbox-list">
        {emptyMessage === undefined ? null : (
          <EmptyState
            action={threadLine === undefined ? undefined : <p>{threadLine}</p>}
            label="Inbox empty state"
            message={emptyMessage}
          />
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
      marked: row.kind === "ask" && marked.has(row.ask.id),
      onMark,
      onRelease,
      ordinal: row.kind === "ask" ? ordinals.get(row.ask.id) : undefined,
      row,
      section,
    };
    if (paneShown) {
      return (
        <InboxShortRow
          {...shared}
          key={id}
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
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
      </ViewportAnchor>
    </div>
  );

  return page(list, selectedRow);
}
