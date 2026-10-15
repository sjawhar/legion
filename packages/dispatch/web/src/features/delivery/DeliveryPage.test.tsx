import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import {
  errorFrame,
  type FakeDeliverySocket,
  type FakeDeliverySockets,
  installFakeDeliverySockets,
  snapshotFrame,
} from "../../__tests__/delivery-live-harness";
import { api } from "../../api/client";
import type {
  DeliveryLiveServerFrame,
  DeliveryLiveSubscribe,
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPR,
  DeliverySettings,
  DeliveryTimelineResponse,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DeliveryPage } from "./DeliveryPage";

// The list view, so the page renders without the timeline chart, whose library needs a canvas.
const DELIVERY_URL =
  "/delivery?mode=list&from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";
/** The custom window DELIVERY_URL names, as the page subscribes it. */
const URL_WINDOW = { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" };

const emptyCounts = {
  repo: {},
  parent_agent: {},
  session: {},
  issue: {},
  priority: {},
  component: {},
  author: {},
  rework: {},
  deployed: {},
};

const emptyFreshness: DeliveryTimelineResponse["freshness"] = {
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  unfetchable_count: 0,
};

const NOT_CONFIGURED = "DELIVERY_NOT_CONFIGURED";
const notConfiguredMessage =
  "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery";

/** The measures panel's answer for the page's window: nothing in it, so every test that is not
 *  about the panel renders it without asserting on it. */
const emptyMeasures: DeliveryMeasuresResponse = {
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
  computed_at: "2024-06-02T00:00:00Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 0,
      deploys_with_prs: 0,
      per_day: 0,
      with_prs_per_day: 0,
    },
    lead_time: {
      merge_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      first_commit_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_merge: { median_minutes: null, p90_minutes: null, max_minutes: null },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 0, rate: 0 },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 0,
        rate: 0,
        confirmed_or_pending: 0,
        upper_bound_rate: 0,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0,
    deploy_run_success: { concluded: 0, reached_production: 0, cancelled: 0, rate: null },
    daily: [],
  },
  flags_source: "none",
  unowned_p0: [],
  targets: {
    deploys_per_day: 20,
    change_failure_rate: 0.05,
    merge_to_production_minutes: 45,
    opened_to_merge_median_minutes: 60,
    deploy_run_success_rate: 0.9,
    unowned_p0: 0,
  },
  status: {
    deploys_per_day: false,
    change_failure_rate: null,
    change_failure_rate_upper_bound: null,
    merge_to_production_median: null,
    merge_to_production: null,
    opened_to_merge: null,
    deploy_run_success: null,
    unowned_p0: true,
  },
  freshness: emptyFreshness,
};

const emptyTimeline: DeliveryTimelineResponse = {
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {},
  facet_counts: emptyCounts,
  freshness: emptyFreshness,
  issue_titles: {},
  measures: emptyMeasures,
  prs: [],
  runs: [],
  session_groups: [],
  session_pr_counts: {},
  session_titles: {},
  waiting: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

/** The router's query string, where a test reads what the page wrote to the URL. */
function LocationProbe() {
  return <output data-testid="location">{useLocation().search}</output>;
}

function renderPage(url = DELIVERY_URL) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[url]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DeliveryPage />
          <LocationProbe />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

function currentSearch(): URLSearchParams {
  return new URLSearchParams(screen.getByTestId("location").textContent ?? "");
}

let sockets: FakeDeliverySockets;
beforeEach(() => {
  sockets = installFakeDeliverySockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
});

/** The page's delivery socket, once the page has opened one, as Dispatch accepts it. */
async function openSocket(): Promise<FakeDeliverySocket> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The newest subscribe frame of `kind` the page has sent on `socket`, once it has sent one. */
function subscription(
  socket: FakeDeliverySocket,
  kind: DeliveryLiveView["kind"]
): Promise<DeliveryLiveSubscribe> {
  return waitFor(() => socket.subscribed(kind));
}

/** The newest subscribe frame of `view`'s kind, once it subscribes `view`. */
function subscriptionTo(
  socket: FakeDeliverySocket,
  view: DeliveryLiveView
): Promise<DeliveryLiveSubscribe> {
  return waitFor(() => {
    const frame = socket.subscribed(view.kind);
    expect(frame.view).toEqual(view);
    return frame;
  });
}

/** Dispatch sends `frame` to the page. */
function serve(socket: FakeDeliverySocket, frame: DeliveryLiveServerFrame): void {
  act(() => socket.receive(frame));
}

/** The page subscribed its timeline, and Dispatch answered with `body`. */
async function answered(body: DeliveryTimelineResponse): Promise<FakeDeliverySocket> {
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, snapshotFrame(sub, body));
  return socket;
}

