import { afterAll, afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// e2e/api.ts, which seed.ts calls, reads its port when it loads: set it as rig.sh's seed_ts does,
// then load seed.ts. PLAYWRIGHT_BASE_URL would point api.ts somewhere else.
process.env.DISPATCH_E2E_PORT = "18777";
delete process.env.PLAYWRIGHT_BASE_URL;
const seed = await import("./seed");

const origin = "http://127.0.0.1:18777";
const work = mkdtempSync(path.join(tmpdir(), "skill-scenarios-seed-"));
afterAll(() => rmSync(work, { recursive: true, force: true }));
let dirs = 0;
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

type Actor =
  | { kind: "user"; id: string }
  | { kind: "session"; id: string; origin?: { session_title?: string } };
interface FakeAsk {
  id: string;
  issue_key: string;
  author: Actor;
  kind: "question" | "approval";
  question: string;
  options: { label: string; description?: string }[];
  multiple: boolean;
  state: "open" | "answered" | "resolved";
  answer: { user: string; selected: string[]; text: string | null; at: string } | null;
  edited_at: string | null;
  created_at: string;
  approval?: { artifact_id: string; name: string; version: number; requested_version: number };
}
interface FakeArtifact {
  id: string;
  issue_key: string;
  name: string;
  slug: string;
  kind: "doc" | "image" | "file";
  primary: boolean;
  markdown: string;
  versions: { number: number; created_at: string; authors: Actor[]; markdown: string }[];
}
interface Call {
  method: string;
  path: string;
  /** `alice` (or another login) for a cookie call, `agent` for a bearer call. */
  who: string;
  body: Record<string, unknown> | undefined;
}

/** The scratch Dispatch seed.ts talks to, held in memory: just the routes seed.ts calls. */
class FakeDispatch {
  calls: Call[] = [];
  issues = new Map<string, { key: string; title: string; spec_artifact: string }>();
  asks: FakeAsk[] = [];
  messages: { message: Record<string, unknown>; replies: Record<string, unknown>[] }[] = [];
  comments: Record<string, unknown>[] = [];
  artifacts: FakeArtifact[] = [];
  #ids = 0;
  id(prefix: string): string {
    return `${prefix}-${++this.#ids}`;
  }
  issue(key: string, title = "A title"): string {
    const spec = this.id("art");
    this.issues.set(key, { key, title, spec_artifact: spec });
    this.artifacts.push({
      id: spec,
      issue_key: key,
      name: "spec.md",
      slug: "spec",
      kind: "doc",
      primary: true,
      markdown: "",
      versions: [],
    });
    return spec;
  }

  readonly fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    expect(url.origin).toBe(origin);
    const method = init?.method ?? "GET";
    if (url.pathname === "/auth/_dev/signin") {
      const login = url.searchParams.get("login") ?? "";
      return new Response(null, {
        status: 302,
        headers: { "Set-Cookie": `dsession=${login}; Path=/; HttpOnly` },
      });
    }
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const who =
      headers.Authorization === "Bearer e2e-token"
        ? "agent"
        : (/dsession=([^;]+)/.exec(headers.Cookie ?? "")?.[1] ?? "nobody");
    const body = init?.body === undefined ? undefined : JSON.parse(String(init.body));
    this.calls.push({ method, path: url.pathname, who, body });
    const route = `${method} ${url.pathname}`;
    const json = (value: unknown, status = 200) => Response.json(value, { status });
    const actor = (): Actor =>
      who === "agent" ? (body?.actor as Actor) : { kind: "user", id: `${who}@example.invalid` };
    const strip = <T extends { markdown: string }>({ markdown: _markdown, ...rest }: T) => rest;
    // Each route: its method and path pattern, and what it answers for the path's captures.
    const routes: [RegExp, (id: string, second: string) => Response][] = [
      [
        /^POST \/api\/v1\/issues$/,
        () => {
          const key = `LWEVAL-${this.issues.size + 2}`;
          const spec = this.issue(key, body?.title);
          const artifact = this.artifacts.find((a) => a.id === spec);
          if (artifact !== undefined && typeof body?.spec === "string")
            artifact.markdown = body.spec;
          return json({ key, primary_artifact_id: spec }, 201);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/messages$/,
        (issue) => {
          const message = {
            id: this.id("msg"),
            issue_key: issue,
            author: actor(),
            body: body?.body,
            created_at: new Date().toISOString(),
          };
          this.messages.push({ message, replies: [] });
          return json(message, 201);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/asks$/,
        (issue) => {
          const ask: FakeAsk = {
            id: this.id("ask"),
            issue_key: issue,
            author: actor(),
            kind: "question",
            question: body?.question,
            options: body?.options ?? [],
            multiple: body?.multiple ?? false,
            state: "open",
            answer: null,
            edited_at: null,
            created_at: new Date().toISOString(),
          };
          this.asks.push(ask);
          return json(ask, 201);
        },
      ],
      [
        /^POST \/api\/v1\/asks\/([^/]+)\/answer$/,
        (id) => {
          const ask = this.asks.find((a) => a.id === id);
          if (ask === undefined) return json({ error: "not found" }, 404);
          if (body?.expected_edited_at !== ask.edited_at) return json({ code: "ASK_EDITED" }, 409);
          ask.state = "answered";
          ask.answer = {
            user: `${who}@example.invalid`,
            selected: body?.selected,
            text: body?.text ?? null,
            at: new Date().toISOString(),
          };
          return json(ask);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/asks$/,
        (issue) => json(this.asks.filter((a) => a.issue_key === issue)),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/messages$/,
        (issue) => json(this.messages.filter((m) => m.message.issue_key === issue)),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/comments$/,
        (issue) => json(this.comments.filter((c) => c.issue_key === issue)),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)$/,
        (issue) =>
          json({
            key: issue,
            artifacts: this.artifacts.filter((a) => a.issue_key === issue).map(strip),
          }),
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)\/text$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json({ markdown: artifact?.markdown, version: artifact?.versions.length ?? null });
        },
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)\/versions\/(\d+)$/,
        (id, number) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json(artifact?.versions.find((v) => v.number === Number(number)));
        },
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json({ ...artifact, versions: artifact?.versions.map(strip) });
        },
      ],
    ];
    for (const [pattern, answer] of routes) {
      const match = pattern.exec(route);
      if (match !== null) return answer(match[1] ?? "", match[2] ?? "");
    }
    return json({ error: `the fake serves no ${route}` }, 500);
  };
}

