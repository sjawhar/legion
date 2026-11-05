import { afterAll, afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxShadowReport } from "../../api/types";
import { InboxShadowPage } from "./InboxShadowPage";

const getShadow = spyOn(api, "getInboxShadow");
const who = spyOn(api, "whoAmI").mockResolvedValue({
  kind: "user",
  login: "alice",
  event_head: "0",
});
afterEach(() => getShadow.mockReset());
afterAll(() => {
  getShadow.mockRestore();
  who.mockRestore();
});

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { kind: "user", login: "alice", event_head: "0" });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <InboxShadowPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    ...view,
    client,
    close: () => {
      view.unmount();
      client.clear();
    },
  };
}

const clean: InboxShadowReport = {
  checked_at: "2026-11-04T09:00:00Z",
  horizon: 10,
  active_users: 2,
  checked_users: 2,
  uncovered_users: 0,
  mismatched: 0,
  daily_mismatched: 1,
  errors: 0,
  missing: {},
  unexpected: {},
  expansions: { mention: 2 },
  ready: false,
  complete_zero_days: 0,
  users: [
    {
      person: "alice",
      checked_items: 3,
      mismatched: 0,
      errors: 0,
      missing: {},
      unexpected: {},
      expansions: {},
    },
  ],
};

test("a clean comparison retains a bad day and shows own counts beside global coverage", async () => {
  getShadow.mockResolvedValue(clean);
  const view = show();
  try {
    await waitFor(() =>
      expect(view.getByTestId("inbox-shadow-mismatches").textContent).toBe("0 differences")
    );
    expect(view.getByText("Not ready to replace your inbox")).toBeTruthy();
    expect(view.getByText(/1 differences recorded today/)).toBeTruthy();
    expect(view.getByRole("link", { name: "Try unified inbox" }).getAttribute("href")).toBe(
      "/inbox/unified"
    );
    expect(view.getByText("Your comparison and coverage")).toBeTruthy();
    expect(view.getByText("alice")).toBeTruthy();
    expect(view.queryByText("bob")).toBeNull();
    expect(view.queryByLabelText("Filter people")).toBeNull();
    expect(view.getByText(/Asks and replies only/)).toBeTruthy();
  } finally {
    view.close();
  }
});

test("a failed comparison never reports a false zero or complete coverage", async () => {
  getShadow.mockRejectedValue(new Error("database unavailable"));
  const view = show();
  try {
    await waitFor(() =>
      expect(view.getByRole("alert").textContent).toContain("database unavailable")
    );
    expect(view.queryByTestId("inbox-shadow-mismatches")).toBeNull();
    expect(view.queryByText("All active people checked")).toBeNull();
  } finally {
    view.close();
  }
});

test("a failed refresh hides an earlier clean comparison instead of claiming current zero", async () => {
  getShadow.mockResolvedValueOnce(clean).mockRejectedValue(new Error("refresh unavailable"));
  const view = show();
  try {
    await waitFor(() =>
      expect(view.getByTestId("inbox-shadow-mismatches").textContent).toBe("0 differences")
    );
    await act(async () => {
      await view.client.invalidateQueries({ queryKey: ["inbox-shadow"] });
    });
    await waitFor(() =>
      expect(view.getByRole("alert").textContent).toContain("refresh unavailable")
    );
    expect(view.queryByTestId("inbox-shadow-mismatches")).toBeNull();
    expect(view.queryByText("All active people checked")).toBeNull();
  } finally {
    view.close();
  }
});
