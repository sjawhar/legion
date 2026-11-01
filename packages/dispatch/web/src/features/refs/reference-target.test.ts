import { afterEach, expect, spyOn, test } from "bun:test";
import {
  focusManager,
  QueryClient,
  QueryClientProvider,
  type QueryKey,
  QueryObserver,
  type QueryObserverOptions,
} from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import type { GraphReferences, IssueReferenceSummary } from "../../api/types";
import {
  blockQuery,
  documentSummaryQuery,
  issueSummaryQuery,
  messageQuery,
  prefetchReference,
  useReferenceTarget,
} from "./reference-target";

let restore = () => {};

afterEach(() => {
  restore();
  focusManager.setFocused(undefined);
});

/** Every api read a reference can make, spied so a test sees which ran and none reaches the
 *  network. */
function spyReads() {
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "doc-1",
        kind: "doc",
        latest_version: 2,
        name: "design.md",
        primary: true,
        size: null,
        slug: "design",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Design decision",
  };
  const spies = {
    getArtifactText: spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "Live",
      token: "sha256:live",
      version: null,
    }),
    getArtifactVersion: spyOn(api, "getArtifactVersion").mockResolvedValue({} as never),
    getAsk: spyOn(api, "getAsk").mockResolvedValue({} as never),
    getComment: spyOn(api, "getComment").mockResolvedValue({} as never),
    // Only what the hover warm-up reads: the primary spec among the issue's artifacts.
    getIssue: spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [{ id: "spec-1", kind: "doc" }],
      key: "CORE-1",
      primary_artifact_id: "spec-1",
    } as never),
    getIssueSummary: spyOn(api, "getIssueSummary").mockResolvedValue(summary),
    getMessage: spyOn(api, "getMessage").mockResolvedValue({} as never),
    getProjectArtifact: spyOn(api, "getProjectArtifact").mockResolvedValue({} as never),
    getProjectDocumentSummary: spyOn(api, "getProjectDocumentSummary").mockResolvedValue({
      id: "doc-9",
      kind: "doc",
      latest_version: 4,
      name: "Runbook.md",
      project: "CORE",
      size: null,
      slug: "runbook-md",
    }),
    getReferences: spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: { id: "CORE-1/spec#intro", kind: "block" },
    } as never),
  };
  restore = () => {
    for (const spy of Object.values(spies)) {
      spy.mockRestore();
    }
  };
  return spies;
}

/** Waits until the client holds exactly `expected`, the keys of every prefetch started, loaded. */
async function expectKeys(queryClient: QueryClient, expected: unknown[][]): Promise<void> {
  await waitFor(() =>
    expect(
      queryClient
        .getQueryCache()
        .getAll()
        .filter((query) => query.state.status === "success")
        .map((query) => [...query.queryKey])
    ).toEqual(expected)
  );
}

test("summary reads are fetched again by events, never by age", () => {
  for (const options of [issueSummaryQuery("CORE-1"), documentSummaryQuery("CORE", "runbook")]) {
    expect(options.staleTime).toBe(Number.POSITIVE_INFINITY);
  }
  expect([...issueSummaryQuery("CORE-1").queryKey]).toEqual(["issue-summary", "CORE-1"]);
  expect([...documentSummaryQuery("CORE", "runbook").queryKey]).toEqual([
    "doc-summary",
    "CORE",
    "runbook",
  ]);
});

/**
 * The person comes back to the tab. Resolves once the client has handled the focus: its focus
 * listener awaits `resumePausedMutations` before it refetches, so awaiting the same call after it
 * finds any refetch the focus starts already at its read.
 */
async function refocus(queryClient: QueryClient): Promise<void> {
  focusManager.setFocused(false);
  focusManager.setFocused(true);
  await queryClient.resumePausedMutations();
}

/**
 * Observes `options` as a mounted reference does, with `read` failing its first call: the failed
 * read is fetched again on focus, a healthy one is not, and a refresh that fails over the data it
 * held (an event's, which fetches the query directly) is fetched again on the next focus.
 */
async function expectFocusRecoversFailedRead<TData, TKey extends QueryKey>(
  options: QueryObserverOptions<TData, Error, TData, TData, TKey>,
  read: { mock: { calls: unknown[] }; mockRejectedValueOnce: (error: Error) => unknown }
): Promise<void> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.mount();
  read.mockRejectedValueOnce(new Error("503 Service Unavailable"));
  const observer = new QueryObserver(queryClient, options);
  const unsubscribe = observer.subscribe(() => {});
  try {
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("error"));
    expect(read.mock.calls).toHaveLength(1);

    await refocus(queryClient);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("success"));
    expect(read.mock.calls).toHaveLength(2);

    await refocus(queryClient);
    expect(read.mock.calls).toHaveLength(2);

    read.mockRejectedValueOnce(new Error("503 Service Unavailable"));
    await queryClient
      .getQueryCache()
      .find({ exact: true, queryKey: options.queryKey })
      ?.fetch()
      .catch(() => undefined);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("error"));
    expect(read.mock.calls).toHaveLength(3);
    await refocus(queryClient);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("success"));
    expect(read.mock.calls).toHaveLength(4);
  } finally {
    unsubscribe();
    queryClient.unmount();
  }
}

test("a summary or block read that failed is fetched again when the window regains focus, and a healthy one is not", async () => {
  const reads = spyReads();
  await expectFocusRecoversFailedRead(issueSummaryQuery("CORE-1"), reads.getIssueSummary);
  await expectFocusRecoversFailedRead(
    documentSummaryQuery("CORE", "runbook-md"),
    reads.getProjectDocumentSummary
  );
  await expectFocusRecoversFailedRead(
    blockQuery("dispatch://CORE-1/spec#intro"),
    reads.getReferences
  );
});