test("an unconfigured timeline shows the setup form in its place, reading no settings, and once a save configures it the page's own subscription brings the timeline", async () => {
  const saved: DeliverySettings = {
    deploy_repo: "acme/widgets",
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    population_authors: ["octocat"],
    pr_checks_rollup_job_name: null,
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_names: ["widgets-release"],
    production_apply_step_name: null,
    staging_apply_job_name: null,
    staging_e2e_job_name: null,
    updated_at: "2026-10-07T12:00:00Z",
    updated_by: { id: "alice@example.com", kind: "user" },
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    const queryClient = renderPage();
    const socket = await openSocket();
    const { sub } = await subscription(socket, "timeline");
    serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));

    expect(
      await screen.findByRole("heading", { name: "Set up the delivery timeline" })
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    // DELIVERY_NOT_CONFIGURED already says no record is stored: the form's fields are there at
    // once, empty, with no read of the record and no loading line.
    expect(screen.queryByText("Loading delivery settings…")).toBeNull();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe("");

    fireEvent.change(screen.getByLabelText("Deploy repository"), {
      target: { value: "acme/widgets" },
    });
    fireEvent.change(screen.getByLabelText("Deploy workflow"), {
      target: { value: ".github/workflows/deploy.yml" },
    });
    fireEvent.change(screen.getByLabelText("Production jobs"), {
      target: { value: "widgets-release" },
    });
    fireEvent.change(screen.getByLabelText("PR checks workflow"), {
      target: { value: ".github/workflows/pr-checks.yml" },
    });
    fireEvent.change(screen.getByLabelText("Population authors"), {
      target: { value: "octocat" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    // The save reads nothing again: the page stays subscribed, and Dispatch answers that same
    // subscription once a configuration exists.
    expect(socket.subscribes()).toHaveLength(1);
    expect(queryClient.isFetching()).toBe(0);
    serve(socket, snapshotFrame(sub, emptyTimeline));

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a reconnect while the timeline is unconfigured keeps the setup form and its unsaved draft", async () => {
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  await screen.findByRole("heading", { name: "Set up the delivery timeline" });
  fireEvent.change(screen.getByLabelText("Deploy repository"), {
    target: { value: "acme/widgets" },
  });
  const draft = () => (screen.getByLabelText("Deploy repository") as HTMLInputElement).value;

  // Coming back to the tab (TanStack's focus listener is on `window`) reads nothing.
  act(() => {
    window.dispatchEvent(new Event("visibilitychange"));
  });
  expect(socket.subscribes()).toHaveLength(1);

  // Dispatch restarts: the page opens a new socket at once and subscribes again. Any render of
  // the timeline's body while that waits would unmount the form, so the draft surviving says none
  // happened.
  act(() => socket.drop(1012));
  expect(sockets.sockets).toHaveLength(2);
  const again = sockets.latest();
  act(() => again.open());
  expect(again.subscribed("timeline").sub).toBe(sub);
  expect(draft()).toBe("acme/widgets");
  expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
  expect(screen.queryByText("Loading delivery timeline…")).toBeNull();

  serve(again, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  expect(screen.getByRole("heading", { name: "Set up the delivery timeline" })).toBeDefined();
  expect(draft()).toBe("acme/widgets");
  expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
});

test("an unconfigured timeline that Dispatch then refuses otherwise shows that failure and Retry, not the setup form", async () => {
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, NOT_CONFIGURED, notConfiguredMessage));
  await screen.findByRole("heading", { name: "Set up the delivery timeline" });

  act(() => socket.drop(1012));
  const again = sockets.latest();
  act(() => again.open());
  serve(again, errorFrame(sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
  expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
});

test("a configured timeline renders with no setup form, subscribing the window and facets the URL names", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderPage();
    const socket = await openSocket();
    const { sub } = await subscriptionTo(socket, {
      kind: "timeline",
      window: URL_WINDOW,
      query: "",
    });
    serve(socket, snapshotFrame(sub, emptyTimeline));

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(socket.subscribes()).toHaveLength(1);
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    getDeliverySettings.mockRestore();
  }
});

test("any other refusal keeps the page and offers Retry, not the setup form, and Retry subscribes again", async () => {
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(socket, errorFrame(sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
  expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  const retried = await waitFor(() => {
    const frame = socket.subscribed("timeline");
    expect(frame.sub).not.toBe(sub);
    return frame;
  });
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub });
  serve(socket, snapshotFrame(retried.sub, emptyTimeline));
  expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
  expect(screen.queryByText("Couldn't load the delivery timeline.")).toBeNull();
});

const listedPR: DeliveryPR = {
  additions: 12,
  author: "octocat",
  components: ["ACME/api"],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 3,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: "ACME-1",
  issue_title: "Ship widgets",
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: "P0",
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: a waiting widget",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

test("the facet column offers each value with its count, subscribes a pick and keeps the view in the URL", async () => {
  const timeline: DeliveryTimelineResponse = {
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    facet_counts: {
      ...emptyCounts,
      component: { "ACME/api": 1 },
      issue: { "ACME-1": 1 },
      priority: { P0: 1, __no_issue__: 3 },
      repo: { "acme/other": 5, "acme/widgets": 1 },
    },
    issue_titles: { "ACME-1": "Ship widgets" },
    prs: [listedPR],
  };

  renderPage();
  const socket = await answered(timeline);

  expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
  expect(screen.getByRole("button", { name: /^Repository/ }).textContent).toBe("Any repository");
  fireEvent.click(screen.getByRole("button", { name: /^Priority/ }));
  const priorityOptions = within(screen.getByRole("listbox", { name: "Priority options" }));
  const options = priorityOptions.getAllByRole("option").map((option) => option.textContent);
  expect(options).toEqual(["No issue3", "P01"]);
  fireEvent.click(priorityOptions.getByRole("option", { name: /^P0/ }));

  // Dispatch applies the facets: the pick is a view of its own, its query the GET route's.
  await subscriptionTo(socket, { kind: "timeline", window: URL_WINDOW, query: "priority=P0" });
  expect(screen.getByRole("button", { name: /^Priority/ }).textContent).toBe("1 selected");

  fireEvent.click(screen.getByRole("button", { name: /^Dispatch issue/ }));
  expect(screen.getByRole("option", { name: /ACME-1 — Ship widgets/ })).toBeDefined();

  const colorBy = screen.getByLabelText("Color merges by") as HTMLSelectElement;
  expect([...colorBy.options].map((option) => option.textContent)).toEqual([
    "Repository",
    "Author",
    "Priority",
    "Component",
    "Parent agent",
  ]);
  fireEvent.change(colorBy, { target: { value: "component" } });
  expect(colorBy.value).toBe("component");
  // The list's dot follows the colour-by facet, and the row reads its issue with its priority.
  expect(screen.getByText("ACME-1 · P0")).toBeDefined();
});

test("the header names the window and the freshness row the prototype's six sources", async () => {
  renderPage();
  await answered({
    ...emptyTimeline,
    freshness: { ...emptyTimeline.freshness, last_error: "rate limited" },
  });

  expect(await screen.findByRole("heading", { name: "Delivery timeline" })).toBeDefined();
  const generated = await screen.findByText(/^Generated .* · window/);
  expect(generated.textContent).not.toContain("1970");
  // No brush window in the URL: nothing to clear.
  expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
  const row = screen.getByRole("region", { name: "Source freshness" });
  const sources = [...row.querySelectorAll("[data-source]")].map((node) =>
    node.getAttribute("data-source")
  );
  expect(sources).toEqual(["prs", "runs", "ci", "dispatch", "agents", "events"]);
  expect(within(row).getByText("Deploy runs never checked: rate limited")).toBeDefined();
});

test("while a facet pick waits for its snapshot, the header and freshness keep the last answer's time, never the epoch", async () => {
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(
    socket,
    snapshotFrame(sub, {
      ...emptyTimeline,
      facet_counts: { ...emptyCounts, priority: { P0: 1 } },
      prs: [listedPR],
    })
  );
  await screen.findByText(/^Generated .* · window/);
  fireEvent.click(screen.getByRole("button", { name: /^Priority/ }));
  fireEvent.click(
    within(screen.getByRole("listbox", { name: "Priority options" })).getByRole("option", {
      name: /^P0/,
    })
  );
  const picked = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "priority=P0",
  });

  // The previous answer stays on screen while the new view's snapshot is out.
  expect(screen.getByText(/^Generated .* · window/).textContent).not.toContain("1970");
  const row = screen.getByRole("region", { name: "Source freshness" });
  expect(row.querySelector('[data-source="dispatch"]')?.textContent).toMatch(
    /^Dispatch \d+ s ago$/
  );
  serve(socket, snapshotFrame(picked.sub, { ...emptyTimeline, prs: [listedPR] }));
});

test("the list sorts by its headers and a row opens the PR's details", async () => {
  const shipped: DeliveryPR = {
    ...listedPR,
    deploy_run: 500,
    deployed_at: "2024-06-01T01:40:00Z",
    deployed_status: "deployed",
    id: "acme/widgets#2",
    merged_at: "2024-06-01T00:30:00Z",
    number: 2,
    title: "feat: a shipped widget",
    url: "https://github.com/acme/widgets/pull/2",
  };
  renderPage();
  await answered({
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    prs: [listedPR, shipped],
  });

  const titles = async () =>
    (await screen.findAllByRole("row"))
      .slice(1)
      .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));
  expect(await titles()).toEqual(["feat: a waiting widget", "feat: a shipped widget"]);
  fireEvent.click(screen.getByRole("button", { name: /^Merged/ }));
  expect(await titles()).toEqual(["feat: a shipped widget", "feat: a waiting widget"]);

  fireEvent.click(screen.getByText("feat: a shipped widget"));
  const details = await screen.findByRole("dialog", { name: "Details" });
  expect(within(details).getByRole("link", { name: "Open on GitHub" }).getAttribute("href")).toBe(
    "https://github.com/acme/widgets/pull/2"
  );
  expect(within(details).getByText("ACME-1 — Ship widgets")).toBeDefined();
  expect(within(details).getByText("Public API")).toBeDefined();
  expect(within(details).getByText("70 min")).toBeDefined();
  fireEvent.click(within(details).getByRole("button", { name: "Close details" }));
  expect(screen.queryByRole("dialog", { name: "Details" })).toBeNull();
});

