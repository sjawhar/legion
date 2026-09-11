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
      project: "CORE",
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
          <Route path="/issues/:key/*" element={<IssuePage />} />
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

async function openTitleEditor(): Promise<HTMLInputElement> {
  const existing = screen.queryByLabelText("Issue title");
  if (existing instanceof HTMLInputElement) {
    return existing;
  }
  fireEvent.click(await screen.findByRole("heading", { level: 1 }));
  return (await screen.findByLabelText("Issue title")) as HTMLInputElement;
}

async function openRouteEditor(): Promise<HTMLInputElement> {
  const existing = screen.queryByLabelText("Route");
  if (existing instanceof HTMLInputElement) {
    return existing;
  }
  fireEvent.click(await screen.findByRole("button", { name: /No route|Messages also reach/ }));
  return (await screen.findByLabelText("Route")) as HTMLInputElement;
}

test("IssuePage keeps an unsaved route draft when a stale refetch arrives", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    fireEvent.click(await screen.findByText("No route — messages stay on the issue"));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

    fireEvent.change(route, { target: { value: "" } });
    await waitFor(() => expect(route.value).toBe(""));
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
    fireEvent.click(await screen.findByText("No route — messages stay on the issue"));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

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
    fireEvent.click(await screen.findByText("No route — messages stay on the issue"));
    let route = await openRouteEditor();
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    route = await openRouteEditor();
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
    fireEvent.click(await screen.findByText("No route — messages stay on the issue"));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.click(saveRoute);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.route).toBe("role:a")
    );
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

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

test("IssuePage ignores a same-task duplicate title save", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "Guarded title" } });
    fireEvent.blur(title);
    fireEvent.blur(title);

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Guarded title" });
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores same-task duplicate route saves", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const route = await openRouteEditor();
    const saveRoute = screen.getByRole("button", { name: "Save route" });
    const routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(route, { target: { value: "role:guarded" } });
    fireEvent.submit(routeForm);
    fireEvent.submit(routeForm);

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:guarded" });
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores a same-task duplicate status save", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    fireEvent.change(status, { target: { value: "in_progress" } });
    fireEvent.change(status, { target: { value: "in_progress" } });

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "in_progress" });
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage preserves an edit made after a failed title save and retries that draft", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "a" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }));

    title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "ab" } });
    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(title.value).toBe("ab");

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "ab" }));
    expect(title.value).toBe("ab");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not retry an invalid route draft after a failed save", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    let routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.submit(routeForm);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    route = await openRouteEditor();
    fireEvent.change(route, { target: { value: "not-a-route" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    });
    expect(patchIssue).toHaveBeenCalledTimes(1);

    saveRoute = screen.getByRole("button", { name: "Save route" });
    routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(route, { target: { value: "role:b" } });
    fireEvent.submit(routeForm);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:b" }));
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not retry an empty title draft after a failed save", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "a" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }));

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    });
    expect(patchIssue).toHaveBeenCalledTimes(1);

    fireEvent.change(title, { target: { value: "b" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "b" }));
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage queues a title draft edited while a save is pending", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "a" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }));

    title = await openTitleEditor();
    fireEvent.change(title, { target: { value: "ab" } });
    await act(async () => {});
    fireEvent.blur(title);
    act(() => {
      firstSave.resolve({ ...issue, title: "a" });
    });

    await waitFor(() =>
      expect(patchIssue.mock.calls).toEqual([
        ["CORE-1", { title: "a" }],
        ["CORE-1", { title: "ab" }],
      ])
    );
    expect(title.value).toBe("ab");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage queues a route draft edited while a save is pending", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    let routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.submit(routeForm);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });
    routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }

    fireEvent.change(route, { target: { value: "role:b" } });
    await act(async () => {});
    fireEvent.submit(routeForm);
    act(() => {
      firstSave.resolve({ ...issue, route: "role:a" });
    });

    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:b" }));
    expect(route.value).toBe("role:b");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage drains a title draft after a different issue write settles", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    const title = await openTitleEditor();
    fireEvent.change(status, { target: { value: "in_progress" } });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "in_progress" })
    );

    fireEvent.change(title, { target: { value: "Queued title" } });
    fireEvent.blur(title);
    act(() => {
      firstSave.resolve({ ...issue, status: "in_progress" });
    });

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Queued title" })
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not submit a route that was typed but never saved", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const statusSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => statusSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    const route = await openRouteEditor();
    fireEvent.change(status, { target: { value: "in_progress" } });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "in_progress" })
    );

    fireEvent.change(route, { target: { value: "role:legion" } });
    act(() => {
      statusSave.resolve({ ...issue, status: "in_progress" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(route.value).toBe("role:legion");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries an unchanged failed title draft", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    fireEvent.click(await screen.findByRole("heading", { level: 1, name: "Review the spec" }));
    const title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    fireEvent.change(title, { target: { value: "a" } });
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }));

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(title.value).toBe("a");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }));
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries an unchanged failed route draft", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const route = await openRouteEditor();
    const routeForm = screen.getByRole("button", { name: "Save route" }).closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(route, { target: { value: "role:a" } });
    fireEvent.submit(routeForm);
    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(route.value).toBe("role:a");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }));
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries a failed drained title before sending the queued route", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const statusSave = Promise.withResolvers<Issue>();
  const titleSave = Promise.withResolvers<Issue>();
  patchIssue
    .mockImplementationOnce(() => statusSave.promise)
    .mockImplementationOnce(() => titleSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    fireEvent.change(status, { target: { value: "in_progress" } });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "in_progress" })
    );

    const title = await openTitleEditor();
    const route = await openRouteEditor();
    const routeForm = screen.getByRole("button", { name: "Save route" }).closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    fireEvent.change(title, { target: { value: "Queued title" } });
    fireEvent.blur(title);
    fireEvent.change(route, { target: { value: "role:queued" } });
    fireEvent.submit(routeForm);

    act(() => {
      statusSave.resolve({ ...issue, status: "in_progress" });
    });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Queued title" })
    );
    titleSave.reject(new Error("offline"));
    await screen.findByRole("alert");
    expect(patchIssue).toHaveBeenCalledTimes(2);

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:queued" })
    );
    expect(patchIssue).toHaveBeenCalledTimes(4);
  } finally {
    unmount();
    restore();
  }
});