test("a message reference's read sits outside the issue details' key", () => {
  expect([...messageQuery("CORE-1", "m-1").queryKey]).toEqual(["issue-message", "CORE-1", "m-1"]);
});

test("hovering an issue warms the full issue and its spec text, which the hover card shows", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { key: "CORE-1", kind: "issue" });
  await expectKeys(queryClient, [
    ["issue", "CORE-1"],
    ["artifact", "spec-1", "text"],
  ]);
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
});

test("hovering an issue's document warms its issue summary and its text, not the full issue", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { key: "CORE-1", kind: "artifact", slug: "design" });
  await expectKeys(queryClient, [
    ["issue-summary", "CORE-1"],
    ["artifact", "doc-1", "text"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
});

test("hovering a project document warms its summary and the version it pins", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, {
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
    version: 3,
  });
  await expectKeys(queryClient, [
    ["doc-summary", "CORE", "runbook-md"],
    ["artifact", "doc-9", 3],
  ]);
  expect(reads.getProjectArtifact).not.toHaveBeenCalled();
});

test("hovering an ask, a comment or a message warms its own read and nothing of its issue or document", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { id: "ask-1", key: "CORE-1", kind: "ask" });
  prefetchReference(queryClient, { id: "comment-1", key: "CORE-1", kind: "comment" });
  prefetchReference(queryClient, { id: "m-1", key: "CORE-1", kind: "message" });
  prefetchReference(queryClient, {
    item: { id: "ask-2", kind: "ask" },
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
  });
  await expectKeys(queryClient, [
    ["ask", "ask-1"],
    ["comment", "comment-1"],
    ["issue-message", "CORE-1", "m-1"],
    ["ask", "ask-2"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
  expect(reads.getProjectDocumentSummary).not.toHaveBeenCalled();
});

test("hovering a block of a comment or message warms its graph read alone, no summary of a document", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, {
    block: { id: "b2" },
    id: "c-1",
    key: "CORE-1",
    kind: "comment",
  });
  prefetchReference(queryClient, {
    block: { id: "b1" },
    id: "m-1",
    key: "CORE-1",
    kind: "message",
  });
  prefetchReference(queryClient, {
    block: { id: "b3" },
    item: { id: "c-2", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
  });
  await expectKeys(queryClient, [
    ["references", "to", "dispatch://CORE-1/comment/c-1#b-b2"],
    ["references", "to", "dispatch://CORE-1/message/m-1#b-b1"],
    ["references", "to", "dispatch://CORE/artifact/runbook-md/comment/c-2#b-b3"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
  expect(reads.getProjectDocumentSummary).not.toHaveBeenCalled();
  expect(reads.getComment).not.toHaveBeenCalled();
  expect(reads.getMessage).not.toHaveBeenCalled();
});

/** A block's graph read: its current text, and the pin a pinned reference's read carries. */
function blockRead(text: string | undefined, pin?: NonNullable<GraphReferences["node"]["pin"]>) {
  return {
    edges: [],
    node: { block_id: "b2", id: "CORE-1/spec#b2", kind: "block", pin, text },
  } satisfies GraphReferences;
}

// A link to a block is titled by the block's text, from its graph read, which an event that can
// change the block fetches again. An edit that deletes the block makes the read answer 404: the
// link drops the title it showed and reads as a freshly loaded page's link (no title, so its
// short form). A pinned reference to the removed block answers 200 with the pin's `removed` state
// and keeps the pinned text; any other failure keeps the title it last read.
test.each([
  [
    "a comment block deleted by its author",
    { block: { id: "b2" }, id: "c-1", key: "CORE-1", kind: "comment" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a message block that is gone",
    { block: { id: "b2" }, id: "m-1", key: "CORE-1", kind: "message" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a spec block deleted by a document edit",
    { block: { id: "b2" }, key: "CORE-1", kind: "spec" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a spec block whose refresh failed with a 500",
    { block: { id: "b2" }, key: "CORE-1", kind: "spec" } as const,
    new ApiError(500, { code: "INTERNAL", error: "boom" }),
    "Beta as written.",
  ],
  [
    "a pinned comment block whose refresh answers it removed",
    { block: { id: "b2", pin: "1" }, id: "c-1", key: "CORE-1", kind: "comment" } as const,
    blockRead(undefined, {
      ref: "dispatch://CORE-1/comment/c-1#b-b2@v1",
      removed_version: 2,
      state: "removed",
      text: "Beta as written.",
      version: 1,
    }),
    "Beta as written.",
  ],
] as const)("the link to %s is titled as the refresh leaves it", async (_case, route, refresh, kept) => {
  const pin =
    "pin" in route.block
      ? {
          ref: "dispatch://CORE-1/comment/c-1#b-b2@v1",
          state: "unchanged" as const,
          text: "Beta as written.",
          version: 1,
        }
      : undefined;
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValueOnce(blockRead("Beta as written.", pin))
    .mockImplementationOnce(() =>
      refresh instanceof Error ? Promise.reject(refresh) : Promise.resolve(refresh)
    );
  restore = () => getReferences.mockRestore();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const hook = renderHook(() => useReferenceTarget(route, { text: false }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  try {
    await waitFor(() => expect(hook.result.current.title).toBe("Beta as written."));

    await act(() => queryClient.refetchQueries({ queryKey: ["references", "to"] }));
    expect(getReferences).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(hook.result.current.title).toBe(kept));
  } finally {
    hook.unmount();
  }
});
