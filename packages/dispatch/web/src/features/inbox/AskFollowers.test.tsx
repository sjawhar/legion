import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { Agent, AskFollower } from "../../api/types";
import { AskFollowers } from "./AskFollowers";

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

function renderFollowers(
  removeFollower: (askId: string, sessionId: string) => Promise<void>,
  agents: Agent[]
) {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <AskFollowers askId="ask-1" followers={followers} removeFollower={removeFollower} />
    </QueryClientProvider>
  );
  return {
    queryClient,
    unmount: () => {
      view.unmount();
      listAgents.mockRestore();
    },
  };
}

test("AskFollowers names live followers by title and offline ones by shortened id", async () => {
  const page = renderFollowers(async () => {}, [agent("0123456789abcdef", "Planner")]);

  try {
    const section = screen.getByRole("region", { name: "Followers" });
    expect(within(section).getByRole("heading", { level: 3 }).textContent).toBe("Followed by 2");
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

test("AskFollowers unfollows only after the human confirms, then refreshes the ask thread", async () => {
  const removed: string[][] = [];
  const page = renderFollowers(async (askId, sessionId) => {
    removed.push([askId, sessionId]);
  }, []);
  const invalidated: unknown[] = [];
  const original = page.queryClient.invalidateQueries.bind(page.queryClient);
  page.queryClient.invalidateQueries = (filters, ...rest) => {
    invalidated.push(filters?.queryKey);
    return original(filters, ...rest);
  };

  try {
    const section = screen.getByRole("region", { name: "Followers" });
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

test("AskFollowers shows a retryable error when unfollowing fails", async () => {
  let attempts = 0;
  const page = renderFollowers(async () => {
    attempts += 1;
    if (attempts === 1) {
      throw new Error("boom");
    }
  }, []);

  try {
    const section = screen.getByRole("region", { name: "Followers" });
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
