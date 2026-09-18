import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type {
  Agent,
  Message,
  MessageDelivery,
  MessageRead,
  UserAgentStates,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon, DisclosureToggle } from "../../components/DisclosureToggle";
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
import { resolveAuthor } from "../conversation/authors";
import { MentionComposer, type ReplyTarget } from "../conversation/MentionComposer";
import { firstLine, replyQuoteText } from "../conversation/ReplyQuote";
import { ReplyTurn, ThreadReplies } from "../conversation/ReplyTurn";
import {
  type TargetedMessageAttempt,
  TargetedMessageCard,
} from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { sessionLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildInboxPath, buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { userPreferenceStorageKey } from "../shell/userPreference";

const INACTIVE_AFTER_MS = 10 * 60_000;

/** The grey-dot rule: a session unseen for ten minutes folds under `Inactive (N)`. */
function isInactive(agent: Agent, now: number): boolean {
  return now - agent.last_seen >= INACTIVE_AFTER_MS;
}

/** A message's delivery attempts as `TargetedMessageCard` shows them, all aimed at one session. */
function deliveryAttempts(
  deliveries: readonly MessageDelivery[],
  targetName: string
): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    attempt: attempt.attempt,
    createdAt: attempt.created_at,
    delivery: attempt.delivery,
    error: attempt.error,
    state: attempt.state,
    targetName,
  }));
}

/** Open asks from each session whose turn is the viewer's, keyed by session ID. */
export type NeedsYouBySession = ReadonlyMap<string, number>;

