import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { type ReactNode, useMemo } from "react";

import { AgentThread } from "./AgentThread";
import { type AgentConversation, isRunning, toThreadMessages } from "./conversation";

/** A message this viewer sent, echoed back into its own thread. */
export interface SentMessage {
  readonly id: string;
  readonly at: number;
  readonly body: string;
}

/**
 * Everything that reads the conversation, in one component so a boundary can be put around it.
 *
 * assistant-ui converts every message while the runtime is built, so a message it refuses throws
 * from `useExternalStoreRuntime` — in the render of whichever component calls it. While that was
 * the page, no boundary the page rendered could catch it and a bad frame took the header and the
 * delivery controls with the thread. Here, a boundary the page puts around this component does
 * catch it, and the page's own render never touches a frame.
 */
export function AgentRuntimeThread({
  conversation,
  onNew,
  placeholder,
  resetKey,
  sent,
}: {
  conversation: AgentConversation;
  onNew: (message: { content: readonly { type: string; text?: string }[] }) => Promise<void>;
  placeholder: string;
  resetKey: string;
  sent: readonly SentMessage[];
}): ReactNode {
  const messages = useMemo(() => {
    const streamed = toThreadMessages(conversation);
    const echoes: ThreadMessageLike[] = sent.map((entry) => ({
      content: [{ text: entry.body, type: "text" as const }],
      createdAt: new Date(entry.at),
      id: entry.id,
      role: "user" as const,
    }));
    return [...streamed, ...echoes].sort(
      (left, right) => (left.createdAt?.getTime() ?? 0) - (right.createdAt?.getTime() ?? 0)
    );
  }, [conversation, sent]);

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
      <AgentThread placeholder={placeholder} resetKey={resetKey} />
    </AssistantRuntimeProvider>
  );
}
