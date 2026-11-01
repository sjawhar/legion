import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { userPreferenceStorageKey, useUserPreference } from "./userPreference";

function PreferenceProbe() {
  const [view, setView] = useUserPreference(
    "project.issue-view",
    (stored) => (stored === "board" ? "board" : "list"),
    (next) => next
  );
  return (
    <button onClick={() => setView(view === "list" ? "board" : "list")} type="button">
      {view}
    </button>
  );
}

test("a user preference stays at its caller default until login, then follows that login's storage", async () => {
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const aliceKey = userPreferenceStorageKey("alice", "project.issue-view");
  const bobKey = userPreferenceStorageKey("bob", "project.issue-view");
  const undefinedKey = userPreferenceStorageKey("undefined", "project.issue-view");
  window.localStorage.setItem(aliceKey, "board");
  window.localStorage.setItem(bobKey, "list");
  window.localStorage.removeItem(undefinedKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PreferenceProbe />
    </QueryClientProvider>
  );

  try {
    expect(screen.getByRole("button").textContent).toBe("list");
    fireEvent.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("board");
    expect(window.localStorage.getItem(undefinedKey)).toBeNull();

    await act(async () => whoAmIResult.resolve({ event_head: "0", kind: "user", login: "alice" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("board"));

    fireEvent.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("list");

    act(() => queryClient.setQueryData(["whoami"], { kind: "user", login: "bob" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("list"));
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(aliceKey);
    window.localStorage.removeItem(bobKey);
    window.localStorage.removeItem(undefinedKey);
  }
});
