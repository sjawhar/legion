import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, MessageRead, UserAgentStates } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { PinButton } from "../../components/PinButton";
import {
  borderDefault,
  card,
  connectionDotConnecting,
  dangerText,
  disclosureButtonText,
  inputClasses,
  liveDotBg,
  offlineDotBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConversationComposer } from "../conversation/ConversationComposer";
import { TargetedMessageCard } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { sessionLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildInboxPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { userPreferenceStorageKey } from "../shell/userPreference";

const INACTIVE_AFTER_MS = 10 * 60_000;

/** The grey-dot rule: a session unseen for ten minutes folds under `Inactive (N)`. */
function isInactive(agent: Agent, now: number): boolean {
  return now - agent.last_seen >= INACTIVE_AFTER_MS;
}

function freshness(agent: Agent): { dot: string; label: string } {
  const now = Date.now();
  if (isInactive(agent, now)) return { dot: offlineDotBg, label: "Seen 10 minutes ago or longer" };
  if (now - agent.last_seen < 2 * 60_000) {
    return { dot: liveDotBg, label: "Seen less than 2 minutes ago" };
  }
  return { dot: connectionDotConnecting, label: "Seen less than 10 minutes ago" };
}

function FreshnessDot({ agent }: { agent: Agent }): ReactNode {
  const status = freshness(agent);
  return (
    <span className="inline-flex shrink-0 items-center" role="status" title={status.label}>
      <span aria-hidden className={`h-2.5 w-2.5 rounded-full ${status.dot}`} />
      <span className="sr-only">{status.label}</span>
    </span>
  );
}

export function orderAgents(agents: readonly Agent[], pinned: readonly string[]): Agent[] {
  const pinOrder = new Map(pinned.map((sessionID, index) => [sessionID, index]));
  return [...agents].sort((left, right) => {
    const leftPin = pinOrder.get(left.session_id);
    const rightPin = pinOrder.get(right.session_id);
    if (leftPin !== undefined || rightPin !== undefined) {
      if (leftPin === undefined) return 1;
      if (rightPin === undefined) return -1;
      return leftPin - rightPin;
    }
    if (left.last_activity === null || right.last_activity === null) {
      if (left.last_activity === null && right.last_activity !== null) return 1;
      if (left.last_activity !== null && right.last_activity === null) return -1;
    } else if (left.last_activity !== right.last_activity) {
      return right.last_activity.localeCompare(left.last_activity);
    }
    return left.title.localeCompare(right.title) || left.session_id.localeCompare(right.session_id);
  });
}

/**
 * Active sessions (and every pinned one, whatever its age) in list order; inactive sessions,
 * ordered the same way, for the collapsed `Inactive (N)` disclosure beneath them.
 */
export function partitionAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  now: number
): { active: Agent[]; inactive: Agent[] } {
  const active: Agent[] = [];
  const inactive: Agent[] = [];
  for (const agent of agents) {
    (isInactive(agent, now) && !pinned.includes(agent.session_id) ? inactive : active).push(agent);
  }
  return { active: orderAgents(active, pinned), inactive: orderAgents(inactive, pinned) };
}

function AgentTargetedMessage({ agent, read }: { agent: Agent; read: MessageRead }): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") => api.createMessageDelivery(read.message.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: ["agents", agent.session_id, "messages"] }),
  });
  const reply = read.replies.at(-1);
  const label = sessionLabel(agent.session_id, agent.title);
  return (
    <TargetedMessageCard
      answer={
        reply === undefined
          ? undefined
          : {
              author: reply.author.kind === "user" ? reply.author.id : label,
              body: (
                <div className={textPrimaryOnCanvas}>
                  {read.replies.map((item) => (
                    <MarkdownBody key={item.id} markdown={item.body} variant="inline" />
                  ))}
                </div>
              ),
            }
      }
      body={
        <>
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <span className="font-semibold">
              {read.message.author.kind === "user" ? read.message.author.id : label}
            </span>
            <Timestamp at={read.message.created_at} />
          </p>
          <div className={textPrimaryOnCanvas}>
            <MarkdownBody markdown={read.message.body} variant="inline" />
          </div>
        </>
      }
      canBtw={agent.capabilities.includes("btw")}
      deliveries={read.message.deliveries.map((attempt) => ({
        attempt: attempt.attempt,
        createdAt: attempt.created_at,
        delivery: attempt.delivery,
        error: attempt.error,
        state: attempt.state,
        targetName: label,
      }))}
      header={null}
      isClosed={false}
      onRetry={retry.mutate}
      retrying={retry.isPending}
      targetName={label}
      turnID={`message:${read.message.id}`}
    />
  );
}

