import { type MutationKey, useIsMutating, useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";

/** The sends named `mutationKey`, or a key beneath it (TanStack matches a key's prefix), as the
 *  host of the composers that make them reads them. */
export interface Sending {
  /** Whether one is out, as of the last render: what the host's own controls show. */
  readonly sending: boolean;
  /** Whether one is out now, for an event handler. The mutation cache counts a send pending from
   *  the task that starts it - `mutate` marks it before its first await - so a control pressed in
   *  Send's own task, before React has rendered the send, still reads it; and a send whose
   *  composer has unmounted still counts until the server answers. */
  readonly sendingNow: () => boolean;
}

export function useSending(mutationKey: MutationKey): Sending {
  const queryClient = useQueryClient();
  const sending = useIsMutating({ mutationKey }) > 0;
  const sendingNow = useCallback(
    () => queryClient.isMutating({ mutationKey }) > 0,
    [mutationKey, queryClient]
  );
  return { sending, sendingNow };
}
