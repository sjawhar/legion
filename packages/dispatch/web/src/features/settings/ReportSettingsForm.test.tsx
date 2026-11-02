import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { ReportSettings } from "../../api/types";
import { ReportSettingsForm } from "./ReportSettingsForm";

const projects = [
  { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  { created_at: "2026-09-10T00:00:00Z", key: "OPS", name: "Operations", open_asks: 0 },
];

const stored: ReportSettings = {
  agent_route: "role:dispatch-fixer",
  project: "OPS",
  updated_at: "2026-11-01T12:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

function renderForm(settings: ReportSettings) {
  const read = spyOn(api, "getReportSettings").mockResolvedValue(settings);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ReportSettingsForm />
    </QueryClientProvider>
  );
  return {
    queryClient,
    restore: () => {
      cleanup();
      read.mockRestore();
      listProjects.mockRestore();
    },
  };
}

test("the form starts from where reports go now", async () => {
  const view = renderForm(stored);
  try {
    await waitFor(() =>
      expect((screen.getByLabelText("Project for reports") as HTMLSelectElement).value).toBe("OPS")
    );
    expect((screen.getByLabelText("Agent role") as HTMLInputElement).value).toBe(
      "role:dispatch-fixer"
    );
  } finally {
    view.restore();
  }
});

test("saving sends the project and the role, an empty role as null, and refreshes the record", async () => {
  const view = renderForm({ agent_route: null, project: null, updated_at: null, updated_by: null });
  const put = spyOn(api, "putReportSettings").mockResolvedValue(stored);
  const invalidate = spyOn(view.queryClient, "invalidateQueries");
  try {
    await screen.findByRole("option", { name: "CORE · Core" });
    fireEvent.change(screen.getByLabelText("Project for reports"), { target: { value: "CORE" } });
    fireEvent.click(screen.getByRole("button", { name: "Save report settings" }));
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(put).toHaveBeenCalledWith({ agent_route: null, project: "CORE" }, keyedAttempt);
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["report-settings"] }));
  } finally {
    put.mockRestore();
    view.restore();
  }
});

test("a role the server refuses shows its reason", async () => {
  const view = renderForm({
    agent_route: null,
    project: "CORE",
    updated_at: null,
    updated_by: null,
  });
  const put = spyOn(api, "putReportSettings").mockRejectedValue(
    new ApiError(400, {
      code: "REPORT_SETTINGS_INPUT",
      error: "agent_route names a session, whose id changes when the agent restarts",
    })
  );
  try {
    await screen.findByRole("option", { name: "CORE · Core" });
    fireEvent.change(screen.getByLabelText("Agent role"), {
      target: { value: " session:0123456789abcdef " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save report settings" }));
    expect(await screen.findByText(/whose id changes when the agent restarts/)).toBeTruthy();
    expect(put).toHaveBeenCalledWith(
      { agent_route: "session:0123456789abcdef", project: "CORE" },
      keyedAttempt
    );
  } finally {
    put.mockRestore();
    view.restore();
  }
});