let dispatch: FakeDispatch;
let fetchSpy: ReturnType<typeof spyOn<typeof globalThis, "fetch">>;
let logSpy: ReturnType<typeof spyOn<typeof console, "log">>;
beforeEach(() => {
  dispatch = new FakeDispatch();
  fetchSpy = spyOn(globalThis, "fetch").mockImplementation(dispatch.fetch as typeof fetch);
  logSpy = spyOn(console, "log").mockImplementation(() => {});
});
afterEach(() => {
  fetchSpy.mockRestore();
  logSpy.mockRestore();
});

const runSession = { kind: "session", id: "ses-run", origin: { session_title: "Run" } } as const;
/** A run directory whose transcript header names the run's session, `ses-run`. */
function runDir(): string {
  const dir = freshDir("run");
  mkdirSync(path.join(dir, "sessions"));
  writeFileSync(
    path.join(dir, "sessions", "2026-10-24_ses-run.jsonl"),
    `${JSON.stringify({ type: "session", id: "ses-run", cwd: "/run/cwd" })}\n`
  );
  return dir;
}
/** A fixture file for issue LWEVAL-2, with `seeded` written beside it as seeded.json. */
function fixture(dir: string, spec: string, seeded: string[] = []): string {
  const file = path.join(dir, "fixture.json");
  writeFileSync(file, `${JSON.stringify({ issue: "LWEVAL-2", spec_artifact: spec })}\n`);
  writeFileSync(path.join(dir, "seeded.json"), `${JSON.stringify(seeded)}\n`);
  return file;
}
function ask(fields: Partial<FakeAsk> & Pick<FakeAsk, "id">): FakeAsk {
  return {
    issue_key: "LWEVAL-2",
    author: runSession,
    kind: "question",
    question: `Question of ${fields.id}?`,
    options: [{ label: "Yes", description: "Do it." }, { label: "No" }],
    multiple: false,
    state: "open",
    answer: null,
    edited_at: null,
    created_at: "2026-10-24T10:00:00Z",
    ...fields,
  };
}

