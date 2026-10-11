/**
 * What AgentRuntimeThread and the live stream's own conversion tell AgentThread about a message,
 * carried in assistant-ui's untyped `metadata.custom`: `dispatch` marks the session's own reply
 * sent through Dispatch rather than streamed, `author` names whoever other than the viewer sent
 * the session a message (whether Dispatch's stored copy shows or the turn the session took from
 * it), and `model` names the `provider/model` that produced a streamed assistant turn
 * (LEGION-548), set only when the session's own frame carried one. The viewer's own messages
 * carry neither `dispatch` nor `author`.
 */
export interface DispatchMarks {
  readonly author?: string;
  readonly dispatch?: true;
  readonly model?: string;
}

/** The `metadata` a stored message is handed to assistant-ui with. */
export function dispatchMetadata(marks: DispatchMarks): { custom: Record<string, unknown> } {
  return { custom: { ...marks } };
}

/** Reads the marks back from a message's `metadata.custom`. */
export function readDispatchMarks(custom: Readonly<Record<string, unknown>>): DispatchMarks {
  return {
    author: typeof custom.author === "string" ? custom.author : undefined,
    dispatch: custom.dispatch === true ? true : undefined,
    model: typeof custom.model === "string" ? custom.model : undefined,
  };
}
