import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { Agent, AskFollower, Subscriber } from "../../api/types";
import { AskRecipients, type AskRecipientsProps } from "./AskRecipients";

const followers: AskFollower[] = [
  { session_id: "0123456789abcdef", since: "2026-09-14T10:00:00Z" },
  { session_id: "fedcba9876543210", since: "2026-09-14T10:05:00Z" },
];

function agent(sessionId: string, title: string): Agent {
  return {
    capabilities: [],
    dir: "/w",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "m",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

function subscriber(sessionId: string, title: string, extra: Partial<Subscriber> = {}): Subscriber {
  return {
    last_seen: Date.now(),
    live: true,
    removable: true,
    session_id: sessionId,
    title,
    topics: ["notifications.dispatch.issue.CORE-1.>"],
    ...extra,
  };
}

function renderRecipients({
  agents = [],
  props = {},
  removeFollower = async () => {},
  subscribers = [],
}: {
  agents?: Agent[];
  props?: Partial<AskRecipientsProps>;
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
  /** An Error rejects the subscribers fetch, as an unreachable Envoy listener does. */
  subscribers?: Subscriber[] | Error;
} = {}) {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers");
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers");
  if (subscribers instanceof Error) {
    getIssueSubscribers.mockRejectedValue(subscribers);
    getArtifactSubscribers.mockRejectedValue(subscribers);
  } else {
    getIssueSubscribers.mockResolvedValue(subscribers);
    getArtifactSubscribers.mockResolvedValue(subscribers);
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <AskRecipients
        askId="ask-1"
        followers={followers}
        owner={{ artifact_id: null, issue_key: "CORE-1" }}
        removeFollower={removeFollower}
        {...props}
      />
    </QueryClientProvider>
  );
  return {
    getArtifactSubscribers,
    getIssueSubscribers,
    queryClient,
    unmount: () => {
      view.unmount();
      listAgents.mockRestore();
      getIssueSubscribers.mockRestore();
      getArtifactSubscribers.mockRestore();
    },
  };
}

test("AskRecipients names live followers by title and offline ones by shortened id", async () => {
  const page = renderRecipients({ agents: [agent("0123456789abcdef", "Planner")] });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(within(section).getByRole("heading", { level: 3 }).textContent).toBe("Reaches 2");
    const planner = await within(section).findByText("Planner");
    expect(planner.getAttribute("title")).toBe("0123456789abcdef");
    const items = within(section).getAllByRole("listitem");
    expect(within(items[0] as HTMLElement).getByTitle("Live")).toBeTruthy();
    expect(within(items[1] as HTMLElement).getByText("session:fedcba98…")).toBeTruthy();
    expect(within(items[1] as HTMLElement).getByTitle("Not live")).toBeTruthy();
  } finally {
    page.unmount();
  }
});

test("AskRecipients unfollows only after the human confirms, then refreshes the ask thread", async () => {
  const removed: string[][] = [];
  const page = renderRecipients({
    removeFollower: async (askId, sessionId) => {
      removed.push([askId, sessionId]);
    },
  });
  const invalidated: unknown[] = [];
  const original = page.queryClient.invalidateQueries.bind(page.queryClient);
  page.queryClient.invalidateQueries = (filters, ...rest) => {
    invalidated.push(filters?.queryKey);
    return original(filters, ...rest);
  };

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const buttons = within(section).getAllByRole("button", { name: "Unfollow" });
    fireEvent.click(buttons[1] as HTMLElement);
    const dialog = screen.getByRole("dialog", { name: "Unfollow" });
    expect(dialog.textContent).toContain("Unfollow session:fedcba98… from this ask?");

    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(removed).toEqual([]);

    fireEvent.click(buttons[1] as HTMLElement);
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Unfollow" })).getByRole("button", {
        name: "Confirm",
      })
    );
    await waitFor(() => expect(removed).toEqual([["ask-1", "fedcba9876543210"]]));
    await waitFor(() => expect(invalidated).toContainEqual(["ask-thread", "ask-1"]));
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    page.unmount();
  }
});

test("AskRecipients shows a retryable error when unfollowing fails", async () => {
  let attempts = 0;
  const page = renderRecipients({
    removeFollower: async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new Error("boom");
      }
    },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    fireEvent.click(within(section).getAllByRole("button", { name: "Unfollow" })[0] as HTMLElement);
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Could not unfollow.");

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(attempts).toBe(2));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  } finally {
    page.unmount();
  }
});

