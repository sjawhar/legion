import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Artifact, Comment, IssueDetails, MessageRead } from "../../api/types";
import { ItemTimestamp } from "./ItemTimestamp";
import type { DispatchReferenceRoute } from "./routes";

const at = "2026-09-10T12:00:00.000Z";
const alice = { id: "alice", kind: "user" } as const;
const message: DispatchReferenceRoute = { id: "message-1", key: "CORE-1", kind: "message" };
const read: MessageRead = {
  message: {
    author: alice,
    body: "Status?",
    created_at: at,
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
  },
  replies: [],
};
const spec: Artifact = {
  created_at: at,
  created_by: alice,
  id: "artifact-spec",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  slug: "spec",
  versions: [],
};
const issue: IssueDetails = {
  artifacts: [spec],
  assignee: null,
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: at,
  created_by: alice,
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  open_asks: [],
  parent: null,
  primary_artifact_id: spec.id,
  priority: null,
  progress: { children: null, tasks: null },
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  status: "in_progress",
  title: "Sextant calibration",
  updated_at: at,
};

let getIssue: ReturnType<typeof spyOn<typeof api, "getIssue">>;
beforeEach(() => {
  getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
});
afterEach(() => {
  getIssue.mockRestore();
});

/** The address bar: the router's current path and query. */
function AddressBar(): ReactNode {
  const location = useLocation();
  return <output data-testid="address">{`${location.pathname}${location.search}`}</output>;
}

function renderTime(
  route: DispatchReferenceRoute | undefined,
  start = "/issues/CORE-1/conversation"
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[start]}>
      <QueryClientProvider client={queryClient}>
        <ItemTimestamp at={at} route={route} />
        <AddressBar />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

const address = () => screen.getByTestId("address").textContent;

test("an item's time links to the item's own address and still shows the full date on hover", () => {
  const { view } = renderTime(message);
  try {
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/messages/message-1");
    const time = link.querySelector("time");
    expect(time?.getAttribute("dateTime")).toBe(at);
    expect(time?.getAttribute("title")).toBe(new Date(at).toLocaleString());
  } finally {
    view.unmount();
  }
});

test("a click reads the item, then puts its address in the address bar", async () => {
  const pending = Promise.withResolvers<MessageRead>();
  const getMessage = spyOn(api, "getMessage").mockReturnValue(pending.promise);
  const { queryClient, view } = renderTime(message);
  try {
    expect(fireEvent.click(screen.getByRole("link"))).toBe(false);
    await waitFor(() => expect(getMessage).toHaveBeenCalledWith("CORE-1", "message-1"));
    // Until the read lands the page stays where it is, so the issue page never shows its
    // "Loading issue…" over the conversation the reader is in.
    expect(address()).toBe("/issues/CORE-1/conversation");

    pending.resolve(read);
    await waitFor(() => expect(address()).toBe("/issues/CORE-1/messages/message-1"));
    expect(queryClient.getQueryData<MessageRead>(["issue-message", "CORE-1", "message-1"])).toEqual(
      read
    );
  } finally {
    view.unmount();
    getMessage.mockRestore();
  }
});

test("a comment beside its document lands on the document's own address, never through a redirect", async () => {
  const comment: Comment = {
    anchor: {
      artifact_id: spec.id,
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "the horizon mirror",
      version: 1,
    },
    ask_id: null,
    author: alice,
    body: "Two arc-minutes out.",
    created_at: at,
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
  };
  const getComment = spyOn(api, "getComment").mockResolvedValue({ comment, replies: [] });
  const { view } = renderTime(
    { id: "comment-1", key: "CORE-1", kind: "comment" },
    "/issues/CORE-1/spec"
  );
  try {
    const link = screen.getByRole("link");
    // Copy link keeps the comment's own address, which lands wherever the comment lives.
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/comments/comment-1");
    fireEvent.click(link);
    await waitFor(() => expect(address()).toBe("/issues/CORE-1/spec?comment=comment-1"));
  } finally {
    view.unmount();
    getComment.mockRestore();
  }
});

test("a click whose read fails still opens the address, which says what went wrong", async () => {
  const getMessage = spyOn(api, "getMessage").mockRejectedValue(new Error("network error"));
  const { view } = renderTime(message);
  try {
    fireEvent.click(screen.getByRole("link"));
    await waitFor(() => expect(address()).toBe("/issues/CORE-1/messages/message-1"));
  } finally {
    view.unmount();
    getMessage.mockRestore();
  }
});

test("Cmd, Ctrl and Shift clicks are the browser's, as on any link", () => {
  const getMessage = spyOn(api, "getMessage").mockResolvedValue(read);
  const { view } = renderTime(message);
  try {
    for (const modifier of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }]) {
      // Not prevented: the browser opens the link's address in a new tab or window.
      expect(fireEvent.click(screen.getByRole("link"), modifier)).toBe(true);
    }
    expect(getMessage).not.toHaveBeenCalled();
    expect(address()).toBe("/issues/CORE-1/conversation");
  } finally {
    view.unmount();
    getMessage.mockRestore();
  }
});

test("a project document's comment opens on its document with no item read", async () => {
  const getComment = spyOn(api, "getComment");
  const { view } = renderTime({
    item: { id: "comment-1", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "notes",
  });
  try {
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe("/projects/CORE/documents/notes?comment=comment-1");
    fireEvent.click(link);
    await waitFor(() => expect(address()).toBe("/projects/CORE/documents/notes?comment=comment-1"));
    expect(getComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getComment.mockRestore();
  }
});

test("an item with no address shows its time as plain text", () => {
  const { view } = renderTime(undefined);
  try {
    expect(screen.queryByRole("link")).toBeNull();
    expect(view.container.querySelector("time")?.getAttribute("dateTime")).toBe(at);
  } finally {
    view.unmount();
  }
});
