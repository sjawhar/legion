import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { AGENT_LIST_REFILLING, ApiError, api } from "../../api/client";
import type { Agent, AskFollower, Subscriber } from "../../api/types";
import { AskRecipients, type AskRecipientsProps, reachLabel, reachRows } from "./AskRecipients";

const followers: AskFollower[] = [
  { session_id: "0123456789abcdef", since: "2026-09-14T10:00:00Z" },
  { session_id: "fedcba9876543210", since: "2026-09-14T10:05:00Z" },
];

function agent(sessionId: string, title: string): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "m",
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

async function renderRecipients({
  agents = [],
  props = {},
  removeFollower = async () => {},
  subscribers = [],
}: {
  /** An Error rejects the agent registry read, as an unreachable Envoy listener does. */
  agents?: Agent[] | Error;
  props?: Partial<AskRecipientsProps>;
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
  /** An Error rejects the subscribers fetch, as an unreachable Envoy listener does. */
  subscribers?: Subscriber[] | Error;
} = {}) {
  const listAgents = spyOn(api, "listAgents");
  if (agents instanceof Error) {
    listAgents.mockRejectedValue(agents);
  } else {
    listAgents.mockResolvedValue(agents);
  }
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
  // Each recipient is an `AgentName`, whose live link needs a router.
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskRecipients
          askId="ask-1"
          claimant={null}
          followers={followers}
          owner={{ artifact_id: null, issue_key: "CORE-1" }}
          removeFollower={removeFollower}
          route={null}
          routeHolder={null}
          routeStatus={null}
          {...props}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const user = userEvent.setup();
  // The routing list is folded at rest; every assertion below is about what it holds, so the
  // harness opens it once and each test reads the open list.
  await user.click(screen.getByRole("button", { name: /^Reaches/ }));
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
    user,
  };
}

test("AskRecipients folds its routing list at rest and still names the count", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskRecipients
          askId="ask-1"
          claimant={null}
          followers={followers}
          owner={{ artifact_id: null, issue_key: "CORE-1" }}
          removeFollower={async () => {}}
          route={null}
          routeHolder={null}
          routeStatus={null}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const toggle = screen.getByRole("button", { name: "Reaches 2" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("list", { name: "Followers" })).not.toBeInTheDocument();

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByRole("list", { name: "Followers" })).toBeTruthy();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getIssueSubscribers.mockRestore();
  }
});

test("AskRecipients names followers by registry title, else by shortened id, with the registry's running state and a live link", async () => {
  const page = await renderRecipients({ agents: [agent("0123456789abcdef", "Planner")] });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(within(section).getByRole("button", { name: /^Reaches/ }).textContent).toBe("Reaches 2");
    const planner = await within(section).findByText("Planner");
    expect(planner.closest("[title]")?.getAttribute("title")).toBe("0123456789abcdef");
    const items = within(section).getAllByRole("listitem");
    const running = items[0]?.querySelector('[data-agent-name="0123456789abcdef"]');
    const stopped = items[1]?.querySelector('[data-agent-name="fedcba9876543210"]');
    if (!(running instanceof HTMLElement) || !(stopped instanceof HTMLElement)) {
      throw new Error("a follower is not drawn by AgentName");
    }
    await waitFor(() => expect(running.dataset.agentState).toBe("running"));
    expect(within(running).getByRole("img", { name: "Running" })).toBeTruthy();
    expect(within(running).getByRole("link", { name: "Planner" })).toHaveAttribute(
      "href",
      "/agents/0123456789abcdef/live"
    );
    expect(stopped.dataset.agentState).toBe("not-running");
    expect(within(stopped).getByRole("img", { name: "Not running" })).toBeTruthy();
    expect(within(stopped).getByRole("link", { name: "session:fedcba98…" })).toBeTruthy();
  } finally {
    page.unmount();
  }
});

