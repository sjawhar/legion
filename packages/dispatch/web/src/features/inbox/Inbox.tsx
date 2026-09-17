import { useQuery } from "@tanstack/react-query";
import { type FocusEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { AskTurn, InboxRow } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
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
import { closestMatching, roveFocus } from "../shell/roving";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { AskCard } from "./AskCard";
import { BlockedOnYou, waitingOnYou } from "./BlockedOnYou";

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

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  const active = document.activeElement;
  return active instanceof HTMLElement && active.matches(ROW_SELECTOR) ? active : null;
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
        <QueryError
          message={write.error ?? `Could not assign ${issueKey} to you.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}

/** Where a row sits: whose turn it is, or - in the Mine view - the Unassigned band. */
type InboxSection = AskTurn | "unassigned";

function InboxItem({
  ask,
  assignLive,
  onAnswered,
  onAssignLive,
  onRelease,
  section,
  viewer,
}: {
  ask: InboxRow;
  /** True while this row's "Assign to me" write is in flight or failed, whatever section the
   *  optimistic update put the row in. */
  assignLive: boolean;
  onAnswered: (id: string) => void;
  onAssignLive: (askId: string, live: boolean) => void;
  /** Set on the held row - one the server dropped or moved to another section that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  section: InboxSection;
  /** The signed-in lowercase login; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const owner = ask.issue?.key ?? ask.issue_key;
  const title = ask.issue?.title ?? owner ?? "Document ask";
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
      <div className="mb-2 flex flex-wrap items-center gap-2">
        {ask.document === undefined ? (
          owner === null ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>{title}</p>
          ) : (
            <Link
              className={`flex flex-col items-start gap-1 text-sm md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
              data-inbox-owner=""
              to={buildIssuePath({ id: ask.id, key: owner, kind: "ask" })}
              {...referenceTriggerProps({ key: owner, kind: "issue" })}
            >
              <span className="font-semibold">{owner}</span>
              <span>{title}</span>
            </Link>
          )
        ) : (
          <Link
            className={`flex flex-col items-start gap-1 text-sm font-semibold md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
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
            {ask.document.project} · {ask.document.name}
          </Link>
        )}
        <InboxRowChip ask={ask} />
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
      </div>
      <AskCard ask={ask} onAnswered={onAnswered} />
    </li>
  );
}

const SECTION_TITLES: Record<InboxSection, string> = {
  agent: "Waiting on agents",
  human: "Waiting on you",
  unassigned: "Unassigned",
};

/** The viewer's own rows: asks on issues assigned to their lowercase login. */
function isMine(row: InboxRow, viewer: string): boolean {
  return row.issue?.assignee === viewer;
}

/** Rows nobody holds: asks on unassigned issues, and every document ask (a document has no
 *  assignee). */
function isUnassigned(row: InboxRow): boolean {
  return (row.issue?.assignee ?? null) === null;
}

function storedInboxView(login: string): InboxView | undefined {
  const stored = window.localStorage.getItem(userPreferenceStorageKey(login, "inbox.view"));
  return stored === "mine" || stored === "everyone" ? stored : undefined;
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
 *  band is an assignment, shown at once. */
function heldRow(
  seen: PlacedRow | undefined,
  listed: InboxRow | undefined,
  now: InboxSection | undefined
): PlacedRow | undefined {
  if (seen === undefined) return undefined;
  if (listed === undefined) return seen;
  const movedBetweenTurnSections =
    now !== undefined &&
    now !== seen.section &&
    now !== "unassigned" &&
    seen.section !== "unassigned";
  return movedBetweenTurnSections ? { ask: listed, section: seen.section } : undefined;
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
  const view: InboxView =
    filter.view ?? (login === undefined ? undefined : storedInboxView(login)) ?? "mine";
  const selectView = (next: InboxView) => {
    if (login !== undefined) {
      window.localStorage.setItem(userPreferenceStorageKey(login, "inbox.view"), next);
    }
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
  ]);

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  const agent = filter.agent;
  const fromAgent =
    agent === undefined
      ? inbox.data
      : inbox.data.filter((ask) => ask.author.kind === "session" && ask.author.id === agent);
  const inView = (rows: readonly InboxRow[]) =>
    view === "everyone" ? rows : rows.filter((row) => isMine(row, viewer) || isUnassigned(row));
  const shown = inView(filter.section === "needs-you" ? waitingOnYou(fromAgent) : fromAgent);
  // A row the inbox lists without a turn belongs to no section, exactly as before the views.
  const sectionOf = (row: InboxRow): InboxSection | undefined =>
    view === "mine" && !isMine(row, viewer) ? "unassigned" : row.waiting_on;
  const seen =
    anchor === null || anchor.id === answered.current
      ? undefined
      : presented.current.find(({ ask }) => ask.id === anchor.id);
  const listed = seen === undefined ? undefined : shown.find((ask) => ask.id === seen.ask.id);
  const now = listed === undefined ? undefined : sectionOf(listed);
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

  if (shown.length === 0 && held === undefined) {
    return (
      <div className="space-y-6">
        {viewSwitch}
        {chip}
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
      shown.filter((ask) => sectionOf(ask) === section && ask.id !== held?.ask.id),
      held?.section === section ? held.ask : undefined,
      presented.current
    );
  const sections = (["human", "agent", "unassigned"] as const)
    .map((section) => [section, rowsIn(section)] as const)
    .filter(([, rows]) => rows.length > 0);
  presented.current = sections.flatMap(([section, rows]) => rows.map((ask) => ({ ask, section })));

  // One list, keyed by ask id, with the section headings as items between the rows: a row that
  // changes section moves within the same parent, so React moves its node instead of remounting
  // it - its draft, selection, disclosures, and focus stay, and the viewport anchor can find it.
  return (
    <ViewportAnchor
      className="space-y-6"
      group="data-inbox-section"
      item={ROW_ATTRIBUTE}
      ref={viewport}
      rootRef={listRef}
    >
      {viewSwitch}
      {chip}
      {agent === undefined ? <BlockedOnYou asks={inView(inbox.data)} /> : null}
      <ul className="space-y-3">
        {sections.flatMap(([section, rows], index) => [
          <li
            className={index === 0 ? undefined : "pt-3"}
            key={`heading-${section}`}
            role="presentation"
          >
            <h2 className={`text-base font-semibold ${textMutedOnCanvas}`}>
              {SECTION_TITLES[section]}
            </h2>
          </li>,
          ...rows.map((ask) => (
            <InboxItem
              ask={ask}
              assignLive={assigning.has(ask.id)}
              onAnswered={recordAnswered}
              onAssignLive={onAssignLive}
              key={ask.id}
              onRelease={ask.id === held?.ask.id ? release : undefined}
              section={section}
              viewer={viewer}
            />
          )),
        ])}
      </ul>
    </ViewportAnchor>
  );
}
