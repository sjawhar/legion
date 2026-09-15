import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { applyEventInvalidations, prependEventToLog } from "../api/sse";
import type { Event } from "../api/types";

function event(
  type: Event["type"],
  payload: Record<string, unknown> = {},
  overrides: Partial<Event> = {}
): Event {
  return {
    actor: { kind: "user", id: "alice" },
    created_at: "2026-09-09T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    seq: 4,
    type,
    ...overrides,
  } as Event;
}

test("ask events refresh the issue, its asks list, user state, and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.answered"));

  expect(invalidated).toContainEqual(["issue", "CORE-1"]);
  expect(invalidated).toContainEqual(["asks", "CORE-1"]);
  expect(invalidated).toContainEqual(["user-state"]);
  expect(invalidated).toContainEqual(["inbox"]);
  expect(invalidated).toContainEqual(["projects"]);
});
test("an ask edit refreshes the issue, its asks list, user state, the inbox, and the ask's own read", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };
  const edited: Extract<Event, { type: "ask.edited" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    id: 13,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: "2026-09-11T00:00:00Z",
      edited_by: { id: "session-1", kind: "session" },
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      previous: { multiple: false, options: [], question: "Before?", urgency: "med" },
      question: "After?",
      state: "open",
      urgency: "med",
    },
    seq: 5,
    type: "ask.edited",
  };

  applyEventInvalidations(queryClient, edited);

  expect(invalidated).toContainEqual(["issue", "CORE-1"]);
  expect(invalidated).toContainEqual(["asks", "CORE-1"]);
  expect(invalidated).toContainEqual(["user-state"]);
  expect(invalidated).toContainEqual(["inbox"]);
  expect(invalidated).toContainEqual(["ask", "ask-1"]);
  expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
  // The sidebar's open-ask counts are unchanged by a reworded question.
  expect(invalidated).not.toContainEqual(["projects"]);
});

test("issue changes refresh user state and the inbox", () => {
  for (const type of ["issue.closed", "issue.updated"] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(queryClient, event(type));

    expect(invalidated).toContainEqual(["issue", "CORE-1"]);
    expect(invalidated).toContainEqual(["user-state"]);
    expect(invalidated).toContainEqual(["inbox"]);
  }
});

test("issue creation and updates refresh the affected project board query", () => {
  for (const type of ["issue.created", "issue.updated"] as const) {
    const invalidated: unknown[][] = [];
    applyEventInvalidations(
      {
        invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
          invalidated.push([...queryKey]);
          return Promise.resolve();
        },
      },
      event(type, {}, { project: "CORE" })
    );
    expect(invalidated).toContainEqual(["issues", "project", "CORE"]);
  }
});

test("artifact versions refresh the document and its anchored margin items", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("artifact.version", { artifact_id: "artifact-1" }));

  expect(invalidated).toContainEqual(["artifacts", "CORE-1"]);
  expect(invalidated).toContainEqual(["artifact", "artifact-1"]);
  expect(invalidated).toContainEqual(["comments", "CORE-1"]);
});

test("message events refresh the affected issue messages and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("message.created"));

  expect(invalidated).toContainEqual(["issue", "CORE-1"]);
  expect(invalidated).toContainEqual(["messages", "CORE-1"]);
  expect(invalidated).toContainEqual(["inbox"]);
});

test("an ask event refreshes only the document that carries the ask", () => {
  const cases: Array<[Event["type"], Record<string, unknown>]> = [
    [
      "ask.opened",
      { id: "ask-1", block_artifact: { id: "artifact-a", primary: true, slug: "spec" } },
    ],
    ["ask.answered", { anchor: { artifact_id: "artifact-a", mark_id: "m-1" }, id: "ask-1" }],
    ["ask.resolved", { anchor: null, id: "ask-1" }],
  ];
  for (const [type, payload] of cases) {
    const invalidated: unknown[][] = [];
    applyEventInvalidations(
      {
        invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
          invalidated.push([...queryKey]);
          return Promise.resolve();
        },
      },
      event(type, payload)
    );

    expect(invalidated).not.toContainEqual(["artifact", "artifact-b"]);
    expect(invalidated).not.toContainEqual(["artifact"]);
    if (type === "ask.resolved") {
      expect(invalidated.some((key) => key[0] === "artifact")).toBe(false);
    } else {
      expect(invalidated).toContainEqual(["artifact", "artifact-a"]);
    }
  }
});

