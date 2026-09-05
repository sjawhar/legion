import { describe, expect, it } from "bun:test";

import {
  closeIssue,
  createSseRouter,
  extractIssueNumberFromSubject,
  getInstallationOwners,
  postComment,
  searchDispatchThreads,
} from "../api";
import { collectAnswers, collectAsks, openAsks } from "../asks";
import { renderSidebar, visibleSidebarThreads } from "../components/sidebar";
import { renderThreadDetail, type ThreadDetailInput } from "../components/thread-detail";
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

function detail(
  issue: Issue,
  comments: Comment[],
  extra: Partial<ThreadDetailInput> = {}
): ThreadDetailInput {
  const asks = collectAsks(issue.body, comments);
  const answers = collectAnswers(comments);
  return {
    issue,
    urgency: "med",
    comments,
    asks,
    answers,
    openAsks: openAsks(asks, answers),
    subThreads: [],
    repo: issue.repo,
    addressed: false,
    ...extra,
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

    const html = renderThreadDetail(
      detail(issue, comments, {
        urgency: "blocking",
        subThreads: [thread({ number: 15, title: "Follow-up", parentNumber: 12 })],
      })
    );

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

    const html = renderThreadDetail(detail(issue, [], { urgency: "blocking" }));

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

    const html = renderThreadDetail(detail(issue, []));

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

    const html = renderThreadDetail(detail(issue, comments));

    // The interactive ask form must be gone once an answer exists…
    expect(html).not.toMatch(/<form class="ask-form"/);
    // …but the question keeps its history card (Playwright selects it by askId),
    // with the answer rendered directly beneath it.
    expect(html).toContain('<div class="ask-history" data-ask-id="R">');
    expect(html).toContain("Sanity check");
    expect(html).toContain("Did the migration land cleanly?");
    expect(html).toContain(">yes<");
  });

  it("renders one form per open ask, history under answered asks, and a follow-up turn card", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Two asks",
      body: "<!-- dispatch:thread\nrequestId: R\nurgency: med\norigin:\n    host: omp\n    sessionId: ses_1\n    sessionTitle: 'pm: e2e submitter identity'\n    tmux: main:3.0\n    pane: '%15'\nask:\n    - askId: R\n      question: Color?\n      header: Color\n      options:\n        - label: blue\n    - askId: R.1\n      question: Size?\n      header: Size\n      options:\n        - label: small\n-->\n\nOpening body",
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const comments: Comment[] = [
      {
        id: 1,
        body: '<!-- dispatch:answer\nforThread: 12\nforAsk: "R"\nanswers:\n  - - "blue"\n-->\n\n**Color** — Color?\nblue',
        createdAt: now,
        updatedAt: now,
        authorLogin: "sami",
      },
      {
        id: 2,
        body: "<!-- dispatch:ask\nrequestId: F\norigin:\n    host: omp\n    sessionId: ses_2\n    sessionTitle: renamed\nask:\n    - askId: F\n      question: Which lane?\n      header: Lane\n      options:\n        - label: A\n        - label: B\n-->\n\n## Context\n\nThe reply changed the question.\n\n## Question\n\nWhich lane?",
        createdAt: now,
        updatedAt: now,
        authorLogin: "agent",
      },
    ];
    const html = renderThreadDetail(detail(issue, comments));

    // Forms only for the open asks, each naming its id; the answered ask R keeps
    // its data-ask-id on the ask-history div (Playwright selects it there) but has no form.
    expect(html).toMatch(/<form class="ask-form"[^>]*data-ask-id="R\.1"/);
    expect(html).toMatch(/<form class="ask-form"[^>]*data-ask-id="F"/);
    expect(html).not.toMatch(/<form class="ask-form"[^>]*data-ask-id="R"/);
    expect(html).toContain('<div class="ask-history" data-ask-id="R">');
    expect(html.match(/class="ask-form"/g)?.length).toBe(2);
    // The answered ask shows its answer beneath the question, and the answer
    // comment is not repeated in the conversation (one pill on the page).
    expect(html).toContain("answer-pill");
    expect(html).toContain(">blue<");
    expect(html.match(/class="answer-pill"/g)?.length).toBe(1);
    // The follow-up renders as a turn card with its prose and a waiting marker for its open ask.
    expect(html).toContain('id="turn-2"');
    expect(html).toContain("The reply changed the question.");
    expect(html).toContain("ask-waiting");
    // Session identity on the header origin line, with copy, and the tmux jump kept.
    expect(html).toContain('<span class="origin-session-title">pm: e2e submitter identity</span>');
    expect(html).toContain('<code class="origin-session-id">ses_1</code>');
    expect(html).toContain('data-action="copy-session-id" data-copy-text="ses_1"');
    expect(html).toContain('data-copy-text="tmux switch-client -t %15"');
    // The follow-up turn shows the session that asked it.
    expect(html).toContain("renamed");
    // No plumbing visible.
    expect(html).not.toContain("requestId");
    expect(html).not.toContain("dispatch:");
    // The conversation has comments even though the answer renders under its
    // question rather than in place; no false empty state.
    expect(html).not.toContain("No comments yet.");
  });

  it("renders an answer for an ask that is not on the thread in place, marked as such", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "T",
      body: "<!-- dispatch:thread\nrequestId: R\nurgency: med\n-->\n\nBody",
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const comments: Comment[] = [
      {
        id: 9,
        body: '<!-- dispatch:answer\nforThread: 12\nforAsk: "ghost"\nanswers:\n  - - "x"\n-->\n\nsummary',
        createdAt: now,
        updatedAt: now,
        authorLogin: "sami",
      },
    ];
    const html = renderThreadDetail(detail(issue, comments));
    expect(html).toContain("answer to a question no longer on this thread");
    expect(html).toContain(">x<");
  });

  it("shows a second answer to the same ask in the conversation instead of hiding it", () => {
    const issue: Issue = {
      repo: "sjawhar/legion",
      number: 12,
      title: "Changed mind",
      body: "<!-- dispatch:thread\nrequestId: R\nurgency: med\nask:\n    - askId: R\n      question: Color?\n      header: Color\n      options:\n        - label: blue\n        - label: red\n-->\n\nBody",
      state: "OPEN",
      stateReason: null,
      updatedAt: now,
      createdAt: now,
      authorLogin: "agent",
    };
    const answer = (id: number, value: string): Comment => ({
      id,
      body: `<!-- dispatch:answer\nforThread: 12\nforAsk: "R"\nanswers:\n  - - "${value}"\n-->\n\n**Color** — Color?\n${value}`,
      createdAt: now,
      updatedAt: now,
      authorLogin: "sami",
    });
    const comments = [answer(1, "blue"), answer(2, "red")];
    const html = renderThreadDetail(detail(issue, comments));

    // The first answer is the one the question shows; the second stays at its
    // own position, tagged, so nothing on GitHub is invisible.
    expect(html).toMatch(/<div class="ask-history" data-ask-id="R">[\s\S]*?>blue</);
    expect(html).toMatch(/data-comment-id="2"[\s\S]*?later answer[\s\S]*?>red</);
    expect(html).not.toContain('data-comment-id="1"');
    expect(html.match(/class="answer-pill"/g)?.length).toBe(2);
    expect(html.match(/class="comment-tag">later answer</g)?.length).toBe(1);
    expect(html).not.toContain("no longer on this thread");
    // The ask is answered: no form.
    expect(html).not.toMatch(/<form class="ask-form"/);
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
      persistAddressed: async () => {},
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
      persistAddressed: async () => {},
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
      persistAddressed: async () => {},
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
      persistAddressed: async () => {},
    };

    const controller = createDashboardController({ owners: ["sjawhar"], api });
    await controller.loadThreads();
    await controller.selectThread("sjawhar/legion", 12);

    expect(controller.render()).toContain("Color?");
    expect(controller.render()).toContain("Other (specify)");
    expect(controller.render().match(/class="ask-form"/g)?.length).toBe(2);
    await expect(controller.submitAskAnswer("nope", ["blue"])).rejects.toThrow(
      "askId nope is not on this thread"
    );
    await controller.submitAskAnswer("R", ["blue"]);

    expect(calls[0]?.startsWith("<!-- dispatch:answer\n")).toBe(true);
    expect(calls[0]).toContain("forThread: 12");
    expect(calls[0]).toContain('forAsk: "R"');
    expect(calls[0]).toContain("Color"); // header in summary
    expect(calls[0]).toContain("Color?"); // question prompt in summary
    expect(calls[0]).toContain("blue"); // answer value in summary
    const after = controller.render();
    expect(after).toContain("Color?");
    expect(after).toContain("answer-pill");
    expect(after).toContain(">blue<");
    // The other ask is still open: exactly one form remains, and it names R.1.
    expect(after.match(/class="ask-form"/g)?.length).toBe(1);
    expect(after).toMatch(/<form class="ask-form"[^>]*data-ask-id="R\.1"/);
    // Loaded comments are authoritative for the sidebar's "needs you" count.
    expect(controller.sidebarFilters().openAskCounts).toEqual({ "sjawhar/legion#12": 1 });
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
      persistAddressed: async () => {},
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