test("AskRecipients unfollows only after the human confirms, then refreshes the ask thread", async () => {
  const removed: string[][] = [];
  const page = await renderRecipients({
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

  const { user } = page;
  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const buttons = within(section).getAllByRole("button", { name: "Unfollow" });
    await user.click(buttons[1] as HTMLElement);
    const dialog = screen.getByRole("dialog", { name: "Unfollow" });
    expect(dialog).toHaveTextContent("Unfollow session:fedcba98… from this ask?");

    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(removed).toEqual([]);

    await user.click(buttons[1] as HTMLElement);
    await user.click(
      within(screen.getByRole("dialog", { name: "Unfollow" })).getByRole("button", {
        name: "Confirm",
      })
    );
    await waitFor(() => expect(removed).toEqual([["ask-1", "fedcba9876543210"]]));
    await waitFor(() => expect(invalidated).toContainEqual(["ask-thread", "ask-1"]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients shows a retryable error when unfollowing fails", async () => {
  let attempts = 0;
  const page = await renderRecipients({
    removeFollower: async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new Error("boom");
      }
    },
  });

  const { user } = page;
  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await user.click(
      within(section).getAllByRole("button", { name: "Unfollow" })[0] as HTMLElement
    );
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Confirm" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not unfollow.");

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(attempts).toBe(2));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
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
  const page = await renderRecipients({
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
      expect(within(section).getByRole("button", { name: /^Reaches/ }).textContent).toBe(
        "Reaches 3"
      )
    );
    expect(page.getIssueSubscribers).toHaveBeenCalledWith("CORE-1");
    expect(page.getArtifactSubscribers).not.toHaveBeenCalled();
    expect(section.querySelectorAll('[data-agent-name="0123456789abcdef"]')).toHaveLength(1);

    const viaIssue = within(section).getByRole("list", { name: "Via issue subscription" });
    const rows = within(viaIssue).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    const po = rows[0] as HTMLElement;
    const wide = rows[1] as HTMLElement;
    expect(within(po).getByText("Platform PO").closest("[title]")?.getAttribute("title")).toBe(
      "b0b0b0b0b0b0b0b0"
    );
    expect(within(wide).getByText("via notifications.dispatch.>")).toBeTruthy();
    expect(within(wide).queryByRole("button")).not.toBeInTheDocument();

    await page.user.click(within(po).getByRole("button", { name: "Unsubscribe" }));
    const dialog = screen.getByRole("dialog", { name: "Unsubscribe" });
    expect(dialog).toHaveTextContent("Unsubscribe Platform PO from CORE-1?");
    await page.user.click(within(dialog).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(unsubscribed).toEqual([["CORE-1", "b0b0b0b0b0b0b0b0"]]));
    expect(removed).toEqual([]);
    await waitFor(() => expect(invalidated).toContainEqual(["subscribers", "CORE-1"]));
  } finally {
    page.unmount();
    unsubscribeIssueSession.mockRestore();
  }
});

