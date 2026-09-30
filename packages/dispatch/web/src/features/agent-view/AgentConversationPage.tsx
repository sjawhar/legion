import { DELIVERY_CAPABILITIES, type MessageDeliveryMode } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { api } from "../../api/client";
import { agentMessagesQuery, whoAmIQuery } from "../../api/queries";
import {
  connectionDotConnecting,
  connectionDotFailed,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  liveDotBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useMarkRepliesRead } from "../agents/unread";
import { useAgents } from "../conversation/useAgents";
import { sessionLabel } from "../refs/actor";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentRuntimeThread } from "./AgentRuntimeThread";
import { type AgentStreamStatus, useAgentStream } from "./useAgentStream";

/** What the live dot says about the relay, and how it reads to a screen reader. */
const STATUS: Record<AgentStreamStatus, { dot: string; label: string }> = {
  connecting: { dot: connectionDotConnecting, label: "Connecting to the session" },
  live: { dot: liveDotBg, label: "Live" },
  reconnecting: { dot: connectionDotConnecting, label: "Reconnecting" },
  unavailable: { dot: connectionDotFailed, label: "This session's conversation is unavailable" },
};

/** Each mode as the composer names it: Send is Enter at the session's terminal. */
const MODE_LABELS: Record<MessageDeliveryMode, string> = {
  aside: "Aside",
  btw: "BTW",
  steer: "Send",
};

/** Where each mode stands in the composer. Send comes first wherever the session takes it, since
 *  that is Enter at its terminal, and Aside next, so a session that takes only asides (a Claude
 *  Code session, which refuses a steer) opens on Aside. A mode the contracts add must be placed
 *  here before it compiles. */
const MODE_ORDER: Record<MessageDeliveryMode, number> = { aside: 1, btw: 2, steer: 0 };

/** The modes a human can talk to a session in, narrowed to the ones it advertises, in
 *  `MODE_ORDER`; the first is what the composer sends until the human picks. */
function deliveryModes(capabilities: readonly string[]): MessageDeliveryMode[] {
  const advertised = DELIVERY_CAPABILITIES.filter((mode) => capabilities.includes(mode)).sort(
    (left, right) => MODE_ORDER[left] - MODE_ORDER[right]
  );
  return advertised.length === 0 ? ["aside"] : advertised;
}

/**
 * What an empty transcript means for this session, which only the session can say: it answers
 * the relay on its control subject, or nobody is there. Saying "the next turn appears here" to a
 * viewer of a session that answers nothing was a promise the page could not keep. Not every
 * silent plugin has a newer one to install - no claude-envoy release streams at all - so the
 * text names what is true of the plugin rather than telling its reader to upgrade. The relay
 * keeps asking, so a session restarted onto a plugin that streams turns this around without a
 * reload, and the wording stays neutral until the first answer arrives.
 */
function emptyText(responding: boolean | undefined): string {
  if (responding === false) {
    return "This session is not answering the live view: its Envoy plugin does not stream (Claude Code sessions don't, nor pi-legion-envoy before 5.8.0), or the session is no longer running. Nothing appears here until it answers.";
  }
  return "Nothing yet. This session's next turn appears here as it happens.";
}

