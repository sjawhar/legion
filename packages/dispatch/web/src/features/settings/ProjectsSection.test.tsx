import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ApiError, api } from "../../api/client";
import type { Project } from "../../api/types";
import { ProjectsSection } from "./ProjectsSection";

const core: Project = { created_at: "2026-09-01T00:00:00Z", key: "CORE", name: "Core" };

// Dispatch made the project and lost its answer (`409 KEY_COMMITTED`): the form lets go of the key
// and name and says what was made, the list is read again, and a second press - with nothing in
// the form - creates nothing, where the old draft would have tried the same project again.
test("a create answered KEY_COMMITTED clears the form, says the project was made, and a second press sends nothing", async () => {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const createProject = spyOn(api, "createProject").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    await screen.findByRole("cell", { name: "CORE" });
    const key = screen.getByLabelText<HTMLInputElement>("Key");
    fireEvent.change(key, { target: { value: "qa" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Quality" } });
    fireEvent.click(screen.getByRole("button", { name: "New project" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "Project QA was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(key.value).toBe("");
    expect(screen.getByLabelText<HTMLInputElement>("Name").value).toBe("");
    expect(screen.queryByRole("alert")).toBeNull();
    await waitFor(() => expect(listProjects.mock.calls.length).toBeGreaterThan(1));

    fireEvent.submit(key.closest("form") as HTMLFormElement);
    await Bun.sleep(20);
    expect(createProject).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    createProject.mockRestore();
  }
});
