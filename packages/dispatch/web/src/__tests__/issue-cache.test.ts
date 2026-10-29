import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { api } from "../api/client";
import { mergeIssue } from "../api/issue-cache";
import { issueQuery } from "../api/queries";
import type { IssueDetails } from "../api/types";
import { issue as fixture } from "../features/margin/margin-fixture";

function cachedAfterPatch(before: IssueDetails, patch: Partial<IssueDetails>): IssueDetails {
  const queryClient = new QueryClient();
  queryClient.setQueryData(["issue", before.key], before);
  // A PATCH answers the `Issue` fields alone; route_status is never among them.
  const { route_status: _status, route_holder: _holder, ...response } = { ...before, ...patch };
  mergeIssue(queryClient, response);
  const cached = queryClient.getQueryData<IssueDetails>(["issue", before.key]);
  if (cached === undefined) throw new Error("the merge dropped the cached issue");
  return cached;
}

const unheld: IssueDetails = { ...fixture, route: "role:sre", route_status: "no_holder" };

test("a PATCH that moves the route drops the old route's status instead of wearing it", () => {
  const cached = cachedAfterPatch(unheld, { route: "role:platform-po" });
  expect(cached.route).toBe("role:platform-po");
  expect(cached.route_status).toBeNull();
  expect(cached.route_holder).toBeNull();
});

test("a PATCH that leaves the route alone keeps its status until the next read", () => {
  const cached = cachedAfterPatch(unheld, { title: "Renamed" });
  expect(cached.title).toBe("Renamed");
  expect(cached.route_status).toBe("no_holder");
});

// W35. A read already out when a write changed the issue - a slow GET started before a PATCH - can
// be answered after the PATCH's own answer was put in its place. Its issue is the older one (an
// earlier `updated_at`, read whole: `…00.1Z` is before `…00.123456Z`), so the cache keeps the
// PATCH's; a read that is not older replaces the cached issue as any read does.
test("a slow GET answered after a PATCH leaves the PATCH's answer in place", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const read: IssueDetails = { ...fixture, status: "todo", updated_at: "2026-10-01T00:00:00.1Z" };
  const slow = Promise.withResolvers<IssueDetails>();
  const getIssue = spyOn(api, "getIssue")
    .mockResolvedValueOnce(read)
    .mockImplementationOnce(() => slow.promise);
  const observer = new QueryObserver(queryClient, issueQuery(read.key));
  const unsubscribe = observer.subscribe(() => {});
  const cached = () => queryClient.getQueryData<IssueDetails>(["issue", read.key]);
  try {
    await observer.refetch();
    expect(cached()?.status).toBe("todo");

    const refetch = observer.refetch();
    const { artifacts: _artifacts, children: _children, open_asks: _asks, ...issue } = read;
    mergeIssue(queryClient, {
      ...issue,
      status: "done",
      updated_at: "2026-10-01T00:00:00.123456Z",
    });
    slow.resolve(read);
    await refetch;
    expect(cached()).toMatchObject({ status: "done", updated_at: "2026-10-01T00:00:00.123456Z" });

    getIssue.mockResolvedValueOnce({
      ...read,
      status: "done",
      updated_at: "2026-10-01T00:00:00.2Z",
    });
    await observer.refetch();
    expect(cached()).toMatchObject({ status: "done", updated_at: "2026-10-01T00:00:00.2Z" });
  } finally {
    unsubscribe();
    getIssue.mockRestore();
  }
});
