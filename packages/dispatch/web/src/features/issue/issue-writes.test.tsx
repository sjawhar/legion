import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { Issue, IssueDetails } from "../../api/types";
import { IssuePage } from "./IssuePage";

const issue: IssueDetails = {
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "doc",
      name: "spec.md",
      primary: true,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  parent: null,
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((completion) => {
    resolve = completion;
  });
  return { promise, resolve };
}

function renderIssuePage() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route
            path="/issues/:key/*"
            element={<IssuePage user={{ kind: "user", login: "alice" }} />}
          />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

function narrowIssue(details: IssueDetails): Issue {
  const { artifacts: _artifacts, children: _children, open_asks: _openAsks, ...rest } = details;
  return rest;
}

function stubIssueApi() {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false },
  });
  // The server answers a PATCH with the narrow `Issue`, not `IssueDetails`: no artifacts,
  // children, or open asks. The stub mirrors that so the page is exercised against the
  // real response shape.
  const patchIssue = spyOn(api, "patchIssue").mockImplementation(async (_key, update) =>
    narrowIssue({ ...issue, ...update })
  );

  return {
    getIssue,
    patchIssue,
    restore: () => {
      getIssue.mockRestore();
      getIssueEvents.mockRestore();
      getInbox.mockRestore();
      getMyState.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

test("IssuePage keeps an unsaved route draft when a stale refetch arrives", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    const route = (await screen.findByLabelText("Route")) as HTMLInputElement;
    const saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));

    fireEvent.change(route, { target: { value: "" } });
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, route: "role:a" }));
    });
    await waitFor(() => expect(route.value).toBe(""));

    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }));
    expect(route.value).toBe("");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps a route cleared while the previous save was still in flight", async () => {
  // The CI route-clear flake: the user clears the field while the earlier PATCH is
  // pending, and its response lands before React flushes the passive effects of the
  // clear. The success callback must see the cleared draft, not a stale mirror.
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<IssueDetails>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    const route = (await screen.findByLabelText("Route")) as HTMLInputElement;
    const saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));

    await act(async () => {
      fireEvent.change(route, { target: { value: "" } });
      firstSave.resolve({ ...issue, route: "role:a" });
      await firstSave.promise;
      await Promise.resolve();
    });

    expect(route.value).toBe("");
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }));
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps the document and a route draft across a save response", async () => {
  // A PATCH response is the narrow `Issue`. Applying it wholesale to the cached
  // `IssueDetails` drops `artifacts`, which swaps the whole page body for the
  // missing-document error and remounts the form on the next refetch with the
  // server's route in place of whatever the user had typed since.
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const route = (await screen.findByLabelText("Route")) as HTMLInputElement;
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    fireEvent.change(route, { target: { value: "" } });

    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: "Save route" }) as HTMLButtonElement).disabled
      ).toBe(false)
    );
    expect(screen.queryByText("Could not load this issue's primary document.")).toBeNull();
    expect(screen.getByLabelText("Route")).toBe(route);
    expect(route.value).toBe("");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores a stale route refetch after a newer successful save", async () => {
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();
  const staleRefetch = deferred<IssueDetails>();

  try {
    const route = (await screen.findByLabelText("Route")) as HTMLInputElement;
    const saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.route).toBe("role:a")
    );

    getIssue.mockImplementationOnce(() => staleRefetch.promise);
    act(() => {
      void queryClient.invalidateQueries({ queryKey: ["issue", "CORE-1"] });
    });
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));

    fireEvent.change(route, { target: { value: "" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }));
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.route).toBe("")
    );

    await act(async () => {
      staleRefetch.resolve({ ...issue, route: "role:a" });
    });
    await waitFor(() => expect(route.value).toBe(""));
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps an unsaved title draft when a stale refetch arrives", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<IssueDetails>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    fireEvent.click(await screen.findByRole("heading", { level: 1, name: "Review the spec" }));
    let title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    fireEvent.change(title, { target: { value: "A" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "A" }));

    // Blur exits click-to-edit mode; re-enter it to start a second, still-unsaved edit
    // before the first save's response arrives.
    fireEvent.click(await screen.findByRole("heading", { level: 1, name: "A" }));
    title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    fireEvent.change(title, { target: { value: "Edited locally" } });
    await act(async () => {
      firstSave.resolve({ ...issue, title: "A" });
    });
    await waitFor(() => expect(title.value).toBe("Edited locally"));

    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Edited locally" })
    );
    expect(title.value).toBe("Edited locally");
  } finally {
    unmount();
    restore();
  }
});
