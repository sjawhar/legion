import { describe, expect, it } from "bun:test";

import {
  closeIssue,
  createSseRouter,
  extractIssueNumberFromSubject,
  getInstallationOwners,
  postComment,
  searchDispatchThreads,
} from "../api";
import { renderSidebar, visibleSidebarThreads } from "../components/sidebar";
import { renderThreadDetail } from "../components/thread-detail";
import { createDashboardController, renderAppShell } from "../main";
import type { Comment, Issue, Thread } from "../types";

const now = "2026-05-22T12:00:00Z";

function thread(overrides: Partial<Thread>): Thread {
  return {
    repo: "sjawhar/legion",
    number: 10,
    title: "Needs decision",
    body: "---\nurgency: med\nrequestId: R\n---\n\nBody",
    state: "OPEN",
    urgency: "med",
    openAskCount: 0,
    parentNumber: 1,
    updatedAt: now,
    createdAt: now,
    authorLogin: "agent",
    commentCount: 0,
    ...overrides,
  };
}

describe("dashboard read-side rendering", () => {
  it("groups threads by parent, pins blocking rows, filters by status/urgency/search, and renders badges", () => {
    const threads = [
      thread({ number: 11, title: "Later low", urgency: "low", parentNumber: 1 }),
      thread({ number: 12, title: "Blocked deploy", urgency: "blocking", parentNumber: 2 }),
      thread({
        number: 13,
        title: "Closed note",
        state: "CLOSED",
        urgency: "high",
        parentNumber: 1,
      }),
      thread({ number: 14, title: "Child hidden", urgency: "med", parentNumber: 11 }),
    ];

    const visible = visibleSidebarThreads(threads, {
      status: "open",
      urgency: "all",
      search: "",
      selectedKey: "sjawhar/legion#12",
      showAddressed: false,
    });

    expect(visible.map((entry) => entry.thread.number)).toEqual([12, 14, 11]);
    const html = renderSidebar(threads, {
      status: "open",
      urgency: "blocking",
      search: "Blocked",
      selectedKey: "sjawhar/legion#12",
      showAddressed: false,
    });
    expect(html).toContain("#12");
    expect(html).toContain("Blocked deploy");
    expect(html).toContain("blocking");
    expect(html).not.toContain("0 sub");
    expect(html).not.toContain("Later low");
  });

  it("surfaces hidden-by-addressed count when the addressed filter empties the list", () => {
    const threads = [
      thread({ number: 21, title: "Only thread, already addressed", urgency: "med" }),
    ];
    const addressed = { "sjawhar/legion#21": "2026-05-25T13:00:00Z" };
    // thread.updatedAt is `now` (2026-05-22T12:00:00Z) — before the marker —
    // so the thread counts as still-addressed.
    const html = renderSidebar(threads, {
      status: "all",
      urgency: "all",
      search: "",
      selectedKey: undefined,
      showAddressed: false,
      addressed,
    });
    expect(html).toContain("1 addressed thread hidden");
    expect(html).toContain("Show addressed");
    expect(html).not.toContain("Only thread, already addressed");
  });

  it("matches search against the thread's origin cwd", () => {
    const threads = [
      thread({
        number: 31,
        title: "Needs decision",
        origin: {
          host: "omp",
          machine: "example-host",
          cwd: "/home/ubuntu/legion",
          tmux: "main:3.0",
        },
      }),
      thread({ number: 32, title: "Unrelated thread" }),
    ];
    const visible = visibleSidebarThreads(threads, {
      status: "all",
      urgency: "all",
      search: "ubuntu/legion",
      selectedKey: undefined,
      showAddressed: false,
    });
    expect(visible.map((entry) => entry.thread.number)).toEqual([31]);
  });

  it("renders detail without the meta marker, with conversations, marker activity rows, and inline sub-threads", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Blocked deploy",
      body: "---\nurgency: blocking\nrequestId: R\n---\n\nOpening body",
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const comments: Comment[] = [
      { id: 1, body: "normal reply", createdAt: now, updatedAt: now, authorLogin: "sami" },
      {
        id: 2,
        body: "---\nkind: urgency\nurgency: high\n---\n",
        createdAt: now,
        updatedAt: now,
        authorLogin: "agent",
      },
    ];

    const html = renderThreadDetail({
      issue,
      urgency: "blocking",
      comments,
      subThreads: [thread({ number: 15, title: "Follow-up", parentNumber: 12 })],
      repo: "sjawhar/legion",
      addressed: false,
    });

    expect(html).toContain("Blocked deploy");
    expect(html).toContain("Opening body");
    expect(html).not.toContain("urgency: blocking");
    expect(html).toContain("normal reply");
    expect(html).toContain("urgency set to <strong>high</strong>");
    expect(html).toContain("Follow-up");
    expect(html).not.toContain("origin-line");
  });

  it("renders an origin line with host/machine/cwd/tmux and a copy button carrying the tmux command", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Blocked deploy",
      body: [
        "---",
        "urgency: blocking",
        "requestId: R",
        "origin:",
        "  host: omp",
        "  machine: example-host",
        "  cwd: /home/ubuntu/legion",
        "  tmux: main:3.0",
        "  pane: '%840'",
        "---",
        "",
        "Opening body",
      ].join("\n"),
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };

    const html = renderThreadDetail({
      issue,
      urgency: "blocking",
      comments: [],
      subThreads: [],
      repo: "sjawhar/legion",
      addressed: false,
    });

    expect(html).toContain("origin-line");
    expect(html).toContain("From omp on example-host");
    expect(html).toContain("/home/ubuntu/legion");
    expect(html).toContain("tmux main:3.0");
    expect(html).toContain('data-copy-text="tmux switch-client -t %840"');
  });

  it("shows the tmux target but only offers to copy a literal pane id", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 13,
      title: "Hostile marker",
      body: '---\nurgency: med\nrequestId: R\norigin:\n  tmux: "main:3.0; rm -rf ~"\n  pane: "%1; rm -rf ~"\n---\n\nBody',
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };

    const html = renderThreadDetail({
      issue,
      urgency: "med",
      comments: [],
      subThreads: [],
      repo: "sjawhar/legion",
      addressed: false,
    });

    expect(html).toContain("tmux main:3.0; rm -rf ~");
    expect(html).not.toContain("data-copy-text");
  });

  it("keeps the ask question context visible after an answer has been submitted", () => {
    const askBody = `---\nurgency: med\nrequestId: R\nask:\n  - question: "Did the migration land cleanly?"\n    header: "Sanity check"\n    options:\n      - {label: "yes"}\n      - {label: "no"}\n---\n\nPlease confirm`;
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 42,
      title: "Awaiting confirmation",
      body: askBody,
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const answeredBody = `---\nkind: answer\nforThread: 42\nanswers:\n  - - yes\n---\n\n**Sanity check** — Did the migration land cleanly?\nyes`;
    const comments: Comment[] = [
      { id: 11, body: answeredBody, createdAt: now, updatedAt: now, authorLogin: "sami" },
    ];

    const html = renderThreadDetail({
      issue,
      urgency: "med",
      comments,
      subThreads: [],
      repo: "sjawhar/legion",
      addressed: false,
    });

    // The interactive ask form must be hidden once an answer exists…
    expect(html).not.toContain('class="ask-form"');
    // …but the question context must remain visible in the opening section,
    // separate from the answer comment card buried in the conversation.
    expect(html).toContain('class="ask-context"');
    expect(html).toContain("Sanity check");
    expect(html).toContain("Did the migration land cleanly?");
  });

  it("routes synthetic SSE subjects to sidebar, comment, and metadata refetches", () => {
    const calls: string[] = [];
    const router = createSseRouter({
      refetchSidebar: () => {
        calls.push("sidebar");
      },
      refetchComments: (_repo, number) => {
        calls.push(`comments:${number}`);
      },
      refetchIssue: (_repo, number) => {
        calls.push(`issue:${number}`);
      },
      highlightThread: (_repo, number) => calls.push(`highlight:${number}`),
    });

    router({
      repo: "sjawhar/legion",
      subject: "notifications.github.sjawhar.legion.issue.12.comment",
      payload: {},
    });
    router({
      repo: "sjawhar/legion",
      subject: "notifications.github.sjawhar.legion.issue.12.sub_issue",
      payload: {},
    });
    router({
      repo: "sjawhar/legion",
      subject: "notifications.github.sjawhar.legion.issue.12",
      payload: {},
    });
    router({
      repo: "sjawhar/legion",
      subject: "notifications.github.sjawhar.legion.issue.12.closed",
      payload: {},
    });
    router({
      repo: "sjawhar/legion",
      subject: "notifications.github.sjawhar.legion.issue.12.reopened",
      payload: {},
    });

    expect(calls).toEqual([
      "comments:12",
      "highlight:12",
      "sidebar",
      "highlight:12",
      // bare issue.12: refetch issue + sidebar (state may now match/miss filter)
      "issue:12",
      "sidebar",
      "highlight:12",
      // .closed: same path as bare, sidebar needs to drop or update the entry
      "issue:12",
      "sidebar",
      "highlight:12",
      // .reopened: same path
      "issue:12",
      "sidebar",
      "highlight:12",
    ]);
  });

  it("exercises the read-side flow through the app controller with realistic fixtures", async () => {
    const comments: Comment[] = [
      { id: 1, body: "hello", createdAt: now, updatedAt: now, authorLogin: "sami" },
    ];
    const searchCalls: string[][] = [];
    const api = {
      searchDispatchThreads: async (owners: string[]) => {
        searchCalls.push(owners);
        return [
          thread({ number: 12, title: "Blocked deploy", urgency: "blocking", parentNumber: 1 }),
          thread({ number: 15, title: "Sub decision", urgency: "high", parentNumber: 12 }),
        ];
      },
      getIssue: async (_repo: string, number: number) => ({
        repo: _repo,
        number,
        title: "Blocked deploy",
        body: "---\nurgency: blocking\nrequestId: R\n---\n\nOpening body",
        state: "OPEN" as const,
        stateReason: null,
        updatedAt: now,
        createdAt: now,
        authorLogin: "agent",
      }),
      getComments: async () => comments,
      postComment: async () => {
        throw new Error("not used");
      },
      closeIssue: async () => {
        throw new Error("not used");
      },
    };

    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    await controller.selectThread("sjawhar/legion", 12);

    // The controller boots its thread list from the resolved installation
    // owners in a single owner-scoped call, not a per-repo fan-out.
    expect(searchCalls).toEqual([["sjawhar"]]);
    expect(controller.render()).toContain("Blocked deploy");
    expect(controller.render()).toContain("Opening body");
    expect(controller.visibleThreads().map((entry) => entry.thread.number)).toEqual([12, 15]);
    controller.highlightThread("sjawhar/legion", 12);
    expect(controller.render()).toContain("live-highlight");
    expect(controller.nextSelection("j")).toEqual({ repo: "sjawhar/legion", number: 15 });
    expect(controller.toggleSidebar()).toBe(false);
    expect(controller.toggleHelp()).toBe(true);
  });

  it("shows the search error in the sidebar instead of an empty list, and keeps the last good list", async () => {
    let fail = false;
    const api = {
      searchDispatchThreads: async () => {
        if (fail) throw new Error("GraphQL 401: bad credentials");
        return [thread({ number: 12, title: "Blocked deploy" })];
      },
      getIssue: async () => {
        throw new Error("not used");
      },
      getComments: async () => [],
      postComment: async () => {
        throw new Error("not used");
      },
      closeIssue: async () => {
        throw new Error("not used");
      },
    };
    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    expect(controller.render()).toContain("Blocked deploy");
    expect(controller.render()).not.toContain("load-error");

    fail = true;
    await controller.loadThreads();
    const html = controller.render();
    expect(html).toContain("load-error");
    expect(html).toContain("GraphQL 401: bad credentials");
    expect(controller.visibleThreads().map((entry) => entry.thread.number)).toEqual([12]);

    fail = false;
    await controller.loadThreads();
    expect(controller.render()).not.toContain("load-error");
  });

  it("posts replies with optimistic append and replaces the placeholder with the API comment", async () => {
    const comments: Comment[] = [];
    const calls: Array<{ repo: string; number: number; body: string }> = [];
    const api = {
      searchDispatchThreads: async () => [thread({ number: 12 })],
      getIssue: async (_repo: string, number: number) => ({
        repo: _repo,
        number,
        title: "Needs decision",
        body: "---\nurgency: med\nrequestId: R\n---\n\nOpening body",
        state: "OPEN" as const,
        stateReason: null,
        updatedAt: now,
        createdAt: now,
        authorLogin: "agent",
      }),
      getComments: async () => comments,
      postComment: async (repo: string, number: number, body: string) => {
        calls.push({ repo, number, body });
        return { id: 99, body, createdAt: now, updatedAt: now, authorLogin: "sami" };
      },
      closeIssue: async () => {
        throw new Error("not used");
      },
    };

    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    await controller.selectThread("sjawhar/legion", 12);
    const posting = controller.postReply("verifying reply");

    expect(controller.render()).toContain("verifying reply");
    expect(controller.render()).toContain("disabled");
    await posting;

    expect(calls).toEqual([{ repo: "sjawhar/legion", number: 12, body: "verifying reply" }]);
    expect(controller.state.comments.get("sjawhar/legion#12")).toEqual([
      { id: 99, body: "verifying reply", createdAt: now, updatedAt: now, authorLogin: "sami" },
    ]);
  });

  it("renders unanswered asks and submits answer marker comments", async () => {
    const ask = [
      {
        header: "Color",
        question: "Color?",
        options: [
          { label: "red", description: "warm" },
          { label: "blue", description: "cool" },
        ],
        custom: true,
      },
      {
        header: "Picks",
        question: "Pick 1+",
        options: [
          { label: "a", description: "" },
          { label: "b", description: "" },
        ],
        multiple: true,
      },
    ];
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Needs answer",
      body: `---\nurgency: med\nrequestId: R\nask:\n${ask
        .map(
          (q) =>
            `  - question: ${JSON.stringify(q.question)}\n    header: ${JSON.stringify(q.header)}\n    options:\n${q.options
              .map((o) => `      - {label: ${JSON.stringify(o.label)}}`)
              .join("\n")}`
        )
        .join("\n")}\n---\n\nChoose`,
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const calls: string[] = [];
    const api = {
      searchDispatchThreads: async () => [
        thread({ number: 12, openAskCount: 1, body: issue.body }),
      ],
      getIssue: async () => issue,
      getComments: async () => [],
      postComment: async (_repo: string, _number: number, body: string) => {
        calls.push(body);
        return { id: 100, body, createdAt: now, updatedAt: now, authorLogin: "sami" };
      },
      closeIssue: async () => issue,
    };

    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    await controller.selectThread("sjawhar/legion", 12);

    expect(controller.render()).toContain("Color?");
    expect(controller.render()).toContain("Other (specify)");
    await controller.submitAskAnswer([["blue"], ["a", "b"]]);

    expect(calls[0]?.startsWith("<!-- dispatch:answer\n")).toBe(true);
    expect(calls[0]).toContain("forThread: 12");
    expect(calls[0]).toContain("Color"); // header in summary
    expect(calls[0]).toContain("Color?"); // question prompt in summary
    expect(calls[0]).toContain("blue"); // answer value in summary
    expect(controller.render()).toContain("Color?");
    expect(controller.render()).toContain("answer-pill");
    expect(controller.render()).toContain(">blue<");
  });

  it("posts urgency marker comments and closes issues optimistically", async () => {
    const calls: string[] = [];
    const closed: Array<{ repo: string; number: number; reason: "completed" | "not_planned" }> = [];
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Needs decision",
      body: "---\nurgency: med\nrequestId: R\n---\n\nOpening body",
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const api = {
      searchDispatchThreads: async () => [thread({ number: 12, urgency: "med" })],
      getIssue: async () => issue,
      getComments: async () => [],
      postComment: async (_repo: string, _number: number, body: string) => {
        calls.push(body);
        return { id: 77, body, createdAt: now, updatedAt: now, authorLogin: "sami" };
      },
      closeIssue: async (repo: string, number: number, reason: "completed" | "not_planned") => {
        closed.push({ repo, number, reason });
        return { ...issue, state: "CLOSED" as const, stateReason: reason };
      },
    };

    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    await controller.selectThread("sjawhar/legion", 12);
    const urgencyPost = controller.setUrgency("high");

    expect(controller.render()).toContain("urgency-badge-high");
    await urgencyPost;
    expect(calls[0]?.startsWith("<!-- dispatch:urgency\n")).toBe(true);
    expect(calls[0]).toContain("Urgency set to **high**.");

    const closePost = controller.closeSelectedIssue("completed");
    expect(controller.render()).toContain("resolved");
    await closePost;
    expect(closed).toEqual([{ repo: "sjawhar/legion", number: 12, reason: "completed" }]);
  });
});

