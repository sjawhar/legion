import { afterAll, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { main } from "./fetch-spec-sources";

const scratch = realpathSync(mkdtempSync(path.join(tmpdir(), "fetch-spec-sources-")));
afterAll(() => rmSync(scratch, { recursive: true, force: true }));

function dir(name: string): string {
  const made = path.join(scratch, name);
  mkdirSync(made, { recursive: true });
  return made;
}

const alice = { kind: "user", id: "alice@example.com" };
const bob = { kind: "user", id: "bob@example.com" };
const agent = (title?: string) => ({
  kind: "session",
  id: "session-1",
  ...(title === undefined ? {} : { origin: { session_title: title } }),
});

function message(id: string, author: object, body: string, createdAt: string) {
  return {
    id,
    issue_key: "ACME-7",
    author,
    body,
    target: null,
    in_reply_to: null,
    deliveries: [],
    created_at: createdAt,
  };
}

function comment(id: string, author: object, body: string, createdAt: string, quote?: string) {
  return {
    id,
    issue_key: "ACME-7",
    author,
    body,
    anchor:
      quote === undefined
        ? null
        : {
            artifact_id: "art-7",
            block_id: "b1",
            mark_id: "m1",
            version: 2,
            quote,
            orphaned: false,
          },
    reply_to: null,
    ask_id: null,
    created_at: createdAt,
  };
}

function ask(fields: {
  id: string;
  author: object;
  kind: string;
  question: string;
  options: object[];
  state: string;
  created_at: string;
  answer: object | null;
}) {
  return {
    ...fields,
    issue_key: "ACME-7",
    multiple: false,
    urgency: "normal",
    anchor: null,
    opened_event_id: 1,
    edited_at: null,
    last_reply: null,
  };
}

/** Production Dispatch as the test serves it: path to answer. */
const ROUTES: Record<string, unknown> = {
  "/api/v1/issues": [
    { key: "ACME-7", title: "Make the widget faster", status: "done" },
    { key: "ACME-8", title: "Other", status: "todo" },
  ],
  "/api/v1/issues/ACME-7": {
    key: "ACME-7",
    title: "Make the widget faster",
    primary_artifact_id: "art-7",
  },
  "/api/v1/issues/ACME-7/messages": [
    {
      message: message("m1", alice, "Please make the widget faster.", "2026-10-16T10:00:00Z"),
      replies: [
        message("m2", agent("widget work"), "On it.", "2026-10-16T12:00:00Z"),
        message("m3", alice, "Thanks.\n", "2026-10-16T14:00:00.000001Z"),
      ],
    },
  ],
  "/api/v1/issues/ACME-7/comments": [
    comment("c1", bob, "Why 5 s?", "2026-10-16T11:00:00Z", "The widget waits 5 s.\n\nIt is slow."),
    comment("c2", agent(), "Fixed.", "2026-10-16T13:00:00Z"),
    comment("c3", bob, "Good.", "2026-10-16T15:00:00Z"),
    // An unanchored comment whose body starts with a block quote: the person's own paste.
    comment("c4", bob, "> The ticket says 5 s.\n\nIt is wrong.", "2026-10-16T15:30:00Z"),
  ],
  "/api/v1/issues/ACME-7/asks": [
    ask({
      id: "a2",
      author: agent("widget work"),
      kind: "question",
      question: "Which cache?",
      options: [{ label: "Redis", description: "A cache server." }, { label: "None" }],
      state: "answered",
      created_at: "2026-10-16T16:00:00Z",
      answer: {
        user: "alice@example.com",
        selected: ["Redis"],
        text: "Use Redis.",
        at: "2026-10-16T17:00:00Z",
      },
    }),
    ask({
      id: "a1",
      author: agent(),
      kind: "approval",
      question: "Approve the spec?",
      options: [{ label: "Approve" }, { label: "Request changes" }],
      state: "open",
      created_at: "2026-10-15T09:00:00Z",
      answer: null,
    }),
  ],
  "/api/v1/issues/ACME-8/messages": [],
  "/api/v1/issues/ACME-8/comments": [],
  "/api/v1/issues/ACME-8/asks": [],
  "/api/v1/artifacts/art-7/text": { markdown: "## Summary\n\nThe widget is slow.\n", version: 3 },
  "/api/v1/projects/ACME/artifacts/brief-md": {
    id: "art-9",
    name: "brief.md",
    slug: "brief-md",
    created_at: "2026-10-20T08:00:00Z",
    versions: [
      { number: 1, created_at: "2026-10-20T08:00:00Z" },
      { number: 2, created_at: "2026-10-21T09:30:00Z" },
    ],
  },
  "/api/v1/projects/ACME/artifacts/brief-md/text": {
    markdown: "# Brief\n\nMake specs short.\n",
    version: 2,
  },
};

function world() {
  const sent: { url: URL; init: RequestInit }[] = [];
  const fetch = async (input: string, init: RequestInit): Promise<Response> => {
    const url = new URL(input);
    sent.push({ url, init });
    const body = url.host === "dispatch.test" ? ROUTES[url.pathname] : undefined;
    return body === undefined
      ? new Response(JSON.stringify({ error: "not found", code: "NOT_FOUND" }), {
          status: 404,
          headers: { "content-type": "application/json" },
        })
      : new Response(JSON.stringify(body), { headers: { "content-type": "application/json" } });
  };
  return { fetch, sent };
}

function io(fetch: ReturnType<typeof world>["fetch"]) {
  const stdout: string[] = [];
  const stderr: string[] = [];
  return {
    stdout,
    stderr,
    io: {
      env: {
        HOME: scratch,
        DISPATCH_URL: "http://dispatch.test",
        DISPATCH_TOKEN: "dispatch-token",
      },
      cwd: scratch,
      fetch,
      sleep: async () => {},
      out: (line: string) => stdout.push(line),
      err: (line: string) => stderr.push(line),
    },
  };
}

const read = (file: string) => readFileSync(file, "utf8");

test("an issue key and a project document each write their files; the document gets no old-spec.md", async () => {
  const out = path.join(dir("sources-run"), "sources");
  // A stale file from an earlier fetch of the document does not survive the new fetch.
  mkdirSync(path.join(out, "acme-brief-md"), { recursive: true });
  writeFileSync(path.join(out, "acme-brief-md", "old-spec.md"), "stale\n");
  const { fetch, sent } = world();
  const run = io(fetch);
  const code = await main(["--out", out, "ACME-7", "dispatch://ACME/artifact/brief-md"], run.io);
  expect(run.stderr).toEqual([]);
  expect(code).toBe(0);
  expect(sent.every(({ init }) => init.method === "GET" && init.body === undefined)).toBe(true);
  expect(sent.some(({ url }) => url.search !== "")).toBe(false);

  const issue = path.join(out, "acme-7");
  expect(readdirSync(issue).toSorted()).toEqual([
    "asks.json",
    "messages.md",
    "old-spec.md",
    "title.txt",
  ]);
  expect(read(path.join(issue, "title.txt"))).toBe("Make the widget faster\n");
  expect(read(path.join(issue, "messages.md"))).toBe(
    [
      "<!-- message 2026-10-16T10:00:00Z -->",
      "Please make the widget faster.",
      "",
      "<!-- comment 2026-10-16T11:00:00Z anchored -->",
      "> The widget waits 5 s.",
      ">",
      "> It is slow.",
      "",
      "Why 5 s?",
      "",
      "<!-- message 2026-10-16T14:00:00.000001Z -->",
      "Thanks.",
      "",
      "<!-- comment 2026-10-16T15:00:00Z -->",
      "Good.",
      "",
      "<!-- comment 2026-10-16T15:30:00Z -->",
      "> The ticket says 5 s.",
      "",
      "It is wrong.",
      "",
    ].join("\n")
  );
  expect(JSON.parse(read(path.join(issue, "asks.json")))).toEqual([
    {
      id: "a1",
      kind: "approval",
      question: "Approve the spec?",
      options: [{ label: "Approve" }, { label: "Request changes" }],
      multiple: false,
      state: "open",
      created_at: "2026-10-15T09:00:00Z",
      answer: null,
    },
    {
      id: "a2",
      kind: "question",
      question: "Which cache?",
      options: [{ label: "Redis", description: "A cache server." }, { label: "None" }],
      multiple: false,
      state: "answered",
      created_at: "2026-10-16T16:00:00Z",
      answer: {
        selected: ["Redis"],
        text: "Use Redis.",
        user: "alice@example.com",
        at: "2026-10-16T17:00:00Z",
      },
    },
  ]);
  expect(read(path.join(issue, "old-spec.md"))).toBe("## Summary\n\nThe widget is slow.\n");

  const document = path.join(out, "acme-brief-md");
  expect(readdirSync(document).toSorted()).toEqual(["asks.json", "messages.md", "title.txt"]);
  expect(existsSync(path.join(document, "old-spec.md"))).toBe(false);
  expect(read(path.join(document, "title.txt"))).toBe("brief.md\n");
  expect(read(path.join(document, "messages.md"))).toBe(
    "<!-- message 2026-10-21T09:30:00Z -->\n# Brief\n\nMake specs short.\n"
  );
  expect(JSON.parse(read(path.join(document, "asks.json")))).toEqual([]);

  expect(run.stdout).toEqual([
    `${issue}: title.txt, messages.md (5 entries), asks.json (2 asks), old-spec.md`,
    `${document}: title.txt, messages.md (1 entries), asks.json (0 asks)`,
  ]);
});

test("--agent-texts writes every message, comment and ask by a session in the window", async () => {
  const out = path.join(dir("agent-texts-run"), "texts");
  const { fetch, sent } = world();
  const run = io(fetch);
  const code = await main(
    [
      "--agent-texts",
      "--since",
      "2026-10-16T00:00:00Z",
      "--until",
      "2026-10-16T16:00:00Z",
      "--out",
      out,
    ],
    run.io
  );
  expect(run.stderr).toEqual([]);
  expect(code).toBe(0);
  const lists = sent.filter(({ url }) => url.pathname === "/api/v1/issues");
  expect(lists).toHaveLength(1);
  expect(lists[0]?.url.search).toBe("");
  // The ask at exactly --until and the ask before --since are left out, and so is every text by a
  // person.
  expect(JSON.parse(read(path.join(out, "agent-texts.json")))).toEqual([
    {
      issue: "ACME-7",
      id: "m2",
      kind: "message",
      body: "On it.",
      origin_session_title: "widget work",
      created_at: "2026-10-16T12:00:00Z",
    },
    {
      issue: "ACME-7",
      id: "c2",
      kind: "comment",
      body: "Fixed.",
      origin_session_title: null,
      created_at: "2026-10-16T13:00:00Z",
    },
  ]);
  expect(run.stdout).toEqual([
    `${path.join(out, "agent-texts.json")}: 2 texts by sessions (1 messages, 1 comments, 0 asks) read from 2 issues`,
  ]);

  const wider = io(world().fetch);
  expect(
    await main(
      [
        "--agent-texts",
        "--since",
        "2026-10-15T00:00:00Z",
        "--until",
        "2026-10-17T00:00:00Z",
        "--out",
        out,
      ],
      wider.io
    )
  ).toBe(0);
  const asks = JSON.parse(read(path.join(out, "agent-texts.json"))).filter(
    (row: { kind: string }) => row.kind === "ask"
  );
  expect(asks).toEqual([
    {
      issue: "ACME-7",
      id: "a1",
      kind: "ask",
      body: "Approve the spec?\nApprove\nRequest changes",
      origin_session_title: null,
      created_at: "2026-10-15T09:00:00Z",
    },
    {
      issue: "ACME-7",
      id: "a2",
      kind: "ask",
      body: "Which cache?\nRedis\nA cache server.\nNone",
      origin_session_title: "widget work",
      created_at: "2026-10-16T16:00:00Z",
    },
  ]);
});

test("an --out inside a checkout, or a reference of another shape, exits 2 and reads nothing", async () => {
  const checkout = dir("checkout");
  mkdirSync(path.join(checkout, ".git"));
  const { fetch, sent } = world();
  const inside = io(fetch);
  const out = path.join(checkout, "sources");
  expect(await main(["--out", out, "ACME-7"], inside.io)).toBe(2);
  expect(inside.stderr).toEqual([
    `fetch-spec-sources: --out ${out} is inside the checkout ${checkout}. Production text must stay out of a checkout, where git or jj can record it. Name a directory outside every checkout.`,
  ]);
  const bad = io(fetch);
  expect(await main(["--out", path.join(dir("bad"), "s"), "acme-7"], bad.io)).toBe(2);
  expect(bad.stderr).toEqual([
    "fetch-spec-sources: acme-7 is neither an issue key (ACME-1) nor a project document (dispatch://ACME/artifact/<slug>)",
  ]);
  expect(sent).toEqual([]);
});