/** When an exchange last moved: its newest reply, else the root message. */
function exchangeActivityAt(read: MessageRead): string {
  return (read.replies.at(-1) ?? read.message).created_at;
}

/**
 * The exchanges a viewer still sees after a Clear: those whose newest message is after
 * `clearedBefore`. A message asked before the Clear but answered after it stays, because the
 * answer is news.
 */
function exchangesAfter(
  exchanges: readonly MessageRead[],
  clearedBefore: string | undefined
): readonly MessageRead[] {
  if (clearedBefore === undefined) return exchanges;
  const cutoff = Date.parse(clearedBefore);
  return exchanges.filter((read) => Date.parse(exchangeActivityAt(read)) > cutoff);
}

function AgentMessageList({ agent }: { agent: Agent }): ReactNode {
  const queryClient = useQueryClient();
  const messages = useQuery({
    queryFn: () => api.listAgentMessages(agent.session_id),
    queryKey: ["agents", agent.session_id, "messages"],
  });
  const agentState = useQuery({
    queryFn: () => api.getMyAgentState(),
    queryKey: ["user-agent-state"],
  });
  const [showOlder, setShowOlder] = useState(false);
  const [showCleared, setShowCleared] = useState(false);
  // The cutoff is the newest visible message's own timestamp, not the browser clock: both are
  // compared against `created_at` (the server's clock), so a slow browser clock would otherwise
  // make Clear a silent no-op. This hides exactly what the viewer saw.
  const clear = useMutation({
    mutationFn: (clearedBefore: string) =>
      api.putAgentState(agent.session_id, { cleared_before: clearedBefore }),
    onSuccess: (next) => {
      setShowCleared(false);
      queryClient.setQueryData<UserAgentStates>(["user-agent-state"], (current) => ({
        ...current,
        [agent.session_id]: next,
      }));
    },
  });
  const label = sessionLabel(agent.session_id, agent.title);
  if (messages.isPending || agentState.isPending) return null;
  if (messages.isError || agentState.isError) {
    return <p className={`mt-3 text-sm ${dangerText}`}>Could not load this conversation.</p>;
  }
  if (messages.data.length === 0) return null;
  const clearedBefore = agentState.data[agent.session_id]?.cleared_before;
  const unread = exchangesAfter(messages.data, clearedBefore);
  const visible = showCleared ? messages.data : unread;
  const [newest, ...older] = visible;
  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      {newest === undefined ? null : (
        <ol aria-label={`Conversation with ${label}`} className="space-y-2">
          <AgentTargetedMessage agent={agent} key={newest.message.id} read={newest} />
          {older.length === 0 ? null : (
            <li>
              <DisclosureToggle
                expanded={showOlder}
                label={`Show ${older.length} older`}
                onToggle={() => setShowOlder((open) => !open)}
              />
            </li>
          )}
          {showOlder
            ? older.map((read) => (
                <AgentTargetedMessage agent={agent} key={read.message.id} read={read} />
              ))
            : null}
        </ol>
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        {clearedBefore === undefined || unread.length === messages.data.length ? null : (
          <p className={`flex flex-wrap items-center gap-x-1 text-sm ${textMutedOnCanvas}`}>
            <span>
              Cleared <Timestamp at={clearedBefore} />
            </span>
            <span aria-hidden>·</span>
            <button
              aria-pressed={showCleared}
              className={`inline-flex min-h-11 items-center underline-offset-2 hover:underline md:min-h-8 ${textMutedHoverToSecondary}`}
              onClick={() => setShowCleared((open) => !open)}
              type="button"
            >
              {showCleared ? "Hide again" : "Show anyway"}
            </button>
          </p>
        )}
        {visible.length === 0 ? null : (
          <button
            className={`ml-auto inline-flex min-h-11 items-center text-sm md:min-h-8 ${textMutedHoverToSecondary}`}
            disabled={clear.isPending}
            onClick={() =>
              clear.mutate(
                visible
                  .map(exchangeActivityAt)
                  .reduce((latest, at) => (Date.parse(at) > Date.parse(latest) ? at : latest))
              )
            }
            type="button"
          >
            Clear conversation
          </button>
        )}
      </div>
    </div>
  );
}

function AgentMessageComposer({ agent }: { agent: Agent }): ReactNode {
  const queryClient = useQueryClient();
  const [issueKey, setIssueKey] = useState("");
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: ["agents", "issue-picker"],
  });
  const label = sessionLabel(agent.session_id, agent.title);

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      <button
        aria-expanded={issuePickerOpen}
        aria-label="Choose issue"
        className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        onClick={() => setIssuePickerOpen((open) => !open)}
        type="button"
      >
        Issue: {issueKey === "" ? "No issue" : issueKey}
      </button>
      {issuePickerOpen ? (
        <div className="mt-2">
          {issues.isPending ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading issues…</p>
          ) : null}
          {issues.isError ? (
            <p className={`text-sm ${dangerText}`}>Could not load issues.</p>
          ) : null}
          {issues.data === undefined ? null : (
            <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
              Issue (optional)
              <select
                aria-label="Issue"
                className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
                onChange={(event) => {
                  setIssueKey(event.target.value);
                  setIssuePickerOpen(false);
                }}
                value={issueKey}
              >
                <option value="">No issue</option>
                {issues.data.map((issue) => (
                  <option key={issue.key} value={issue.key}>
                    {issue.key} · {issue.title}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      ) : null}
      <ConversationComposer
        agents={[agent]}
        embedded
        onSent={() =>
          void queryClient.invalidateQueries({
            queryKey: ["agents", agent.session_id, "messages"],
          })
        }
        onSend={({ body, delivery, target }) =>
          issueKey === ""
            ? api.createAgentMessage(agent.session_id, { body, delivery })
            : api.createMessage(issueKey, { body, delivery, target })
        }
        recipientSlot={
          <span className={`text-sm font-medium ${textSecondaryOnCanvas}`}>To: {label}</span>
        }
        route={`session:${agent.session_id}`}
      />
    </div>
  );
}

function ChevronIcon({ expanded }: { expanded: boolean }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className={`h-4 w-4 shrink-0 transition-transform ${expanded ? "rotate-180" : ""}`}
      fill="none"
      viewBox="0 0 16 16"
    >
      <path
        d="M4 6l4 4 4-4"
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="1.5"
      />
    </svg>
  );
}

/** The page's fold control: a labelled chevron button that reveals rows beneath it (the
 * `Inactive (N)` sessions, a conversation's `Show N older` exchanges). */
function DisclosureToggle({
  expanded,
  label,
  onToggle,
}: {
  expanded: boolean;
  label: string;
  onToggle: () => void;
}): ReactNode {
  return (
    <button
      aria-expanded={expanded}
      className={`flex min-h-11 items-center gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
      onClick={onToggle}
      type="button"
    >
      {label}
      <span className={disclosureButtonText}>
        <ChevronIcon expanded={expanded} />
      </span>
    </button>
  );
}

/** An ask-count pill; with a count it links to the Inbox narrowed to this agent's asks. */
function AskCountPill({
  agent,
  count,
  section,
  selected = false,
  title,
  wording,
}: {
  agent: Agent;
  count: number;
  section?: "needs-you";
  selected?: boolean;
  title: string;
  wording: string;
}): ReactNode {
  const pill = (
    <LabelPill selected={selected}>
      {wording} {count}
    </LabelPill>
  );
  if (count === 0) return pill;
  return (
    <Link
      className="inline-flex min-h-11 items-center rounded-full md:min-h-8"
      title={title}
      to={buildInboxPath({ agent: agent.session_id, section })}
    >
      {pill}
    </Link>
  );
}

function AgentRow({
  agent,
  needsYou,
  onPin,
  pinned,
}: {
  agent: Agent;
  needsYou: number;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  const label = sessionLabel(agent.session_id, agent.title);
  const machineAndDir = `${agent.machine_id} · ${agent.dir}`;
  const [expanded, setExpanded] = useState(false);
  const detailsId = useId();

  return (
    <article className={`rounded-xl border ${card} ${borderDefault}`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-1.5">
        <div className="flex min-w-0 flex-auto flex-wrap items-center gap-x-2 md:flex-nowrap">
          <FreshnessDot agent={agent} />
          <h2 className={`max-w-56 shrink-0 text-sm font-semibold ${textPrimaryOnCanvas}`}>
            <button
              aria-controls={detailsId}
              aria-expanded={expanded}
              className={`flex min-h-11 max-w-full items-center gap-1 text-left md:min-h-8 ${textPrimaryOnCanvas}`}
              onClick={() => setExpanded((open) => !open)}
              title={label}
              type="button"
            >
              <span className="min-w-0 truncate">{label}</span>
              <span className={disclosureButtonText}>
                <ChevronIcon expanded={expanded} />
              </span>
            </button>
          </h2>
          <CopyButton value={agent.session_id} what="session ID">
            ID
          </CopyButton>
          {agent.title.trim() === "" ? null : (
            <CopyButton value={agent.title} what="session title">
              title
            </CopyButton>
          )}
          <span
            className={`order-last min-w-0 basis-full truncate text-xs md:order-none md:flex-1 md:basis-0 ${textMutedOnCanvas}`}
            title={machineAndDir}
          >
            {machineAndDir}
          </span>
          {agent.roles.map((role) => (
            <LabelPill key={role}>{role}</LabelPill>
          ))}
        </div>
        <div className="ml-auto flex flex-wrap items-center justify-end gap-2">
          <span className={`text-xs ${textMutedOnCanvas}`}>
            {agent.last_activity === null ? (
              "No dispatch activity"
            ) : (
              <Timestamp at={agent.last_activity} />
            )}
          </span>
          {needsYou === 0 ? null : (
            <AskCountPill
              agent={agent}
              count={needsYou}
              section="needs-you"
              selected
              title={`Asks from ${label} waiting on you`}
              wording="Needs you"
            />
          )}
          <AskCountPill
            agent={agent}
            count={agent.open_asks}
            title={`Open asks from ${label}`}
            wording="Open asks"
          />
          <PinButton
            label={pinned ? `Unpin ${label}` : `Pin ${label}`}
            onClick={onPin}
            pinned={pinned}
            title={pinned ? "Unpin agent" : "Pin agent"}
          />
        </div>
      </div>
      {expanded ? (
        <div className={`border-t px-4 pb-4 ${borderDefault}`} id={detailsId}>
          <div
            className={`mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs ${textMutedOnCanvas}`}
          >
            {agent.capabilities.map((capability) => (
              <span key={capability}>{capability}</span>
            ))}
            <span>
              Seen <Timestamp at={new Date(agent.last_seen).toISOString()} />
            </span>
          </div>
          <AgentMessageList agent={agent} />
          <AgentMessageComposer agent={agent} />
        </div>
      ) : null}
    </article>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, error, isError, isPending } = useAgents(true, true);
  const inbox = useQuery({ queryFn: () => api.getInbox(), queryKey: ["inbox"] });
  const whoAmI = useQuery({ queryFn: () => api.whoAmI(), queryKey: ["whoami"] });
  const preferenceKey =
    whoAmI.data?.kind === "user"
      ? userPreferenceStorageKey(whoAmI.data.login, "agents.pinned")
      : undefined;
  const [pinned, setPinned] = useState<readonly string[]>([]);
  useEffect(() => {
    if (preferenceKey === undefined) {
      setPinned([]);
      return;
    }
    const raw = window.localStorage.getItem(preferenceKey);
    try {
      const parsed: unknown = raw === null ? [] : JSON.parse(raw);
      setPinned(
        Array.isArray(parsed)
          ? parsed.filter((value): value is string => typeof value === "string")
          : []
      );
    } catch {
      setPinned([]);
    }
  }, [preferenceKey]);
  const needsYouBySession = useMemo(() => {
    const counts = new Map<string, number>();
    for (const ask of waitingOnYou(inbox.data ?? [])) {
      if (ask.author.kind !== "session") continue;
      counts.set(ask.author.id, (counts.get(ask.author.id) ?? 0) + 1);
    }
    return counts;
  }, [inbox.data]);
  // Not memoised: the split is a function of the clock, like the freshness dot beside each row,
  // and is recomputed on every render of this page.
  const { active, inactive } = partitionAgents(agents, pinned, Date.now());
  const [showInactive, setShowInactive] = useState(false);
  const togglePin = (sessionID: string) => {
    setPinned((current) => {
      const next = current.includes(sessionID)
        ? current.filter((candidate) => candidate !== sessionID)
        : [...current, sessionID];
      if (preferenceKey !== undefined) {
        window.localStorage.setItem(preferenceKey, JSON.stringify(next));
      }
      return next;
    });
  };

  if (isPending) return <LoadingSkeleton label="Loading agents" />;
  if (isError) return <p className={dangerText}>Could not load agents: {error}</p>;

  return (
    <section aria-label="Agents">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Agents
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Live Envoy sessions and their Dispatch activity.
        </p>
      </header>
      {agents.length === 0 ? (
        <EmptyState label="Agents empty state" message="No agents are connected." />
      ) : (
        <div className="space-y-3">
          {active.map((agent) => (
            <AgentRow
              agent={agent}
              key={agent.session_id}
              needsYou={needsYouBySession.get(agent.session_id) ?? 0}
              onPin={() => togglePin(agent.session_id)}
              pinned={pinned.includes(agent.session_id)}
            />
          ))}
          {inactive.length === 0 ? null : (
            <>
              <DisclosureToggle
                expanded={showInactive}
                label={`Inactive (${inactive.length})`}
                onToggle={() => setShowInactive((open) => !open)}
              />
              {showInactive ? (
                <section aria-label="Inactive" className="space-y-3">
                  {inactive.map((agent) => (
                    <AgentRow
                      agent={agent}
                      key={agent.session_id}
                      needsYou={needsYouBySession.get(agent.session_id) ?? 0}
                      onPin={() => togglePin(agent.session_id)}
                      pinned={false}
                    />
                  ))}
                </section>
              ) : null}
            </>
          )}
        </div>
      )}
    </section>
  );
}