/** Whether Dispatch has heard from the session at all: an event, or an open ask. */
function hasDispatchSignal(agent: Agent, needsYou: NeedsYouBySession): boolean {
  return (
    agent.last_activity !== null || agent.open_asks > 0 || (needsYou.get(agent.session_id) ?? 0) > 0
  );
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

/**
 * Pinned sessions first, in pin order; then whoever needs the viewer, then the most open asks,
 * then the newest Dispatch activity (sessions Dispatch never heard from last), then the title.
 */
export function orderAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  needsYou: NeedsYouBySession
): Agent[] {
  const pinOrder = new Map(pinned.map((sessionID, index) => [sessionID, index]));
  return [...agents].sort((left, right) => {
    const leftPin = pinOrder.get(left.session_id);
    const rightPin = pinOrder.get(right.session_id);
    if (leftPin !== undefined || rightPin !== undefined) {
      if (leftPin === undefined) return 1;
      if (rightPin === undefined) return -1;
      return leftPin - rightPin;
    }
    const byNeedsYou = (needsYou.get(right.session_id) ?? 0) - (needsYou.get(left.session_id) ?? 0);
    if (byNeedsYou !== 0) return byNeedsYou;
    if (left.open_asks !== right.open_asks) return right.open_asks - left.open_asks;
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
 * Three lists in `orderAgents` order: `active` — live sessions Dispatch has heard from, plus
 * every pinned one whatever its age or silence; `quiet` — live sessions with no Dispatch
 * signal, for the collapsed `No Dispatch activity (N)` disclosure; `inactive` — sessions unseen
 * for ten minutes, for the collapsed `Inactive (N)` disclosure beneath it. A silent unseen
 * session is inactive.
 */
export function partitionAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  needsYou: NeedsYouBySession,
  now: number
): { active: Agent[]; quiet: Agent[]; inactive: Agent[] } {
  const active: Agent[] = [];
  const quiet: Agent[] = [];
  const inactive: Agent[] = [];
  for (const agent of agents) {
    if (pinned.includes(agent.session_id)) active.push(agent);
    else if (isInactive(agent, now)) inactive.push(agent);
    else if (hasDispatchSignal(agent, needsYou)) active.push(agent);
    else quiet.push(agent);
  }
  return {
    active: orderAgents(active, pinned, needsYou),
    quiet: orderAgents(quiet, pinned, needsYou),
    inactive: orderAgents(inactive, pinned, needsYou),
  };
}

/** A reply the agent composer answers: the quoted message and the conversation it belongs to
 *  (an issue, or none), which decides the route the reply is created through. */
interface AgentReply {
  readonly issueKey: string | null;
  readonly target: ReplyTarget;
}

/** The delivery a reply into this exchange inherits: the agent, in the mode of the exchange's
 *  most recent attempt across the root and every delivered reply. */
function exchangeDelivery(agent: Agent, read: MessageRead): NonNullable<ReplyTarget["thread"]> {
  let last: MessageDelivery | undefined;
  for (const attempt of [read.message, ...read.replies].flatMap((item) => item.deliveries)) {
    if (last === undefined || Date.parse(attempt.created_at) >= Date.parse(last.created_at)) {
      last = attempt;
    }
  }
  return {
    delivery: last?.delivery ?? "steer",
    target: `session:${agent.session_id}`,
    title: sessionLabel(agent.session_id, agent.title),
  };
}

function agentReplyTo(agent: Agent, read: MessageRead, node: Message, author: string): AgentReply {
  return {
    issueKey: read.message.issue_key,
    target: {
      author,
      excerpt: firstLine(node.body),
      id: node.id,
      parentKind: "message",
      thread: exchangeDelivery(agent, read),
      ...(node.issue_key === null
        ? {}
        : { to: buildIssuePath({ id: node.id, key: node.issue_key, kind: "message" }) }),
    },
  };
}

function AgentExchangeReply({
  agent,
  onReply,
  read,
  reply,
  titles,
}: {
  agent: Agent;
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  reply: Message;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") => api.createMessageDelivery(reply.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: ["agents", agent.session_id, "messages"] }),
  });
  const label = sessionLabel(agent.session_id, agent.title);
  const author = resolveAuthor(reply.author, titles);
  const parent = [read.message, ...read.replies].find((item) => item.id === reply.in_reply_to);
  const answer = read.replies.find(
    (candidate) => candidate.in_reply_to === reply.id && candidate.author.kind === "session"
  );
  return (
    <ReplyTurn
      at={reply.created_at}
      author={author}
      body={
        <div className={textPrimaryOnCanvas}>
          <MarkdownBody markdown={reply.body} variant="inline" />
        </div>
      }
      delivery={
        reply.deliveries.length === 0
          ? undefined
          : {
              answeredBy:
                answer === undefined ? undefined : resolveAuthor(answer.author, titles).label,
              attempts: deliveryAttempts(reply.deliveries, label),
              retry: {
                canBtw: agent.capabilities.includes("btw"),
                onRetry: retry.mutate,
                retrying: retry.isPending,
              },
              targetName: label,
            }
      }
      onReply={() => onReply(agentReplyTo(agent, read, reply, author.label))}
      quote={
        parent === undefined
          ? undefined
          : {
              text: replyQuoteText(
                resolveAuthor(parent.author, titles).label,
                firstLine(parent.body)
              ),
              ...(parent.issue_key === null
                ? {}
                : {
                    to: buildIssuePath({ id: parent.id, key: parent.issue_key, kind: "message" }),
                  }),
            }
      }
      turnID={`message:${reply.id}`}
    />
  );
}

function AgentTargetedMessage({
  agent,
  onReply,
  read,
}: {
  agent: Agent;
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") => api.createMessageDelivery(read.message.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: ["agents", agent.session_id, "messages"] }),
  });
  const label = sessionLabel(agent.session_id, agent.title);
  const titles = useMemo(
    () => new Map([[agent.session_id, agent.title]]),
    [agent.session_id, agent.title]
  );
  const asker = resolveAuthor(read.message.author, titles);
  const answer = read.replies.find((reply) => reply.author.kind === "session");
  return (
    <TargetedMessageCard
      answeredBy={answer === undefined ? undefined : resolveAuthor(answer.author, titles).label}
      body={
        <>
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <span className="font-semibold">{asker.label}</span>
            <Timestamp at={read.message.created_at} />
          </p>
          <div className={textPrimaryOnCanvas}>
            <MarkdownBody markdown={read.message.body} variant="inline" />
          </div>
        </>
      }
      canBtw={agent.capabilities.includes("btw")}
      deliveries={deliveryAttempts(read.message.deliveries, label)}
      header={null}
      isClosed={false}
      onReply={() => onReply(agentReplyTo(agent, read, read.message, asker.label))}
      onRetry={retry.mutate}
      retrying={retry.isPending}
      targetName={label}
      thread={
        read.replies.length === 0 ? null : (
          <ThreadReplies>
            {read.replies.map((reply) => (
              <AgentExchangeReply
                agent={agent}
                key={reply.id}
                onReply={onReply}
                read={read}
                reply={reply}
                titles={titles}
              />
            ))}
          </ThreadReplies>
        )
      }
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

function AgentMessageList({
  agent,
  onReply,
}: {
  agent: Agent;
  onReply: (reply: AgentReply) => void;
}): ReactNode {
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
          <AgentTargetedMessage
            agent={agent}
            key={newest.message.id}
            onReply={onReply}
            read={newest}
          />
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
                <AgentTargetedMessage
                  agent={agent}
                  key={read.message.id}
                  onReply={onReply}
                  read={read}
                />
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

function AgentMessageComposer({
  agent,
  onCancelReply,
  replyTo,
}: {
  agent: Agent;
  onCancelReply: () => void;
  replyTo: AgentReply | null;
}): ReactNode {
  const queryClient = useQueryClient();
  const [issueKey, setIssueKey] = useState("");
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: ["agents", "issue-picker"],
  });
  // Replies retain their parent owner: issue-attached legacy messages stay on that issue's
  // message route, while issue-less roots keep the S3-deferred direct session channel.
  const replyIssueKey = replyTo?.issueKey;
  const useDirectChannel =
    replyIssueKey === null || (replyIssueKey === undefined && issueKey === "");
  const composerOwner = useDirectChannel
    ? { kind: "session" as const, sessionId: agent.session_id }
    : { issueKey: replyIssueKey ?? issueKey, kind: "issue" as const };

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      {replyTo === null ? (
        <button
          aria-expanded={issuePickerOpen}
          aria-label="Choose issue"
          className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={() => setIssuePickerOpen((open) => !open)}
          type="button"
        >
          Issue: {issueKey === "" ? "No issue" : issueKey}
        </button>
      ) : null}
      {issuePickerOpen && replyTo === null ? (
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
      <MentionComposer
        initialMentions={
          useDirectChannel
            ? undefined
            : [{ target: `session:${agent.session_id}`, title: agent.title || agent.session_id }]
        }
        key={useDirectChannel ? `session:${agent.session_id}` : `issue:${issueKey}`}
        onCancelReply={onCancelReply}
        onClose={onCancelReply}
        onSent={() => {
          onCancelReply();
          void queryClient.invalidateQueries({
            queryKey: ["agents", agent.session_id, "messages"],
          });
        }}
        owner={composerOwner}
        replyTo={replyTo?.target ?? null}
      />
    </div>
  );
}

/** A whose-turn pill: a non-zero ask count linking to the Inbox narrowed to this agent's asks. */
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
  return (
    <Link
      className="inline-flex min-h-11 items-center rounded-full md:min-h-8"
      title={title}
      to={buildInboxPath({ agent: agent.session_id, section })}
    >
      <LabelPill selected={selected}>
        {wording} {count}
      </LabelPill>
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
  // The inbox query and the agent list are polled separately, so the two counts can briefly
  // disagree in either direction; a negative remainder renders nothing.
  const waitingOnAgent = agent.open_asks - needsYou;
  const [expanded, setExpanded] = useState(false);
  const [replyTo, setReplyTo] = useState<AgentReply | null>(null);
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
              "No Dispatch activity"
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
              title={`Open asks from ${label} waiting on your answer`}
              wording="Needs you"
            />
          )}
          {waitingOnAgent <= 0 ? null : (
            <AskCountPill
              agent={agent}
              count={waitingOnAgent}
              title={`Open asks from ${label} whose next move is ${label}'s`}
              wording="Waiting on agent"
            />
          )}
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
          <AgentMessageList agent={agent} onReply={setReplyTo} />
          <AgentMessageComposer
            agent={agent}
            onCancelReply={() => setReplyTo(null)}
            replyTo={replyTo}
          />
        </div>
      ) : null}
    </article>
  );
}

