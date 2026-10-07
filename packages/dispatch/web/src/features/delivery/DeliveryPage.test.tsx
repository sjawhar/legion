import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { DeliverySettings, DeliveryTimelineResponse } from "../../api/types";
import { DeliveryPage } from "./DeliveryPage";

// The list view, so the page renders without the timeline chart, whose library needs a canvas.
const DELIVERY_URL =
  "/delivery?mode=list&from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

const emptyTimeline: DeliveryTimelineResponse = {
  freshness: {
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    unfetchable_count: 0,
  },
  prs: [],
  runs: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

const notConfigured = new ApiError(404, {
  code: "DELIVERY_NOT_CONFIGURED",
  error: "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery",
});

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[DELIVERY_URL]}>
      <QueryClientProvider client={queryClient}>
        <DeliveryPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

test("an unconfigured timeline shows the setup form in its place, reading no settings, and a save brings the timeline", async () => {
  const saved: DeliverySettings = {
    deploy_repo: "acme/widgets",
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    population_authors: ["octocat"],
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_name: "widgets-release",
    updated_at: "2026-10-07T12:00:00Z",
    updated_by: { id: "alice@example.com", kind: "user" },
  };
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockResolvedValue(emptyTimeline);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    renderPage();

    expect(
      await screen.findByRole("heading", { name: "Set up the delivery timeline" })
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Show timeline" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    // The timeline's DELIVERY_NOT_CONFIGURED already says no record is stored: the form's fields
    // are there at once, empty, with no read of the record and no loading line.
    expect(screen.queryByText("Loading delivery settings…")).toBeNull();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe("");

    fireEvent.change(screen.getByLabelText("Deploy repository"), {
      target: { value: "acme/widgets" },
    });
    fireEvent.change(screen.getByLabelText("Deploy workflow"), {
      target: { value: ".github/workflows/deploy.yml" },
    });
    fireEvent.change(screen.getByLabelText("Production job"), {
      target: { value: "widgets-release" },
    });
    fireEvent.change(screen.getByLabelText("PR checks workflow"), {
      target: { value: ".github/workflows/pr-checks.yml" },
    });
    fireEvent.change(screen.getByLabelText("Population authors"), {
      target: { value: "octocat" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));

    expect(await screen.findByText("No PRs match the current filters.")).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a refetch of the unconfigured timeline, as tab focus starts, keeps the setup form and its unsaved draft", async () => {
  const refetch = Promise.withResolvers<DeliveryTimelineResponse>();
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockReturnValueOnce(refetch.promise);

  try {
    const queryClient = renderPage();
    await screen.findByRole("heading", { name: "Set up the delivery timeline" });
    fireEvent.change(screen.getByLabelText("Deploy repository"), {
      target: { value: "acme/widgets" },
    });

    // Coming back to the tab (TanStack's focus listener is on `window`) refetches the timeline,
    // and while that refetch runs the query holds no error at all. Any render of the timeline's
    // body in between would unmount the form, so the draft surviving says none happened.
    window.dispatchEvent(new Event("visibilitychange"));
    await waitFor(() => expect(queryClient.isFetching()).toBe(1));
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe(
      "acme/widgets"
    );
    expect(screen.queryByRole("button", { name: "Show timeline" })).toBeNull();
    expect(screen.queryByText("Loading delivery timeline…")).toBeNull();

    refetch.reject(notConfigured);
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(screen.getByRole("heading", { name: "Set up the delivery timeline" })).toBeDefined();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe(
      "acme/widgets"
    );
    expect(screen.queryByRole("button", { name: "Show timeline" })).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a refetch of the unconfigured timeline that fails otherwise shows that failure and Retry, not the setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "database unavailable" }));

  try {
    renderPage();
    await screen.findByRole("heading", { name: "Set up the delivery timeline" });

    window.dispatchEvent(new Event("visibilitychange"));

    expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a configured timeline renders with no setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue(emptyTimeline);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderPage();

    expect(await screen.findByText("No PRs match the current filters.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Show timeline" })).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    await waitFor(() => expect(getDeliveryTimeline).toHaveBeenCalledTimes(1));
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
    getDeliverySettings.mockRestore();
  }
});

test("any other timeline failure keeps the page and offers Retry, not the setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockRejectedValue(
    new ApiError(503, { code: "INTERNAL", error: "database unavailable" })
  );

  try {
    renderPage();

    expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});