/** A source directory as fetch-spec-sources.ts writes it: two messages, an anchored comment and an
 * unanchored comment whose body starts with a block quote, all by people, and three asks of which
 * only the answered question is input. */
function source(oldSpec?: string): string {
  const dir = freshDir("source");
  writeFileSync(path.join(dir, "title.txt"), "Make the widget faster\n");
  writeFileSync(
    path.join(dir, "messages.md"),
    [
      "<!-- message 2026-10-16T17:53:32.037069Z -->",
      "Please make the widget faster.",
      "",
      "<!-- comment 2026-10-17T09:00:00Z anchored -->",
      "> The widget waits 5 s.",
      "",
      "Why 5 s?",
      "",
      "<!-- message 2026-10-18T12:00:00Z -->",
      "One second is enough.",
      "",
      "<!-- comment 2026-10-18T13:00:00Z -->",
      "> The ticket says 5 s.",
      "",
      "It is wrong.",
      "",
    ].join("\n")
  );
  writeFileSync(
    path.join(dir, "asks.json"),
    JSON.stringify([
      {
        id: "src-ask-1",
        kind: "question",
        question: "Cache the widget or shorten its wait?",
        options: [{ label: "Cache", description: "Keep the last result." }, { label: "Shorten" }],
        multiple: false,
        state: "answered",
        created_at: "2026-10-16T18:00:00Z",
        answer: {
          selected: ["Shorten"],
          text: "The wait is the cost.",
          user: "person@example.invalid",
          at: "2026-10-17T10:00:00Z",
        },
      },
      {
        id: "src-ask-2",
        kind: "question",
        question: "An ask nobody answered?",
        options: [],
        multiple: false,
        state: "open",
        created_at: "2026-10-16T19:00:00Z",
        answer: null,
      },
      {
        id: "src-ask-3",
        kind: "approval",
        question: "Approve spec.md version 3?",
        options: [{ label: "Approve" }, { label: "Request changes" }],
        multiple: false,
        state: "answered",
        created_at: "2026-10-16T20:00:00Z",
        answer: {
          selected: ["Approve"],
          text: null,
          user: "p@example.invalid",
          at: "2026-10-16T21:00:00Z",
        },
      },
    ])
  );
  if (oldSpec !== undefined) writeFileSync(path.join(dir, "old-spec.md"), oldSpec);
  return dir;
}
const writes = () =>
  dispatch.calls
    .filter((call) => call.method === "POST")
    .map((call) => `${call.who} ${call.path.replace(/[^/]+-\d+/g, "<id>")}`);

