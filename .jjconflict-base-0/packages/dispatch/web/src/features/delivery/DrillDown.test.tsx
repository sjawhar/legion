import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { Agent, DeliveryPR } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DrillDown } from "./DrillDown";

const pr: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: "Planner one",
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: ["sess-planner-1", "sess-helper-2", "sess-untitled-3"],
  title: "feat: widget 1",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

const helper: Agent = {
  capabilities: [],
  dir: "/home/alice/legion",
  host: "devbox-alice",
  last_activity: null,
  last_seen: 1,
  machine_id: "devbox-alice",
  open_asks: 0,
  roles: [],
  session_id: "sess-helper-2",
  title: "Helper",
};

function renderDrillDown(sessions: readonly string[]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The agent registry, as the shell's poller last read it: only the helper is running.
  queryClient.setQueryData(["agents"], [helper]);
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DrillDown
            components={{}}
            onClose={() => {}}
            prs={[{ ...pr, sessions: [...sessions] }]}
            runs={[]}
            selection={{ id: pr.id, kind: "pr" }}
            sessionGroups={[]}
            sessionTitles={{
              "sess-planner-1": "Planner one",
              "sess-untitled-3": "sess-untitled-3",
            }}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The `<dd>` that follows the `<dt>` named `term`. */
function detail(term: string): HTMLElement {
  const dt = within(screen.getByRole("dialog", { name: "Details" })).getByText(term);
  const dd = dt.nextElementSibling;
  if (!(dd instanceof HTMLElement)) throw new Error(`no value after ${term}`);
  return dd;
}

// A pull request's sessions are agents, each linked to its live conversation and named by its
// registry title, else the title the delivery read stored, else its short id (a stored title
// that is the session's own id is no title).
test("a pull request's sessions each link to their live conversation", () => {
  renderDrillDown(pr.sessions);
  try {
    const sessions = within(detail("Sessions"));
    const links = sessions.getAllByRole("link");
    expect(links.map((link) => [link.textContent, link.getAttribute("href")])).toEqual([
      ["Planner one", "/agents/sess-planner-1/live"],
      ["Helper", "/agents/sess-helper-2/live"],
      ["session:sess-unt…", "/agents/sess-untitled-3/live"],
    ]);
    expect(
      links.map((link) => link.closest("[data-agent-name]")?.getAttribute("data-agent-state"))
    ).toEqual(["not-running", "running", "not-running"]);
    // The Parent agent stays the facet value the server wrote, not an agent.
    expect(within(detail("Parent agent(s)")).queryByRole("link")).toBeNull();
    expect(detail("Parent agent(s)").textContent).toBe("Planner one");
  } finally {
    cleanup();
  }
});

test("a pull request with no sessions shows a dash", () => {
  renderDrillDown([]);
  try {
    expect(detail("Sessions").textContent).toBe("\u2014");
  } finally {
    cleanup();
  }
});
