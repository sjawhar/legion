import { expect, spyOn, test } from "bun:test";
import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Agent, ClaimHolderLiveness, Issue, IssueClaim, IssueDetails } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { claimIsTakeable } from "./ClaimChip";
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
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  verification_artifact_id: "",
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

function header(issue: Issue, liveness: ClaimHolderLiveness = {}): { unmount: () => void } {
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
    state: "live",
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
      state: "live",
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
// (dispatch://INFRA-392). The chip has to tell the two apart, say since when it has been quiet
// (R5), and say how long the wait is.
test("a holder that missed its heartbeats reads as unresponsive since its last heartbeat, not gone, and names when the claim comes free", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  // Six minutes of silence: inside the claim's grace, so the server would still refuse a take.
  const lastSeen = Date.now() - 6 * 60_000;
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    unresponsive("session-one", "Implementer", lastSeen),
  ]);
  const view = header({ ...base, claim });
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    expect(screen.queryByText("not running")).toBeNull();
    // The state and the moment it started, which is what tells a reader whether to wait.
    expect(stalled.textContent).toContain("unresponsive since");
    expect(
      within(stalled)
        .getByTestId("issue-claim-unresponsive-since")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(new Date(lastSeen).toISOString());
    const chip = screen.getByTestId("issue-claim");
    expect(chip.getAttribute("title")).toContain("missed its heartbeats without ending");
    expect(chip.getAttribute("title")).toContain(
      `last seen ${new Date(lastSeen).toLocaleString()}`
    );
    // The wait the chip names is the sum the server refuses by, not a guess of its own, and the
    // issue is not free until then.
    const takeableAt = lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS;
    expect(chip.getAttribute("title")).toContain(
      `keeps this issue until ${new Date(takeableAt).toLocaleString()}, when another agent may take it`
    );
    expect(
      within(stalled)
        .getByTestId("issue-claim-takeable")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(new Date(takeableAt).toISOString());
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

// Past the grace the server grants the next agent that asks the issue, so a chip that still read
// as held would hide pickable work - the fault the Unclaimed filter had too. The holder has still
// not ended, so it is drawn as unresponsive and not as gone.
test("an unresponsive holder past its takeable moment reads takeable now, still unresponsive and never not running", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const lastSeen = Date.now() - (ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS + 60_000);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    unresponsive("session-one", "Implementer", lastSeen),
  ]);
  const view = header({ ...base, claim });
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(stalled.textContent).toContain("unresponsive since");
    expect(within(stalled).getByTestId("issue-claim-takeable").textContent).toBe(", takeable now");
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    expect(screen.queryByText("not running")).toBeNull();
    const chip = screen.getByTestId("issue-claim");
    expect(chip.getAttribute("title")).toContain("another agent may take it now");
    expect(chip.getAttribute("title")).toContain("It has not ended, and may yet answer.");
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("unresponsive");
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
  const lastSeen = new Date(Date.now() - 6 * 60_000).toISOString();
  const takeableAt = new Date(Date.now() + 14 * 60_000).toISOString();
  const liveness: ClaimHolderLiveness = {
    holder_state: "unresponsive",
    last_seen: lastSeen,
    takeable_at: takeableAt,
  };
  const view = header({ ...base, claim }, liveness);
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    // Both moments on the chip are the read's own, not the registry's.
    expect(
      within(stalled)
        .getByTestId("issue-claim-unresponsive-since")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(lastSeen);
    expect(
      within(stalled)
        .getByTestId("issue-claim-takeable")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(takeableAt);
    expect(screen.getByTestId("issue-claim").getAttribute("title")).toContain(
      `keeps this issue until ${new Date(takeableAt).toLocaleString()}`
    );
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

/** A registry holding one unresponsive session, as `claimIsTakeable` reads one. */
function stalledRegistry(sessionID: string, lastSeen: number | undefined) {
  return {
    lastSeen: (id: string) => (id === sessionID ? lastSeen : undefined),
    state: (id: string) =>
      id === sessionID ? ("unresponsive" as const) : ("not-running" as const),
    titles: new Map([[sessionID, "Implementer"]]),
  };
}

// The rule the chip's tone and the Unclaimed filter share is the server's own
// (`claimBlockedBy`): the claim holds until the holder's last registration plus the grace, and
// is takeable after it. A holder nothing dates keeps the claim, because the server refuses that
// take as well.
test("claimIsTakeable follows the server's refusal: held inside the grace, takeable past it", () => {
  const now = Date.parse("2026-09-24T07:00:00Z");
  const inside = now - (ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS - 60_000);
  const past = now - (ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS + 60_000);
  expect(claimIsTakeable(claim, stalledRegistry("session-one", inside), now)).toBe(false);
  expect(claimIsTakeable(claim, stalledRegistry("session-one", past), now)).toBe(true);
  // The exact moment is still the holder's: the server grants the take after it, not at it
  // (`judgeClaim`'s `now.After(takeableAt)`), so a chip that read it free here would paint
  // "takeable now" and list the issue as unclaimed for a take the server answers 409.
  expect(
    claimIsTakeable(
      claim,
      stalledRegistry("session-one", now - ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS),
      now
    )
  ).toBe(false);
  // One millisecond past it the server grants the take, and so does this.
  expect(
    claimIsTakeable(
      claim,
      stalledRegistry("session-one", now - ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS - 1),
      now
    )
  ).toBe(true);
  // Nothing dates the silence, so nothing frees the claim.
  expect(claimIsTakeable(claim, stalledRegistry("session-one", undefined), now)).toBe(false);
  // A session the latest successful read does not list at all has gone, and that claim is free.
  expect(claimIsTakeable(claim, stalledRegistry("session-two", now), now)).toBe(true);
  // A person's claim has no session to miss a heartbeat, and only a person releases it.
  expect(
    claimIsTakeable(
      { actor: { id: "alice", kind: "user" }, at: claim.at },
      stalledRegistry("session-one", past),
      now
    )
  ).toBe(false);
});