describe("GitHub API client shaping", () => {
  it("searches dispatch threads via GraphQL across every owner and parses repo + marker metadata", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body));
      expect(body.query).toContain("search(query: $search");
      expect(body.query).toContain("repository { owner { login } name }");
      expect(body.query).toContain("comments(last: 30) {");
      expect(body.query).toContain(
        "nodes { databaseId body createdAt updatedAt author { login } }"
      );
      expect(body.variables.search).toBe(
        "is:issue is:open label:dispatch-thread user:sjawhar user:acme-org"
      );
      return new Response(
        JSON.stringify({
          data: {
            search: {
              nodes: [
                {
                  number: 12,
                  title: "Blocked deploy",
                  body: "---\nurgency: blocking\nrequestId: R\n---\n\nBody",
                  state: "OPEN",
                  updatedAt: now,
                  createdAt: now,
                  author: { login: "agent" },
                  comments: { totalCount: 2, nodes: [] },
                  parent: { number: 641 },
                  repository: { owner: { login: "sjawhar" }, name: "legion" },
                },
              ],
            },
          },
        }),
        { headers: { "content-type": "application/json" } }
      );
    }) as typeof fetch;
    try {
      expect(await searchDispatchThreads(["sjawhar", "acme-org"])).toEqual([
        {
          repo: "sjawhar/legion",
          number: 12,
          title: "Blocked deploy",
          body: "---\nurgency: blocking\nrequestId: R\n---\n\nBody",
          state: "OPEN",
          urgency: "blocking",
          openAskCount: 0,
          parentNumber: 641,
          updatedAt: now,
          createdAt: now,
          authorLogin: "agent",
          commentCount: 2,
        },
      ]);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("counts open asks from the search window so the sidebar can mark threads that need you", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(
        JSON.stringify({
          data: {
            search: {
              nodes: [
                {
                  number: 12,
                  title: "Two asks, one answered",
                  body: "<!-- dispatch:thread\nrequestId: R\nurgency: med\nask:\n    - askId: R\n      question: a?\n    - askId: R.1\n      question: b?\n-->\n\nBody",
                  state: "OPEN",
                  updatedAt: now,
                  createdAt: now,
                  author: { login: "agent" },
                  comments: {
                    totalCount: 1,
                    nodes: [
                      {
                        databaseId: 5,
                        body: '<!-- dispatch:answer\nforThread: 12\nforAsk: "R"\nanswers:\n  - - "yes"\n-->\n\ns',
                        createdAt: now,
                        updatedAt: now,
                        author: { login: "sami" },
                      },
                    ],
                  },
                  parent: null,
                  repository: { owner: { login: "sjawhar" }, name: "legion" },
                },
              ],
            },
          },
        }),
        { headers: { "content-type": "application/json" } }
      )) as typeof fetch;
    try {
      const [thread] = await searchDispatchThreads(["sjawhar"]);
      expect(thread?.openAskCount).toBe(1);
      const filters = { status: "open", urgency: "all", search: "", showAddressed: false } as const;
      expect(renderSidebar([thread as Thread], filters)).toContain("needs you");
      expect(renderSidebar([{ ...(thread as Thread), openAskCount: 0 }], filters)).not.toContain(
        "needs you"
      );
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("returns no threads and skips the request when there are no owners", async () => {
    expect(await searchDispatchThreads([])).toEqual([]);
  });

  it("extracts issue numbers from known SSE subject shapes", () => {
    expect(extractIssueNumberFromSubject("notifications.github.o.r.issue.99.comment")).toBe(99);
    expect(extractIssueNumberFromSubject("notifications.github.o.r.issue.99")).toBe(99);
    expect(extractIssueNumberFromSubject("notifications.github.o.r.pull.99")).toBeNull();
  });

  it("posts comments and closes issues through the REST proxy with expected request shapes", async () => {
    const originalFetch = globalThis.fetch;
    const calls: Array<{ input: string; init?: RequestInit }> = [];
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input: String(input), init });
      return new Response(
        JSON.stringify({
          id: 1,
          number: 12,
          title: "Closed",
          body: "ok",
          state: "closed",
          state_reason: "completed",
          created_at: now,
          updated_at: now,
          user: { login: "sami" },
        }),
        { headers: { "content-type": "application/json" } }
      );
    }) as typeof fetch;
    try {
      await postComment("sjawhar/legion", 12, "hello");
      await closeIssue("sjawhar/legion", 12, "completed");
    } finally {
      globalThis.fetch = originalFetch;
    }

    expect(calls[0]?.input).toBe("/api/github/rest/repos/sjawhar/legion/issues/12/comments");
    expect(calls[0]?.init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ body: "hello" });
    expect(calls[1]?.input).toBe("/api/github/rest/repos/sjawhar/legion/issues/12");
    expect(calls[1]?.init?.method).toBe("PATCH");
    expect(JSON.parse(String(calls[1]?.init?.body))).toEqual({
      state: "closed",
      state_reason: "completed",
    });
  });

  it("derives distinct owner logins from the installations proxy response", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      expect(String(input)).toBe("/api/installations?per_page=100");
      return new Response(
        JSON.stringify({
          installations: [
            { account: { login: "sjawhar" } },
            { account: { login: "acme-org" } },
            { account: { login: "sjawhar" } },
            { account: null },
          ],
        }),
        { headers: { "content-type": "application/json" } }
      );
    }) as typeof fetch;
    try {
      expect(await getInstallationOwners()).toEqual(["sjawhar", "acme-org"]);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});

describe("SPA shell", () => {
  it("renders auth affordance and app regions", () => {
    const html = renderAppShell();

    expect(html).toContain("Sign in with GitHub");
    expect(html).toContain("dashboard-root");
    expect(html).toContain("owner-label");
    expect(html).toContain("owner-error-overlay");
  });
});