test("the measures panel sits between the freshness row and the view controls and shows the timeline's own measures, subscribing no second view", async () => {
  const withP0 = (key: string): DeliveryTimelineResponse => ({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, repo: { "acme/widgets": 1 } },
    prs: [listedPR],
    measures: {
      ...emptyMeasures,
      unowned_p0: [{ key, title: "Production deploy gate flakes" }],
    },
  });

  renderPage();
  const socket = await answered(withP0("ACME-103"));
  const p0 = await screen.findByRole("link", { name: "ACME-103" });
  expect(p0.getAttribute("href")).toBe("/issues/ACME-103");
  const freshness = screen.getByRole("region", { name: "Source freshness" });
  const panel = screen.getByLabelText("KPI targets");
  const controls = screen.getByRole("group", { name: "View" });
  expect(freshness.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(panel.compareDocumentPosition(controls) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: /^Repository/ }));
  fireEvent.click(
    within(screen.getByRole("listbox", { name: "Repository options" })).getByRole("option", {
      name: /^acme\/widgets/,
    })
  );
  // The facet subscribes the timeline's new view, and the panel follows that one answer.
  const picked = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "repo=acme%2Fwidgets",
  });
  serve(socket, snapshotFrame(picked.sub, withP0("ACME-104")));
  expect(await screen.findByRole("link", { name: "ACME-104" })).toBeDefined();
  expect(socket.subscribes().filter((frame) => frame.view.kind === "measures")).toHaveLength(0);
});