describe("spec-writing", () => {
  // Catches: a seeding that posts the source out of time order, posts a person's words as the
  // agent, opens a seeded ask as the run's session, or seeds the open and the approval asks.
  test("posts the source's input in time order, the messages and answers as alice", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.specWriting(file, source());
    expect(writes()).toEqual([
      "alice /api/v1/issues",
      "alice /api/v1/issues/<id>/messages",
      "agent /api/v1/issues/<id>/asks",
      "alice /api/v1/issues/<id>/messages",
      "alice /api/v1/asks/<id>/answer",
      "alice /api/v1/issues/<id>/messages",
      "alice /api/v1/issues/<id>/messages",
    ]);
    const posts = dispatch.calls.filter((call) => call.method === "POST");
    expect(posts[0]?.body).toEqual({
      project: "LWEVAL",
      force: true,
      title: "Make the widget faster",
    });
    expect(posts.map((call) => call.body?.body).filter((body) => body !== undefined)).toEqual([
      "Please make the widget faster.",
      "> The widget waits 5 s.\n\nWhy 5 s?",
      "One second is enough.",
      "> The ticket says 5 s.\n\nIt is wrong.",
    ]);
    expect(posts[2]?.body).toEqual({
      question: "Cache the widget or shorten its wait?",
      options: [{ label: "Cache", description: "Keep the last result." }, { label: "Shorten" }],
      multiple: false,
      actor: { kind: "session", id: seed.SEED_ASKER.id },
    });
    expect(seed.SEED_ASKER.id).not.toBe(runSession.id);
    expect(posts[4]?.body).toEqual({
      selected: ["Shorten"],
      text: "The wait is the cost.",
      expected_edited_at: null,
    });
  });

  // Catches: a seeding that leaves seeded.json out, or writes it somewhere a run's readers miss.
  test("writes the fixture and, beside it, seeded.json with each seeded ask's id", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.specWriting(file, source());
    const issue = [...dispatch.issues.values()][0];
    expect(JSON.parse(readFileSync(file, "utf8"))).toEqual({
      issue: issue?.key,
      spec_artifact: issue?.spec_artifact,
    });
    expect(JSON.parse(readFileSync(path.join(dir, "seeded.json"), "utf8"))).toEqual(
      dispatch.asks.map((a) => a.id)
    );
    expect(dispatch.asks).toHaveLength(1);
  });
});

describe("spec-rewrite", () => {
  // Catches: a rewrite that starts from an empty spec instead of the source's old one.
  test("creates the issue with the source's old-spec.md as its spec", async () => {
    const dir = freshDir("seed");
    await seed.specRewrite(path.join(dir, "fixture.json"), source("## Summary\n\nThe old spec.\n"));
    const created = dispatch.calls.find((call) => call.path === "/api/v1/issues");
    expect(created?.body?.spec).toBe("## Summary\n\nThe old spec.\n");
  });

  // Catches: a rewrite of a source with no old spec, which would be a spec-writing run mislabelled.
  test("refuses a source with no old-spec.md, naming it, and writes nothing", async () => {
    const dir = freshDir("seed");
    const src = source();
    await expect(seed.specRewrite(path.join(dir, "fixture.json"), src)).rejects.toThrow(
      path.join(src, "old-spec.md")
    );
    expect(writes()).toEqual([]);
  });
});

describe("state", () => {
  function world(spec: string) {
    const plan = dispatch.id("art");
    dispatch.asks.push(
      ask({ id: "seeded-open" }),
      ask({ id: "agent-open" }),
      ask({ id: "agent-answered", state: "answered" }),
      ask({
        id: "approval-spec",
        kind: "approval",
        options: [{ label: "Approve" }, { label: "Request changes" }],
        approval: { artifact_id: spec, name: "spec.md", version: 4, requested_version: 4 },
      }),
      ask({
        id: "approval-plan",
        kind: "approval",
        approval: { artifact_id: plan, name: "plan.md", version: 1, requested_version: 1 },
      })
    );
  }

  // Catches: a state that lists a seeded ask, an answered ask or an approval request as an open
  // question, misses the spec's approval request, or takes another document's for it.
  test("writes and prints each open question and the spec's open approval request", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    world(spec);
    const dir = runDir();
    const result = await seed.state(fixture(dir, spec, ["seeded-open"]), 3, dir);
    const expected = {
      turn: 3,
      open_asks: [
        {
          id: "agent-open",
          question: "Question of agent-open?",
          options: [
            { label: "Yes", description: "Do it." },
            { label: "No", description: "" },
          ],
          multiple: false,
        },
      ],
      approval: { ask_id: "approval-spec", version: 4 },
    };
    expect(result).toEqual(expected);
    expect(JSON.parse(readFileSync(path.join(dir, "state-3.json"), "utf8"))).toEqual(expected);
    expect(logSpy.mock.calls.map((call) => JSON.parse(String(call[0])))).toEqual([expected]);
  });

  // Catches: a state that reports an approval when the spec has none open.
  test("approval is null when no approval request on the spec is open", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "agent-open" }));
    const dir = runDir();
    expect((await seed.state(fixture(dir, spec), 1, dir)).approval).toBeNull();
  });
});

