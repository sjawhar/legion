import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, Issue, IssueClaim, IssueDetails } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { IssueHeader } from "./IssueHeader";
import { stateForIssue } from "./pins";

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
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  status: "in_progress",
  title: "Claim the issue before working it",
  updated_at: "2026-09-24T06:00:00Z",
};

function header(issue: Issue): { unmount: () => void } {
  const details: IssueDetails = {
    ...issue,
    artifacts: [],
    children: [],
    open_asks: [],
    referenced_by_count: 0,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>{children}</MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return render(
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={details}
      state={stateForIssue(undefined, issue.key)}
    />,
    { wrapper }
  );
}

/** One live session, as `GET /api/v1/agents` lists it. */
function running(sessionID: string, title: string): Agent {
  return {
    session_id: sessionID,
    title,
    dir: "/w",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: 1,
    open_asks: 0,
    last_activity: null,
  };
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
    expect(screen.getByText("Implementer")).toBeDefined();
    expect(screen.queryByText("not running")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Release the claim on CORE-1" }));
    await waitFor(() => expect(release).toHaveBeenCalledWith("CORE-1"));
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
