import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { type ReactNode, useMemo } from "react";

import type { MessageRead } from "../../api/types";
import { actorName, isViewer } from "../refs/actor";
import { AgentThread } from "./AgentThread";
import { type AgentConversation, isRunning, toThreadMessages } from "./conversation";
import { dispatchMetadata } from "./dispatch-marks";

/**
 * Everything that reads the conversation, in one component so a boundary can be put around it.
 *
 * assistant-ui converts every message while the runtime is built, so a message it refuses throws
 * from `useExternalStoreRuntime` — in the render of whichever component calls it. Called from the
 * page, no boundary the page rendered could catch it, and a bad frame would take the header and
 * the delivery controls with the thread. Here, a boundary the page puts around this component does
 * catch it, and the page's own render never touches a frame.
 */
export function AgentRuntimeThread({
  conversation,
  empty,
  onNew,
  placeholder,
  resetKey,
  sessionId,
  stored,
  viewer,
}: {
  conversation: AgentConversation;
  empty: string;
  onNew: (message: { content: readonly { type: string; text?: string }[] }) => Promise<void>;
  placeholder: string;
  resetKey: string;
  sessionId: string;
  stored: readonly MessageRead[];
  /** The signed-in human's login: their own messages read as theirs, every other one names its
   *  author. */
  viewer: string | undefined;
}): ReactNode {
  const messages = useMemo(() => {
    const streamed = toThreadMessages(conversation);
    // Dispatch's side of the conversation, interleaved with the stream by time: what the session
    // wrote is its reply, what the viewer wrote is theirs, and anything anyone else sent the
    // session (another human's direct message, an issue message, another agent) says who.
    const dispatch: ThreadMessageLike[] = stored
      .flatMap((read) => [read.message, ...read.replies])
      .map((message) => {
        const content = [{ text: message.body, type: "text" as const }];
        const createdAt = new Date(message.created_at);
        const id = `dispatch:${message.id}`;
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
        const author = isViewer(message.author, viewer)
          ? undefined
          : [actorName(message.author), message.issue_key].filter(Boolean).join(" · ");
        return {
          content,
          createdAt,
          id,
          metadata: dispatchMetadata({ author }),
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

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread empty={empty} placeholder={placeholder} resetKey={resetKey} />
    </AssistantRuntimeProvider>
  );
}