test("message events refresh no document: a message cannot change one", () => {
  for (const type of ["message.created", "message.delivery", "message.answered"] as const) {
    const invalidated: unknown[][] = [];
    applyEventInvalidations(
      {
        invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
          invalidated.push([...queryKey]);
          return Promise.resolve();
        },
      },
      event(type)
    );

    expect(invalidated).toContainEqual(["messages", "CORE-1"]);
    expect(invalidated.some((key) => key[0] === "artifact")).toBe(false);
  }
});

test("issue-less agent messages refresh only that agent conversation", () => {
  const invalidated: unknown[][] = [];
  applyEventInvalidations(
    {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    },
    event(
      "message.delivery",
      { target: "session:planner-session" },
      { artifact_id: null, issue_key: null }
    )
  );

  expect(invalidated).toEqual([["agents", "planner-session", "messages"]]);
});

test("issue-targeted session messages refresh that agent conversation", () => {
  for (const type of ["message.created", "message.delivery", "message.answered"] as const) {
    const invalidated: unknown[][] = [];
    applyEventInvalidations(
      {
        invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
          invalidated.push([...queryKey]);
          return Promise.resolve();
        },
      },
      event(type, { target: "session:planner-session" })
    );

    expect(invalidated).toContainEqual(["agents", "planner-session", "messages"]);
  }
});

test("comment thread events refresh the anchored document and the inbox", () => {
  for (const type of [
    "comment.created",
    "comment.resolved",
    "comment.reopened",
    "comment.edited",
  ] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(
      queryClient,
      event(type, { anchor: { artifact_id: "artifact-1", block_id: "b-1", mark_id: "m-1" } })
    );

    expect(invalidated).toContainEqual(["comments", "CORE-1"]);
    expect(invalidated).toContainEqual(["artifact", "artifact-1"]);
    expect(invalidated).not.toContainEqual(["artifact"]);
    expect(invalidated).toContainEqual(["inbox"]);
    expect(invalidated).toContainEqual(["user-state"]);
  }
});

test("every event type refreshes user state so unread badges stay live across tabs", () => {
  for (const type of [
    "issue.created",
    "artifact.created",
    "ask.opened",
    "comment.resolved",
    "suggestion.accepted",
    "message.created",
    "child.status",
    "subscription.removed",
  ] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(queryClient, event(type));

    expect(invalidated).toContainEqual(["user-state"]);
    expect(invalidated).toContainEqual(["inbox"]);
  }
});

test("user state events refresh only the matching signed-in user's state", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };
  const stateEvent = event(
    "user_state.updated",
    { login: "bob", state: { dismissed: [], last_read_seq: 0, pinned: false } },
    { issue_key: null, project: "CORE" }
  );

  applyEventInvalidations(queryClient, stateEvent, "alice");
  expect(invalidated).toEqual([]);

  applyEventInvalidations(
    queryClient,
    event(
      "user_state.updated",
      { login: "alice", state: { dismissed: [], last_read_seq: 0, pinned: false } },
      { issue_key: null, project: "CORE" }
    ),
    "alice"
  );
  expect(invalidated).toEqual([["user-state"], ["inbox"]]);
});

test("a comment reply to an ask refreshes that ask's thread", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("comment.created", { ask_id: "ask-1" }));

  expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
});

test("a follower change refreshes the ask's thread, which lists its followers", () => {
  for (const type of ["ask.follower_added", "ask.follower_removed"] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(
      queryClient,
      event(type, { ask_id: "ask-1", by: { kind: "user", id: "alice" }, session_id: "s2" })
    );

    expect(invalidated).toContainEqual(["issue", "CORE-1"]);
    expect(invalidated).toContainEqual(["inbox"]);
    expect(invalidated).toContainEqual(["ask", "ask-1"]);
    expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
  }
});