test("AskRecipients says when the subscribers fetch failed and retries it, instead of counting only followers", async () => {
  const page = await renderRecipients({ subscribers: new Error("listener unreachable") });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const alert = await within(section).findByRole("alert");
    expect(alert).toHaveTextContent("Subscribers unavailable — Envoy listener unreachable.");
    expect(page.getIssueSubscribers).toHaveBeenCalledTimes(1);

    page.getIssueSubscribers.mockResolvedValue([subscriber("b0b0b0b0b0b0b0b0", "Platform PO")]);
    await page.user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(within(section).getByRole("button", { name: /^Reaches/ }).textContent).toBe(
        "Reaches 3"
      )
    );
    expect(page.getIssueSubscribers).toHaveBeenCalledTimes(2);
    expect(within(section).queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

// While Envoy's agent list refills, the card says so - not that the listener is unreachable -
// counts nobody, and shows each follower's running state as unknown rather than not running.
test("AskRecipients says the agent list is refilling and claims no count or offline follower", async () => {
  const refilling = new ApiError(503, { code: "ENVOY_REFILLING", error: AGENT_LIST_REFILLING });
  const page = await renderRecipients({ agents: refilling, subscribers: refilling });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    const notice = await within(section).findByTestId("agent-list-refilling");
    expect(notice.textContent).toBe(AGENT_LIST_REFILLING);
    expect(within(section).queryByRole("alert")).not.toBeInTheDocument();
    expect(within(section).queryByRole("button", { name: /^Reaches/ })).not.toBeInTheDocument();
    expect(within(section).getByRole("button", { name: "Recipients" })).toBeTruthy();
    await waitFor(() =>
      expect(within(section).getAllByRole("img", { name: "Running state unknown" })).toHaveLength(2)
    );
    expect(within(section).queryByRole("img", { name: "Not running" })).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients tells the human an unfollowed session still hears the ask through its issue subscription", async () => {
  const page = await renderRecipients({
    subscribers: [subscriber("0123456789abcdef", "Planner")],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(page.getIssueSubscribers).toHaveBeenCalledWith("CORE-1"));
    await waitFor(() =>
      expect(within(section).getByRole("button", { name: /^Reaches/ }).textContent).toBe(
        "Reaches 2"
      )
    );
    const buttons = within(section).getAllByRole("button", { name: "Unfollow" });

    await page.user.click(buttons[0] as HTMLElement);
    const shared = screen.getByRole("dialog", { name: "Unfollow" });
    expect(shared).toHaveTextContent(
      "Unfollow session:01234567… from this ask? Its answer and replies still reach them through the issue subscription. They will be told."
    );
    await page.user.click(within(shared).getByRole("button", { name: "Cancel" }));

    await page.user.click(buttons[1] as HTMLElement);
    const only = screen.getByRole("dialog", { name: "Unfollow" });
    expect(only).toHaveTextContent(
      "Unfollow session:fedcba98… from this ask? Its answer and replies will no longer reach them. They will be told."
    );
  } finally {
    page.unmount();
  }
});

test("AskRecipients reads a project document's subscribers for a document-owned ask", async () => {
  const page = await renderRecipients({
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
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskRecipients
          askId="ask-1"
          claimant={null}
          followers={followers}
          owner={{ artifact_id: null, issue_key: "CORE-1" }}
          route={null}
          routeHolder={null}
          routeStatus={null}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const toggle = within(screen.getByRole("region", { name: "Recipients" })).getByRole("button", {
      name: /^Reaches/,
    });
    expect(toggle.textContent).toBe("Reaches 2");
    // Opening the list mounts the followers' names, which read the agent registry; once that read
    // has begun, the card's fetches have all started, and the one needing the `person` scope is
    // not among them.
    await user.click(toggle);
    await waitFor(() => expect(listAgents).toHaveBeenCalled());
    expect(getIssueSubscribers).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listAgents.mockRestore();
    getIssueSubscribers.mockRestore();
  }
});

const claimant = "c1c1c1c1c1c1c1c1";
const routed = "e2e2e2e2e2e2e2e2";
const holder = "d3d3d3d3d3d3d3d3";

function reachesText(section: HTMLElement): string | null {
  return within(section).getByRole("button", { name: /^Reaches/ }).textContent;
}

test("AskRecipients lists the issue's claimant via claim, drawn by AgentName, with no control", async () => {
  const page = await renderRecipients({
    agents: [agent(claimant, "Implementer")],
    props: { claimant },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const issueRows = within(section).getByRole("list", { name: "Via claim or route" });
    const row = within(issueRows).getByRole("listitem");
    expect(
      (await within(row).findByText("Implementer")).closest("[title]")?.getAttribute("title")
    ).toBe(claimant);
    expect(within(row).getByText("via claim")).toBeTruthy();
    expect(await within(row).findByRole("img", { name: "Running" })).toBeTruthy();
    expect(within(row).getByRole("link", { name: "Implementer" })).toHaveAttribute(
      "href",
      `/agents/${claimant}/live`
    );
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients lists a claimant that already follows once, as a follower", async () => {
  const page = await renderRecipients({ props: { claimant: "0123456789abcdef" } });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 2");
    expect(section.querySelectorAll('[data-agent-name="0123456789abcdef"]')).toHaveLength(1);
    expect(
      within(section).queryByRole("list", { name: "Via claim or route" })
    ).not.toBeInTheDocument();
    expect(within(section).queryByText("via claim")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients lists a session route's session via route", async () => {
  const page = await renderRecipients({ props: { route: `session:${routed}` } });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const row = within(within(section).getByRole("list", { name: "Via claim or route" })).getByRole(
      "listitem"
    );
    expect(
      within(row).getByText("session:e2e2e2e2…").closest("[title]")?.getAttribute("title")
    ).toBe(routed);
    expect(within(row).getByText("via route")).toBeTruthy();
    expect(await within(row).findByRole("img", { name: "Not running" })).toBeTruthy();
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients names a role route's holder via the route, live, and counts it", async () => {
  const page = await renderRecipients({
    agents: [{ ...agent(holder, "Reviewer"), roles: ["reviewer"] }],
    props: { route: "role:reviewer", routeHolder: holder },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const row = within(within(section).getByRole("list", { name: "Via claim or route" })).getByRole(
      "listitem"
    );
    expect(
      (await within(row).findByText("Reviewer")).closest("[title]")?.getAttribute("title")
    ).toBe(holder);
    expect(within(row).getByText("via route (role:reviewer)")).toBeTruthy();
    expect(await within(row).findByRole("img", { name: "Running" })).toBeTruthy();
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients lists a role route nobody holds as the route itself, held by nobody and uncounted", async () => {
  const page = await renderRecipients({
    props: { route: "role:reviewer", routeStatus: "no_holder" },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 2");
    const row = within(within(section).getByRole("list", { name: "Via claim or route" })).getByRole(
      "listitem"
    );
    expect(within(row).getByText("role:reviewer")).toHaveAttribute("title", "role:reviewer");
    expect(within(row).getByText("via route")).toBeTruthy();
    // A role is not an agent: no running state, the issue's vacancy marker instead.
    expect(within(row).queryByRole("img")).not.toBeInTheDocument();
    expect(within(row).getByTestId("issue-route-unreachable")).toHaveTextContent(
      "Nobody holds it right now"
    );
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients marks a role route the listener did not judge as reach unknown, without calling it offline or reaching nobody", async () => {
  const page = await renderRecipients({
    props: { route: "role:reviewer", routeStatus: "unknown" },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 2 + route (unknown)");
    const row = within(within(section).getByRole("list", { name: "Via claim or route" })).getByRole(
      "listitem"
    );
    expect(within(row).getByText("role:reviewer")).toHaveAttribute("title", "role:reviewer");
    expect(within(row).getByText("via route")).toBeTruthy();
    expect(within(row).getByText("Route reach unknown")).toHaveAttribute(
      "title",
      "The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route."
    );
    expect(within(row).queryByRole("img")).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients tells the human an unfollowed or unsubscribed session may still hear the ask through a role route whose holder is unknown", async () => {
  const maybe =
    "A person's answer and replies may still reach them through the route: Dispatch could not tell who holds it.";
  const page = await renderRecipients({
    props: { claimant: "0123456789abcdef", route: "role:reviewer", routeStatus: "unknown" },
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(reachesText(section)).toBe("Reaches 3 + route (unknown)"));
    const unfollow = within(section).getAllByRole("button", { name: "Unfollow" });

    await page.user.click(unfollow[0] as HTMLElement);
    const claimed = screen.getByRole("dialog", { name: "Unfollow" });
    expect(claimed).toHaveTextContent(
      "Unfollow session:01234567… from this ask? A person's answer and replies still reach them through the claim. They will be told."
    );
    await page.user.click(within(claimed).getByRole("button", { name: "Cancel" }));

    await page.user.click(unfollow[1] as HTMLElement);
    const follower = screen.getByRole("dialog", { name: "Unfollow" });
    expect(follower).toHaveTextContent(
      `Unfollow session:fedcba98… from this ask? ${maybe} They will be told.`
    );
    await page.user.click(within(follower).getByRole("button", { name: "Cancel" }));

    await page.user.click(within(section).getByRole("button", { name: "Unsubscribe" }));
    expect(screen.getByRole("dialog")).toHaveTextContent(`${maybe} They will be told.`);
  } finally {
    page.unmount();
  }

  // A role nobody holds and a session route the listener did not judge both say who receives
  // them, so a session they do not reach keeps today's text.
  for (const props of [
    { route: "role:reviewer", routeStatus: "no_holder" as const },
    { route: `session:${routed}`, routeStatus: "unknown" as const },
  ]) {
    const settled = await renderRecipients({ props });
    try {
      const section = screen.getByRole("region", { name: "Recipients" });
      await settled.user.click(
        within(section).getAllByRole("button", { name: "Unfollow" })[1] as HTMLElement
      );
      expect(screen.getByRole("dialog", { name: "Unfollow" })).toHaveTextContent(
        "Unfollow session:fedcba98… from this ask? Its answer and replies will no longer reach them. They will be told."
      );
    } finally {
      settled.unmount();
    }
  }
});

test("AskRecipients still counts a session route's session when its reach is unknown, without calling it offline", async () => {
  // The listener did not answer the registry read either, so the session's running state is
  // unknown too.
  const page = await renderRecipients({
    agents: new Error("listener unreachable"),
    props: { route: `session:${routed}`, routeStatus: "unknown" },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const row = within(within(section).getByRole("list", { name: "Via claim or route" })).getByRole(
      "listitem"
    );
    expect(
      within(row).getByText("session:e2e2e2e2…").closest("[title]")?.getAttribute("title")
    ).toBe(routed);
    expect(within(row).getByText("via route")).toBeTruthy();
    expect(within(row).getByText("Route reach unknown")).toBeTruthy();
    await waitFor(() => expect(page.queryClient.getQueryState(["agents"])?.status).toBe("error"));
    expect(within(row).getByRole("img", { name: "Running state unknown" })).toBeTruthy();
    expect(within(row).queryByRole("img", { name: "Not running" })).not.toBeInTheDocument();
    expect(within(row).queryByTestId("issue-route-unreachable")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients counts a role route's holder once when it also holds the claim", async () => {
  const page = await renderRecipients({
    agents: [{ ...agent(claimant, "Implementer"), roles: ["reviewer"] }],
    props: { claimant, route: "role:reviewer", routeHolder: claimant },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const rows = within(
      within(section).getByRole("list", { name: "Via claim or route" })
    ).getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(await within(rows[0] as HTMLElement).findByText("Implementer")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("via claim")).toBeTruthy();
    expect(within(section).queryByText("role:reviewer")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients tells the human an unfollowed or unsubscribed role holder still hears the ask through the route", async () => {
  const page = await renderRecipients({
    props: { route: "role:reviewer", routeHolder: "fedcba9876543210" },
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(reachesText(section)).toBe("Reaches 3"));
    expect(
      within(section).queryByRole("list", { name: "Via claim or route" })
    ).not.toBeInTheDocument();
    const unfollow = within(section).getAllByRole("button", { name: "Unfollow" });
    await page.user.click(unfollow[1] as HTMLElement);
    const routedDialog = screen.getByRole("dialog", { name: "Unfollow" });
    expect(routedDialog).toHaveTextContent(
      "Unfollow session:fedcba98… from this ask? A person's answer and replies still reach them through the route. They will be told."
    );
    await page.user.click(within(routedDialog).getByRole("button", { name: "Cancel" }));
  } finally {
    page.unmount();
  }

  const subscribed = await renderRecipients({
    props: { route: "role:reviewer", routeHolder: "b0b0b0b0b0b0b0b0" },
    subscribers: [subscriber("b0b0b0b0b0b0b0b0", "Platform PO")],
  });
  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(reachesText(section)).toBe("Reaches 3"));
    expect(
      within(section).queryByRole("list", { name: "Via claim or route" })
    ).not.toBeInTheDocument();
    await subscribed.user.click(within(section).getByRole("button", { name: "Unsubscribe" }));
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "A person's answer and replies still reach them through the route. They will be told."
    );
  } finally {
    subscribed.unmount();
  }
});

test("AskRecipients lists a route session that is also the claimant once, via claim", async () => {
  const page = await renderRecipients({ props: { claimant, route: `session:${claimant}` } });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 3");
    const rows = within(
      within(section).getByRole("list", { name: "Via claim or route" })
    ).getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(within(rows[0] as HTMLElement).getByText("via claim")).toBeTruthy();
    expect(within(section).queryByText("via route")).not.toBeInTheDocument();
  } finally {
    page.unmount();
  }
});

test("AskRecipients lists a subscribed claimant or route session once, as a subscriber that keeps Unsubscribe", async () => {
  const page = await renderRecipients({
    props: { claimant, route: `session:${routed}` },
    subscribers: [
      subscriber(claimant, "Implementer"),
      subscriber(routed, "Routed"),
      subscriber("b0b0b0b0b0b0b0b0", "Platform PO"),
    ],
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    await waitFor(() => expect(reachesText(section)).toBe("Reaches 5"));
    expect(section.querySelectorAll(`[data-agent-name="${claimant}"]`)).toHaveLength(1);
    expect(section.querySelectorAll(`[data-agent-name="${routed}"]`)).toHaveLength(1);
    expect(
      within(section).queryByRole("list", { name: "Via claim or route" })
    ).not.toBeInTheDocument();
    const viaIssue = within(section).getByRole("list", { name: "Via issue subscription" });
    expect(within(viaIssue).getAllByRole("listitem")).toHaveLength(3);
    const buttons = within(viaIssue).getAllByRole("button", { name: "Unsubscribe" });
    expect(buttons).toHaveLength(3);

    await page.user.click(buttons[0] as HTMLElement);
    const claimed = screen.getByRole("dialog");
    expect(claimed).toHaveTextContent(
      "A person's answer and replies still reach them through the claim. They will be told."
    );
    await page.user.click(within(claimed).getByRole("button", { name: "Cancel" }));

    await page.user.click(buttons[1] as HTMLElement);
    const routedDialog = screen.getByRole("dialog");
    expect(routedDialog).toHaveTextContent(
      "A person's answer and replies still reach them through the route. They will be told."
    );
    await page.user.click(within(routedDialog).getByRole("button", { name: "Cancel" }));

    await page.user.click(buttons[2] as HTMLElement);
    const plain = screen.getByRole("dialog");
    expect(plain.textContent).not.toContain("still reach");
  } finally {
    page.unmount();
  }
});

test("AskRecipients tells the human an unfollowed claimant or route session still hears a person's answer", async () => {
  const page = await renderRecipients({
    props: { claimant: "0123456789abcdef", route: "session:fedcba9876543210" },
  });

  try {
    const section = screen.getByRole("region", { name: "Recipients" });
    expect(reachesText(section)).toBe("Reaches 2");
    const buttons = within(section).getAllByRole("button", { name: "Unfollow" });

    await page.user.click(buttons[0] as HTMLElement);
    const claimed = screen.getByRole("dialog", { name: "Unfollow" });
    expect(claimed).toHaveTextContent(
      "Unfollow session:01234567… from this ask? A person's answer and replies still reach them through the claim. They will be told."
    );
    await page.user.click(within(claimed).getByRole("button", { name: "Cancel" }));

    await page.user.click(buttons[1] as HTMLElement);
    const routedDialog = screen.getByRole("dialog", { name: "Unfollow" });
    expect(routedDialog).toHaveTextContent(
      "Unfollow session:fedcba98… from this ask? A person's answer and replies still reach them through the route. They will be told."
    );
  } finally {
    page.unmount();
  }
});

const roleRoute = { route: "role:reviewer", routeHolder: holder };
const noReach = {
  claimant: null,
  followers: [],
  route: null,
  routeHolder: null,
  routeStatus: null,
  subscribers: [],
};

test("reachRows counts a role route held by the claimant once, under the claim", () => {
  const reach = reachRows({ ...noReach, ...roleRoute, claimant: holder });
  expect(reach.issueRows).toEqual([{ id: holder, reaches: true, via: "via claim" }]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBe(holder);
});

test("reachRows counts a role route held by a follower once, as the follower", () => {
  const reach = reachRows({
    ...noReach,
    ...roleRoute,
    followers: [{ session_id: holder, since: "2026-09-14T10:00:00Z" }],
  });
  expect(reach.issueRows).toEqual([]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBe(holder);
});

test("reachRows counts a role route held by a subscriber once, as the subscriber", () => {
  const reach = reachRows({ ...noReach, ...roleRoute, subscribers: [subscriber(holder, "R")] });
  expect(reach.issueRows).toEqual([]);
  expect(reach.subscribers.map((row) => row.session_id)).toEqual([holder]);
  expect(reach.count).toBe(1);
});

test("reachRows names a role route's holder beside the route when nothing else lists it", () => {
  const reach = reachRows({ ...noReach, ...roleRoute, claimant });
  expect(reach.issueRows).toEqual([
    { id: claimant, reaches: true, via: "via claim" },
    { id: holder, reaches: true, via: "via route (role:reviewer)" },
  ]);
  expect(reach.count).toBe(2);
});

test("reachRows lists a role route nobody holds as the bare route, reaching and counting nobody", () => {
  const reach = reachRows({ ...noReach, claimant, route: "role:reviewer" });
  expect(reach.issueRows).toEqual([
    { id: claimant, reaches: true, via: "via claim" },
    { id: "role:reviewer", reaches: false, via: "via route" },
  ]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBeUndefined();
});

test("reachRows counts an offline session route's own session, which the listener does not name", () => {
  const reach = reachRows({ ...noReach, route: `session:${routed}` });
  expect(reach.issueRows).toEqual([{ id: routed, reaches: true, via: "via route" }]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBe(routed);
  // Live, the server names the same session as the route's holder: still one row.
  expect(reachRows({ ...noReach, route: `session:${routed}`, routeHolder: routed })).toEqual(reach);
});

test("reachRows lists a role route the listener did not judge as the bare route, uncounted and flagged unknown", () => {
  const reach = reachRows({ ...noReach, claimant, route: "role:reviewer", routeStatus: "unknown" });
  expect(reach.issueRows).toEqual([
    { id: claimant, reaches: true, via: "via claim" },
    { id: "role:reviewer", reaches: false, reachUnknown: true, via: "via route" },
  ]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBeUndefined();
  expect(reach.unknownRoute).toBe(true);
  expect(reachLabel(reach)).toBe("Reaches 1 + route (unknown)");
});

test("reachRows keeps counting a session route's session when its reach is unknown", () => {
  const reach = reachRows({ ...noReach, route: `session:${routed}`, routeStatus: "unknown" });
  expect(reach.issueRows).toEqual([
    { id: routed, reaches: true, reachUnknown: true, via: "via route" },
  ]);
  expect(reach.count).toBe(1);
  expect(reach.routeSession).toBe(routed);
  expect(reach.unknownRoute).toBe(false);
  expect(reachLabel(reach)).toBe("Reaches 1");
});

test("reachRows reads a role route nobody holds as reaching nobody, not as unknown", () => {
  const reach = reachRows({ ...noReach, route: "role:reviewer", routeStatus: "no_holder" });
  expect(reach.issueRows).toEqual([{ id: "role:reviewer", reaches: false, via: "via route" }]);
  expect(reach.unknownRoute).toBe(false);
  expect(reachLabel(reach)).toBe("Reaches 0");
});
