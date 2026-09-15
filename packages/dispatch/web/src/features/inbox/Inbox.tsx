import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useRef } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import {
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { PriorityControl } from "../issue/PriorityControl";
import { actorLabel } from "../refs/actor";
import { COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildInboxPath, buildIssuePath, buildProjectPath, parseInboxSearch } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
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

const ROW_SELECTOR = "[data-inbox-row]";

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  const active = document.activeElement;
  return active instanceof HTMLElement && active.matches(ROW_SELECTOR) ? active : null;
}

function rowAround(node: Element | null): HTMLElement | null {
  return node?.closest<HTMLElement>(ROW_SELECTOR) ?? null;
}

function InboxItem({ ask }: { ask: InboxRow }): ReactNode {
  const owner = ask.issue?.key ?? ask.issue_key;
  const title = ask.issue?.title ?? owner ?? "Unassigned ask";
  return (
    <li
      className={`rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing}`}
      data-inbox-row=""
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
      </div>
      <AskCard ask={ask} />
    </li>
  );
}

function AskSection({ asks, title }: { asks: readonly InboxRow[]; title: string }): ReactNode {
  if (asks.length === 0) return null;
  return (
    <section>
      <h2 className={`mb-3 text-base font-semibold ${textMutedOnCanvas}`}>{title}</h2>
      <ul className="space-y-3">
        {asks.map((ask) => (
          <InboxItem ask={ask} key={ask.id} />
        ))}
      </ul>
    </section>
  );
}

export function Inbox(): ReactNode {
  const { search } = useLocation();
  const filter = parseInboxSearch(search);
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });
  const { titles } = useAgents(filter.agent !== undefined);
  const listRef = useRef<HTMLDivElement>(null);
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  const step = (delta: 1 | -1) => {
    const all = rows();
    const current = rowAround(document.activeElement);
    const index = current === null ? -1 : all.indexOf(current);
    const next =
      index === -1
        ? delta === 1
          ? 0
          : all.length - 1
        : Math.min(all.length - 1, Math.max(0, index + delta));
    all[next]?.focus();
  };
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

  if (inbox.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  const agent = filter.agent;
  const fromAgent =
    agent === undefined
      ? inbox.data
      : inbox.data.filter((ask) => ask.author.kind === "session" && ask.author.id === agent);
  const shown = filter.section === "needs-you" ? waitingOnYou(fromAgent) : fromAgent;
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

  if (shown.length === 0) {
    return (
      <div className="space-y-6">
        {chip}
        <EmptyState
          label="Inbox empty state"
          message={agent === undefined ? "Nothing needs you" : `No open asks from ${agentTitle}`}
        />
      </div>
    );
  }

  const waiting = waitingOnYou(shown);
  const waitingOnAgents = shown.filter((ask) => ask.waiting_on === "agent");

  return (
    <div className="space-y-6" ref={listRef}>
      {chip}
      {agent === undefined ? <BlockedOnYou asks={inbox.data} /> : null}
      <AskSection asks={waiting} title="Waiting on you" />
      <AskSection asks={waitingOnAgents} title="Waiting on agents" />
    </div>
  );
}
