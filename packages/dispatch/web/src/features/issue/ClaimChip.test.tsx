import { expect, spyOn, test } from "bun:test";
import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Agent, Issue, IssueClaim, IssueClaimLiveness, IssueDetails } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { IssueHeader } from "./IssueHeader";
import { stateForIssue } from "./pins";
import type { IssuePriorityWrite } from "./useIssuePriority";

const claim: IssueClaim = {
  actor: { kind: "session", id: "session-one", origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

const base: Issue = {
  assignee: "alice",
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  closed_at: null,
  created_at: "2026-09-24T05:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  status: "in_progress",
  title: "Claim the issue before working it",
  updated_at: "2026-09-24T06:00:00Z",
};

/** These rows are about the claim chip, not the priority: the header's write never runs. */
const idlePriorityWrite: IssuePriorityWrite = {
  failed: false,
  pending: false,
  retry: () => undefined,
  submit: () => undefined,
};

function header(issue: Issue, liveness: Partial<IssueClaimLiveness> = {}): { unmount: () => void } {
  const details: IssueDetails = {
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    ...issue,
    ...liveness,
    artifacts: [],
    children: [],
    open_asks: [],
    progress: { tasks: null, children: null },
    referenced_by_count: 0,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The header reads the project's repository mappings at once for an issue that resolves to
  // none, as these do; a project that maps none shows no repository item.
  const mappings = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>{children}</MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={details}
      priorityWrite={idlePriorityWrite}
      state={stateForIssue(undefined, issue.key)}
    />,
    { wrapper }
  );
  return {
    unmount: () => {
      view.unmount();
      mappings.mockRestore();
    },
  };
}

/** One live session, as `GET /api/v1/agents` lists it. */
function running(sessionID: string, title: string): Agent {
  return {
    session_id: sessionID,
    title,
    dir: "/w",
    host: "host",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: 1,
    open_asks: 0,
    last_activity: null,
  };
}

/** The same session after it has missed its heartbeats: the listener still lists it, and says
 *  so on the row (`state`), with the heartbeat it was last seen at. */
function unresponsive(sessionID: string, title: string, lastSeen: number): Agent {
  return { ...running(sessionID, title), last_seen: lastSeen, state: "unresponsive" };
}

test("a claimed issue names its holder and releases the claim from the header", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    running("session-one", "Implementer"),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockResolvedValue({ ...base, claim: null });
  const view = header({ ...base, claim });
  try {
    expect(await screen.findByTitle(/Claimed by Implementer/)).toBeDefined();
    // The chip is the only place a reader is told what a claim is.
    expect(await screen.findByTitle(/two agents never take the same work/)).toBeDefined();
    // The holder is drawn by `AgentName`: its name links to its live conversation, and the
    // registry lists it, so it reads as running and the chip adds nothing.
    const chip = screen.getByTestId("issue-claim");
    expect(within(chip).getByRole("link", { name: "Implementer" }).getAttribute("href")).toBe(
      "/agents/session-one/live"
    );
    await waitFor(() =>
      expect(
        chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
      ).toBe("running")
    );
    expect(screen.queryByText("not running")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Release the claim on CORE-1" }));
    await waitFor(() => expect(release).toHaveBeenCalledWith("CORE-1", keyedAttempt));
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A claim whose session the registry no longer lists is the one any agent may take, and the
// Unclaimed filter counts it as free: the chip has to say so, or a reader sees held work.
test("a holder the registry no longer lists reads as not running", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    running("session-two", "Someone else"),
  ]);
  const view = header({ ...base, claim });
  try {
    expect(await screen.findByText("not running")).toBeDefined();
    expect(screen.getByTitle(/no longer running, so anyone may claim it/)).toBeDefined();
    const chip = screen.getByTestId("issue-claim");
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("not-running");
    expect(within(chip).getByRole("img", { name: "Not running" })).toBeDefined();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// Releasing can fail — an expired session, a 404, a dropped connection — and a button that
// silently returns to "Release" tells a human their click did nothing.
test("a release that fails says so and offers a retry", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    running("session-one", "Implementer"),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(new Error("network error"));
  const view = header({ ...base, claim });
  try {
    fireEvent.click(await screen.findByRole("button", { name: "Release the claim on CORE-1" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Could not release this claim.");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

test("the claim shows the holder's live title, not the one stamped on the claim", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    {
      session_id: "session-one",
      title: "Live registry title",
      dir: "/w",
      host: "host",
      machine_id: "host",
      roles: [],
      capabilities: [],
      last_seen: 1,
      open_asks: 0,
      last_activity: null,
    },
  ]);
  const view = header({ ...base, claim });
  try {
    expect(await screen.findByText("Live registry title")).toBeDefined();
    expect(screen.queryByText("Implementer")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

test("an unclaimed issue shows no holder and nothing to release", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = header(base);
  try {
    expect(await screen.findByRole("heading", { level: 1 })).toBeDefined();
    expect(screen.queryByTitle(/Claimed by/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Release the claim/ })).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A holder that has missed its heartbeats has not ended: it is still working the issue, and the
// chip that called it "not running" sent the next agent to take work that was being done
// (dispatch://INFRA-392). The chip has to tell the two apart, and say how long the wait is.
test("a holder that missed its heartbeats reads as unresponsive, not gone, and names when the claim comes free", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const lastSeen = Date.parse("2026-09-24T06:30:00Z");
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    unresponsive("session-one", "Implementer", lastSeen),
  ]);
  const view = header({ ...base, claim });
  try {
    expect(await screen.findByTestId("issue-claim-unresponsive")).toBeDefined();
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    expect(screen.queryByText("not running")).toBeNull();
    const chip = screen.getByTestId("issue-claim");
    expect(chip.getAttribute("title")).toContain("missed its heartbeats without ending");
    // The wait the chip names is the sum the server refuses by, not a guess of its own.
    expect(chip.getAttribute("title")).toContain(
      new Date(lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS).toLocaleString()
    );
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("unresponsive");
    expect(within(chip).getByRole("img", { name: "Unresponsive" })).toBeDefined();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// The issue read judged the holder against its own record, by id; the agent list is a cache
// that can be missing a session it still holds. When the two disagree the read wins, or the
// page would call a stalled holder gone the moment the list dropped it.
test("the issue read's own holder_state outranks an agent list that does not name the holder", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const takeableAt = "2026-09-24T06:50:00Z";
  const liveness: Partial<IssueClaimLiveness> = {
    holder_state: "unresponsive",
    last_seen: "2026-09-24T06:30:00Z",
    takeable_at: takeableAt,
  };
  const view = header({ ...base, claim }, liveness);
  try {
    expect(await screen.findByTestId("issue-claim-unresponsive")).toBeDefined();
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    expect(screen.getByTestId("issue-claim").getAttribute("title")).toContain(
      new Date(takeableAt).toLocaleString()
    );
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});
