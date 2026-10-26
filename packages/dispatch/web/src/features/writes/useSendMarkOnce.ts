import { useMutation } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

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
  readonly send: (value: T) => Promise<R>;
  readonly onSuccess?: (result: R) => void;
}

/**
 * The write every read mark makes: `send(mark.value)` once per `mark.key`, whenever the server
 * still needs it (`shouldSend`). The key, not the value, decides when to write, so a caller that
 * rebuilds its value on every render sends nothing more, and a retry carries the value the mark
 * was made with. A failed write is retried twice with backoff; if it still fails while it is the
 * view's latest write, its key is forgotten, so it is sent again when `shouldSend` or `wake` next
 * changes, rather than the mark staying unsent until the key changes. A write that a newer key
 * replaced while it was retrying forgets nothing when it fails: the newer write stands for the
 * view, and forgetting its key would send it once more.
 */
export function useSendMarkOnce<T, R>({
  mark,
  shouldSend,
  wake,
  send,
  onSuccess,
}: SendMarkOnceOptions<T, R>): void {
  // The mark this view last sent, so a re-render does not send its key again; compared by
  // identity, so a failure can tell its own write from a newer one with the same key.
  const sent = useRef<ReadMark<T> | undefined>(undefined);
  const latest = useRef({ mark, onSuccess, send });
  latest.current = { mark, onSuccess, send };
  const { mutate } = useMutation({
    mutationFn: (sending: ReadMark<T>) => latest.current.send(sending.value),
    onError: (_error, failed) => {
      if (sent.current === failed) sent.current = undefined;
    },
    onSuccess: (result) => latest.current.onSuccess?.(result),
    retry: 2,
  });
  const key = mark?.key;
  // biome-ignore lint/correctness/useExhaustiveDependencies: wake is a trigger the effect does not read.
  useEffect(() => {
    const current = latest.current.mark;
    if (!shouldSend || current === undefined || sent.current?.key === current.key) return;
    sent.current = current;
    mutate(current);
  }, [key, mutate, shouldSend, wake]);
}