test("a brush window narrows the measures to it with a view of its own, leaves the timeline's view on the whole window, and clearing it returns to the timeline's measures", async () => {
  const brushStart = "2024-06-01T06:00:00.000Z";
  const brushEnd = "2024-06-01T12:00:00.000Z";
  renderPage(
    `${DELIVERY_URL}&ws=${encodeURIComponent(brushStart)}&we=${encodeURIComponent(brushEnd)}`
  );
  const socket = await openSocket();
  const timeline = await subscriptionTo(socket, {
    kind: "timeline",
    window: URL_WINDOW,
    query: "",
  });
  const brushed = await subscriptionTo(socket, {
    kind: "measures",
    window: { from: brushStart, to: brushEnd },
    query: "",
  });
  serve(
    socket,
    snapshotFrame(timeline.sub, {
      ...emptyTimeline,
      measures: { ...emptyMeasures, unowned_p0: [{ key: "ACME-1", title: "the whole window" }] },
    })
  );
  serve(
    socket,
    snapshotFrame(brushed.sub, {
      ...emptyMeasures,
      unowned_p0: [{ key: "ACME-2", title: "the brush window" }],
    })
  );
  expect(await screen.findByRole("link", { name: "ACME-2" })).toBeDefined();

  fireEvent.click(await screen.findByRole("button", { name: "clear brush window" }));
  expect(await screen.findByRole("link", { name: "ACME-1" })).toBeDefined();
  // The brush's view ends with it, and the timeline's never moved.
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub: brushed.sub });
  expect(socket.subscribes()).toHaveLength(2);
});

