import { expect, spyOn, test } from "bun:test";
import { type Query, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { CredentialPendingResponse } from "../../api/types";
import { credentialPendingQuery, useCredentialRequests } from "./pending";

function renderPending() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = renderHook(() => useCredentialRequests(), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  });
  return { queryClient, view };
}

// A `null`-holding query (no secrets broker) can neither fetch again on its own nor have the poll
// armed: `enabled` and `refetchInterval` both read the same `query.state.data !== null` fact, so
// if one ever answered differently from the other a brokerless Dispatch would start polling.
test("credentialPendingQuery's enabled and refetchInterval agree: armed for a list, off for null", () => {
  const query = credentialPendingQuery();
  const heldNull = { state: { data: null } } as unknown as Query<CredentialPendingResponse | null>;
  const heldList = {
    state: { data: { pending: [] } },
  } as unknown as Query<CredentialPendingResponse | null>;

  expect(query.enabled).toBeInstanceOf(Function);
  expect(query.refetchInterval).toBeInstanceOf(Function);
  const enabled = query.enabled as (query: typeof heldNull) => boolean;
  const refetchInterval = query.refetchInterval as (query: typeof heldNull) => number | false;

  expect(enabled(heldNull)).toBe(false);
  expect(refetchInterval(heldNull)).toBe(false);
  expect(enabled(heldList)).toBe(true);
  expect(typeof refetchInterval(heldList)).toBe("number");
  expect(refetchInterval(heldList)).toBeGreaterThan(0);
});

// LEGION-575 round 2. A background poll that fails after a list already loaded must not blank a
// good list: `useCredentialRequests` used to key `status: "failed"` off `isError` alone, which
// TanStack also sets on a *refetch* error with the last good `data` still attached - so the
// Inbox's section, its empty state and the Needs-you count would all drop a real pending request
// for as long as one poll tick failed, self-healing only on the next successful tick.
test("a poll that fails after a list already loaded keeps showing that list, not a failure", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending")
    .mockResolvedValueOnce({
      pending: [
        {
          identifiers: ["DEMO_API_KEY"],
          kind: "agent_secret",
          record_id: "req-1",
          requested_at: "2026-09-27T00:00:00Z",
        },
      ],
    })
    .mockRejectedValueOnce(new Error("network"));
  const { queryClient, view } = renderPending();
  try {
    await waitFor(() => expect(view.result.current.status).toBe("listed"));
    expect(view.result.current.requests).toHaveLength(1);

    await queryClient.refetchQueries({ queryKey: ["credential-pending"] });

    expect(getCredentialPending).toHaveBeenCalledTimes(2);
    expect(view.result.current.status).toBe("listed");
    expect(view.result.current.requests).toHaveLength(1);
    expect(view.result.current.requests[0]?.record_id).toBe("req-1");
  } finally {
    view.unmount();
    getCredentialPending.mockRestore();
  }
});
