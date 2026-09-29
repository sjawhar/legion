import { useQuery } from "@tanstack/react-query";
import { type FocusEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { PriorityControl } from "../issue/PriorityControl";
import { useIssueAssignee } from "../issue/useIssueAssignee";
import { actorLabel } from "../refs/actor";
import { COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { referenceTriggerProps } from "../refs/RefPreview";
import {
  buildInboxPath,
  buildIssuePath,
  buildProjectPath,
  type InboxView,
  parseInboxSearch,
} from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import { useUserPreference } from "../shell/userPreference";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { AskCard } from "./AskCard";
import { type AskOrdinal, askIssueKey, askOrdinals, controlName } from "./ask-name";
import { BlockedOnYou, waitingOnYou } from "./BlockedOnYou";
import { BulkSnoozeBar } from "./BulkSnoozeBar";
import { SnoozeControl } from "./SnoozeControl";
import {
  COLLAPSED_SECTIONS,
  INBOX_SECTIONS,
  type InboxSection,
  isMine,
  isTurnSection,
  isUnassigned,
  SECTION_TITLES,
  sectionOf,
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
    return <ReplyChip>{actorLabel(ask.last_reply.author)} replied</ReplyChip>;
  }
  return null;
}

const ROW_ATTRIBUTE = "data-inbox-row";
const ROW_SELECTOR = `[${ROW_ATTRIBUTE}]`;
/** The bulk bar's snooze picker: `h` hands it focus and Escape takes it back to the list. */
const BULK_PICKER_SELECTOR = "[data-inbox-bulk-snooze]";
/** An ask card: the question, its options and its answer controls, which own their own keys. */
const ASK_CARD = "[data-ask-card]";

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  return focusedMatching(ROW_SELECTOR);
}

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

/** Takes an issue nobody holds for the viewer: one PATCH through the shared assignee write, so
 *  the row moves from the Unassigned band into Mine at once and rolls back if the server
 *  refuses. The row leaves the band optimistically while the save is in flight, so the control
 *  tells the Inbox when its write is live (pending or failed) and the Inbox keeps it mounted
 *  wherever the row sits until then: `Assigning…`, and on refusal the server's reason with Retry,
 *  are shown by this same instance rather than lost to a remount. */
function AssignToMe({
  askId,
  issueKey,
  onLive,
  viewer,
}: {
  /** The row this control sits in; liveness is registered per row, never per issue, so a
   *  sibling row of the same issue leaving the list cannot drop this row's registration. */
  askId: string;
  issueKey: string;
  /** Registers this row while its write is in flight or failed; unregistered once it settles
   *  cleanly or the control unmounts. */
  onLive: (askId: string, live: boolean) => void;
  viewer: string;
}): ReactNode {
  const write = useIssueAssignee(issueKey);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live) return;
    onLive(askId, true);
    return () => onLive(askId, false);
  }, [askId, live, onLive]);
  return (
    <>
      <button
        aria-label={`Assign ${issueKey} to me`}
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={write.pending}
        onClick={() => write.submit(viewer)}
        type="button"
      >
        {write.pending ? "Assigning…" : "Assign to me"}
      </button>
      {write.failed ? (
        <div className="basis-full">
          <QueryError
            message={write.error ?? `Could not assign ${issueKey} to you.`}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </div>
      ) : null}
    </>
  );
}

