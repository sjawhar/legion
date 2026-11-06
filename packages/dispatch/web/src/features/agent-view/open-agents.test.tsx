import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { useOpenAgents, usePaneShare } from "./open-agents";
import { DEFAULT_PANE_SHARE } from "./workspace-width";

const ALICE: AuthenticatedUser = { event_head: "0", kind: "user", login: "alice" };
const SESSIONS = ["ses-1", "ses-2", "ses-3"];
const openKey = userPreferenceStorageKey("alice", "agents.open");
const pinnedKey = userPreferenceStorageKey("alice", "agents.pinned");
const shareKey = userPreferenceStorageKey("alice", "agents.pane-share");

function OpenAgentsProbe({ from = 0, to = 0 }: { from?: number; to?: number }) {
  const { closeAgent, open, openAgent, reorder } = useOpenAgents();
  return (
    <div>
      <span data-testid="open-list">{open.join(" ")}</span>
      {SESSIONS.map((sessionId) => (
        <span key={sessionId}>
          <button onClick={() => openAgent(sessionId)} type="button">
            {`open ${sessionId}`}
          </button>
          <button onClick={() => closeAgent(sessionId)} type="button">
            {`close ${sessionId}`}
          </button>
        </span>
      ))}
      <button onClick={() => reorder(from, to)} type="button">
        reorder
      </button>
    </div>
  );
}

function PaneShareProbe({ share: wanted }: { share: number }) {
  const [share, setShare] = usePaneShare();
  return (
    <div>
      <span data-testid="share">{String(share)}</span>
      <button onClick={() => setShare(wanted)} type="button">
        set
      </button>
    </div>
  );
}

function press(name: string): void {
  fireEvent.click(screen.getByRole("button", { name }));
}

function openList(): string {
  return screen.getByTestId("open-list").textContent ?? "";
}

/** A client whose identity has already answered, for the cases that are not about the wait. */
function signedInClient(): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], ALICE);
  return queryClient;
}

test("agents opened before the identity answers are kept, and join what that login had stored", async () => {
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  window.localStorage.setItem(openKey, JSON.stringify(["ses-3"]));
  const view = render(
    <QueryClientProvider client={queryClient}>
      <OpenAgentsProbe />
    </QueryClientProvider>
  );

  try {
    press("open ses-1");
    press("open ses-2");
    expect(openList()).toBe("ses-1 ses-2");
    // Nothing is written for an identity that has not answered (`userPreference.ts`).
    expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-3"]));

    await act(async () => whoAmIResult.resolve(ALICE));
    await waitFor(() =>
      expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-3", "ses-1", "ses-2"]))
    );
    expect(openList()).toBe("ses-3 ses-1 ses-2");
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
  }
});

test("an agent closed again before the identity answers is not written back", async () => {
  const whoAmIResult = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(whoAmIResult.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  window.localStorage.removeItem(openKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <OpenAgentsProbe />
    </QueryClientProvider>
  );

  try {
    press("open ses-1");
    press("open ses-2");
    press("close ses-1");
    expect(openList()).toBe("ses-2");

    await act(async () => whoAmIResult.resolve(ALICE));
    await waitFor(() =>
      expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-2"]))
    );
    expect(openList()).toBe("ses-2");
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
  }
});

test("opening an agent the list holds adds no second row and leaves its place", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(ALICE);
  const queryClient = signedInClient();
  window.localStorage.removeItem(openKey);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <OpenAgentsProbe />
    </QueryClientProvider>
  );

  try {
    press("open ses-1");
    press("open ses-2");
    press("open ses-1");
    expect(openList()).toBe("ses-1 ses-2");
    expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-1", "ses-2"]));
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
  }
});

test("closing an agent drops its row and leaves that login's pinned agents untouched", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(ALICE);
  const queryClient = signedInClient();
  const pinned = JSON.stringify(["ses-1", "ses-9"]);
  window.localStorage.setItem(openKey, JSON.stringify(["ses-1", "ses-2"]));
  window.localStorage.setItem(pinnedKey, pinned);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <OpenAgentsProbe />
    </QueryClientProvider>
  );

  try {
    press("close ses-1");
    expect(openList()).toBe("ses-2");
    expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-2"]));
    expect(window.localStorage.getItem(pinnedKey)).toBe(pinned);
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
    window.localStorage.removeItem(pinnedKey);
  }
});

test("a row dragged to another place is moved and kept, and an index off the list is ignored", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(ALICE);
  const queryClient = signedInClient();
  const stored = JSON.stringify(["ses-1", "ses-2", "ses-3"]);
  window.localStorage.setItem(openKey, stored);
  const probe = (from: number, to: number) => (
    <QueryClientProvider client={queryClient}>
      <OpenAgentsProbe from={from} to={to} />
    </QueryClientProvider>
  );
  const view = render(probe(0, 2));

  try {
    press("reorder");
    expect(openList()).toBe("ses-2 ses-3 ses-1");
    expect(window.localStorage.getItem(openKey)).toBe(JSON.stringify(["ses-2", "ses-3", "ses-1"]));

    view.rerender(probe(2, 0));
    press("reorder");
    expect(openList()).toBe("ses-1 ses-2 ses-3");

    for (const [from, to] of [
      [0, 3],
      [3, 0],
      [-1, 1],
      [1, -1],
    ]) {
      view.rerender(probe(from, to));
      press("reorder");
      expect(openList()).toBe("ses-1 ses-2 ses-3");
    }
    expect(window.localStorage.getItem(openKey)).toBe(stored);
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
  }
});

test("a stored value that is no JSON array of session ids reads as the ids alone", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(ALICE);
  const queryClient = signedInClient();

  try {
    for (const [stored, expected] of [
      ['{"a":1}', ""],
      ["not json", ""],
      ['["ses-1",2,"ses-2"]', "ses-1 ses-2"],
    ]) {
      window.localStorage.setItem(openKey, stored);
      const view = render(
        <QueryClientProvider client={queryClient}>
          <OpenAgentsProbe />
        </QueryClientProvider>
      );
      try {
        expect([stored, openList()]).toEqual([stored, expected]);
      } finally {
        view.unmount();
      }
    }
  } finally {
    whoAmI.mockRestore();
    window.localStorage.removeItem(openKey);
  }
});

test("the pane share answers the default for text that is no number, and is held inside 0 and 1", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(ALICE);
  const queryClient = signedInClient();
  window.localStorage.setItem(shareKey, "wide");
  const probe = (share: number) => (
    <QueryClientProvider client={queryClient}>
      <PaneShareProbe share={share} />
    </QueryClientProvider>
  );
  const view = render(probe(0.7));

  try {
    expect(screen.getByTestId("share").textContent).toBe(String(DEFAULT_PANE_SHARE));

    press("set");
    expect(screen.getByTestId("share").textContent).toBe("0.7");
    expect(window.localStorage.getItem(shareKey)).toBe("0.7");

    view.rerender(probe(1.4));
    press("set");
    expect(screen.getByTestId("share").textContent).toBe("1");
    expect(window.localStorage.getItem(shareKey)).toBe("1");

    view.rerender(probe(-0.3));
    press("set");
    expect(screen.getByTestId("share").textContent).toBe("0");
    expect(window.localStorage.getItem(shareKey)).toBe("0");
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    window.localStorage.removeItem(shareKey);
  }
});
