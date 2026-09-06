// The issues the fixture backend serves. Three shapes the dashboard must
// handle: a conversation thread in the HTML-comment encoding with a settled
// body ask, an open body ask, and a follow-up ask; a legacy front-matter
// thread; and a plain pull request that the opening body references.

export interface FixtureComment {
  id: number;
  body: string;
  author: string;
  createdAt: string;
}

export interface FixtureIssue {
  repo: string;
  number: number;
  title: string;
  body: string;
  state: "open" | "closed";
  createdAt: string;
  author: string;
  comments: FixtureComment[];
}

const t0 = "2026-09-05T10:00:00Z";
export const REPO = "acme-org/example-repo";

export const SESSION_ID = "01a05ac6-3b19-7000-9d2b-1e5f0a6c2b7d";

export const conversationThread: FixtureIssue = {
  repo: REPO,
  number: 12,
  title: "E2E harness: which identity submits test PRs?",
  body: [
    "<!-- dispatch:thread",
    "requestId: R12",
    "urgency: high",
    "origin:",
    "    host: omp",
    "    machine: example-host",
    "    cwd: /home/ubuntu/legion",
    "    tmux: dev4:4.2",
    "    pane: '%15'",
    `    sessionId: ${SESSION_ID}`,
    "    sessionTitle: 'pm: e2e submitter identity'",
    "ask:",
    "    - askId: R12",
    "      question: Which identity should the harness use?",
    "      header: Submitter",
    "      options:",
    "        - label: Env var",
    "        - label: Shared bot",
    "    - askId: R12.1",
    "      question: Roll out to CI now?",
    "      header: Rollout",
    "      options:",
    "        - label: Now",
    "        - label: Next week",
    "-->",
    "",
    "**E2E harness: which identity submits test PRs?**",
    "",
    "## Context",
    "",
    `The harness in #7 submits PRs as the shared bot, see ${REPO}#9 and https://github.com/${REPO}/pull/9. Unrelated: #999.`,
    "",
    "## Question",
    "",
    "Pick an identity.",
  ].join("\n"),
  state: "open",
  createdAt: t0,
  author: "agent",
  comments: [
    { id: 101, body: "Why not the bot?", author: "sami", createdAt: "2026-09-05T10:05:00Z" },
    {
      id: 102,
      body: '<!-- dispatch:answer\nforThread: 12\nforAsk: "R12"\nanswers:\n  - - "Env var"\n-->\n\n**Submitter** — Which identity should the harness use?\nEnv var',
      author: "sami",
      createdAt: "2026-09-05T10:06:00Z",
    },
    {
      id: 103,
      body: [
        "<!-- dispatch:ask",
        "requestId: F1",
        "origin:",
        "    host: omp",
        "    sessionId: 01a05ac6-3b19-7000-9d2b-000000000002",
        "    sessionTitle: 'pm: e2e submitter identity (handoff)'",
        "ask:",
        "    - askId: F1",
        "      question: Which variable name?",
        "      header: Variable",
        "      options:",
        "        - label: E2E_SUBMITTER_EMAIL",
        "        - label: E2E_SUBMITTER",
        "-->",
        "",
        "## Context",
        "",
        "The bot cannot be told apart from real submitters.",
        "",
        "## Question",
        "",
        "Which variable name?",
      ].join("\n"),
      author: "agent",
      createdAt: "2026-09-05T10:07:00Z",
    },
  ],
};

export const legacyThread: FixtureIssue = {
  repo: REPO,
  number: 7,
  title: "Pick a color",
  body: "---\nurgency: med\nrequestId: L7\nask:\n  - question: Color?\n    header: Color\n    options:\n      - label: blue\n      - label: red\n---\n\n**Pick a color**\n\n## Context\n\nLegacy thread.\n\n## Question\n\nBlue or red?",
  state: "open",
  createdAt: t0,
  author: "agent",
  comments: [
    {
      id: 71,
      body: "---\nkind: answer\nforThread: 7\nanswers:\n  - [blue]\n---\n\n**Color** — Color?\nblue",
      author: "sami",
      createdAt: "2026-09-05T10:01:00Z",
    },
  ],
};

export const referencedPr: FixtureIssue = {
  repo: REPO,
  number: 9,
  title: "Add e2e submitter identity",
  body: "not a dispatch thread",
  state: "open",
  createdAt: t0,
  author: "agent",
  comments: [],
};

export const fixtureIssues: FixtureIssue[] = [conversationThread, legacyThread, referencedPr];

/** An answer comment as the dashboard (or another dashboard) writes it. */
export function answerComment(threadNumber: number, askId: string, value: string): string {
  return `<!-- dispatch:answer\nforThread: ${threadNumber}\nforAsk: ${JSON.stringify(askId)}\nanswers:\n  - - ${JSON.stringify(value)}\n-->\n\n${value}`;
}

export interface FixtureAsk {
  askId: string;
  question: string;
  options: string[];
}

/** A follow-up ask comment. JSON strings are valid double-quoted YAML scalars, so any id round-trips. */
export function askComment(requestId: string, asks: FixtureAsk[]): string {
  return [
    "<!-- dispatch:ask",
    `requestId: ${JSON.stringify(requestId)}`,
    "ask:",
    ...asks.flatMap((ask) => [
      `    - askId: ${JSON.stringify(ask.askId)}`,
      `      question: ${JSON.stringify(ask.question)}`,
      "      options:",
      ...ask.options.map((label) => `        - label: ${JSON.stringify(label)}`),
    ]),
    "-->",
    "",
    "## Question",
    "",
    ...asks.map((ask) => ask.question),
  ].join("\n");
}