describe("answer", () => {
  // Catches: an answer posted as the agent, without the ask's current edited_at, or without the
  // stand-in's text.
  test("answers each line's ask as alice with its selection and text", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "agent-open", edited_at: "2026-10-24T10:05:00Z" }));
    const dir = runDir();
    const answers = path.join(dir, "answers-1.jsonl");
    writeFileSync(
      answers,
      `${JSON.stringify({ ask: "agent-open", selected: ["Yes"], text: "Yes, do it." })}\n`
    );
    await seed.answer(fixture(dir, spec), answers);
    expect(dispatch.calls.filter((call) => call.method === "POST")).toEqual([
      {
        method: "POST",
        path: "/api/v1/asks/agent-open/answer",
        who: "alice",
        body: {
          selected: ["Yes"],
          text: "Yes, do it.",
          expected_edited_at: "2026-10-24T10:05:00Z",
        },
      },
    ]);
  });

  // Catches: a stand-in answer to a seeded ask, which would change the person's recorded input.
  test("refuses a line that answers a seeded ask, and posts nothing", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "seeded-open" }));
    const dir = runDir();
    const answers = path.join(dir, "answers-1.jsonl");
    writeFileSync(answers, `${JSON.stringify({ ask: "seeded-open", selected: [], text: "x" })}\n`);
    await expect(seed.answer(fixture(dir, spec, ["seeded-open"]), answers)).rejects.toThrow(
      "seeded-open"
    );
    expect(writes()).toEqual([]);
  });
});

describe("approve", () => {
  // Catches: an approval sent as the agent, with another label than Dispatch's Approve, or to
  // another document's request.
  test("answers the spec's open approval request as alice with Approve", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(
      ask({
        id: "approval-plan",
        kind: "approval",
        approval: { artifact_id: "other", name: "plan.md", version: 1, requested_version: 1 },
      }),
      ask({
        id: "approval-spec",
        kind: "approval",
        edited_at: "2026-10-24T11:00:00Z",
        approval: { artifact_id: spec, name: "spec.md", version: 2, requested_version: 2 },
      })
    );
    const dir = runDir();
    await seed.approve(fixture(dir, spec));
    expect(dispatch.calls.filter((call) => call.method === "POST")).toEqual([
      {
        method: "POST",
        path: "/api/v1/asks/approval-spec/answer",
        who: "alice",
        body: { selected: ["Approve"], expected_edited_at: "2026-10-24T11:00:00Z" },
      },
    ]);
  });

  // Catches: an approve that succeeds with nothing to approve.
  test("fails when the spec has no open approval request", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const dir = runDir();
    await expect(seed.approve(fixture(dir, spec))).rejects.toThrow("no open approval request");
  });
});

