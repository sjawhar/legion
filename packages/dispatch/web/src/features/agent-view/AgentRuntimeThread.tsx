import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import type { AgentStreamCommand } from "@legion/contracts";
import { type ReactNode, useMemo } from "react";

import type { Message, MessageRead } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { actorName, isViewer } from "../refs/actor";
import { AgentThread } from "./AgentThread";
import { type AgentConversation, dispatchTurns, isRunning, toThreadMessages } from "./conversation";
import { dispatchMetadata, type KeyedMessage, turnKey } from "./dispatch-marks";

/** Who wrote a stored message, when it was not the viewer: its author, and its issue if any. */
function otherAuthor(message: Message, viewer: string | undefined): string | undefined {
  return isViewer(message.author, viewer)
    ? undefined
    : [actorName(message.author), message.issue_key].filter(Boolean).join(" · ");
}

/** The id a stored Dispatch message is rendered under, which a streamed turn that took that
 *  message over carries as its `turn` mark: one key for both, spelt here alone. */
function dispatchId(messageId: string): string {
  return `dispatch:${messageId}`;
}

/**
 * Everything that reads the conversation, in one component so a boundary can be put around it.
 *
 * assistant-ui converts every message while the runtime is built, so a message it refuses throws
 * from `useExternalStoreRuntime` — in the render of whichever component calls it. Called from the
 * page, it would throw where no boundary the page renders can catch it, and a bad frame would take
 * the header and the delivery controls with the thread. Here, a boundary the page puts around this
 * component does catch it, and the page's own render never touches a frame.
 */
export function AgentRuntimeThread({
  commands,
  conversation,
  empty,
  onNew,
  ownSendCount,
  placeholder,
  replayCount,
  resetKey,
  sendRefusal,
  sessionId,
  stored,
  viewer,
}: {
  /** The slash commands the composer completes, or undefined to offer none. */
  commands: readonly AgentStreamCommand[] | undefined;
  conversation: AgentConversation;
  empty: string;
  onNew: (message: { content: readonly { type: string; text?: string }[] }) => Promise<void>;
  /** Bumped once per message this viewer sent that the session accepted, so their own send takes
   *  them back to the live end (`AgentThread`'s `useLiveFollow`). */
  ownSendCount: number;
  placeholder: string;
  /** Bumped once per rebuild of the whole transcript from the session's replay
   *  (`useAgentStream`'s `replays`). */
  replayCount: number;
  resetKey: string;
  /** Why the composer's Send cannot be pressed now, or null while it can (`AgentThread`). */
  sendRefusal: string | null;
  sessionId: string;
  stored: readonly MessageRead[];
  /** The signed-in human's login: their own messages read as theirs, every other one names its
   *  author. */
  viewer: string | undefined;
}): ReactNode {
  const messages = useMemo(() => {
    const storedMessages = stored.flatMap((read) => [read.message, ...read.replies]);
    // A person's direct message the session took as its own user turn is in the stream too,
    // normally tagged with its Dispatch id: then it shows once, where the session took it, still
    // naming whoever wrote it. An untagged one is returned unchanged and its stored copy is never
    // taken, so it shows twice (`packages/pi-envoy/AGENTS.md` says which turns go untagged). Any
    // bus client can publish a tag, so only a person's stored message can be one, and only a
    // streamed message saying exactly what that person sent replaces its stored copy.
    const byID = new Map(
      storedMessages
        .filter((message) => message.author.kind === "user")
        .map((message) => [message.id, message])
    );
    const turns = dispatchTurns(conversation);
    const taken = new Set<string>();
    const streamed = toThreadMessages(conversation).map((message) => {
      const turn = turns.get(message.id ?? "");
      const source = byID.get(turn?.dispatchMessageId ?? "");
      if (turn === undefined || source === undefined || turn.text !== source.body) return message;
      taken.add(source.id);
      // The streamed turn keeps the key the stored copy would have had, so it is one turn under
      // one key for `useLiveFollow` whether the reader sees this frame or, after a rebuild that
      // dropped it, the stored copy below under that same `dispatch:<id>` id.
      return {
        ...message,
        metadata: dispatchMetadata({
          author: otherAuthor(source, viewer),
          turn: dispatchId(source.id),
        }),
      };
    });
    // Dispatch's side of the conversation, interleaved with the stream by time: what the session
    // wrote is its reply, what the viewer wrote is theirs, and anything anyone else sent the
    // session (another human's direct message, an issue message, another agent) says who. The
    // stream's times are whole milliseconds, so the merge below compares milliseconds; the stored
    // messages carry microseconds, so they are put in time order first, and the stable merge keeps
    // that order between two stored messages in one millisecond.
    const dispatch: KeyedMessage[] = storedMessages
      .filter((message) => !taken.has(message.id))
      .sort((left, right) => compareTimestamps(left.created_at, right.created_at))
      .map((message) => {
        const content = [{ text: message.body, type: "text" as const }];
        const createdAt = new Date(message.created_at);
        const id = dispatchId(message.id);
        if (message.author.kind === "session" && message.author.id === sessionId) {
          return {
            content,
            createdAt,
            id,
            metadata: dispatchMetadata({ dispatch: true }),
            role: "assistant" as const,
            status: { reason: "stop", type: "complete" } as const,
          };
        }
        return {
          content,
          createdAt,
          id,
          metadata: dispatchMetadata({ author: otherAuthor(message, viewer) }),
          role: "user" as const,
        };
      });
    return [...streamed, ...dispatch].sort(
      (left, right) => (left.createdAt?.getTime() ?? 0) - (right.createdAt?.getTime() ?? 0)
    );
  }, [conversation, sessionId, stored, viewer]);

  const runtime = useExternalStoreRuntime({
    // The frames already arrive in assistant-ui's own message shape, so the store's converter
    // is the identity: `conversation.ts` is the one place the session's wire shape is read.
    convertMessage: (message: ThreadMessageLike) => message,
    isRunning: isRunning(conversation),
    messages,
    onNew,
  });

  // What `useLiveFollow` reads the transcript from, in render order; `AgentThread` renders the
  // same key into each turn's `data-message-id`, so the hook can find the turn a reader was on.
  const contentIds = useMemo(
    () => messages.map((message) => turnKey(message.metadata?.custom ?? {}, message.id)),
    [messages]
  );

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread
        commands={commands}
        contentIds={contentIds}
        empty={empty}
        ownSendCount={ownSendCount}
        placeholder={placeholder}
        replayCount={replayCount}
        resetKey={resetKey}
        sendRefusal={sendRefusal}
        sessionId={sessionId}
      />
    </AssistantRuntimeProvider>
  );
}