export function AgentConversationPage(): ReactNode {
  const { sessionId = "" } = useParams<{ sessionId: string }>();
  const { agents } = useAgents(true, true);
  const agent = agents.find((candidate) => candidate.session_id === sessionId);
  const label = sessionLabel(sessionId, agent?.title ?? "");
  useDocumentTitle(label);

  const { conversation, responding, status } = useAgentStream(sessionId);
  const modes = deliveryModes(agent?.capabilities ?? []);
  // The human's pick holds only while the session still offers it: the session list loads after
  // the page, and a pick the session does not advertise would be refused.
  const [picked, setPicked] = useState<MessageDeliveryMode | null>(null);
  const mode = picked !== null && modes.includes(picked) ? picked : (modes[0] ?? "aside");
  const [sendError, setSendError] = useState<string | null>(null);
  // The human's direct messages and the session's replies to them, as Dispatch stores them. A
  // person's Send or Aside to an Oh My Pi session becomes the session's own user turn, which the
  // stream carries tagged with this message's id, and the thread shows it once. A BTW, an issue
  // message, and any message to a session that takes no user turn from Envoy (a Claude Code
  // session) arrive as notices the stream has no frame for, and the session answers them through
  // dispatch_message, which the stream shows only as that tool call. Without these the thread
  // would show replies to messages the human cannot see, and no replies at all. Seeing them here
  // is reading them.
  const queryClient = useQueryClient();
  const stored = useQuery(agentMessagesQuery(sessionId));
  useMarkRepliesRead(sessionId, stored.data);
  const viewer = useQuery(whoAmIQuery()).data;

  // Talking to the agent is Dispatch's existing targeted delivery: the stream itself stays
  // read-only and this adds no write path of its own. The session confirms a Send or Aside with
  // Dispatch before it takes one as its user's own turn.
  const onNew = useCallback(
    async (message: { content: readonly { type: string; text?: string }[] }) => {
      const body = message.content
        .filter((part) => part.type === "text")
        .map((part) => part.text ?? "")
        .join("\n")
        .trim();
      if (body === "") return;
      setSendError(null);
      try {
        await api.createAgentMessage(sessionId, { body, delivery: mode });
        await queryClient.invalidateQueries({ queryKey: agentMessagesQuery(sessionId).queryKey });
      } catch (error) {
        setSendError(error instanceof Error ? error.message : "could not reach the session");
      }
    },
    [mode, queryClient, sessionId]
  );
  const presence = STATUS[status];

  // The shell hands this route the viewport below its header as a flex column; the page takes
  // all of it and the thread is its only scroller, so the header and composer never leave the
  // screen and the document never scrolls.
  return (
    <div className="flex min-h-0 flex-1 flex-col" data-testid="agent-conversation">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/agents">
          ← Agents
        </Link>
        <h1 className={`min-w-0 truncate text-lg font-semibold ${textPrimaryOnCanvas}`}>{label}</h1>
        <span className="flex items-center gap-1.5">
          <span
            aria-hidden="true"
            className={`inline-block h-2 w-2 rounded-full ${status === "unavailable" ? connectionDotFailed : presence.dot}`}
          />
          <span className={`text-xs ${textMutedOnCanvas}`}>{presence.label}</span>
        </span>
        {agent === undefined ? (
          <span className={`text-xs ${textMutedOnCanvas}`}>
            Not in the live session list — it may have ended.
          </span>
        ) : (
          <span className={`min-w-0 truncate text-xs ${textMutedOnCanvas}`}>
            {agent.machine_id} · {agent.dir}
          </span>
        )}
        <label className={`ml-auto flex items-center gap-2 text-xs ${textMutedOnCanvas}`}>
          Send as
          <select
            aria-label="Delivery mode"
            className={`min-h-8 rounded-lg px-2 py-1 text-xs ${inputClasses(false)}`}
            onChange={(event) => setPicked(event.target.value as MessageDeliveryMode)}
            value={mode}
          >
            {modes.map((candidate) => (
              <option key={candidate} value={candidate}>
                {MODE_LABELS[candidate]}
              </option>
            ))}
          </select>
        </label>
      </header>
      <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>
        Live from the session: its turns are relayed while this page is open and are not stored.
        Messages sent to it through Dispatch, and its Dispatch replies, are kept.
      </p>
      {stored.isError ? (
        <p className={`mt-2 text-sm ${dangerText}`}>
          Could not load your messages with this session.
        </p>
      ) : null}
      {status === "unavailable" ? (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-stream-unavailable">
          This Dispatch cannot reach the session's conversation.
        </p>
      ) : null}
      {sendError === null ? null : (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-send-error">
          Could not send: {sendError}
        </p>
      )}
      {/* Everything that reads a frame lives inside this boundary, in `AgentRuntimeThread`:
          assistant-ui converts every message while the runtime is built, so a message it
          refuses throws in the render of whichever component calls `useExternalStoreRuntime`.
          While that was this page, this boundary sat above nothing and a bad frame took the
          header and the delivery controls with the thread. The transcript has a boundary of its
          own inside `AgentThread`, which keeps the composer when the thread's own rendering is
          what failed. */}
      <ErrorBoundary region="this conversation" resetKey={sessionId}>
        <AgentRuntimeThread
          conversation={conversation}
          empty={emptyText(responding)}
          onNew={onNew}
          placeholder={`Message ${label} — delivered as ${MODE_LABELS[mode]}…`}
          resetKey={sessionId}
          sessionId={sessionId}
          stored={stored.data ?? []}
          viewer={viewer?.kind === "user" ? viewer.login : undefined}
        />
      </ErrorBoundary>
    </div>
  );
}
