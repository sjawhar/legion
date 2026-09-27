import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { mergeIssue } from "../api/issue-cache";
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