test("a brushed measures failure keeps the timeline and offers its own Retry", async () => {
  const brush = `&ws=${encodeURIComponent("2024-06-01T06:00:00.000Z")}&we=${encodeURIComponent("2024-06-01T12:00:00.000Z")}`;
  renderPage(`${DELIVERY_URL}${brush}`);
  const socket = await openSocket();
  const timeline = await subscription(socket, "timeline");
  const brushed = await subscription(socket, "measures");
  serve(socket, snapshotFrame(timeline.sub, emptyTimeline));
  serve(socket, errorFrame(brushed.sub, "INTERNAL", "database unavailable"));

  expect(await screen.findByText("Couldn't load the delivery measures.")).toBeDefined();
  expect(screen.getByText("No PRs in the current filter/window.")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  const retried = await waitFor(() => {
    const frame = socket.subscribed("measures");
    expect(frame.sub).not.toBe(brushed.sub);
    return frame;
  });
  serve(socket, snapshotFrame(retried.sub, emptyMeasures));
  expect(await screen.findByLabelText("KPI targets")).toBeDefined();
  expect(screen.queryByText("Couldn't load the delivery measures.")).toBeNull();
});

test("the drill-down is a dialog: focus moves into it, Escape closes it and focus returns to the row", async () => {
  renderPage();
  await answered({ ...emptyTimeline, prs: [listedPR] });

  const row = await screen.findByRole("row", { name: /feat: a waiting widget/ });
  row.focus();
  fireEvent.keyDown(row, { key: "Enter" });
  const details = await screen.findByRole("dialog", { name: "Details" });
  await waitFor(() =>
    expect(document.activeElement).toBe(
      within(details).getByRole("button", { name: "Close details" })
    )
  );
  fireEvent.keyDown(document.body, { key: "Escape" });
  expect(screen.queryByRole("dialog", { name: "Details" })).toBeNull();
  expect(document.activeElement).toBe(row);
});

test("colouring by Parent agent offers the values to give their own colour and lane, kept in the URL", async () => {
  renderPage(`${DELIVERY_URL}&colorBy=parentAgent&agents=group%3Ag1&agents=group%3Ag1`);
  await answered({
    ...emptyTimeline,
    color_counts: {
      ...emptyTimeline.color_counts,
      parent_agent: { "group:g1": 3, Reviewer: 2, __no_session__: 1 },
    },
    session_groups: [
      {
        created_at: "2026-10-11T00:00:00Z",
        created_by: { id: "alice@example.com", kind: "user" },
        id: "g1",
        name: "Release crew",
        sessions: ["s1", "s2"],
        updated_at: "2026-10-11T00:00:00Z",
        updated_by: { id: "alice@example.com", kind: "user" },
      },
    ],
  });

  const trigger = await screen.findByRole("button", { name: "Own colour and lane: 1 chosen" });
  fireEvent.click(trigger);
  const listbox = within(screen.getByRole("listbox", { name: "Own colour and lane options" }));
  expect(listbox.getAllByRole("option").map((option) => option.getAttribute("aria-label"))).toEqual(
    ["Release crew, 3 PRs", "Reviewer, 2 PRs"]
  );
  fireEvent.click(listbox.getByRole("option", { name: /^Reviewer/ }));
  await waitFor(() => expect(currentSearch().getAll("agents")).toEqual(["group:g1", "Reviewer"]));

  fireEvent.change(screen.getByLabelText("Color merges by"), { target: { value: "repo" } });
  expect(screen.queryByRole("button", { name: /^Own colour and lane/ })).toBeNull();
});

test("a malformed custom window falls back to the 28-day preset and removes its bad parameters", async () => {
  renderPage("/delivery?mode=list&from=not-a-date&to=also-not-a-date");
  const window = await screen.findByLabelText("Window");
  expect((window as HTMLSelectElement).value).toBe("28d");
  await waitFor(() => {
    expect(currentSearch().has("from")).toBe(false);
    expect(currentSearch().has("to")).toBe(false);
    expect(currentSearch().has("window")).toBe(false);
  });
  const socket = await openSocket();
  await subscription(socket, "timeline");
  for (const frame of socket.subscribes()) {
    expect(frame.view).toEqual({ kind: "timeline", window: { preset: 28 }, query: "" });
  }
});

test("a preset subscribes by its days, and the header names the window Dispatch resolved", async () => {
  renderPage("/delivery?mode=list&window=7d");
  const socket = await openSocket();
  const { sub } = await subscriptionTo(socket, {
    kind: "timeline",
    window: { preset: 7 },
    query: "",
  });
  const resolved = { from: "2026-10-05T12:00:00Z", to: "2026-10-12T12:00:00Z" };
  serve(socket, snapshotFrame(sub, { ...emptyTimeline, window: resolved }));

  const header = await screen.findByText(/^Generated .* · window/);
  expect(header.textContent).toContain(
    `${new Date(resolved.from).toLocaleDateString()}–${new Date(resolved.to).toLocaleDateString()}`
  );
  expect(currentSearch().get("window")).toBe("7d");
  expect(currentSearch().has("from")).toBe(false);
  expect(currentSearch().has("to")).toBe(false);
});

test("a facet pick leaves the window rolling: it writes no from or to, and subscribes by the preset", async () => {
  renderPage("/delivery?mode=list");
  const socket = await answered({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, priority: { P0: 1 } },
  });
  fireEvent.click(await screen.findByRole("button", { name: /^Priority/ }));
  fireEvent.click(
    within(screen.getByRole("listbox", { name: "Priority options" })).getByRole("option", {
      name: /^P0/,
    })
  );
  await waitFor(() => expect(currentSearch().getAll("priority")).toEqual(["P0"]));
  expect(currentSearch().has("from")).toBe(false);
  expect(currentSearch().has("to")).toBe(false);
  await subscriptionTo(socket, {
    kind: "timeline",
    window: { preset: 28 },
    query: "priority=P0",
  });
});