test("AskRecipients adds the issue's subscribers once each, removes one through the issue route, and leaves a wide topic alone", async () => {
  const unsubscribed: string[][] = [];
  const unsubscribeIssueSession = spyOn(api, "unsubscribeIssueSession").mockImplementation(
    async (key, sessionId) => {
      unsubscribed.push([key, sessionId]);
    }
  );
  const removed: string[][] = [];
  const page = renderRecipients({
    props: { followers: [followers[0] as AskFollower] },
    removeFollower: async (askId, sessionId) => {
      removed.push([askId, sessionId]);
    },
    subscribers: [
      // The follower again: the ask already reaches it, so it is not listed twice.
      subscriber("0123456789abcdef", "Planner"),
      subscriber("b0b0b0b0b0b0b0b0", "Platform PO"),
      subscriber("c0c0c0c0c0c0c0c0", "Everything", {
        removable: false,
        topics: ["notifications.dispatch.>"],
        via: "notifications.dispatch.>",
      }),
    ],
  });
  const invalidated: unknown[] = [];
  const original = page.queryClient.invalidateQueries.bind(page.queryClient);
  page.queryClient.invalidateQueries = (filters, ...rest) => {
    invalidated.push(filters?.queryKey);
    return original(filters, ...rest);
  };

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() =>
      expect(within(section).getByRole("heading", { level: 3 }).textContent).toBe("Reaches 3")
    );
    expect(page.getIssueSubscribers).toHaveBeenCalledWith("CORE-1");
    expect(page.getArtifactSubscribers).not.toHaveBeenCalled();
    expect(within(section).getAllByTitle("0123456789abcdef")).toHaveLength(1);

    const viaIssue = within(section).getByRole("list", { name: "Via issue subscription" });
    const rows = within(viaIssue).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    const po = rows[0] as HTMLElement;
    const wide = rows[1] as HTMLElement;
    expect(within(po).getByText("Platform PO").getAttribute("title")).toBe("b0b0b0b0b0b0b0b0");
    expect(within(wide).getByText("via notifications.dispatch.>")).toBeTruthy();
    expect(within(wide).queryByRole("button")).toBeNull();

    fireEvent.click(within(po).getByRole("button", { name: "Unsubscribe" }));
    const dialog = screen.getByRole("dialog", { name: "Unsubscribe" });
    expect(dialog.textContent).toContain("Unsubscribe Platform PO from CORE-1?");
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(unsubscribed).toEqual([["CORE-1", "b0b0b0b0b0b0b0b0"]]));
    expect(removed).toEqual([]);
    await waitFor(() => expect(invalidated).toContainEqual(["subscribers", "CORE-1"]));
  } finally {
    page.unmount();
    unsubscribeIssueSession.mockRestore();
  }
});

test("AskRecipients says when the subscribers fetch failed and retries it, instead of counting only followers", async () => {
  const page = renderRecipients({ subscribers: new Error("listener unreachable") });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const alert = await within(section).findByRole("alert");
    expect(alert.textContent).toContain("Subscribers unavailable — Envoy listener unreachable.");
    expect(page.getIssueSubscribers).toHaveBeenCalledTimes(1);

    page.getIssueSubscribers.mockResolvedValue([subscriber("b0b0b0b0b0b0b0b0", "Platform PO")]);
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(within(section).getByRole("heading", { level: 3 }).textContent).toBe("Reaches 3")
    );
    expect(page.getIssueSubscribers).toHaveBeenCalledTimes(2);
    expect(within(section).queryByRole("alert")).toBeNull();
  } finally {
    page.unmount();
  }
});

test("AskRecipients tells the human an unfollowed session still hears the ask through its issue subscription", async () => {
  const page = renderRecipients({
    subscribers: [subscriber("0123456789abcdef", "Planner")],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(page.getIssueSubscribers).toHaveBeenCalledWith("CORE-1"));
    await waitFor(() =>
      expect(within(section).getByRole("heading", { level: 3 }).textContent).toBe("Reaches 2")
    );
    const buttons = within(section).getAllByRole("button", { name: "Unfollow" });

    fireEvent.click(buttons[0] as HTMLElement);
    const shared = screen.getByRole("dialog", { name: "Unfollow" });
    expect(shared.textContent).toContain(
      "Unfollow session:01234567… from this ask? Its answer and replies still reach them through the issue subscription. They will be told."
    );
    fireEvent.click(within(shared).getByRole("button", { name: "Cancel" }));

    fireEvent.click(buttons[1] as HTMLElement);
    const only = screen.getByRole("dialog", { name: "Unfollow" });
    expect(only.textContent).toContain(
      "Unfollow session:fedcba98… from this ask? Its answer and replies will no longer reach them. They will be told."
    );
  } finally {
    page.unmount();
  }
});

test("AskRecipients reads a project document's subscribers for a document-owned ask", async () => {
  const page = renderRecipients({
    props: { owner: { artifact_id: "artifact-9", issue_key: null } },
    subscribers: [subscriber("d0d0d0d0d0d0d0d0", "Doc watcher")],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const viaDocument = await within(section).findByRole("list", {
      name: "Via document subscription",
    });
    expect(within(viaDocument).getByText("Doc watcher")).toBeTruthy();
    expect(page.getArtifactSubscribers).toHaveBeenCalledWith("artifact-9");
    expect(page.getIssueSubscribers).not.toHaveBeenCalled();
  } finally {
    page.unmount();
  }
});

test("AskRecipients never asks for subscribers unless the signed-in principal is a user", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <AskRecipients
        askId="ask-1"
        followers={followers}
        owner={{ artifact_id: null, issue_key: "CORE-1" }}
      />
    </QueryClientProvider>
  );

  try {
    expect(
      within(screen.getByRole("region", { name: "Recipients" })).getByRole("heading", { level: 3 })
        .textContent
    ).toBe("Reaches 2");
    // The agents fetch the same render starts has settled; the human-only one never began.
    await waitFor(() => expect(listAgents).toHaveBeenCalled());
    expect(getIssueSubscribers).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getIssueSubscribers.mockRestore();
  }
});
