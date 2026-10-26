import { DELIVERY_CAPABILITIES, type MessageDeliveryMode } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useRef, useState } from "react";
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
import { MODE_LABELS } from "../conversation/delivery";
import { useAgents } from "../conversation/useAgents";
import { AgentName, useAgentText } from "../refs/AgentName";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentRuntimeThread, type SendOutcome } from "./AgentRuntimeThread";
import { sessionCommands, sessionSettingsView } from "./conversation";
import { SessionSettings } from "./SessionSettings";
import { type SwitchRequest, switchCommand, useSessionSwitch } from "./use-session-switch";
import { type AgentStreamStatus, useAgentStream } from "./useAgentStream";

/** What the stream-status dot says about the relay connection (not whether the agent is
 *  running, which the header's `AgentName` mark shows), and how it reads to a screen reader. */
const STATUS: Record<AgentStreamStatus, { dot: string; label: string }> = {
  connecting: { dot: connectionDotConnecting, label: "Connecting to the session" },
  live: { dot: liveDotBg, label: "Live" },
  reconnecting: { dot: connectionDotConnecting, label: "Reconnecting" },
  unavailable: { dot: connectionDotFailed, label: "This session's conversation is unavailable" },
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
  // The page knows the route's session id and the registry, not a stamped title: the agent is
  // named by its registry title, else its short id.
  const label = useAgentText().sessionText(sessionId, undefined);
  useDocumentTitle(label);

  const { conversation, replays, responding, status } = useAgentStream(sessionId);
  // The model and thinking level the session runs now (INFRA-195), next to its title: what its
  // newest `settings` frame reports, or, for a session that reports none, the most recent
  // assistant turn's own model (LEGION-548), nothing guessed for a client that never sends one.
  const settings = sessionSettingsView(conversation, responding, replays);
  const modes = deliveryModes(agent?.capabilities ?? []);
  // The human's pick holds only while the session still offers it: the session list loads after
  // the page, and a pick the session does not advertise would be refused.
  const [picked, setPicked] = useState<MessageDeliveryMode | null>(null);
  const mode = picked !== null && modes.includes(picked) ? picked : (modes[0] ?? "aside");
  // The slash commands the composer completes (LEGION-394). The session runs typed input only
  // from a Send or an Aside, so a BTW, a side question it answers, offers none; nor does a
  // session that sent no list, whose host cannot run typed input.
  const listed = sessionCommands(conversation);
  const commands =
    mode === "btw" || listed === undefined || listed.length === 0 ? undefined : listed;
  // Why Dispatch refused each session's latest send, by session: a refusal belongs to the
  // session that sent the message, shows only while the reader is on it (even if it landed
  // while they were on another), and clears on that session's next send.
  const [sendErrors, setSendErrors] = useState<ReadonlyMap<string, string>>(() => new Map());
  const sendError = sendErrors.get(sessionId);
  // The human's direct messages and the session's replies to them, as Dispatch stores them. A
  // person's Send or Aside to an Oh My Pi session becomes the session's own user turn, which the
  // stream carries tagged with this message's id, and the thread shows it once; one the stream
  // carries untagged shows twice (`packages/pi-envoy/AGENTS.md`, the phase-worker section, says
  // when). A BTW, an issue message, and any message to a session that takes no user turn from
  // Envoy (a Claude Code session) arrive as notices the stream has no frame for, and the session
  // answers them with a `dispatch message` command, which the stream shows only as a `bash` tool
  // call whose arguments hold that command. Without these the thread would show replies to
  // messages the human cannot see, and no replies at all.
  // Seeing them here is reading them.
  const queryClient = useQueryClient();
  const stored = useQuery(agentMessagesQuery(sessionId));
  useMarkRepliesRead(sessionId, stored.data);
  const viewer = useQuery(whoAmIQuery()).data;
  // The header's switch, one at a time; while one waits for the session's answer neither picker
  // nor Send takes another, so this page's next prompt reaches the session after the switch.
  const sessionSwitch = useSessionSwitch(sessionId, stored.data);
  // One per message of this viewer's the session accepted, which takes the thread back to its
  // live end (`use-live-follow.ts`). Bumped after the write lands, only if the reader is still on
  // the session it went to, and never on a refusal: the reason renders above the transcript,
  // where a reader who has scrolled back can read it without being taken away from what they
  // were reading.
  const [ownSendCount, setOwnSendCount] = useState(0);
  // The session on screen now, for a send that lands after the reader may have moved on.
  const onScreen = useRef(sessionId);
  onScreen.current = sessionId;

  // Every write this page makes to a session, the composer's and the header's: `session` is the
  // one the write went to, taken before the await. A refusal is kept for that session and renders
  // above its transcript; an accepted one counts as the viewer's own send while they are still on
  // that session and refreshes its stored thread. Refused means exactly that Dispatch did not take
  // the message.
  const deliver = useCallback(
    async (session: string, post: () => Promise<unknown>): Promise<SendOutcome> => {
      setSendErrors((previous) => {
        if (!previous.has(session)) return previous;
        const next = new Map(previous);
        next.delete(session);
        return next;
      });
      try {
        await post();
      } catch (error) {
        const reason = error instanceof Error ? error.message : "could not reach the session";
        setSendErrors((previous) => new Map(previous).set(session, reason));
        return "refused";
      }
      if (session === onScreen.current) setOwnSendCount((previous) => previous + 1);
      // The message is Dispatch's now, so refreshing the stored list is outside the send: a
      // refresh that fails is never a refusal, and Send is free again without waiting on it.
      void queryClient.invalidateQueries({ queryKey: agentMessagesQuery(session).queryKey });
      return "sent";
    },
    [queryClient]
  );
  // Talking to the agent is Dispatch's existing targeted delivery: the stream itself stays
  // read-only and this adds no write path of its own. The session confirms a Send or Aside with
  // Dispatch before it takes one as its user's own turn. `session` and `mode` are the ones the
  // send was made under.
  const onSend = useCallback(
    (session: string, body: string): Promise<SendOutcome> =>
      deliver(session, () => api.createAgentMessage(session, { body, delivery: mode })),
    [deliver, mode]
  );
  // A pick is the person's own Send of what they would type at the session's terminal, always as
  // a Send whatever the composer's mode is: a BTW is a side question the session answers, not
  // input it runs. It is stored under the person's name like any Send, which is the record of who
  // switched the session and to what.
  const { run: runSwitch } = sessionSwitch;
  const onPick = useCallback(
    (request: SwitchRequest) =>
      deliver(sessionId, () =>
        runSwitch(request, () =>
          api.createAgentMessage(sessionId, { body: switchCommand(request), delivery: "steer" })
        )
      ),
    [deliver, runSwitch, sessionId]
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
        {/* This is that agent's own live conversation, so its name links nowhere: `AgentName`
            draws it, with its running state, without a link. */}
        <h1 className={`min-w-0 truncate text-lg font-semibold ${textPrimaryOnCanvas}`}>
          <AgentName kind="static" sessionId={sessionId} storedTitle={undefined} />
        </h1>
        <SessionSettings
          onPick={onPick}
          pending={sessionSwitch.pending}
          status={sessionSwitch.status}
          view={settings}
        />
        <span
          className="flex items-center gap-1.5"
          title={`Connection to the session's conversation: ${presence.label}`}
        >
          <span
            aria-hidden="true"
            className={`inline-block h-2 w-2 rounded-full ${status === "unavailable" ? connectionDotFailed : presence.dot}`}
          />
          <span className={`text-xs ${textMutedOnCanvas}`}>
            <span className="sr-only">Connection: </span>
            {presence.label}
          </span>
        </span>
        {agent === undefined ? (
          <span className={`text-xs ${textMutedOnCanvas}`}>
            Not in the live session list — it may have ended.
          </span>
        ) : (
          <span className={`min-w-0 truncate text-xs ${textMutedOnCanvas}`}>
            {agent.host} · {agent.dir}
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
      {agent !== undefined && agent.capabilities.length === 0 ? (
        <p className={`mt-2 text-sm ${dangerText}`} data-testid="agent-takes-no-messages">
          This session advertises no delivery mode, so a message sent here records a failed attempt.
          A Claude Code session takes messages only when it was started with its Envoy channel.
        </p>
      ) : null}
      {sendError === undefined ? null : (
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
          commands={commands}
          conversation={conversation}
          empty={emptyText(responding)}
          onSend={onSend}
          ownSendCount={ownSendCount}
          placeholder={
            sessionSwitch.pending
              ? "Waiting for the session to answer the switch…"
              : `Message ${label} — delivered as ${MODE_LABELS[mode]}${commands === undefined ? "" : ", / for commands"}…`
          }
          replayCount={replays}
          resetKey={sessionId}
          sendDisabled={sessionSwitch.pending}
          sessionId={sessionId}
          stored={stored.data ?? []}
          viewer={viewer?.kind === "user" ? viewer.login : undefined}
        />
      </ErrorBoundary>
    </div>
  );
}
