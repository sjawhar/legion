import { expect, type Mock, spyOn, test } from "bun:test";
import type { IssueClaimHolderState } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  IssueClaim,
  IssueDetails,
  Message,
  SearchResponse,
  SearchResultsPage,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SearchPalette } from "./SearchPalette";
import { handoffBody } from "./send-to-agent";

const words = "find out why the rota is empty and write it up";
const session = "session-one";
const issueKey = "CORE-7";

/** One hit, so the words answer to something and the new-issue offer is not in the list beside
 *  this one: this row is about the agent, not about what the search found. */
function oneHit(): SearchResponse & SearchResultsPage {
  return {
    limit: 20,
    offset: 0,
    reachable: 1,
    results: [
      {
        href: `/issues/${issueKey}`,
        id: issueKey,
        kind: "issue",
        owner: { key: issueKey, kind: "issue", status: "in_progress", title: "The rota" },
        rank: 1,
        snippet: "The <mark>rota</mark> is empty.",
      },
    ],
    took_ms: 1,
    total: 1,
  };
}

const sessionClaim: IssueClaim = {
  actor: { id: session, kind: "session", origin: { session_title: "Implementer" } },
  at: "2026-11-07T06:00:00Z",
};

function issueDetails(
  claim: IssueClaim | null,
  holderState: IssueClaimHolderState | null
): IssueDetails {
  return {
    artifacts: [],
    assignee: null,
    blocked_by: [],
    children: [],
    claim,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-07T05:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    holder_state: holderState,
    key: issueKey,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 7,
    open_asks: [],
    parent: null,
    primary_artifact_id: "artifact-1",
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "in_progress",
    takeable_at: null,
    title: "The rota",
    updated_at: "2026-11-07T06:00:00Z",
  };
}

interface Stubs {
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  readonly restore: () => void;
}

/** The bar over the issue page, with the issue read answering the claim given and the send
 *  stubbed. The registry answers nothing: the claim's own `holder_state` is what the issue read
 *  reports, and it is what the bar judges the holder by. */
function openBar(claim: IssueClaim | null, holderState: IssueClaimHolderState | null): Stubs {
  const search = spyOn(api, "search").mockResolvedValue(oneHit());
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueDetails(claim, holderState));
  const listAgents = spyOn(api, "listAgents").mockRejectedValue(new Error("no registry"));
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue({} as Message);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${issueKey}`]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SearchPalette onClose={() => {}} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createAgentMessage,
    restore: () => {
      for (const stub of [search, getIssue, listAgents, createAgentMessage]) {
        stub.mockRestore();
      }
      view.unmount();
      queryClient.clear();
    },
  };
}

/** Types the words and waits for the hits, so a row still absent after this is absent because
 *  the bar does not offer it and not because the bar has not answered yet. */
async function typeWords(): Promise<void> {
  fireEvent.change(screen.getByRole("combobox", { name: "Search" }), { target: { value: words } });
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
}

const offerLabel = `Send this to the agent on ${issueKey}`;

test("offers the words to the agent holding the claim, and sends nothing until the row is pressed", async () => {
  const bar = openBar(sessionClaim, "live");
  try {
    await typeWords();
    await waitFor(() => expect(screen.getByText(offerLabel)).toBeDefined());
    expect(bar.createAgentMessage).not.toHaveBeenCalled();
  } finally {
    bar.restore();
  }
});

test("approving sends one message naming the issue and the words, and the bar says it was sent", async () => {
  const bar = openBar(sessionClaim, "live");
  try {
    await typeWords();
    fireEvent.click(await waitFor(() => screen.getByText(offerLabel)));

    await waitFor(() => expect(bar.createAgentMessage).toHaveBeenCalled());
    expect(bar.createAgentMessage.mock.calls[0]?.[0]).toBe(session);
    expect(bar.createAgentMessage.mock.calls[0]?.[1]).toEqual({
      body: `${issueKey}: ${words}`,
      delivery: "aside",
    });
    await waitFor(() =>
      expect(screen.getByTestId("search-send-to-agent-said").textContent).toContain(
        `the agent on ${issueKey}`
      )
    );
    // One approval is one message: the row is gone, so there is no second press to make.
    expect(screen.queryByText(offerLabel)).toBeNull();
    expect(bar.createAgentMessage).toHaveBeenCalledTimes(1);
  } finally {
    bar.restore();
  }
});

test("the row is absent with no claim, a person's claim, a holder that stopped, or no registry", async () => {
  const absent: [string, IssueClaim | null, IssueClaimHolderState | null][] = [
    ["no claim at all", null, null],
    [
      "a person's claim",
      { actor: { id: "alice", kind: "user" }, at: "2026-11-07T06:00:00Z" },
      null,
    ],
    ["a holder that stopped", sessionClaim, "gone"],
    ["a holder that stopped answering", sessionClaim, "unresponsive"],
    ["a registry nothing could read", sessionClaim, null],
  ];
  for (const [what, claim, holderState] of absent) {
    const bar = openBar(claim, holderState);
    try {
      await typeWords();
      expect(screen.queryByText(offerLabel), what).toBeNull();
    } finally {
      bar.restore();
    }
  }
});

test("the body names the issue, then the person's words as they typed them", () => {
  expect(handoffBody("CORE-7", "write the rota up")).toBe("CORE-7: write the rota up");
});
