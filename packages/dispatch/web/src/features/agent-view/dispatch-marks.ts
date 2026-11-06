import type { ThreadMessageLike } from "@assistant-ui/react";

import type { Actor } from "../../api/types";

/**
 * Who other than the viewer sent the session a message, kept as an identity until
 * `AgentThread` renders it through `AgentName`: the writer as Dispatch recorded it (its id, its
 * kind and, for a session, the title it stamped on the write, the name's fallback when the
 * registry no longer lists it), and the issue the message was sent on, if any.
 */
export interface DispatchAuthor {
  readonly actor: Actor;
  readonly issue?: string;
}

/**
 * What AgentRuntimeThread and the live stream's own conversion tell AgentThread about a message,
 * carried in assistant-ui's untyped `metadata.custom`: `dispatch` marks the session's own reply
 * sent through Dispatch rather than streamed, `author` says who other than the viewer sent the
 * session a message (whether Dispatch's stored copy shows or the turn the session took from it),
 * and `model` names the `provider/model` that produced a streamed assistant turn (LEGION-548),
 * set only when the session's own frame carried one. The viewer's own messages carry neither
 * `dispatch` nor `author`. `turn` is the key a turn keeps across a rebuild of the transcript,
 * set only where it differs from the message's own id - a streamed turn that took over a stored
 * Dispatch message, which the same message carries as `dispatch:<id>` when only the stored copy
 * is shown.
 */
export interface DispatchMarks {
  readonly author?: DispatchAuthor;
  readonly dispatch?: true;
  readonly model?: string;
  readonly turn?: string;
}

/** The `metadata` a stored message is handed to assistant-ui with. */
export function dispatchMetadata(marks: DispatchMarks): { custom: Record<string, unknown> } {
  return { custom: { ...marks } };
}

const AUTHOR_KINDS: Record<Actor["kind"], true> = { session: true, system: true, user: true };

/** The `author` mark, when it has the shape `dispatchMetadata` wrote. It is returned as the
 *  very object the metadata holds, never a copy; a rebuild of the transcript writes a new one,
 *  so a `useAuiState` selector (compared by `Object.is`) reads its fields, not the mark. */
function readAuthor(value: unknown): DispatchAuthor | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const { actor, issue } = value as Record<string, unknown>;
  if (typeof actor !== "object" || actor === null) return undefined;
  const { id, kind } = actor as Record<string, unknown>;
  return typeof id === "string" &&
    typeof kind === "string" &&
    Object.hasOwn(AUTHOR_KINDS, kind) &&
    (issue === undefined || typeof issue === "string")
    ? (value as DispatchAuthor)
    : undefined;
}

/** Reads the marks back from a message's `metadata.custom`. */
export function readDispatchMarks(custom: Readonly<Record<string, unknown>>): DispatchMarks {
  return {
    author: readAuthor(custom.author),
    dispatch: custom.dispatch === true ? true : undefined,
    model: typeof custom.model === "string" ? custom.model : undefined,
    turn: typeof custom.turn === "string" ? custom.turn : undefined,
  };
}

/**
 * A message this view hands assistant-ui, carrying the id it was keyed by. The library leaves
 * `id` optional for a caller that lets its runtime mint one; this view never does, because the
 * key below is what the transcript is read back through.
 */
export type KeyedMessage = ThreadMessageLike & { readonly id: string };

/**
 * The key a turn keeps across a rebuild of the transcript: the `turn` mark where a streamed turn
 * took a stored Dispatch message over, and the message's own id otherwise.
 * `AgentRuntimeThread` computes it for `useLiveFollow`'s `contentIds`, and `AgentThread` renders
 * it into every turn's `data-message-id`; the hook matches the two by string equality, so both
 * sides spell the key here rather than each in its own expression.
 */
export function turnKey(custom: Readonly<Record<string, unknown>>, id: string): string {
  return readDispatchMarks(custom).turn ?? id;
}
