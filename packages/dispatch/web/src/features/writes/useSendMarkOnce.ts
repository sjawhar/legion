import { useEffect, useRef } from "react";

import type { WriteBounds, WriteRequest } from "../../api/client";
import { plainWrite, useWrites } from "./write-store";

/** One read mark: what it is for (`key`) and what a send carries (`value`). */
export interface ReadMark<T> {
  readonly key: string;
  readonly value: T;
}

export interface SendMarkOnceOptions<T, R> {
  /** The mark this render would send. A change of key is a new mark; the same key is never sent
   *  twice while its last send stands. */
  readonly mark: ReadMark<T> | undefined;
  /** Whether the server still needs a mark: the view sends nothing while this is false. */
  readonly shouldSend: boolean;
  /** A value whose change makes the hook look again without making a new mark - the server's
   *  unread count, say - so a mark whose send failed for good is sent once more. */
  readonly wake?: unknown;
  /** What the marks of this view are ordered on: one mark of it goes out after the one before. */
  readonly target: string;
  /** The request a mark of `value` makes, which a mark waiting behind the one before is kept as
   *  for a reload (`WriteSpec.request`). */
  readonly request: (value: T) => WriteRequest;
  /** One attempt of the mark's write, under the bounds the write store gives it: it makes
   *  `request(value)`. */
  readonly send: (value: T, bounds: WriteBounds) => Promise<R>;
  readonly onSuccess?: (result: R) => void;
}

/**
 * The write every read mark makes: `send(mark.value)` once per `mark.key`, whenever the server
 * still needs it (`shouldSend`), as a quiet write of the one write store (`Writes`): tried under
 * its key like any write and kept across a reload, never shown, and let go if Dispatch has not
 * confirmed it five minutes after it was made. The key, not the value, decides when to write, so
 * a caller that rebuilds its value on every render sends nothing more, and a retry carries the
 * value the mark was made with. A mark that ends without landing - refused, or let go - while it
 * is the view's latest has its key forgotten, so it is sent again when `shouldSend` or `wake`
 * next changes, rather than the mark staying unsent until the key changes. A mark a newer key
 * replaced forgets nothing when it ends: the newer one stands for the view.
 */
export function useSendMarkOnce<T, R>({
  mark,
  shouldSend,
  target,
  wake,
  request,
  send,
  onSuccess,
}: SendMarkOnceOptions<T, R>): void {
  const writes = useWrites();
  // The mark this view last sent, so a re-render does not send its key again; compared by
  // identity, so an ending can tell its own write from a newer one with the same key.
  const sent = useRef<ReadMark<T> | undefined>(undefined);
  const latest = useRef({ mark, onSuccess, request, send, target });
  latest.current = { mark, onSuccess, request, send, target };
  const key = mark?.key;
  // biome-ignore lint/correctness/useExhaustiveDependencies: wake is a trigger the effect does not read.
  useEffect(() => {
    const current = latest.current.mark;
    if (!shouldSend || current === undefined || sent.current?.key === current.key) return;
    sent.current = current;
    const { request: requestOf, send: write } = latest.current;
    writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "Read mark",
        presentation: "quiet",
        request: requestOf(current.value),
        target: latest.current.target,
        write: (bounds) => write(current.value, bounds),
      })
      .then(
        (result) => {
          if (result !== null) latest.current.onSuccess?.(result);
        },
        () => {
          if (sent.current === current) sent.current = undefined;
        }
      );
  }, [key, shouldSend, wake, writes]);
}