describe("capture-spec", () => {
  function world() {
    const spec = dispatch.issue("LWEVAL-2");
    const artifact = dispatch.artifacts.find((a) => a.id === spec);
    if (artifact === undefined) throw new Error("no spec");
    artifact.markdown = "## Summary\n\nVersion two.";
    artifact.versions = [
      { number: 1, created_at: "2026-10-24T10:00:00Z", authors: [runSession], markdown: "v1" },
      {
        number: 2,
        created_at: "2026-10-24T10:30:00Z",
        authors: [{ kind: "user", id: "alice@example.invalid" }, runSession],
        markdown: "## Summary\n\nVersion two.",
      },
    ];
    for (const [name, slug, kind, markdown] of [
      ["plan.md", "plan-md", "doc", "## Plan"],
      ["notes.md", "notes-md", "doc", "## Notes"],
      ["shot.png", "shot-png", "image", ""],
    ] as const) {
      dispatch.artifacts.push({
        id: dispatch.id("art"),
        issue_key: "LWEVAL-2",
        name,
        slug,
        kind,
        primary: false,
        markdown,
        versions: [],
      });
    }
    const alice = { kind: "user", id: "alice@example.invalid" };
    const message = (id: string, author: unknown, body: string, at: string) => ({
      id,
      issue_key: "LWEVAL-2",
      author,
      body,
      created_at: at,
    });
    dispatch.messages.push(
      {
        message: message("m-alice", alice, "Make it faster.", "2026-10-24T09:00:00Z"),
        replies: [],
      },
      {
        message: message("m-run", runSession, "I wrote a spec.", "2026-10-24T10:40:00Z"),
        replies: [message("m-reply", runSession, "And a plan.", "2026-10-24T10:50:00Z")],
      }
    );
    dispatch.comments.push(
      { ...message("c-run", runSession, "A note on R2.", "2026-10-24T10:45:00Z"), ask_id: null },
      {
        ...message("c-other", { kind: "session", id: "ses-other" }, "Hi.", "2026-10-24T10:46:00Z"),
        ask_id: null,
      }
    );
    dispatch.asks.push(
      // The rig opens a seeded ask with the run's own bearer; its session id is the rig's, but the
      // filter must not rest on that alone.
      ask({
        id: "seeded",
        author: runSession,
        state: "answered",
        created_at: "2026-10-24T08:00:00Z",
      }),
      ask({ id: "agent-ask", created_at: "2026-10-24T10:35:00Z" })
    );
    return spec;
  }

  // Catches: a capture that hands a seeded ask to the readers, misses the run's session's texts,
  // takes another session's, renders an ask body another way than fetch-spec-sources.ts, or files
  // plan.md or a picture among documents/.
  test("writes the run's capture files, the seeded asks left out", async () => {
    const spec = world();
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec, ["seeded"]), dir);
    const read = (name: string) => readFileSync(path.join(dir, name), "utf8");
    expect(read("spec.md")).toBe("## Summary\n\nVersion two.");
    expect(JSON.parse(read("spec-versions.json"))).toEqual([
      { version: 1, created_at: "2026-10-24T10:00:00Z", authors: [runSession], actor: runSession },
      {
        version: 2,
        created_at: "2026-10-24T10:30:00Z",
        authors: [{ kind: "user", id: "alice@example.invalid" }, runSession],
        actor: { kind: "user", id: "alice@example.invalid" },
      },
    ]);
    expect(read("spec-versions/1.md")).toBe("v1");
    expect(read("spec-versions/2.md")).toBe("## Summary\n\nVersion two.");
    expect(JSON.parse(read("agent-asks.json"))).toEqual([
      {
        id: "agent-ask",
        kind: "question",
        question: "Question of agent-ask?",
        options: [{ label: "Yes", description: "Do it." }, { label: "No" }],
        state: "open",
        answer: null,
        created_at: "2026-10-24T10:35:00Z",
      },
    ]);
    expect(JSON.parse(read("agent-texts.json"))).toEqual([
      {
        id: "agent-ask",
        kind: "ask",
        body: "Question of agent-ask?\nYes\nDo it.\nNo",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:35:00Z",
      },
      {
        id: "m-run",
        kind: "message",
        body: "I wrote a spec.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:40:00Z",
      },
      {
        id: "c-run",
        kind: "comment",
        body: "A note on R2.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:45:00Z",
      },
      {
        id: "m-reply",
        kind: "message",
        body: "And a plan.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:50:00Z",
      },
    ]);
    expect(read("plan.md")).toBe("## Plan");
    expect(read("documents/notes-md.md")).toBe("## Notes");
    expect(existsSync(path.join(dir, "documents", "plan-md.md"))).toBe(false);
    expect(existsSync(path.join(dir, "documents", "shot-png.md"))).toBe(false);
    expect(existsSync(path.join(dir, "documents", "spec.md"))).toBe(false);
  });

  // Catches: a plan.md left from an earlier capture when the issue holds none now.
  test("writes no plan.md when the issue holds none", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec), dir);
    expect(existsSync(path.join(dir, "plan.md"))).toBe(false);
    expect(JSON.parse(readFileSync(path.join(dir, "agent-texts.json"), "utf8"))).toEqual([]);
  });
});