test("SSE event prepends a newer event to the loaded log page", () => {
  const queryClient = new QueryClient();
  queryClient.setQueryData(["events", "CORE-1"], {
    pageParams: [null],
    pages: [[event("message.created", {}, { id: 7, seq: 7 })]],
  });

  prependEventToLog(queryClient, event("message.created", {}, { id: 8, seq: 8 }));

  expect(queryClient.getQueryData<{ pages: Event[][] }>(["events", "CORE-1"])?.pages[0]).toEqual([
    event("message.created", {}, { id: 8, seq: 8 }),
    event("message.created", {}, { id: 7, seq: 7 }),
  ]);
});

test("a resolved ask refreshes the inbox, issue count, and its reply thread", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.resolved", { id: "ask-1" }));

  expect(invalidated).toContainEqual(["inbox"]);
  expect(invalidated).toContainEqual(["asks", "CORE-1"]);
  expect(invalidated).toContainEqual(["projects"]);
  expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
});

test("a document event invalidates the artifact, document route, project's documents, projects, and the inbox for asks", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(
    queryClient,
    event("ask.opened", {}, { artifact_id: "artifact-1", issue_key: null, project: "CORE" })
  );

  expect(invalidated).toContainEqual(["artifact", "artifact-1"]);
  expect(invalidated).toContainEqual(["artifact-ref"]);
  expect(invalidated).toContainEqual(["project", "CORE", "artifacts"]);
  expect(invalidated).toContainEqual(["projects"]);
  expect(invalidated).toContainEqual(["inbox"]);
});

test("a subscription removal refreshes the issue's subscribers list", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("subscription.removed", { session_id: "session-1" }));

  expect(invalidated).toContainEqual(["issue", "CORE-1"]);
  expect(invalidated).toContainEqual(["subscribers", "CORE-1"]);
});

test("a document subscription removal refreshes the document's subscribers list", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(
    queryClient,
    event(
      "subscription.removed",
      { session_id: "session-1" },
      { artifact_id: "artifact-1", issue_key: null, project: "CORE" }
    )
  );

  expect(invalidated).toContainEqual(["artifact", "artifact-1"]);
  expect(invalidated).toContainEqual(["subscribers", "artifact-1"]);
  expect(invalidated).not.toContainEqual(["inbox"]);
});

test("live events invalidate ask and comment details plus project document references", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.answered", { id: "ask-1" }));
  applyEventInvalidations(queryClient, event("comment.edited", { id: "comment-1" }));
  applyEventInvalidations(
    queryClient,
    event(
      "comment.created",
      { id: "comment-2", ask_id: "ask-2" },
      {
        artifact_id: "artifact-1",
        issue_key: null,
        project: "CORE",
      }
    )
  );

  expect(invalidated).toContainEqual(["ask", "ask-1"]);
  expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
  expect(invalidated).toContainEqual(["comment", "comment-1"]);
  expect(invalidated).toContainEqual(["comment", "comment-2"]);

  expect(invalidated).toContainEqual(["ask", "ask-2"]);
  expect(invalidated).toContainEqual(["ask-thread", "ask-2"]);
  expect(invalidated).toContainEqual(["project", "CORE", "artifacts"]);
});
test("project, repository setting, and user-state events refresh their live caches", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(
    queryClient,
    event("project.created", {}, { issue_key: null, project: "CORE" })
  );
  applyEventInvalidations(
    queryClient,
    event("settings.repo_project.updated", {}, { issue_key: null, project: "CORE" })
  );
  applyEventInvalidations(queryClient, event("user_state.updated"));

  expect(invalidated).toContainEqual(["projects"]);
  expect(invalidated).toContainEqual(["project", "CORE"]);
  expect(invalidated).toContainEqual(["issues", "project", "CORE"]);
  expect(invalidated).toContainEqual(["repo-projects"]);
  expect(invalidated).toContainEqual(["user-state"]);
});
