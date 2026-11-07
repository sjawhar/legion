import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { AuthenticatedUser, LegionSetting } from "../../api/types";
import { LegionDaemonsSection } from "./LegionDaemonsSection";

/** Every spy a test installed, put back when it ends, so one test's identity or answers are
 *  never the next one's. */
const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const setting: LegionSetting = {
  created_by: { id: "alice", kind: "user" },
  project: "ACME",
  state_url: "http://127.0.0.1:9731",
  updated_at: "2026-11-03T05:04:00Z",
};

/** The three calls the section makes, over rows a test holds. */
interface LegionServer {
  list(): Promise<LegionSetting[]>;
  put(project: string, stateUrl: string): Promise<LegionSetting>;
  remove(project: string): Promise<void>;
}

/** The Legion settings routes' server state as the Settings page reaches it: one row per
 *  project, listed by project key as `ListSettings` does; a save of a project that already names
 *  a daemon moves its URL, and a removal drops the row, so the read a write settles into answers
 *  what the write did rather than the fixture it started from. */
function legionServer(initial: readonly LegionSetting[]): LegionServer {
  let rows = initial.map((row) => ({ ...row }));
  return {
    list: async (): Promise<LegionSetting[]> => rows.map((row) => ({ ...row })),
    put: async (project: string, stateUrl: string): Promise<LegionSetting> => {
      const saved: LegionSetting = {
        ...setting,
        project,
        state_url: stateUrl,
        updated_at: "2026-11-04T06:00:00Z",
      };
      rows = rows.some((row) => row.project === project)
        ? rows.map((row) => (row.project === project ? saved : row))
        : [...rows, saved];
      rows.sort((a, b) => (a.project < b.project ? -1 : 1));
      return { ...saved };
    },
    remove: async (project: string): Promise<void> => {
      rows = rows.filter((row) => row.project !== project);
    },
  };
}

/** Renders the section against `server`. `human` is what `/auth/whoami` says: a person is offered
 *  the field and the Remove, a bearer is not. A bearer's answer names a kind the browser's own
 *  contract does not — the SPA types the route by the one answer it is meant to get — so this
 *  writes the answer a bearer really receives. */
async function renderSection(server: LegionServer, human = true) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(
    human
      ? { event_head: "0", kind: "user", login: "alice" }
      : ({ event_head: "0", kind: "agent", login: "alice" } as unknown as AuthenticatedUser)
  );
  const listLegionSettings = spyOn(api, "listLegionSettings").mockImplementation(server.list);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "ACME", name: "Acme" },
  ]);
  spies.push(whoAmI, listLegionSettings, listProjects);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  spies.push({ mockRestore: () => queryClient.clear() });
  render(
    <QueryClientProvider client={queryClient}>
      <LegionDaemonsSection />
    </QueryClientProvider>
  );
  return within(await screen.findByRole("region", { name: "Legion" }));
}

test("every project that names a daemon is a row of its own", async () => {
  const section = await renderSection(
    legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }])
  );

  expect(await section.findByRole("cell", { name: "ACME" })).toBeDefined();
  expect(section.getByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.getByRole("cell", { name: "https://daemon.example" })).toBeDefined();
  expect(section.queryByText("No project names a Legion daemon yet.")).toBeNull();
});

test("a deployment where no project names a daemon says so", async () => {
  const section = await renderSection(legionServer([]));

  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

test("naming a project's daemon sends the PUT and lists what the server stored", async () => {
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockImplementation(server.put);
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(putLegionSetting).toHaveBeenLastCalledWith("ACME", "http://127.0.0.1:9731", keyedAttempt)
  );
  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  // The form is empty again, ready for the next project's daemon.
  expect(section.getByLabelText("Legion daemon state URL")).toHaveProperty("value", "");
});

test("removing a project's daemon sends the DELETE and drops its row", async () => {
  const server = legionServer([setting]);
  const deleteLegionSetting = spyOn(api, "deleteLegionSetting").mockImplementation(server.remove);
  spies.push(deleteLegionSetting);
  const section = await renderSection(server);

  fireEvent.click(await section.findByRole("button", { name: "Remove the Legion daemon of ACME" }));

  await waitFor(() => expect(deleteLegionSetting).toHaveBeenLastCalledWith("ACME", keyedAttempt));
  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

test("a refusal the server made is shown as the server wrote it, and the row goes back", async () => {
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockRejectedValue(
    new ApiError(403, {
      code: "SCOPE_FORBIDDEN",
      error: "this credential names one project and cannot write a deployment setting",
    })
  );
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(section.getByRole("alert").textContent).toContain(
      "this credential names one project and cannot write a deployment setting"
    )
  );
  expect(section.getByText("No project names a Legion daemon yet.")).toBeDefined();
});

// The field refuses what the route refuses, in the route's own words (`legionStateURL`,
// packages/envoy/internal/dispatch/api/settings_legion.go), rather than sending a URL the server
// is bound to turn down.
test("a URL with a path is refused by the field, in the words the route uses", async () => {
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockImplementation(server.put);
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731/legion/v1/state" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

  expect((await section.findByRole("alert")).textContent).toContain(
    "state_url must be an absolute http(s) URL with a host and no path"
  );
  // No Retry: the URL has to change first.
  expect(section.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(putLegionSetting).not.toHaveBeenCalled();
});

test("a reader who is not a person is shown the daemons and offered no control", async () => {
  const section = await renderSection(legionServer([setting]), false);

  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.queryByLabelText("Legion daemon state URL")).toBeNull();
  expect(section.queryByLabelText("Legion project")).toBeNull();
  expect(section.queryByRole("button", { name: "Save Legion daemon" })).toBeNull();
  expect(section.queryByRole("button", { name: "Remove the Legion daemon of ACME" })).toBeNull();
});
