import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** What the in-flight write is doing, and `undefined` when none is: the moment a snooze is
   *  saving, or `null` for an un-snooze. The optimistic update has already moved the row by
   *  the time it renders, so this - not the row's own `snoozed_until` - is what the reader
   *  just asked for. */
  pendingUntil: string | null | undefined;
  /** Repeats the failed save. */
  retry: () => void;
  /** Snoozes the row until `until` (RFC3339), or brings it back with null. */
  submit: (until: string | null) => void;
}

/**
 * The one way the SPA writes a row's snooze. The new moment lands on every inbox cache at once,
 * so the row drops into `Later` (or climbs back out) on the click, rolls back if the server
 * refuses, and the list refetches once the save settles - the same shape as the issue field
 * writes, without their cross-surface caches: a snooze is on the ask and shows nowhere else.
 */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  const mutation = useMutation({
    mutationFn: (until: string | null) =>
      until === null ? api.unsnoozeAsk(askId) : api.snoozeAsk(askId, until).then(() => undefined),
    onMutate: async (until) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) => (row.id === askId ? { ...row, snoozed_until: until } : row))
      );
      return previous;
    },
    onError: (_error, _until, previous) => {
      for (const [key, data] of previous ?? []) {
        queryClient.setQueryData(key, data);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    error: mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    pendingUntil: mutation.isPending ? mutation.variables : undefined,
    retry: () => {
      if (mutation.variables !== undefined) mutation.mutate(mutation.variables);
    },
    submit: (until) => mutation.mutate(until),
  };
}

/** One ask the server refused, with the reason it gave - the same message the per-row control
 *  shows, so a sign-out never reads like a validation refusal. */
export interface AskSnoozeFailure {
  askId: string;
  reason: string;
}

export interface AskSnoozeManyWrite {
  /** True while a bulk save is in flight. */
  pending: boolean;
  /** Snoozes every id until `until` (RFC3339); resolves with the asks the server refused. */
  submit: (
    ids: readonly string[],
    until: string
  ) => Promise<{ failed: readonly AskSnoozeFailure[] }>;
}

/**
 * The bulk form of `useAskSnooze`, over the same route once per ask: the marked rows drop into
 * `Later` on the pick through the same optimistic write on every inbox cache, and the list
 * refetches once the whole set has settled. Each ask settles on its own, so a refusal rolls back
 * that row alone - to the moment it had before, not to null, which would un-snooze a row that
 * was already deferred - and the ones the server took stay where they landed.
 */
export function useAskSnoozeMany(): AskSnoozeManyWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  const mutation = useMutation({
    mutationFn: async ({ ids, until }: { ids: readonly string[]; until: string }) => {
      const results = await Promise.allSettled(ids.map((id) => api.snoozeAsk(id, until)));
      return ids.flatMap((askId, index) => {
        const result = results[index];
        if (result === undefined || result.status !== "rejected") return [];
        const reason: unknown = result.reason;
        return [{ askId, reason: reason instanceof Error ? reason.message : String(reason) }];
      });
    },
    onMutate: async ({ ids, until }) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      const marked = new Set(ids);
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) => (marked.has(row.id) ? { ...row, snoozed_until: until } : row))
      );
      return previous;
    },
    onSuccess: (failed, _variables, previous) => {
      if (failed.length === 0) return;
      const refused = new Set(failed.map(({ askId }) => askId));
      for (const [key, data] of previous ?? []) {
        const before = new Map((data ?? []).map((row) => [row.id, row.snoozed_until]));
        queryClient.setQueryData<InboxRow[]>(key, (current) =>
          current?.map((row) =>
            refused.has(row.id) ? { ...row, snoozed_until: before.get(row.id) ?? null } : row
          )
        );
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    pending: mutation.isPending,
    submit: async (ids, until) => ({ failed: await mutation.mutateAsync({ ids, until }) }),
  };
}
