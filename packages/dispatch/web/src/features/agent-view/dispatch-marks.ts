/**
 * What AgentRuntimeThread tells AgentThread about a message that came through Dispatch, carried
 * in assistant-ui's untyped `metadata.custom`: `dispatch` marks the session's own reply sent
 * through Dispatch rather than streamed, and `author` names whoever other than the viewer sent the
 * session a message, whether Dispatch's stored copy shows or the turn the session took from it.
 * The viewer's own messages carry neither.
 */
export interface DispatchMarks {
  readonly author?: string;
  readonly dispatch?: true;
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
  };
}
