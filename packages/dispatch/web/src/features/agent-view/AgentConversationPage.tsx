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

/** The modes a human can talk to a session in, narrowed to the ones it advertises. `aside`
 *  is the default everywhere: it reaches the agent without interrupting its turn. */
function deliveryModes(capabilities: readonly string[]): MessageDeliveryMode[] {
  const advertised = DELIVERY_CAPABILITIES.filter((mode) => capabilities.includes(mode));
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
  const [mode, setMode] = useState<MessageDeliveryMode>("aside");
  const [sendError, setSendError] = useState<string | null>(null);
  // The human's direct messages and the session's replies to them, as Dispatch stores them. A
  // targeted delivery reaches the session as a notice rather than one of its own user messages,
  // so the session publishes no frame for it (measured against a live Oh My Pi session), and it
  // answers through dispatch_message, which the stream shows only as that tool call. Without
  // these the thread would show replies to messages the human cannot see, and no replies at all.
  // Seeing them here is reading them.
  const queryClient = useQueryClient();
  const stored = useQuery(agentMessagesQuery(sessionId));
  useMarkRepliesRead(sessionId, stored.data);
  const viewer = useQuery(whoAmIQuery()).data;

  // Talking to the agent is Dispatch's existing targeted delivery, unchanged: the stream itself
  // stays read-only and this adds no write path of its own.
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

  return (
    <div className="flex h-[calc(100vh-6rem)] flex-col" data-testid="agent-conversation">
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
            onChange={(event) => setMode(event.target.value as MessageDeliveryMode)}
            value={mode}
          >
            {modes.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
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
          placeholder={`Message ${label} — delivered as ${mode}…`}
          resetKey={sessionId}
          sessionId={sessionId}
          stored={stored.data ?? []}
          viewer={viewer?.kind === "user" ? viewer.login : undefined}
        />
      </ErrorBoundary>
    </div>
  );
}
