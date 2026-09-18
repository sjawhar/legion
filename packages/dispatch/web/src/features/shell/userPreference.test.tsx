import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { userPreferenceStorageKey, useUserPreference } from "./userPreference";

function PreferenceProbe() {
  const [view, setView] = useUserPreference(
    "inbox.view",
    (stored) => (stored === "everyone" ? "everyone" : "mine"),
    (next) => next
  );
  return (
    <button onClick={() => setView(view === "mine" ? "everyone" : "mine")} type="button">
      {view}
    </button>
  );
}

test("a user preference stays at its caller default until login, then follows that login's storage", async () => {
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const aliceKey = userPreferenceStorageKey("alice", "inbox.view");
  const bobKey = userPreferenceStorageKey("bob", "inbox.view");
  const anonymousKey = userPreferenceStorageKey("anonymous", "inbox.view");
  window.localStorage.setItem(aliceKey, "everyone");
  window.localStorage.setItem(bobKey, "mine");
  window.localStorage.removeItem(anonymousKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PreferenceProbe />
    </QueryClientProvider>
  );

  try {
    expect(screen.getByRole("button").textContent).toBe("mine");
    fireEvent.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("everyone");
    expect(window.localStorage.getItem(anonymousKey)).toBeNull();

    await act(async () => whoAmIResult.resolve({ kind: "user", login: "alice" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("everyone"));

    fireEvent.click(screen.getByRole("button"));
    expect(window.localStorage.getItem(aliceKey)).toBe("mine");

    act(() => queryClient.setQueryData(["whoami"], { kind: "user", login: "bob" }));
    await waitFor(() => expect(screen.getByRole("button").textContent).toBe("mine"));
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(aliceKey);
    window.localStorage.removeItem(bobKey);
    window.localStorage.removeItem(anonymousKey);
  }
});
