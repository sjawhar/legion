import { DELIVERY_CAPABILITIES, MAX_BROADCAST_RECIPIENTS } from "@legion/contracts";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";

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
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { resolveAuthor } from "../conversation/authors";
import type { CarriedDraft } from "../conversation/MentionComposer";
import { MentionComposer, type ReplyTarget } from "../conversation/MentionComposer";
import { firstLine, replyQuoteText } from "../conversation/ReplyQuote";
import { ReplyTurn, ThreadReplies } from "../conversation/ReplyTurn";
import { capabilitiesForTarget, TargetedMessageCard } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { sessionLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildInboxPath, buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { closestMatching } from "../shell/roving";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useUserPreference } from "../shell/userPreference";
import { deliveryAttempts } from "./attempts";
import { EndedAgentsWithReplies } from "./EndedAgentsWithReplies";
import {
  AGENT_ROW_SELECTOR,
  ISSUE_PICKER_SELECTOR,
  leaveAgentComposer,
  useAgentsKeymap,
} from "./keyboard";
import { foldLabel, matchingSelection, selectionSummary, toggleMatching } from "./selection";
import { storeAgentState, unreadRepliesLabel, useMarkRepliesRead, useUnreadAtOpen } from "./unread";

const INACTIVE_AFTER_MS = 10 * 60_000;

/** The composer's one notice slot: the recipient limit, a refused send or the exclusions, one at
 *  a time. In the compact grid it is one line that scrolls sideways like the chips: between the
 *  mode and Send on a narrow screen, adding no height, and the third and last row on a short one. */
const composerLine = `mt-2 text-sm narrow-or-short:order-2 narrow-or-short:col-start-2 narrow-or-short:mt-0 narrow-or-short:min-w-0 narrow-or-short:overflow-x-auto narrow-or-short:text-xs narrow-or-short:whitespace-nowrap short:order-3 short:col-span-4 short:col-start-1 ${dangerText}`;

/** The grey-dot rule: a session unseen for ten minutes folds under `Inactive (N)`. */
function isInactive(agent: Agent, now: number): boolean {
  return now - agent.last_seen >= INACTIVE_AFTER_MS;
}

/** Whether focus has fallen to the document, where a control that is disabled or leaves the page
 *  drops it. That is the only focus this page takes back on the reader's behalf: anywhere else,
 *  the reader put it there. */
