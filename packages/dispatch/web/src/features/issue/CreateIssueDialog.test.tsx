import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { CreateIssueInput, Issue } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { CreateIssueDialog } from "./CreateIssueDialog";

const projects = [
  { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  { created_at: "2026-09-10T00:00:00Z", key: "OPS", name: "Operations", open_asks: 0 },
];

function issue(input: CreateIssueInput, key: string): Issue {
  return {
    closed_at: null,
    created_at: "2026-09-14T00:00:00Z",
    external_links: [],
    key,
    labels: input.labels ?? [],
    pinned: false,
    priority: null,
    project: input.project,
    rank: "a",
    route: null,
    status: "triage",
    title: input.title,
    updated_at: "2026-09-14T00:00:00Z",
  } as unknown as Issue;
}

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{location.pathname}</output>;
}

function renderDialog(
  create: (input: CreateIssueInput) => Promise<Issue>,
  path = "/",
  onClose = () => {}
) {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <CreateIssueDialog createIssue={create} onClose={onClose} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    unmount: () => {
      view.unmount();
      queryClient.clear();
      listProjects.mockRestore();
    },
  };
}

test("preselects the route's project, submits title and spec, and opens the created issue", async () => {
  const calls: CreateIssueInput[] = [];
  let closed = 0;
  const view = renderDialog(
    async (input) => {
      calls.push(input);
      return issue(input, "OPS-7");
    },
    "/projects/OPS",
    () => (closed += 1)
  );
  try {
    const select = await screen.findByRole<HTMLSelectElement>("combobox", { name: "Project" });
    await waitFor(() => expect(select.value).toBe("OPS"));
    const create = screen.getByRole("button", { name: "Create" });
    expect(create.hasAttribute("disabled")).toBe(true);

    fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
      target: { value: "  Ship the keymap  " },
    });
    expect(create.hasAttribute("disabled")).toBe(false);
    fireEvent.change(screen.getByRole("textbox", { name: /Spec/ }), {
      target: { value: "One line of spec." },
    });
    fireEvent.click(create);

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/OPS-7")
    );
    expect(calls).toEqual([
      { project: "OPS", spec: "One line of spec.", title: "Ship the keymap" },
    ]);
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("a failed create keeps the dialog and draft, shows the server message, and retries", async () => {
  let attempts = 0;
  const view = renderDialog(async (input) => {
    attempts += 1;
    if (attempts === 1) {
      throw new ApiError(500, { error: "database unavailable" });
    }
    return issue(input, "CORE-3");
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
      target: { value: "Flaky create" },
    });
    fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("database unavailable");
    expect(screen.getByRole<HTMLInputElement>("textbox", { name: "Title" }).value).toBe(
      "Flaky create"
    );

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-3")
    );
    expect(attempts).toBe(2);
  } finally {
    view.unmount();
  }
});

test("a possible duplicate names the candidate and Create anyway forces the create", async () => {
  const calls: CreateIssueInput[] = [];
  const view = renderDialog(async (input) => {
    calls.push(input);
    if (input.force !== true) {
      throw new ApiError(409, {
        code: "POSSIBLE_DUPLICATE",
        error: "possible duplicate of CORE-1: Calibrate the astrolabe",
      });
    }
    return issue(input, "CORE-2");
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
      target: { value: "Astrolabe calibration" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("CORE-1");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create anyway" }));

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-2")
    );
    expect(calls.map((call) => call.force)).toEqual([undefined, true]);
  } finally {
    view.unmount();
  }
});