function InboxItem({
  ask,
  assignLive,
  marked,
  ordinal,
  onAnswered,
  onAssignLive,
  onMark,
  onSnoozeLive,
  onRelease,
  section,
  threadUpdatedAt,
  viewer,
}: {
  ask: InboxRow;
  /** True while this row's "Assign to me" write is in flight or failed, whatever section the
   *  optimistic update put the row in. */
  assignLive: boolean;
  /** Whether the reader has marked this row for a bulk action. */
  marked: boolean;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  onAnswered: (id: string) => void;
  onAssignLive: (askId: string, live: boolean) => void;
  /** Marks or unmarks this row; `x` and the row's own checkbox are the two ways in. */
  onMark: (askId: string) => void;
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
  /** The signed-in lowercase login; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const owner = askIssueKey(ask);
  const title = ask.issue?.title ?? owner ?? "Document ask";
  const name = controlName(ask, ordinal);
  const releaseOnFocusOut =
    onRelease === undefined
      ? undefined
      : (event: FocusEvent<HTMLLIElement>) => {
          if (!event.currentTarget.contains(event.relatedTarget)) onRelease();
        };
  return (
    <li
      className={`rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing}`}
      data-inbox-row={ask.id}
      data-inbox-section={section}
      onBlur={releaseOnFocusOut}
      onPointerLeave={onRelease}
      tabIndex={-1}
    >
      {/* One line: the issue this question belongs to on the left, the controls that defer or
          reroute it on the right. It wraps rather than overlapping - a refusal from Snooze or
          Assign to me is a full-width row inside the right group, so it never draws over the
          issue key - and the reply chip truncates rather than squeezing the title away. */}
      <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
        <input
          aria-label={`Select ${name}`}
          checked={marked}
          className={`size-4 shrink-0 ${checkboxAccent}`}
          onChange={() => onMark(ask.id)}
          type="checkbox"
        />
        <div className="flex min-w-0 grow basis-48 items-baseline gap-2">
          {ask.document === undefined ? (
            owner === null ? (
              <p className={`truncate text-sm ${textMutedOnCanvas}`}>{title}</p>
            ) : (
              <Link
                className={`flex min-w-0 grow items-baseline gap-2 text-sm ${linkText} ${linkHoverText}`}
                data-inbox-owner=""
                to={buildIssuePath({ id: ask.id, key: owner, kind: "ask" })}
                {...referenceTriggerProps({ key: owner, kind: "issue" })}
              >
                <span className="shrink-0 font-semibold">{owner}</span>
                <TruncatedText>{title}</TruncatedText>
              </Link>
            )
          ) : (
            <Link
              className={`min-w-0 grow truncate text-sm font-semibold ${linkText} ${linkHoverText}`}
              data-inbox-owner=""
              to={buildProjectPath({
                item: { id: ask.id, kind: "ask" },
                kind: "document",
                project: ask.document.project,
                slug: ask.document.slug,
              })}
              {...referenceTriggerProps({
                kind: "document",
                project: ask.document.project,
                slug: ask.document.slug,
              })}
            >
              <TruncatedText>
                {ask.document.project} · {ask.document.name}
              </TruncatedText>
            </Link>
          )}
          <span className="min-w-0 max-w-[40%] shrink truncate">
            <InboxRowChip ask={ask} />
          </span>
        </div>
        {/* `max-w-full` bounds the group to the row: `shrink-0` alone lets a server's own
            refusal reason - `snoozed_until must be in the future`, or anything longer - set the
            group's width and widen the whole document past the viewport on a phone. */}
        <div className="flex max-w-full shrink-0 flex-wrap items-center gap-1">
          {ask.issue_key === null ? null : (
            <PriorityControl issueKey={ask.issue_key} priority={ask.priority} />
          )}
          {ask.issue_key !== null && (section === "unassigned" || assignLive) ? (
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
 *  so the listed row is kept in the section they saw it in. A move into or out of the Unassigned
 *  band is an assignment and a move into or out of Later is a snooze - both the reader's own
 *  click, shown at once. */
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

export function Inbox(): ReactNode {
  const { search } = useLocation();
  const navigate = useNavigate();
  const filter = parseInboxSearch(search);
  // One inbox query, shared with the nav badge, the sidebar, the agents page and the margin; the
  // views below are client-side partitions of it, so an answered row leaves every surface at once.
  const inbox = useQuery(inboxQuery());
  const whoAmI = useQuery(whoAmIQuery());
  // `/auth/whoami` echoes GitHub's casing; issues carry the lowercase login.
  const login = whoAmI.data?.login;
  const viewer = login?.toLowerCase();
  // The URL wins, then the login's remembered choice, then Mine (the first-time default).
  const [rememberedView, setRememberedView] = useUserPreference<InboxView>(
    "inbox.view",
    (stored) => (stored === "everyone" ? "everyone" : "mine"),
    (next) => next
  );
  const view = filter.view ?? rememberedView;
  const selectView = (next: InboxView) => {
    setRememberedView(next);
    navigate(buildInboxPath({ ...filter, view: next }));
  };
  const { titles } = useAgents(filter.agent !== undefined);
  // Rows (by ask id) whose "Assign to me" write is in flight or failed: their control stays
  // mounted through the optimistic move into Mine and the rollback out of it (see `AssignToMe`).
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
  const listedIds = inbox.data?.map((row) => row.id).join(" ");
  useEffect(() => {
    if (listedIds === undefined) return;
    const listed = new Set(listedIds === "" ? [] : listedIds.split(" "));
    setMarked((current) =>
      [...current].every((id) => listed.has(id))
        ? current
        : new Set([...current].filter((id) => listed.has(id)))
    );
  }, [listedIds]);
  // What this view lists, narrowed the same way the bands are, and computed here rather than
  // after the early returns so the bindings below speak for exactly the rows on the page.
  const agent = filter.agent;
  const fromAgent =
    agent === undefined
      ? (inbox.data ?? [])
      : (inbox.data ?? []).filter(
          (ask) => ask.author.kind === "session" && ask.author.id === agent
        );
  const inView = (rows: readonly InboxRow[]) =>
    view === "everyone" || viewer === undefined
      ? rows
      : rows.filter((row) => isMine(row, viewer) || isUnassigned(row));
  const shown = inView(filter.section === "needs-you" ? waitingOnYou(fromAgent) : fromAgent);
  // Later starts folded: its rows are the ones the reader has already dealt with by deferring.
  const [laterOpen, setLaterOpen] = useState(false);
  // Where each row sits, judged against the clock at render: a snoozed row rejoins its turn band
  // on the first render after its moment passes. Computed here, with the selection, rather than
  // after the early returns, because the bar's count and the bindings both read it.
  const place = viewer === undefined ? undefined : { now: Date.now(), view, viewer };
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
  // stay marked under its own reason, so a second pick retries exactly those.
  const snoozePick = (until: string) => {
    const ids = [...selected];
    setBulkRefusal(undefined);
    setBulkSnoozing(ids.length);
    void bulkWrite.submit(ids, until).then(({ failed }) => {
      setBulkSnoozing(0);
      setBulkRefusal(refusalText(failed, ids.length));
      const refused = new Set(failed.map(({ askId }) => askId));
      const taken = new Set(ids.filter((id) => !refused.has(id)));
      setMarked((current) => new Set([...current].filter((id) => !taken.has(id))));
    });
  };
  const listRef = useRef<HTMLElement>(null);
  // The row the reader's hand is on (focus or pointer), read from the DOM as last committed. When
  // the server has dropped it (answered or resolved elsewhere) or handed its turn the other way
  // (their own ask-back, an agent's note or reply), it is kept - `held` - where the reader last
  // saw it, until their focus or pointer leaves it; `release` re-renders so the row is chosen
  // afresh without it and takes its new place then. That covers the reader's own answer while it
  // is in flight (the card shows Answering…, or the error if the server refuses it); once the
  // server has recorded it the row is not held - their click was the end of that interaction -
  // and it leaves with the refetch the success triggers. Assign to me is likewise not a hold:
  // the row moving into Mine at once is the feedback for that click.
  const viewport = useRef<ViewportAnchor>(null);
  const anchor = viewport.current?.interacted() ?? null;
  const presented = useRef<readonly PlacedRow[]>([]);
  const answered = useRef<string | null>(null);
  const [, setReleased] = useState(0);
  const release = () => setReleased((count) => count + 1);
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly.
  const recordAnswered = (id: string) => {
    answered.current = id;
    release();
  };
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  const step = (delta: 1 | -1) => roveFocus(rows(), rowAround(document.activeElement), delta);
  const inFocusedRow = (selector: string) => focusedRow()?.querySelector<HTMLElement>(selector);
  // The bar is reached through its own ref: the empty state a pick can leave behind renders it
  // outside the list.
  const bulkPicker = () => bulkBarRef.current?.querySelector<HTMLElement>(BULK_PICKER_SELECTOR);
  // Whether the reader is outside an ask card: its options are radios and checkboxes, which take
  // no typed text, so the registry passes single keys through to this scope - and the card's own
  // keys are the card's. `IssuePage` withholds its two writing keys on the same question.
  const outsideAskCard = () => document.activeElement?.closest(ASK_CARD) == null;
  // Which row `h` was pressed on, so Escape from the bulk picker - which sits above the bands and
  // has no row to fall back through - is one level out rather than a dead end.
  const pickerOrigin = useRef<string | null>(null);
  useKeymapScope("inbox");
  useKeymap("inbox", [
    { id: "next", keys: "j", label: "Next ask", run: () => step(1), when: () => rows().length > 0 },
    {
      id: "previous",
      keys: "k",
      label: "Previous ask",
      run: () => step(-1),
      when: () => rows().length > 0,
    },
    {
      id: "arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: "Next / previous ask while a row is focused",
      run: (event) => step(event.key === "ArrowDown" ? 1 : -1),
      when: () => focusedRow() !== null,
    },
    {
      id: "answer",
      keys: "Enter",
      label: "Answer the focused ask",
      run: () => inFocusedRow("[data-ask-answer]")?.focus(),
      when: () => inFocusedRow("[data-ask-answer]") != null,
    },
    {
      id: "option",
      keys: ["1", "2", "3", "4", "5", "6", "7", "8", "9"],
      label: "Select option 1–9 of the focused ask",
      run: (event) =>
        focusedRow()
          ?.querySelectorAll<HTMLElement>("[data-ask-option]")
          [Number(event.key) - 1]?.click(),
      when: () => inFocusedRow("[data-ask-option]") != null,
    },
    {
      id: "open",
      keys: "o",
      label: "Open the ask's issue or document",
      run: () => inFocusedRow("[data-inbox-owner]")?.click(),
      when: () => inFocusedRow("[data-inbox-owner]") != null,
    },
    {
      id: "copy-ref",
      keys: "y",
      label: "Copy the focused ask's reference",
      run: () => inFocusedRow(COPY_REF_SELECTOR)?.click(),
      when: () => inFocusedRow(COPY_REF_SELECTOR) != null,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the ask row, then clear the focused row",
      run: () => {
        const row = rowAround(document.activeElement);
        if (row === focusedRow()) {
          row?.blur();
        } else {
          row?.focus();
        }
      },
      when: () => rowAround(document.activeElement) !== null,
    },
    {
      id: "select",
      keys: "x",
      label: "Select or deselect the focused ask",
      run: () => {
        const askId = focusedRow()?.dataset.inboxRow;
        if (askId !== undefined) onMark(askId);
      },
      when: () => focusedRow() !== null,
    },
    {
      id: "snooze",
      keys: "h",
      // One key for both: with rows marked it is the whole selection, otherwise the row in hand.
      label: "Snooze the focused ask, or every selected ask",
      run: () => {
        if (selected.length === 0) {
          inFocusedRow("[data-inbox-snooze]")?.focus();
          return;
        }
        bulkPicker()?.focus();
      },
      // The selection's key works from wherever marking leaves the reader - a row, the checkbox
      // they just ticked, the bar - but not from inside an ask they are answering: an option
      // radio takes no typed text, so the registry passes the key through, and the card's own
      // controls are not the list's. The issue page withholds its writing keys the same way.
      when: () =>
        (selected.length > 0 && outsideAskCard()) || inFocusedRow("[data-inbox-snooze]") != null,
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
      // `back` takes the reader off the row first, and this clears what they marked. A refusal
      // the reader has since unticked the rows of is the same level out, and the keyboard that
      // raised it is the keyboard that dismisses it.
      label: "Clear the selection",
      run: clearSelection,
      when: () => focusedRow() === null && (selected.length > 0 || bulkRefusal !== undefined),
    },
  ]);

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  const seen =
    anchor === null || anchor.id === answered.current
      ? undefined
      : presented.current.find(({ ask }) => ask.id === anchor.id);
  const listed = seen === undefined ? undefined : shown.find((ask) => ask.id === seen.ask.id);
  const now = listed === undefined ? undefined : sectionOf(listed, place);
  const held = heldRow(seen, listed, now);
  const viewSwitch = (
    <fieldset className={`inline-flex rounded-xl border p-1 ${borderDefault}`}>
      <legend className="sr-only">Inbox view</legend>
      {(["mine", "everyone"] as const).map((candidate) => (
        <button
          aria-pressed={view === candidate}
          className={`min-h-11 rounded-lg px-3 text-sm font-medium md:min-h-9 md:px-2 ${
            view === candidate ? surfaceMutedStrongBg : surfaceMutedBg
          } ${textSecondaryOnCanvas}`}
          key={candidate}
          onClick={() => selectView(candidate)}
          type="button"
        >
          {candidate === "mine" ? "Mine" : "Everyone"}
        </button>
      ))}
    </fieldset>
  );
  // The live agent's title when Envoy still lists it; otherwise the author label its asks carry.
  const liveTitle = agent === undefined ? undefined : titles.get(agent)?.trim();
  const agentTitle =
    liveTitle !== undefined && liveTitle !== ""
      ? liveTitle
      : fromAgent[0] === undefined
        ? agent
        : actorLabel(fromAgent[0].author);
  const chip =
    agent === undefined ? null : (
      <Link
        aria-label="Clear agent filter"
        className="inline-flex min-h-11 items-center rounded-full"
        to={buildInboxPath()}
      >
        <LabelPill selected>
          Asks from {agentTitle}
          {filter.section === "needs-you" ? " waiting on you" : ""} · clear
        </LabelPill>
      </Link>
    );

  // The bar outlives an empty list: a pick that empties this view leaves its write in the air, and
  // the refusal that comes back has to land somewhere the reader can see.
  const bulkBar =
    selected.length === 0 && bulkSnoozing === 0 && bulkRefusal === undefined ? null : (
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
    );

  if (shown.length === 0 && held === undefined) {
    return (
      <div className="space-y-6">
        <CredentialRequestsSection />
        {viewSwitch}
        {chip}
        {bulkBar}
        <EmptyState
          label="Inbox empty state"
          message={
            agent === undefined
              ? view === "mine"
                ? "Nothing needs you"
                : "Nothing needs anyone"
              : `No open asks from ${agentTitle}`
          }
        />
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
    // The same rule the selection above reads, so keyboard roving, the viewport anchor,
    // `presented` and the bar all see exactly what the reader sees.
    return {
      rows,
      section,
      shownRows: rows.filter((ask) => !inShutBand(ask, section)),
    };
  }).filter(({ rows }) => rows.length > 0);
  presented.current = sections.flatMap(({ section, shownRows }) =>
    shownRows.map((ask) => ({ ask, section }))
  );
  // Names are read off the rows as they render - band by band, top to bottom - so "ask 2 of 2"
  // never sits above "ask 1 of 2", and a twin folded away in `Later` leaves the row on screen
  // with the short name rather than a count of something the reader cannot see.
  const ordinals = askOrdinals(presented.current.map(({ ask }) => ask));

  // One list, keyed by ask id, with the section headings as items between the rows: a row that
  // changes section moves within the same parent, so React moves its node instead of remounting
  // it - its draft, selection, disclosures, and focus stay, and the viewport anchor can find it.
  return (
    <ViewportAnchor
      className="space-y-4"
      group="data-inbox-section"
      item={ROW_ATTRIBUTE}
      ref={viewport}
      rootRef={listRef}
    >
      <CredentialRequestsSection />
      {viewSwitch}
      {chip}
      {agent === undefined ? <BlockedOnYou asks={inView(inbox.data)} /> : null}
      {bulkBar}
      <ul className="space-y-3">
        {sections.flatMap(({ rows, section, shownRows }, index) => [
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
                  label={`${SECTION_TITLES[section]} (${rows.length})`}
                  onToggle={() => setLaterOpen((open) => !open)}
                  textClassName={textMutedOnCanvas}
                />
              ) : (
                SECTION_TITLES[section]
              )}
            </h2>
          </li>,
          ...shownRows.map((ask) => (
            <InboxItem
              ask={ask}
              assignLive={assigning.has(ask.id)}
              marked={marked.has(ask.id)}
              ordinal={ordinals.get(ask.id)}
              key={ask.id}
              onAnswered={recordAnswered}
              onAssignLive={onAssignLive}
              onMark={onMark}
              onSnoozeLive={onSnoozeLive}
              onRelease={ask.id === held?.ask.id ? release : undefined}
              section={section}
              threadUpdatedAt={inbox.dataUpdatedAt}
              viewer={viewer}
            />
          )),
        ])}
      </ul>
    </ViewportAnchor>
  );
}