/** A collapsed `<label> (N)` disclosure over rows the page keeps out of the way; absent when
 * empty, closed on every load, and open only while this page stays mounted. */
function AgentFold({
  agents,
  label,
  needsYouBySession,
  onPin,
}: {
  agents: readonly Agent[];
  label: string;
  needsYouBySession: NeedsYouBySession;
  onPin: (sessionID: string) => void;
}): ReactNode {
  const [expanded, setExpanded] = useState(false);
  if (agents.length === 0) return null;
  return (
    // A block wrapper, not a fragment: the global `button { display: inline-flex }` would
    // otherwise let two collapsed toggles share one line below the desktop breakpoint.
    <div className="space-y-3">
      <DisclosureToggle
        expanded={expanded}
        label={`${label} (${agents.length})`}
        onToggle={() => setExpanded((open) => !open)}
      />
      {expanded ? (
        <section aria-label={label} className="space-y-3">
          {agents.map((agent) => (
            <AgentRow
              agent={agent}
              key={agent.session_id}
              needsYou={needsYouBySession.get(agent.session_id) ?? 0}
              onPin={() => onPin(agent.session_id)}
              pinned={false}
            />
          ))}
        </section>
      ) : null}
    </div>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, error, isError, isPending } = useAgents(true, true);
  const inbox = useQuery(inboxQuery());
  const whoAmI = useQuery(whoAmIQuery());
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
  const { active, quiet, inactive } = partitionAgents(
    agents,
    pinned,
    needsYouBySession,
    Date.now()
  );
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
          <AgentFold
            agents={quiet}
            label="No Dispatch activity"
            needsYouBySession={needsYouBySession}
            onPin={togglePin}
          />
          <AgentFold
            agents={inactive}
            label="Inactive"
            needsYouBySession={needsYouBySession}
            onPin={togglePin}
          />
        </div>
      )}
    </section>
  );
}
