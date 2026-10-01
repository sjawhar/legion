import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { agentMessagesQuery, inboxQuery, userAgentStateQuery } from "../../api/queries";
import type {
  Agent,
  Message,
  MessageDelivery,
  MessageDeliveryMode,
  MessageRead,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon, DisclosureToggle } from "../../components/DisclosureToggle";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { PinButton } from "../../components/PinButton";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  card,
  checkboxAccent,
  connectionDotConnecting,
  dangerText,
  disclosureButtonText,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
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
import { capabilityLabel, MODE_LABELS } from "../conversation/delivery";
import type { ReplyTarget } from "../conversation/MentionComposer";
import { firstLine, replyQuoteText } from "../conversation/ReplyQuote";
import { ReplyTurn, ThreadReplies } from "../conversation/ReplyTurn";
import { capabilitiesForTarget, TargetedMessageCard } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { sessionLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildInboxPath, buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { closestMatching, focusOnDocument } from "../shell/roving";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useUserPreference } from "../shell/userPreference";
import {
  AgentMessageComposer,
  type AgentReply,
  agentComposerMutationKey,
} from "./AgentMessageComposer";
import { deliveryAttempts } from "./attempts";
import { type BroadcastSend, BroadcastSends, useBroadcastQueue } from "./BroadcastSends";
import { useBroadcastComposition } from "./broadcast-composition";
import { broadcastSendState, type ComposedBroadcast, composedBroadcast } from "./broadcast-plan";
import { EndedAgentsWithReplies } from "./EndedAgentsWithReplies";
import { AGENT_ROW_SELECTOR, leaveAgentComposer, useAgentsKeymap } from "./keyboard";
import { foldLabel, matchingSelection, selectionSummary, toggleMatching } from "./selection";
import { storeAgentState, unreadRepliesLabel, useMarkRepliesRead, useUnreadAtOpen } from "./unread";

const INACTIVE_AFTER_MS = 10 * 60_000;

/** The composer's one notice slot: the recipient limit or the exclusions, one at
 *  a time. In the compact grid it is one line that scrolls sideways like the chips: between the
 *  mode and Send on a narrow screen, adding no height, and the third and last row on a short one. */
const composerLine = `mt-2 text-sm narrow-or-short:order-2 narrow-or-short:col-start-2 narrow-or-short:mt-0 narrow-or-short:min-w-0 narrow-or-short:overflow-x-auto narrow-or-short:text-xs narrow-or-short:whitespace-nowrap short:order-3 short:col-span-4 short:col-start-1 ${dangerText}`;

/** The grey-dot rule: a session unseen for ten minutes folds under `Inactive (N)`. */
function isInactive(agent: Agent, now: number): boolean {
  return now - agent.last_seen >= INACTIVE_AFTER_MS;
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

/** The delivery a reply into this exchange inherits: the agent, in the mode of the exchange's
 *  most recent attempt across the root and every delivered reply. */
function exchangeDelivery(agent: Agent, read: MessageRead): NonNullable<ReplyTarget["thread"]> {
  let last: MessageDelivery | undefined;
  for (const attempt of [read.message, ...read.replies].flatMap((item) => item.deliveries)) {
    if (last === undefined || Date.parse(attempt.created_at) >= Date.parse(last.created_at)) {
      last = attempt;
    }
  }
  const canSteer = agent.capabilities.includes("steer");
  return {
    delivery:
      last?.delivery === "steer" && !canSteer
        ? "btw"
        : (last?.delivery ?? (canSteer ? "steer" : "btw")),
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
  liveAgents,
  onReply,
  read,
  reply,
  replyDisabled,
  titles,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  reply: Message;
  replyDisabled: boolean;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: MessageDeliveryMode) => api.createMessageDelivery(reply.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({
        queryKey: agentMessagesQuery(agent.session_id).queryKey,
      }),
  });
  const label = sessionLabel(agent.session_id, agent.title);
  const author = resolveAuthor(reply.author, titles);
  const parent = [read.message, ...read.replies].find((item) => item.id === reply.in_reply_to);
  const answer = read.replies.find(
    (candidate) => candidate.in_reply_to === reply.id && candidate.author.kind === "session"
  );
  const capabilities = capabilitiesForTarget(read.message.target, liveAgents);
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
                canAside: capabilities?.includes("aside") !== false,
                canBtw: capabilities?.includes("btw") !== false,
                canSteer: capabilities?.includes("steer") !== false,
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
      replyDisabled={replyDisabled}
      turnID={`message:${reply.id}`}
    />
  );
}

