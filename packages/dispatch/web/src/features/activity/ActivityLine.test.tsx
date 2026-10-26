import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Event, IssueDetails } from "../../api/types";
import type { AgentPart } from "../refs/AgentName";
import { ActivityLine } from "./ActivityLine";

const uploaded: Artifact = {
  created_at: "2026-09-15T00:00:00Z",
  created_by: { id: "session-1", kind: "session" },
  id: "artifact-2",
  issue_key: "OPS-52",
  kind: "doc",
  name: "cu-update-2026-09-15.md",
  primary: false,
  project: "OPS",
  slug: "cu-update-2026-09-15-md",
  versions: [],
};

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  artifacts: [
    {
      created_at: "2026-09-15T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "OPS-52",
      kind: "doc",
      name: "spec.md",
      primary: true,
      project: "OPS",
      slug: "spec",
      versions: [],
    },
    uploaded,
  ],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  children: [],
  closed_at: null,
  created_at: "2026-09-15T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "OPS-52",
  labels: [],
  last_seq: 5,
  number: 52,
  open_asks: [],
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "OPS",
  rank: "U",
  referenced_by_count: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  title: "Customer update",
  updated_at: "2026-09-15T00:00:00Z",
};

const base = {
  actor: { id: "session-1", kind: "session" },
  created_at: "2026-09-15T00:00:00Z",
  issue_key: "OPS-52",
  notify: false,
} as const;

function renderLine(event: Event, description: readonly AgentPart[]) {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/issues/OPS-52/conversation"]}>
      <QueryClientProvider client={queryClient}>
        <ActivityLine description={description} event={event} issueKey="OPS-52" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    view,
    cleanup: () => {
      view.unmount();
      getIssue.mockRestore();
      listAgents.mockRestore();
    },
  };
}

test("an added artifact's name links to the artifact", () => {
  const event: Event = {
    ...base,
    id: 4,
    payload: { artifact: uploaded },
    seq: 4,
    type: "artifact.created",
  };
  const { cleanup } = renderLine(event, ["added cu-update-2026-09-15.md"]);

  try {
    const link = screen.getByRole("link", { name: "cu-update-2026-09-15.md" });
    expect(link.getAttribute("href")).toBe("/issues/OPS-52/artifacts/cu-update-2026-09-15-md");
    expect(screen.getByText(/^added/).textContent).toBe("added cu-update-2026-09-15.md");
  } finally {
    cleanup();
  }
});

test("a saved version's name links to that version once the issue's artifacts are known", async () => {
  const event: Event = {
    ...base,
    id: 5,
    payload: {
      artifact_id: "artifact-2",
      name: "cu-update-2026-09-15.md",
      version: {
        authors: [{ id: "session-1", kind: "session" }],
        created_at: "2026-09-15T01:00:00Z",
        named: false,
        number: 2,
        summary: null,
      },
    },
    seq: 5,
    type: "artifact.version",
  };
  const { cleanup } = renderLine(event, ["saved cu-update-2026-09-15.md v2"]);

  try {
    const link = await screen.findByRole("link", { name: "cu-update-2026-09-15.md" });
    expect(link.getAttribute("href")).toBe("/issues/OPS-52/artifacts/cu-update-2026-09-15-md?v=2");
    expect(screen.getByText(/^saved/).textContent).toBe("saved cu-update-2026-09-15.md v2");
  } finally {
    cleanup();
  }
});

test("a comment activity keeps its view link", () => {
  const event: Event = {
    ...base,
    id: 6,
    payload: {
      anchor: null,
      artifact_name: "spec.md",
      ask_id: null,
      author: { id: "session-1", kind: "session" },
      body: "Looks wrong",
      created_at: "2026-09-15T00:00:00Z",
      edited_at: null,
      id: "comment-1",
      issue_key: "OPS-52",
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
    },
    seq: 6,
    type: "comment.created",
  } as Event;
  const { cleanup } = renderLine(event, ["commented on spec.md"]);

  try {
    expect(screen.getByText("commented on spec.md")).not.toBeNull();
    expect(screen.getByRole("link", { name: "view" }).getAttribute("href")).toBe(
      "/issues/OPS-52/comments/comment-1"
    );
  } finally {
    cleanup();
  }
});

// An agent the line names stays an agent until it is drawn: the claim it took names the
// earlier holder through `AgentName`, a link to that session's live conversation.
test("an agent the line names links to its live conversation", () => {
  const holder = {
    id: "session-one",
    kind: "session" as const,
    origin: { session_title: "Implementer" },
  };
  const event = {
    ...base,
    id: 7,
    payload: {
      claim: { actor: base.actor, at: "2026-09-15T00:00:00Z" },
      key: "OPS-52",
      previous_claim: { actor: holder, at: "2026-09-14T00:00:00Z" },
      reason: "takeover",
      status: "in_progress",
    },
    seq: 7,
    type: "issue.claimed",
  } as Event;
  const { cleanup, view } = renderLine(event, ["took the claim from ", { actor: holder }]);

  try {
    expect(view.container.textContent).toBe("took the claim from Implementer");
    expect(screen.getByRole("link", { name: "Implementer" }).getAttribute("href")).toBe(
      "/agents/session-one/live"
    );
  } finally {
    cleanup();
  }
});