function focusOnDocument(): boolean {
  const focused = document.activeElement;
  return focused === null || focused === document.body;
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
  titles,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  reply: Message;
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
                canAside:
                  capabilitiesForTarget(read.message.target, liveAgents)?.includes("aside") !==
                  false,
                canBtw:
                  capabilitiesForTarget(read.message.target, liveAgents)?.includes("btw") !== false,
                canSteer:
                  capabilitiesForTarget(read.message.target, liveAgents)?.includes("steer") !==
                  false,
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
  liveAgents,
  onReply,
  read,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
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
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  onReply: (reply: AgentReply) => void;
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
  const unreadAtOpen = useUnreadAtOpen(messages.data);
  const olderShown = older.filter((read) => unreadAtOpen?.has(read.message.id) === true);
  const olderFolded = older.filter((read) => !olderShown.includes(read));
  const rendered =
    newest === undefined ? [] : [newest, ...olderShown, ...(showOlder ? olderFolded : [])];
  useMarkRepliesRead(
    agent.session_id,
    messages.isPending || agentState.isPending ? undefined : rendered
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

function AgentMessageComposer({
  agent,
  onCancelReply,
  onClose,
  replyTo,
}: {
  agent: Agent;
  onCancelReply: () => void;
  /** One level out of the composer: the row it belongs to takes focus. The composer calls it on
   *  Escape from an untouched draft and on Discard - and also right after a successful send,
   *  which is NOT one level out; that case is filtered below. */
  onClose: () => void;
  replyTo: AgentReply | null;
}): ReactNode {
  const queryClient = useQueryClient();
  const [issueKey, setIssueKey] = useState("");
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  // `MentionComposer` calls `onSent` and then `onClose` on a successful send (its save's
  // `onSuccess`), and a reader who has just sent a message is still writing to this agent: moving
  // focus to the row would turn their next letters into `x` / `i` / `Shift+P` shortcuts. The flag
  // is set on the way past `onSent` and consumed by the `onClose` that follows it.
  const sentJustNow = useRef(false);
  const box = useRef<HTMLDivElement>(null);
  // `MentionComposer` disables its textarea while a send is in flight (`disabled={save.isPending}`),
  // and a disabled field hands focus back to the document. The reader is still writing to this
  // agent, so focus returns the moment React re-enables the field - watched, rather than guessed
  // at with a frame or a timer, because the write's latency is the server's.
  const refocusWatcher = useRef<MutationObserver | null>(null);
  useEffect(() => () => refocusWatcher.current?.disconnect(), []);
  /** Only the focus the disable took is the composer's to give back: through the whole round trip
   *  it sits on the document, so a reader who has clicked something else in the meantime keeps
   *  where they went - otherwise the next keys, `Ctrl+Enter` included, would land in the composer
   *  they have already sent from, addressed to another agent. */
  const refocusComposer = () => {
    const field = box.current?.querySelector("textarea");
    if (field === null || field === undefined) return;
    const takeBack = () => {
      if (focusOnDocument()) field.focus();
    };
    refocusWatcher.current?.disconnect();
    if (!field.disabled) {
      takeBack();
      return;
    }
    const watcher = new MutationObserver(() => {
      if (field.disabled) return;
      watcher.disconnect();
      takeBack();
    });
    watcher.observe(field, { attributeFilter: ["disabled"] });
    refocusWatcher.current = watcher;
  };
  /** The draft as the composer last held it - body and accepted mentions together - so a pick
   *  that remounts it to change the message's owner hands the reader's work to the new
   *  instance rather than dropping it. Opaque here: it is handed back as it was given. */
  const carried = useRef<CarriedDraft | undefined>(undefined);
  const keepCarry = useCallback((draft: CarriedDraft) => {
    carried.current = draft;
  }, []);
  /** Whether the composer's send is in flight (`onSending`). The message is addressed by then, so
   *  neither the picker nor the channel the composer is mounted for may change until it lands. */
  const [sending, setSending] = useState(false);
  /** What the picker's selection reads while it is open, which is the reader's until they commit
   *  it: the select's own keys move it, `Enter`, or a pick made with the pointer or in the native
   *  popup, takes it, and leaving the select without committing puts it back on `issueKey`. */
  const [pendingIssue, setPendingIssue] = useState(issueKey);
  /** Whether the change arriving now is a key on the select stepping its selection, which only
   *  moves it: `Enter` is the pick. The test is the task the change arrives in, not the key.
   *  Chromium, Firefox and WebKit all step a closed select from the key event's own default
   *  action - the arrows, `Home`/`End` and the page keys from `keydown`, type-ahead from
   *  `keypress` - and dispatch `change` in that same task. A key that opens the native popup
   *  instead (the arrows on macOS; `Alt+ArrowDown` in Chromium and Firefox on Linux) steps
   *  nothing, and the pick then made in the popup arrives in a later task, as a pointer's does:
   *  that is a pick made, and it commits at once. So each key on the select marks the flag and
   *  the next task clears it. */
  const movedByKeyboard = useRef(false);
  const markKeyStep = () => {
    movedByKeyboard.current = true;
    setTimeout(() => {
      movedByKeyboard.current = false;
    }, 0);
  };
  /** Bumped by every commit, the issue changed or not, since every commit unmounts the select the
   *  reader is in. The hand-off keys on it rather than on `issueKey`, which re-confirming the
   *  issue already held leaves alone. */
  const [commits, setCommits] = useState(0);
  const commitIssue = (value: string) => {
    setIssueKey(value);
    setIssuePickerOpen(false);
    setCommits((count) => count + 1);
  };
  // A layout effect, so the frame the commit paints already has the field focused rather than
  // the document: the reader's next keystroke is the message, whichever hand made the pick. When
  // the pick changes the channel the composer remounts in that same commit, and the field this
  // finds is the new instance's.
  useLayoutEffect(() => {
    if (commits === 0) return;
    box.current?.querySelector("textarea")?.focus();
  }, [commits]);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: ["agents", "issue-picker"],
  });
  /** The committed issue, once the open list has come back without it: closed since it was
   *  picked. The select and the toggle keep naming it, marked, until the reader picks another -
   *  the issue header's Status select keeps a closed issue's own status among its options the same
   *  way - so what the picker shows, what the toggle says and where the message goes stay one
   *  issue; a send to it is the server's to refuse (409 `ISSUE_CLOSED`). */
  const committedClosed =
    issueKey !== "" &&
    issues.data !== undefined &&
    !issues.data.some((issue) => issue.key === issueKey);
  const committedLabel = committedClosed ? `${issueKey} (closed)` : issueKey;
  // The picker exists to be used, so opening it hands over the control inside it - the same
  // move `MultiSelect` makes with its search box. It is what `i` needs (a key that opened
  // something no keystroke could then reach would be a dead end) and what a pointer wants too,
  // and it waits for the list rather than a frame, since the select renders only once the read
  // lands.
  //
  // That wait is the whole latency of `GET /issues`, and a reader who has roved on in the
  // meantime keeps where they went - `takeBack`'s rule above, widened to the row this composer
  // belongs to: focus is the picker's to take only while it is still where the open left it.
  // Once per open, so a refetch behind the reader never pulls them back either.
  const issueSelect = useRef<HTMLSelectElement>(null);
  const pickerTookFocus = useRef(false);
  useEffect(() => {
    if (!issuePickerOpen) return;
    setPendingIssue(issueKey);
  }, [issueKey, issuePickerOpen]);
  useEffect(() => {
    if (!issuePickerOpen) {
      pickerTookFocus.current = false;
      return;
    }
    // The list is the dependency that matters: the select renders only once it lands.
    if (issues.data === undefined || pickerTookFocus.current || issueSelect.current === null) {
      return;
    }
    // Where the open can have left focus, named: the row `i` was pressed on, the toggle a
    // pointer clicked, or nothing at all. A reader who has gone on - to another row, or into
    // this composer's own field - keeps where they went.
    const active = document.activeElement;
    const openedOn =
      active === null ||
      active === document.body ||
      active === box.current?.closest(AGENT_ROW_SELECTOR) ||
      (active instanceof Element && active.matches(ISSUE_PICKER_SELECTOR));
    if (!openedOn) return;
    pickerTookFocus.current = true;
    issueSelect.current.focus();
  }, [issuePickerOpen, issues.data]);
  // Replies retain their parent owner: issue-attached legacy messages stay on that issue's
  // message route, while issue-less roots keep the S3-deferred direct session channel.
  const replyIssueKey = replyTo?.issueKey;
  const useDirectChannel =
    replyIssueKey === null || (replyIssueKey === undefined && issueKey === "");
  const composerOwner = useDirectChannel
    ? { kind: "session" as const, sessionId: agent.session_id }
    : { issueKey: replyIssueKey ?? issueKey, kind: "issue" as const };
  const channel = useDirectChannel ? "session" : "issue";
  /** The channel the mounted composer was built for. It follows `channel`, except while a send is
   *  in flight: the instance holding that body is the one the server's answer reaches, so a
   *  channel change made meanwhile - a reply started or cancelled, since the picker is disabled -
   *  remounts only once the answer is in, when a success has already emptied the draft `carried`
   *  hands on. Remounted mid-send, the new instance would start from the body in the air, enabled,
   *  where the old one's `clearDraft` could not reach it: one Ctrl+Enter from sending it twice. */
  const [composerChannel, setComposerChannel] = useState(channel);
  if (!sending && composerChannel !== channel) setComposerChannel(channel);
  // A remount takes the focused field out of the page with the instance it belonged to - after a
  // send whose channel change waited for it, say - while the reader is still writing to this
  // agent. As in `refocusComposer`, only focus the remount dropped on the document is taken back.
  const mountedChannel = useRef(composerChannel);
  useLayoutEffect(() => {
    if (mountedChannel.current === composerChannel) return;
    mountedChannel.current = composerChannel;
    if (focusOnDocument()) box.current?.querySelector("textarea")?.focus();
  }, [composerChannel]);

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`} ref={box}>
      {replyTo === null ? (
        <button
          aria-expanded={issuePickerOpen}
          aria-label="Choose issue"
          className={`min-h-11 rounded-lg border px-3 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          data-agent-issue-picker=""
          disabled={sending}
          onClick={() => setIssuePickerOpen((open) => !open)}
          type="button"
        >
          Issue: {issueKey === "" ? "No issue" : committedLabel}
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
                className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(true)}`}
                disabled={sending}
                onBlur={() => {
                  // A step is not a pick until `Enter`, so leaving the select any other way -
                  // Tab, Shift+Tab, a click elsewhere - drops it, as Escape does: the open select
                  // never shows an issue the message is not addressed to.
                  setPendingIssue(issueKey);
                }}
                onChange={(event) => {
                  setPendingIssue(event.target.value);
                  // A step from the select's own keys only moves the selection, so a keyboard
                  // reader can pass the first option to reach the second; `Enter` below is the
                  // pick. Any other change - a pointer's, or one made in the native popup - is a
                  // pick already made.
                  if (movedByKeyboard.current) return;
                  commitIssue(event.target.value);
                }}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    // The commit is this key's, and it stops here: left to bubble it would land
                    // in the message the pick just addressed, as a newline at its top.
                    event.preventDefault();
                    commitIssue(event.currentTarget.value);
                    return;
                  }
                  markKeyStep();
                }}
                onKeyPress={markKeyStep}
                ref={issueSelect}
                value={pendingIssue}
              >
                <option value="">No issue</option>
                {committedClosed ? (
                  <option disabled value={issueKey}>
                    {committedLabel}
                  </option>
                ) : null}
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
        carried={carried.current}
        onCarry={keepCarry}
        // The channel decides which mention the message needs - an issue comment reaches this
        // agent by mentioning it, a direct message does not - so the composer is remounted when
        // the channel changes, and only then; one issue to another keeps the same instance. The
        // pick carries the reader's draft across that remount (`carried`), which waits for a send
        // in flight to land (`composerChannel`).
        key={composerChannel}
        onCancelReply={onCancelReply}
        onClose={() => {
          if (sentJustNow.current) {
            sentJustNow.current = false;
            return;
          }
          onClose();
        }}
        onSending={setSending}
        onSent={() => {
          sentJustNow.current = true;
          onCancelReply();
          void queryClient.invalidateQueries({
            queryKey: agentMessagesQuery(agent.session_id).queryKey,
          });
          // Focus went to the document when the field disabled itself; take it back.
          refocusComposer();
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
  liveAgents,
  needsYou,
  onPin,
  onSelect,
  pinned,
  selected,
}: {
  agent: Agent;
  liveAgents: readonly Agent[];
  needsYou: number;
  onPin: () => void;
  onSelect: (selected: boolean) => void;
  pinned: boolean;
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
  const detailsId = useId();

  return (
    <article
      className={`rounded-xl border outline-none focus-visible:ring-2 ${card} ${borderDefault} ${focusVisibleRing}`}
      data-agent-row={agent.session_id}
      // The issue picker renders only while this row is not answering a message, and a collapsed
      // row has no picker in the DOM at all, so the row itself carries whether `i` can act.
      data-agent-can-pick-issue={replyTo === null ? "" : undefined}
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
          <AgentMessageList agent={agent} liveAgents={liveAgents} onReply={setReplyTo} />
          <div data-agent-composer="">
            <AgentMessageComposer
              agent={agent}
              onCancelReply={() => setReplyTo(null)}
              onClose={leaveAgentComposer}
              replyTo={replyTo}
            />
          </div>
        </div>
      ) : null}
    </article>
  );
}

/** The two folds' labels, which also name them to the pin's focus hand-off (`data-agent-fold`). */
const QUIET_FOLD = "No Dispatch activity";
const INACTIVE_FOLD = "Inactive";

/** A collapsed disclosure over rows the page keeps out of the way, labelled by `foldLabel`;
 * absent when empty, closed on every load, and open only while this page stays mounted. */
function AgentFold({
  agents,
  label,
  liveAgents,
  needsYouBySession,
  onPin,
  onSelect,
  selected,
}: {
  agents: readonly Agent[];
  label: string;
  liveAgents: readonly Agent[];
  needsYouBySession: NeedsYouBySession;
  onPin: (sessionID: string) => void;
  onSelect: (sessionID: string, selected: boolean) => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  const [expanded, setExpanded] = useState(false);
  if (agents.length === 0) return null;
  return (
    // A block wrapper, not a fragment: the global `button { display: inline-flex }` would
    // otherwise let two collapsed toggles share one line below the desktop breakpoint.
    <div className="space-y-3" data-agent-fold={label}>
      <DisclosureToggle
        expanded={expanded}
        label={foldLabel(label, agents, selected)}
        onToggle={() => setExpanded((open) => !open)}
      />
      {expanded ? (
        <section aria-label={label} className="space-y-3">
          {agents.map((agent) => (
            <AgentRow
              agent={agent}
              key={agent.session_id}
              liveAgents={liveAgents}
              needsYou={needsYouBySession.get(agent.session_id) ?? 0}
              onPin={() => onPin(agent.session_id)}
              onSelect={(next) => onSelect(agent.session_id, next)}
              pinned={false}
              selected={selected.has(agent.session_id)}
            />
          ))}
        </section>
      ) : null}
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

/** A session a broadcast would leave out, worded the way the server reports it, so the
 *  composer and the create response say the same thing. */
interface BroadcastExclusionPlan {
  readonly reason: string;
  readonly sessionID: string;
  readonly title: string;
}

/**
 * What sending the current selection would do: the sessions it reaches, and the selected
 * sessions it leaves out. A session that does not advertise the chosen mode is excluded
 * rather than switched to another one - the mode is part of what the sender said - and a
 * selection kept across a session going away excludes it too, which is exactly the judgment
 * the server repeats against its own registry read when the send arrives.
 */
export function broadcastPlan(
  selected: ReadonlySet<string>,
  agents: readonly Agent[],
  delivery: MessageDeliveryMode
): { excluded: BroadcastExclusionPlan[]; recipients: Agent[] } {
  const live = new Map(agents.map((agent) => [agent.session_id, agent]));
  const excluded: BroadcastExclusionPlan[] = [];
  const recipients: Agent[] = [];
  for (const sessionID of selected) {
    const agent = live.get(sessionID);
    if (agent === undefined) {
      excluded.push({ reason: "no live session", sessionID, title: "" });
      continue;
    }
    if (!agent.capabilities.includes(delivery)) {
      excluded.push({ reason: `does not advertise ${delivery}`, sessionID, title: agent.title });
      continue;
    }
    recipients.push(agent);
  }
  return { excluded, recipients };
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
 */
function BroadcastComposer({
  agents,
  body,
  delivery,
  onBody,
  onDelivery,
  onSent,
  selected,
  onDeselect,
}: {
  agents: readonly Agent[];
  body: string;
  delivery: MessageDeliveryMode;
  onBody: (body: string) => void;
  onDelivery: (delivery: MessageDeliveryMode) => void;
  onDeselect: (sessionID: string) => void;
  onSent: () => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { excluded, recipients } = broadcastPlan(selected, agents, delivery);
  // The server counts the `session_ids` it is sent - these recipients - against the shared limit
  // and refuses a send over it; saying so before Send saves the round trip. The same predicate
  // gives the limit the notice slot.
  const overLimit = recipients.length > MAX_BROADCAST_RECIPIENTS;
  const send = useMutation({
    mutationFn: () =>
      api.createBroadcast({
        body,
        delivery,
        session_ids: recipients.map((agent) => agent.session_id),
      }),
    onSuccess: (created) => {
      onSent();
      for (const recipient of created.recipients) {
        void queryClient.invalidateQueries({
          queryKey: agentMessagesQuery(recipient.session_id).queryKey,
        });
      }
      void queryClient.invalidateQueries({ queryKey: ["broadcast"] });
      // The exclusions travel with the navigation: they are a fact about this send, not about
      // the broadcast, so the server stores none and this is the only place they can be shown.
      void navigate(`/agents/broadcasts/${created.id}`, { state: { excluded: created.excluded } });
    },
  });

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
      className={`sticky bottom-0 z-10 mt-3 max-h-[50vh] overflow-y-auto rounded-xl border p-3 narrow-or-short:grid narrow-or-short:grid-cols-[auto_minmax(0,1fr)_auto] narrow-or-short:items-center narrow-or-short:gap-2 short:grid-cols-[auto_minmax(0,1fr)_auto_auto] ${card} ${borderDefault}`}
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
              {mode}
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
      {/* One notice at a time, in priority order: the limit, then a refused send, then the
          exclusions. A higher notice hiding the Excluded line hides no name: every excluded
          session's chip still carries its reason. */}
      {overLimit ? (
        <p className={composerLine}>
          At most {MAX_BROADCAST_RECIPIENTS} recipients per broadcast; this one would reach{" "}
          {recipients.length}.
        </p>
      ) : send.isError ? (
        <p className={composerLine}>
          Could not send: {send.error instanceof Error ? send.error.message : "network error"}
        </p>
      ) : excluded.length === 0 ? null : (
        <p className={composerLine}>
          Excluded:{" "}
          {excluded
            .map((item) => `${sessionLabel(item.sessionID, item.title)} (${item.reason})`)
            .join(", ")}
          . Nothing is sent to them, and no other mode is substituted.
        </p>
      )}
      <button
        className={`mt-2 rounded-lg px-3 py-2 text-sm font-semibold narrow-or-short:order-2 narrow-or-short:col-start-3 narrow-or-short:mt-0 narrow-or-short:justify-self-end short:col-start-4 ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
        disabled={recipients.length === 0 || overLimit || body.trim() === "" || send.isPending}
        onClick={() => send.mutate()}
        type="button"
      >
        {send.isPending ? "Sending…" : `Send to ${recipients.length}`}
      </button>
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
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set());
  // The draft is page state too: the composer unmounts whenever the selection empties, and
  // clearing a selection to pick again must not throw away a typed message or its mode. A
  // successful send clears it.
  const [draft, setDraft] = useState("");
  const [delivery, setDelivery] = useState<MessageDeliveryMode>("btw");
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
   *  move a row between the open list and a fold, which remounts it, and focus leaves the page
   *  with the old node; it follows the row to where it lands instead - or, when that is a closed
   *  fold, to the fold's toggle, as `IssueBoard`'s `focusAfterMove` lands on a collapsed rail - so
   *  the next `j`/`k` go on from the reader's place rather than from the top. */
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
  const listRef = useRef<HTMLElement>(null);
  useAgentsKeymap(listRef);
  // A layout effect, so the frame the move paints already has focus where the row went.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `pinned` is the trigger, not a read; `quiet` is read from the render that moved the row
  useLayoutEffect(() => {
    const sessionID = focusAfterPin.current;
    if (sessionID === null) return;
    focusAfterPin.current = null;
    // Only focus the move dropped on the document is the page's to give back.
    if (!focusOnDocument()) return;
    const list = listRef.current;
    const row = [...(list?.querySelectorAll<HTMLElement>(AGENT_ROW_SELECTOR) ?? [])].find(
      (node) => node.dataset.agentRow === sessionID
    );
    const fold = quiet.some((agent) => agent.session_id === sessionID) ? QUIET_FOLD : INACTIVE_FOLD;
    (row ?? list?.querySelector<HTMLElement>(`[data-agent-fold="${fold}"] > button`))?.focus();
  }, [pinned]);

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
            onClear={() => setSelected(new Set())}
            onToggle={() => setSelected((current) => toggleMatching(matching, current))}
            selected={selected}
          />
          {matching.length === 0 ? (
            <EmptyState
              label="Agents filtered empty state"
              message="No agent matches these filters."
            />
          ) : (
            <div className="space-y-3">
              {active.map((agent) => (
                <AgentRow
                  agent={agent}
                  key={agent.session_id}
                  liveAgents={agents}
                  needsYou={needsYouBySession.get(agent.session_id) ?? 0}
                  onPin={() => togglePin(agent.session_id)}
                  onSelect={(next) => select(agent.session_id, next)}
                  pinned={pinned.includes(agent.session_id)}
                  selected={selected.has(agent.session_id)}
                />
              ))}
              <AgentFold
                agents={quiet}
                label={QUIET_FOLD}
                liveAgents={agents}
                needsYouBySession={needsYouBySession}
                onPin={togglePin}
                onSelect={select}
                selected={selected}
              />
              <AgentFold
                agents={inactive}
                label={INACTIVE_FOLD}
                liveAgents={agents}
                needsYouBySession={needsYouBySession}
                onPin={togglePin}
                onSelect={select}
                selected={selected}
              />
            </div>
          )}
          {selected.size === 0 ? null : (
            <BroadcastComposer
              agents={agents}
              body={draft}
              delivery={delivery}
              onBody={setDraft}
              onDelivery={setDelivery}
              onDeselect={(sessionID) => select(sessionID, false)}
              onSent={() => {
                setSelected(new Set());
                setDraft("");
              }}
              selected={selected}
            />
          )}
        </>
      )}
      <EndedAgentsWithReplies live={agents} />
    </section>
  );
}