function AgentTargetedMessage({
  agent,
  liveAgents,
  onReply,
  read,
  replyDisabled,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  replyDisabled: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: MessageDeliveryMode) =>
      api.createMessageDelivery(read.message.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({
        queryKey: agentMessagesQuery(agent.session_id).queryKey,
      }),
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
      canAside={capabilitiesForTarget(read.message.target, liveAgents)?.includes("aside") !== false}
      canBtw={capabilitiesForTarget(read.message.target, liveAgents)?.includes("btw") !== false}
      canSteer={capabilitiesForTarget(read.message.target, liveAgents)?.includes("steer") !== false}
      deliveries={deliveryAttempts(read.message.deliveries, label)}
      header={null}
      isClosed={false}
      onReply={() => onReply(agentReplyTo(agent, read, read.message, asker.label))}
      onRetry={retry.mutate}
      replyDisabled={replyDisabled}
      retrying={retry.isPending}
      targetName={label}
      thread={
        read.replies.length === 0 ? null : (
          <ThreadReplies>
            {read.replies.map((reply) => (
              <AgentExchangeReply
                agent={agent}
                key={reply.id}
                liveAgents={liveAgents}
                onReply={onReply}
                read={read}
                reply={reply}
                replyDisabled={replyDisabled}
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
  liveAgents,
  onReply,
  open,
  replyDisabled,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
  /** Whether the row is open. Its list stays mounted while it is collapsed, and marks nothing read
   *  there: a reply is read once it is on screen. */
  open: boolean;
  /** Holds every Reply while the row's send is in flight, as the picker is held. */
  replyDisabled: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const messages = useQuery(agentMessagesQuery(agent.session_id));
  const agentState = useQuery(userAgentStateQuery());
  const [showOlder, setShowOlder] = useState(false);
  const [showCleared, setShowCleared] = useState(false);
  const clearedBefore = agentState.data?.[agent.session_id]?.cleared_before;
  const all = messages.data ?? [];
  const unread = exchangesAfter(all, clearedBefore);
  const visible = showCleared ? all : unread;
  const [newest, ...older] = visible;
  // Each of the viewer's own exchanges with a reply newer than how far they had read when the row
  // opened is shown, not left behind "Show N older". The watermark stays where it was while the
  // row is open, so marking those replies read does not fold them away from the viewer reading
  // them.
  const unreadAtOpen = useUnreadAtOpen(messages.data, open);
  const olderShown = older.filter((read) => unreadAtOpen?.has(read.message.id) === true);
  const olderFolded = older.filter((read) => !olderShown.includes(read));
  const rendered =
    newest === undefined ? [] : [newest, ...olderShown, ...(showOlder ? olderFolded : [])];
  useMarkRepliesRead(
    agent.session_id,
    !open || messages.isPending || agentState.isPending ? undefined : rendered
  );
  // The cutoff is the newest visible message's own timestamp, not the browser clock: both are
  // compared against `created_at` (the server's clock), so a slow browser clock would otherwise
  // make Clear a silent no-op. This hides exactly what the viewer saw.
  const clear = useMutation({
    mutationFn: (cutoff: string) => api.putAgentState(agent.session_id, { cleared_before: cutoff }),
    onSuccess: (next) => {
      setShowCleared(false);
      storeAgentState(queryClient, agent.session_id, next);
    },
  });
  const label = sessionLabel(agent.session_id, agent.title);
  if (messages.isPending || agentState.isPending) return null;
  if (messages.isError || agentState.isError) {
    return <p className={`mt-3 text-sm ${dangerText}`}>Could not load this conversation.</p>;
  }
  if (all.length === 0) return null;
  const row = (read: MessageRead) => (
    <AgentTargetedMessage
      agent={agent}
      key={read.message.id}
      liveAgents={liveAgents}
      onReply={onReply}
      read={read}
      replyDisabled={replyDisabled}
    />
  );
  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      {newest === undefined ? null : (
        <ol aria-label={`Conversation with ${label}`} className="space-y-2">
          {row(newest)}
          {olderShown.map(row)}
          {olderFolded.length === 0 ? null : (
            <li>
              <DisclosureToggle
                expanded={showOlder}
                label={`Show ${olderFolded.length} older`}
                onToggle={() => setShowOlder((open) => !open)}
              />
            </li>
          )}
          {showOlder ? olderFolded.map(row) : null}
        </ol>
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        {clearedBefore === undefined || unread.length === all.length ? null : (
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

/** The list a row sits in: the open list, or one of the two folds below it. */
type AgentSection = "active" | "inactive" | "quiet";

function AgentRow({
  agent,
  hidden,
  liveAgents,
  needsYou,
  onPin,
  onSelect,
  pinned,
  section,
  selected,
}: {
  agent: Agent;
  /** A row in a closed fold: still mounted, so a pin or a fold's toggle loses nothing it holds. */
  hidden: boolean;
  liveAgents: readonly Agent[];
  needsYou: number;
  onPin: () => void;
  onSelect: (selected: boolean) => void;
  pinned: boolean;
  section: AgentSection;
  selected: boolean;
}): ReactNode {
  const label = sessionLabel(agent.session_id, agent.title);
  const machineAndDir = `${agent.machine_id} · ${agent.dir}`;
  // The inbox query and the agent list are polled separately, so the two counts can briefly
  // disagree in either direction; a negative remainder renders nothing.
  const waitingOnAgent = agent.open_asks - needsYou;
  const unreadReplies =
    useQuery(userAgentStateQuery()).data?.[agent.session_id]?.unread_replies ?? 0;
  const [expanded, setExpanded] = useState(false);
  const [replyTo, setReplyTo] = useState<AgentReply | null>(null);
  // The conversation and the composer mount on the first open and stay mounted, hidden while the
  // row is collapsed: the draft, a send in flight, its refusal, an upload, the unread set and what
  // the reader unfolded each have one owner, which a collapse no more discards than a pin does.
  const [opened, setOpened] = useState(false);
  if (expanded && !opened) setOpened(true);
  const sending = useIsMutating({ mutationKey: agentComposerMutationKey(agent.session_id) }) > 0;
  const detailsId = useId();

  return (
    <article
      className={`rounded-xl border outline-none focus-visible:ring-2 ${card} ${borderDefault} ${focusVisibleRing}`}
      data-agent-row={agent.session_id}
      data-agent-section={section}
      // The issue picker renders only while this row is not answering a message, and a collapsed
      // row's picker is not on screen, so the row itself carries whether `i` can act.
      data-agent-can-pick-issue={replyTo === null ? "" : undefined}
      hidden={hidden}
      tabIndex={-1}
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-1.5">
        <div className="flex min-w-0 flex-auto flex-wrap items-center gap-x-2 md:flex-nowrap">
          <input
            aria-label={`Select ${label} for broadcast`}
            checked={selected}
            className={`size-4 shrink-0 ${checkboxAccent}`}
            data-agent-select=""
            onChange={(event) => onSelect(event.target.checked)}
            type="checkbox"
          />
          <FreshnessDot agent={agent} />
          <h2 className={`max-w-56 shrink-0 text-sm font-semibold ${textPrimaryOnCanvas}`}>
            <button
              aria-controls={detailsId}
              aria-expanded={expanded}
              className={`flex min-h-11 max-w-full items-center gap-1 text-left md:min-h-8 ${textPrimaryOnCanvas}`}
              data-agent-toggle=""
              onClick={() => setExpanded((open) => !open)}
              title={label}
              type="button"
            >
              <TruncatedText className="min-w-0">{label}</TruncatedText>
              <span className={disclosureButtonText}>
                <ChevronIcon expanded={expanded} />
              </span>
            </button>
          </h2>
          <Link
            className={`min-h-11 rounded-lg border px-2 text-xs font-medium whitespace-nowrap md:min-h-7 md:leading-7 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            title={`Watch ${label}'s conversation live`}
            to={`/agents/${encodeURIComponent(agent.session_id)}/live`}
          >
            Open
          </Link>
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
          {unreadReplies === 0 ? null : (
            // Opening the conversation is what reads it, so the badge opens it.
            <button
              aria-label={`${label} replied: ${unreadReplies} unread`}
              className="inline-flex min-h-11 items-center rounded-full md:min-h-8"
              onClick={() => setExpanded(true)}
              title={`Replies from ${label} you have not read`}
              type="button"
            >
              <LabelPill selected>{unreadRepliesLabel(unreadReplies)}</LabelPill>
            </button>
          )}
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
          {/* `contents` so the keymap has a handle on the pin without a box in the flex row. */}
          <span className="contents" data-agent-pin="">
            <PinButton
              label={pinned ? `Unpin ${label}` : `Pin ${label}`}
              onClick={onPin}
              pinned={pinned}
              title={pinned ? "Unpin agent" : "Pin agent"}
            />
          </span>
        </div>
      </div>
      {opened ? (
        <div className={`border-t px-4 pb-4 ${borderDefault}`} hidden={!expanded} id={detailsId}>
          <div
            className={`mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs ${textMutedOnCanvas}`}
          >
            {agent.capabilities.map((capability) => (
              <span key={capability}>{capabilityLabel(capability)}</span>
            ))}
            <span>
              Seen <Timestamp at={new Date(agent.last_seen).toISOString()} />
            </span>
          </div>
          <AgentMessageList
            agent={agent}
            liveAgents={liveAgents}
            onReply={setReplyTo}
            // Open means on screen: an expanded row in a closed fold is as unseen as a collapsed
            // one, so it marks nothing read, and reopening the fold is an open that freezes its
            // unread set afresh.
            open={expanded && !hidden}
            replyDisabled={sending}
          />
          <div data-agent-composer="">
            <AgentMessageComposer
              agent={agent}
              onCancelReply={() => setReplyTo(null)}
              onClose={leaveAgentComposer}
              replyTo={replyTo}
              sending={sending}
            />
          </div>
        </div>
      ) : null}
    </article>
  );
}

/**
 * A fold's toggle: an item in the one list, between the open rows and the fold's own, labelled by
 * `foldLabel`. Absent when the fold is empty; closed on every load, and open only while this page
 * stays mounted. A block item rather than a bare button, so the toggle keeps its own width and the
 * pin's focus hand-off has a handle on it (`data-agent-fold`).
 */
function FoldToggle({
  agents,
  expanded,
  fold,
  label,
  onToggle,
  selected,
}: {
  agents: readonly Agent[];
  expanded: boolean;
  fold: Exclude<AgentSection, "active">;
  label: string;
  onToggle: () => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  if (agents.length === 0) return null;
  return (
    <div data-agent-fold={fold}>
      <DisclosureToggle
        expanded={expanded}
        label={foldLabel(label, agents, selected)}
        onToggle={onToggle}
      />
    </div>
  );
}

/**
 * What narrows the agent list, mirroring the `envoy broadcast` script's own selectors: one
 * machine, one role, and a directory substring. An empty field matches everything.
 */
export interface AgentFilters {
  readonly dir: string;
  readonly machine: string;
  readonly role: string;
}

export const NO_AGENT_FILTERS: AgentFilters = { dir: "", machine: "", role: "" };

export function filterAgents(agents: readonly Agent[], filters: AgentFilters): Agent[] {
  const dir = filters.dir.trim().toLowerCase();
  return agents.filter(
    (agent) =>
      (filters.machine === "" || agent.machine_id === filters.machine) &&
      (filters.role === "" || agent.roles.includes(filters.role)) &&
      (dir === "" || agent.dir.toLowerCase().includes(dir))
  );
}

/** The machines, roles and directories the live sessions actually occupy: a filter can only
 *  offer what is there, so a stale option can never hide every agent. */
function filterOptions(agents: readonly Agent[]): { machines: string[]; roles: string[] } {
  const machines = new Set<string>();
  const roles = new Set<string>();
  for (const agent of agents) {
    if (agent.machine_id !== "") machines.add(agent.machine_id);
    for (const role of agent.roles) roles.add(role);
  }
  return { machines: [...machines].sort(), roles: [...roles].sort() };
}

function AgentFilterBar({
  agents,
  filters,
  onFilters,
}: {
  agents: readonly Agent[];
  filters: AgentFilters;
  onFilters: (filters: AgentFilters) => void;
}): ReactNode {
  const { machines, roles } = filterOptions(agents);
  const field = `min-h-11 rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`;
  return (
    <fieldset className="mb-3 flex flex-wrap items-center gap-2">
      <legend className="sr-only">Agent filters</legend>
      <select
        aria-label="Machine"
        className={field}
        onChange={(event) => onFilters({ ...filters, machine: event.target.value })}
        value={filters.machine}
      >
        <option value="">All machines</option>
        {machines.map((machine) => (
          <option key={machine} value={machine}>
            {machine}
          </option>
        ))}
      </select>
      <select
        aria-label="Role"
        className={field}
        onChange={(event) => onFilters({ ...filters, role: event.target.value })}
        value={filters.role}
      >
        <option value="">All roles</option>
        {roles.map((role) => (
          <option key={role} value={role}>
            {role}
          </option>
        ))}
      </select>
      <input
        aria-label="Directory contains"
        className={field}
        onChange={(event) => onFilters({ ...filters, dir: event.target.value })}
        placeholder="Directory contains"
        type="search"
        value={filters.dir}
      />
    </fieldset>
  );
}

/**
 * The list's select-all checkbox. Its 13 px inset lines it up with the rows' checkboxes (their
 * 1 px border plus 12 px padding). Matching means every row the filters match, folded sections
 * included, and `Clear selection` also empties the selected rows the filters hide.
 */
function SelectionHeader({
  listed,
  matching,
  onClear,
  onToggle,
  selected,
}: {
  listed: readonly Agent[];
  matching: readonly Agent[];
  onClear: () => void;
  onToggle: () => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  const selection = matchingSelection(listed, matching, selected);
  const { state } = selection;
  const checkbox = useRef<HTMLInputElement>(null);
  const countId = useId();
  // `indeterminate` is a DOM property with no attribute, so React cannot render it.
  useLayoutEffect(() => {
    if (checkbox.current !== null) checkbox.current.indeterminate = state === "some";
  }, [state]);
  return (
    <div className="mb-2 flex min-h-11 items-center gap-x-2 px-[13px] md:min-h-8">
      <input
        aria-describedby={countId}
        aria-label="Select all matching agents"
        checked={state === "all"}
        className={`size-4 shrink-0 ${checkboxAccent}`}
        disabled={matching.length === 0}
        onChange={onToggle}
        ref={checkbox}
        type="checkbox"
      />
      <span className={`min-w-0 flex-1 text-xs ${textMutedOnCanvas}`} id={countId}>
        {selectionSummary(selection)}
      </span>
      {selected.size === 0 ? null : (
        <button
          className={`min-h-11 shrink-0 rounded-lg border px-2 text-xs font-medium md:min-h-7 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={onClear}
          type="button"
        >
          Clear selection
        </button>
      )}
    </div>
  );
}

/**
 * The broadcast a selection is waiting to become: one body, one mode, and one message per
 * recipient. Selection is what the human ticked, so a recipient that a later filter hides is
 * still listed here rather than silently dropped; every selected session is named, including
 * the ones this mode leaves out. It follows the list and sticks to the viewport's bottom, so
 * appearing costs no layout above the rows: the checkbox that summoned it stays under the
 * pointer. A long recipient list scrolls inside it rather than growing it past half the screen.
 * Send hands the request to the page's queue (`onSend`) at the press: `composed`, which
 * `composedBroadcast` decides - the restored request word for word while one is kept, else the
 * live plan under the composition's key - with the plan the composer shows for it, so a session
 * that has come back since a refused send is shown as not in the request being re-sent.
 */
function BroadcastComposer({
  agents,
  body,
  composed,
  delivery,
  onBody,
  onDelivery,
  onSend,
  selected,
  onDeselect,
}: {
  agents: readonly Agent[];
  body: string;
  composed: ComposedBroadcast;
  delivery: MessageDeliveryMode;
  onBody: (body: string) => void;
  onDelivery: (delivery: MessageDeliveryMode) => void;
  onDeselect: (sessionID: string) => void;
  onSend: (send: BroadcastSend) => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  const { plan } = composed;
  const { excluded, recipients } = plan;
  // The server counts the `session_ids` it is sent - these recipients - against the shared limit
  // and refuses a send over it; saying so before Send saves the round trip. The notice line is
  // Send's reason when it refuses for the limit or for nobody to reach; otherwise it still bears
  // on the send, so Send is described by it.
  const sendState = broadcastSendState(plan, delivery, body);
  const noticeId = useId();

  // On a narrow or short screen (`narrow-or-short`, styles.css) the composer is a compact grid,
  // so it takes about a third of a phone screen: the heading on one line, the recipients in one
  // sideways-scrolling line, the message sized to its text (`field-sizing`) from two rows up to
  // `max-h-32`, and the mode beside Send. A short screen (a phone in landscape) folds that into
  // two lines - heading beside the recipients, then the message beside the mode and Send - with
  // the message held at two rows. `min-h-16` is important because styles.css's unlayered
  // below-1280 touch floor (`min-height: 44px`) would otherwise override it and shrink the
  // message box on its first character. Elsewhere none of these classes apply.
  return (
    <section
      aria-label="Broadcast"
      className={`max-h-[50vh] overflow-y-auto rounded-xl border p-3 narrow-or-short:sticky narrow-or-short:bottom-0 narrow-or-short:z-10 narrow-or-short:mt-3 narrow-or-short:grid narrow-or-short:grid-cols-[auto_minmax(0,1fr)_auto] narrow-or-short:items-center narrow-or-short:gap-2 short:grid-cols-[auto_minmax(0,1fr)_auto_auto] ${card} ${borderDefault}`}
    >
      <h2
        className={`text-sm font-semibold narrow-or-short:col-span-full narrow-or-short:truncate short:col-span-1 ${textPrimaryOnCanvas}`}
      >
        Broadcast to {recipients.length} of {selected.size} selected
      </h2>
      <ul
        aria-label="Selected agents"
        className="mt-2 flex flex-wrap gap-2 narrow-or-short:col-span-full narrow-or-short:mt-0 narrow-or-short:flex-nowrap narrow-or-short:overflow-x-auto short:col-span-3"
      >
        {[...selected].map((sessionID) => {
          const agent = agents.find((candidate) => candidate.session_id === sessionID);
          const label = sessionLabel(sessionID, agent?.title ?? "");
          const left = excluded.find((item) => item.sessionID === sessionID);
          return (
            <li className="narrow-or-short:shrink-0" key={sessionID}>
              <button
                className={`min-h-8 rounded-full border px-2 py-1 text-xs narrow-or-short:min-h-11 narrow-or-short:whitespace-nowrap ${secondaryButtonBorder} ${left === undefined ? secondaryButtonText : dangerText} ${secondaryButtonHoverBorder}`}
                onClick={() => onDeselect(sessionID)}
                title={`Remove ${label} from this broadcast`}
                type="button"
              >
                {label}
                {left === undefined ? "" : ` · ${left.reason}`} ✕
              </button>
            </li>
          );
        })}
      </ul>
      <div className="mt-2 flex flex-wrap items-center gap-2 narrow-or-short:order-2 narrow-or-short:mt-0">
        <select
          aria-label="Delivery mode"
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
          onChange={(event) => onDelivery(event.target.value as MessageDeliveryMode)}
          value={delivery}
        >
          {DELIVERY_CAPABILITIES.map((mode) => (
            <option key={mode} value={mode}>
              {MODE_LABELS[mode]}
            </option>
          ))}
        </select>
      </div>
      <textarea
        aria-label="Broadcast message"
        className={`mt-2 block w-full rounded-lg border px-3 py-2 text-sm narrow-or-short:order-1 narrow-or-short:col-span-full narrow-or-short:mt-0 narrow-or-short:max-h-32 short:col-span-2 narrow-or-short:min-h-16! narrow-or-short:field-sizing-content short:max-h-16 ${inputClasses(true)}`}
        onChange={(event) => onBody(event.target.value)}
        placeholder="One message, sent to each selected agent"
        rows={3}
        value={body}
      />
      {sendState.notice === null ? null : (
        <p className={composerLine} id={noticeId}>
          {sendState.notice}
        </p>
      )}
      <RefusableButton
        className="mt-2 narrow-or-short:order-2 narrow-or-short:col-start-3 narrow-or-short:mt-0 narrow-or-short:justify-self-end short:col-start-4"
        describedBy={sendState.notice === null || sendState.refusalOnNotice ? undefined : noticeId}
        onPress={() => onSend({ input: composed.input, selected: [...selected] })}
        refusal={sendState.refusal}
        refusalShownBy={sendState.refusalOnNotice ? noticeId : undefined}
      >
        {sendState.label}
      </RefusableButton>
    </section>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, error, isError, isPending } = useAgents(true, true);
  const inbox = useQuery(inboxQuery());
  const readPinned = useCallback((stored: string | null): readonly string[] => {
    try {
      const parsed: unknown = stored === null ? [] : JSON.parse(stored);
      return Array.isArray(parsed)
        ? parsed.filter((value): value is string => typeof value === "string")
        : [];
    } catch {
      return [];
    }
  }, []);
  const [pinned, setPinned] = useUserPreference("agents.pinned", readPinned, (next) =>
    JSON.stringify(next)
  );
  const needsYouBySession = useMemo(() => {
    const counts = new Map<string, number>();
    for (const ask of waitingOnYou(inbox.data ?? [])) {
      if (ask.author.kind !== "session") continue;
      counts.set(ask.author.id, (counts.get(ask.author.id) ?? 0) + 1);
    }
    return counts;
  }, [inbox.data]);
  const [filters, setFilters] = useState<AgentFilters>(NO_AGENT_FILTERS);
  // Selection is what the human ticked, not what the filters currently show: narrowing the
  // list after ticking a row must not quietly drop that row from the send. Every selected
  // session is named in the composer, so nothing is hidden either way.
  // The draft (message and mode), the selection and a restored request are page state, in one
  // owner (`useBroadcastComposition`). Restore draft, on a send the server refused, puts them
  // back; any message or selection here was started after Send cleared both, so Restore draft
  // refuses rather than replace it - the same test the queue uses to hold off opening a broadcast
  // while another is begun.
  const composition = useBroadcastComposition();
  const { delivery, draft, selected, setSelected } = composition;
  const composing = draft.trim() !== "" || selected.size > 0;
  const queue = useBroadcastQueue(composing);
  const restoreRefusal = composing
    ? "Restore draft would replace the broadcast you have started. Send it, or clear its message and selection, first."
    : null;
  const showComposer = selected.size > 0 && agents.length > 0;
  // Not memoised: the split is a function of the clock, like the freshness dot beside each row,
  // and is recomputed on every render of this page.
  const { active, quiet, inactive } = partitionAgents(
    filterAgents(agents, filters),
    pinned,
    needsYouBySession,
    Date.now()
  );
  // The rows the filters match, in the order the page shows them - the open list, then each fold.
  // Select-all ticks this set in order and the composer names the selection in tick order, so a
  // set ordered any other way (the registry's own, say) would name the recipients in an order the
  // reader never sees, and send them in it.
  const matching = [...active, ...quiet, ...inactive];
  /** The row a pin was made from while it held focus, until the render that moves it. A pin can
   *  move a row between the open list and a fold: React moves its node, which can drop focus to
   *  the document, or the row lands in a closed fold, hidden. Focus follows the row to where it
   *  lands - or, when that is a closed fold, to the fold's toggle, as `IssueBoard`'s
   *  `focusAfterMove` lands on a collapsed rail - so the next `j`/`k` go on from the reader's
   *  place rather than from the top. */
  const focusAfterPin = useRef<string | null>(null);
  const togglePin = (sessionID: string) => {
    if (
      closestMatching(document.activeElement, AGENT_ROW_SELECTOR)?.dataset.agentRow === sessionID
    ) {
      focusAfterPin.current = sessionID;
    }
    const next = pinned.includes(sessionID)
      ? pinned.filter((candidate) => candidate !== sessionID)
      : [...pinned, sessionID];
    setPinned(next);
  };
  const select = (sessionID: string, next: boolean) => {
    setSelected((current) => {
      const updated = new Set(current);
      if (next) updated.add(sessionID);
      else updated.delete(sessionID);
      return updated;
    });
  };
  // Each fold is closed on every load and open only while this page stays mounted.
  const [quietOpen, setQuietOpen] = useState(false);
  const [inactiveOpen, setInactiveOpen] = useState(false);
  const listRef = useRef<HTMLElement>(null);
  useAgentsKeymap(listRef);
  // A layout effect, so the frame the move paints already has focus where the row went.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `pinned` is the trigger, not a read
  useLayoutEffect(() => {
    const sessionID = focusAfterPin.current;
    if (sessionID === null) return;
    focusAfterPin.current = null;
    const list = listRef.current;
    const row = [...(list?.querySelectorAll<HTMLElement>(AGENT_ROW_SELECTOR) ?? [])].find(
      (node) => node.dataset.agentRow === sessionID
    );
    if (row === undefined) return;
    const target = row.hidden
      ? list?.querySelector<HTMLElement>(`[data-agent-fold="${row.dataset.agentSection}"] button`)
      : row;
    // Only the focus the move took is the page's to give back: still on the row, now hidden, or
    // dropped to the document.
    const focused = document.activeElement;
    if (focused !== row && !focusOnDocument()) return;
    if (focused !== target) target?.focus();
  }, [pinned]);
  const rowIn = (section: AgentSection, hidden: boolean) => (agent: Agent) => (
    <AgentRow
      agent={agent}
      hidden={hidden}
      key={agent.session_id}
      liveAgents={agents}
      needsYou={needsYouBySession.get(agent.session_id) ?? 0}
      onPin={() => togglePin(agent.session_id)}
      onSelect={(next) => select(agent.session_id, next)}
      pinned={pinned.includes(agent.session_id)}
      section={section}
      selected={selected.has(agent.session_id)}
    />
  );
  // One list, keyed by session, with each fold's toggle as an item between the rows - the Inbox's
  // pattern (`Inbox.tsx`). A row that moves between the open list and a fold moves within the one
  // parent, so React moves its node instead of remounting it, and a closed fold's rows stay
  // mounted, hidden: nothing a row holds is ever handed from one instance to another. One flat
  // array, not three: each array in the children is a slot of its own, and a row changing slots
  // would remount.
  const rows = [
    ...active.map(rowIn("active", false)),
    <FoldToggle
      agents={quiet}
      expanded={quietOpen}
      fold="quiet"
      key="fold:quiet"
      label="No Dispatch activity"
      onToggle={() => setQuietOpen((open) => !open)}
      selected={selected}
    />,
    ...quiet.map(rowIn("quiet", !quietOpen)),
    <FoldToggle
      agents={inactive}
      expanded={inactiveOpen}
      fold="inactive"
      key="fold:inactive"
      label="Inactive"
      onToggle={() => setInactiveOpen((open) => !open)}
      selected={selected}
    />,
    ...inactive.map(rowIn("inactive", !inactiveOpen)),
  ];

  if (isPending) return <LoadingSkeleton label="Loading agents" />;
  if (isError) return <p className={dangerText}>Could not load agents: {error}</p>;

  return (
    <section aria-label="Agents" ref={listRef}>
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Agents
        </h1>
        <p className={`mt-1 flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
          <span>Live Envoy sessions and their Dispatch activity.</span>
          <Link className={`${linkText} ${linkHoverText}`} to="/agents/broadcasts">
            Broadcasts
          </Link>
        </p>
      </header>
      {agents.length === 0 ? (
        <EmptyState label="Agents empty state" message="No agents are connected." />
      ) : (
        <>
          <AgentFilterBar agents={agents} filters={filters} onFilters={setFilters} />
          <SelectionHeader
            listed={agents}
            matching={matching}
            onClear={() => setSelected(() => new Set())}
            onToggle={() => setSelected((current) => toggleMatching(matching, current))}
            selected={selected}
          />
          {matching.length === 0 ? (
            <EmptyState
              label="Agents filtered empty state"
              message="No agent matches these filters."
            />
          ) : (
            <div className="flex flex-col gap-3">{rows}</div>
          )}
        </>
      )}
      {/* The sends sit outside the composer and outside the list, so a send's row outlives both:
          the composer goes at every press, and the list empties when no agent is connected. On a
          narrow or short screen only the composer sticks (the wrapper is `contents`), and the
          strip stays at the end of the list: the composer alone fills the screen's budget. */}
      {queue.rows.length === 0 && !showComposer ? null : (
        <div className="sticky bottom-0 z-10 mt-3 space-y-2 narrow-or-short:contents">
          {queue.rows.length === 0 ? null : (
            <BroadcastSends
              className="narrow-or-short:mt-3"
              onRestore={(row) => {
                queue.dismiss(row);
                composition.restore(row.send);
              }}
              onRetry={queue.retry}
              restoreRefusal={restoreRefusal}
              rows={queue.rows}
            />
          )}
          {showComposer ? (
            <BroadcastComposer
              agents={agents}
              body={draft}
              composed={composedBroadcast(composition, agents)}
              delivery={delivery}
              onBody={composition.setDraft}
              onDelivery={composition.setDelivery}
              onDeselect={(sessionID) => select(sessionID, false)}
              onSend={(send) => {
                queue.enqueue(send);
                composition.sent();
              }}
              selected={selected}
            />
          ) : null}
        </div>
      )}
      <EndedAgentsWithReplies live={agents} />
    </section>
  );
}