test("the window control: a preset rolls and clears the brush, a custom range writes its days", async () => {
  renderPage(
    `/delivery?mode=list&ws=${encodeURIComponent("2024-06-01T06:00:00Z")}&we=${encodeURIComponent("2024-06-01T12:00:00Z")}`
  );
  const socket = await openSocket();
  const brushed = await subscription(socket, "measures");

  fireEvent.change(await screen.findByLabelText("Window"), { target: { value: "7d" } });
  await waitFor(() => expect(currentSearch().get("window")).toBe("7d"));
  expect(currentSearch().has("ws")).toBe(false);
  expect(currentSearch().has("we")).toBe(false);
  expect(currentSearch().has("from")).toBe(false);
  await subscriptionTo(socket, { kind: "timeline", window: { preset: 7 }, query: "" });
  expect(socket.sent).toContainEqual({ type: "unsubscribe", sub: brushed.sub });

  fireEvent.change(screen.getByLabelText("Window"), { target: { value: "custom" } });
  fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-01-01" } });
  fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-01-07" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(currentSearch().get("from")).toBe("2026-01-01T00:00:00.000Z"));
  expect(currentSearch().get("to")).toBe("2026-01-08T00:00:00.000Z");
  expect(currentSearch().has("window")).toBe(false);
  await subscriptionTo(socket, {
    kind: "timeline",
    window: { from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" },
    query: "",
  });
});

test("the freshness row says Live with the last change while the socket is open, and Reconnecting once it drops", async () => {
  const now = Date.now();
  const ago = (ms: number) => new Date(now - ms).toISOString();
  renderPage();
  const socket = await openSocket();
  const { sub } = await subscription(socket, "timeline");
  serve(
    socket,
    snapshotFrame(
      sub,
      {
        ...emptyTimeline,
        freshness: {
          last_error: null,
          last_event_at: ago(30_000),
          last_reconcile_at: ago(60_000),
          unfetchable_count: 0,
        },
      },
      { changedAt: ago(4 * 60_000) }
    )
  );

  const row = await screen.findByRole("region", { name: "Source freshness" });
  const prs = row.querySelector('[data-source="prs"]');
  expect(prs?.textContent).toBe("PRs Live · last change 4 min ago");
  expect(row.querySelector('[data-source="events"]')?.textContent).toBe(
    "Events Live · last change 4 min ago"
  );

  act(() => socket.drop(1006));
  expect(prs?.textContent).toMatch(/^PRs Reconnecting, \d+ s$/);
});

/** happy-dom has no canvas, and ECharts throws without a 2D context: a context that accepts every
 *  call lets the timeline mount, though it paints nothing. */
function stubCanvasContext(): () => void {
  const original = HTMLCanvasElement.prototype.getContext;
  const state: Record<string | symbol, unknown> = {};
  const context = new Proxy(state, {
    get(target, key) {
      if (key in target) return target[key];
      if (key === "measureText") return (text: string) => ({ width: text.length * 6 });
      if (key === "getImageData") return () => ({ data: new Uint8ClampedArray(4) });
      return () => ({ addColorStop() {} });
    },
    set(target, key, value) {
      target[key] = value;
      return true;
    },
  });
  HTMLCanvasElement.prototype.getContext = (() => context) as unknown as typeof original;
  return () => {
    HTMLCanvasElement.prototype.getContext = original;
  };
}

test("a brush window narrows the merges shown but the waiting line still counts what waits before it", async () => {
  const restoreCanvas = stubCanvasContext();
  try {
    renderPage(
      "/delivery?from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T12%3A00%3A00Z"
    );
    await answered({
      ...emptyTimeline,
      // Merged 28 days before the window and not shipped: the server's `waiting`.
      waiting: [{ merged_at: "2024-05-04T00:00:00Z", deployed_at: null }],
      // Merged inside the window at 01:00, before the brush, and still waiting.
      prs: [listedPR],
    });
    const chart = await screen.findByRole("img", { name: /^Delivery timeline/ });
    expect(chart.getAttribute("aria-label")).toContain(
      "0 merged pull requests, 2 waiting to deploy at the start"
    );
  } finally {
    cleanup();
    restoreCanvas();
  }
});
