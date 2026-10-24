/**
 * Reads the sources of the spec-writing and spec-rewrite scenarios from production Dispatch into a
 * work directory outside every checkout. Run it with your own Dispatch credentials (envoy.json, or
 * DISPATCH_URL with DISPATCH_TOKEN or DISPATCH_TOKEN_FILE) before a batch: rig.sh removes every
 * DISPATCH_* variable. It sends GET requests only.
 *
 *   bun fetch-spec-sources.ts --out <dir> <reference>...
 *
 * A reference is an issue key (ACME-1) or a project document (dispatch://ACME/artifact/<slug>).
 * An issue goes to <dir>/<key in lower case>/, a project document to
 * <dir>/<project in lower case>-<slug>/. The directory is written again from nothing:
 *   title.txt    the issue's title, or the document's name
 *   messages.md  each message and comment by a person, oldest first. Each entry starts with the
 *                line `<!-- message <created_at> -->` or `<!-- comment <created_at> -->`. An anchored
 *                comment starts with its quote, each line after `> `, then a blank line. A project
 *                document is one message: its text, at the time of its latest version.
 *   asks.json    each ask on the issue, every state, oldest first: id, kind, question, options,
 *                multiple, state, created_at, and answer (selected, text, user, at) or null. A
 *                project document gets [].
 *   old-spec.md  the issue's primary spec. A project document gets none.
 *
 *   bun fetch-spec-sources.ts --agent-texts --since <RFC 3339> --until <RFC 3339> --out <dir>
 *
 * writes <dir>/agent-texts.json: every message, comment and ask by a session (author kind
 * `session`) whose created_at is in the window (--since inclusive, --until exclusive), over the
 * issues that label-comments.ts reads (one unpaged `GET /api/v1/issues`). Each row is
 * {issue, id, kind, body, origin_session_title, created_at}. An ask's body is its question, then
 * for each option its label on one line and its description on the next line when it has one.
 *
 * Stdout gives file names and counts only. Exit 1 is a failed read. Exit 2 is a refusal before any
 * read: a bad argument, an --out inside a git or jj checkout, or no Dispatch.
 */
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Actor, Ask, Comment, Message } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import {
  byCreatedAt,
  type CliIo,
  inWindow,
  mapLimit,
  outRefusal,
  ProductionDispatch,
  parseWindow,
  processIo,
  resolveDispatch,
  type Window,
} from "./production-dispatch";

const USAGE =
  "usage: fetch-spec-sources.ts --out <dir> <issue key | dispatch://<PROJECT>/artifact/<slug>>... | --agent-texts --since <RFC 3339> --until <RFC 3339> --out <dir>";

/** The issue keys and project keys Dispatch accepts (packages/envoy/internal/dispatch/text/refs.go). */
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$/;
const DOCUMENT_REF = /^dispatch:\/\/([A-Z][A-Z0-9]{1,9})\/artifact\/([a-z0-9]+(?:-[a-z0-9]+)*)$/;

/** The header line of one messages.md entry. seed.ts splits the file on it. */
export const ENTRY_HEADER = /^<!-- (message|comment) (\S+) -->$/m;

/** How many issues are read at once. */
const CONCURRENCY = 8;

type Reference =
  | { readonly kind: "issue"; readonly key: string }
  | { readonly kind: "document"; readonly project: string; readonly slug: string };

/** One entry of messages.md. */
interface Entry {
  readonly kind: "message" | "comment";
  readonly created_at: string;
  readonly text: string;
}

/** One ask of asks.json. */
export interface SourceAsk {
  readonly id: string;
  readonly kind: Ask["kind"];
  readonly question: string;
  readonly options: { label: string; description?: string }[];
  readonly multiple: boolean;
  readonly state: Ask["state"];
  readonly created_at: string;
  readonly answer: {
    readonly selected: string[];
    readonly text: string | null;
    readonly user: string;
    readonly at: string;
  } | null;
}

/** One row of agent-texts.json. */
export interface AgentText {
  readonly issue: string;
  readonly id: string;
  readonly kind: "message" | "comment" | "ask";
  readonly body: string;
  readonly origin_session_title: string | null;
  readonly created_at: string;
}

function parseReference(text: string): Reference | undefined {
  if (ISSUE_KEY.test(text)) return { kind: "issue", key: text };
  const document = DOCUMENT_REF.exec(text);
  if (document?.[1] !== undefined && document[2] !== undefined) {
    return { kind: "document", project: document[1], slug: document[2] };
  }
  return undefined;
}

/** A comment's text as messages.md holds it: its quote as `>` lines, then its body, then the
 * replacement text when the comment is a suggestion. */
function commentText(comment: Comment): string {
  const parts: string[] = [];
  const quote = comment.anchor?.quote.trimEnd();
  if (quote !== undefined && quote !== "") {
    parts.push(
      quote
        .split("\n")
        .map((line) => (line === "" ? ">" : `> ${line}`))
        .join("\n")
    );
  }
  const body = comment.body.trimEnd();
  if (body !== "") parts.push(body);
  if (comment.suggestion != null) {
    parts.push(`Suggested replacement:\n${comment.suggestion.replace_with.trimEnd()}`);
  }
  return parts.join("\n\n");
}

/** messages.md from its entries, oldest first. Throws when a line of an entry reads as a header,
 * which would split the file in the wrong place. */
function messagesMarkdown(source: string, entries: readonly Entry[]): string {
  const ordered = entries.toSorted(byCreatedAt);
  for (const entry of ordered) {
    if (ENTRY_HEADER.test(entry.text)) {
      throw new Error(
        `${source}: the ${entry.kind} of ${entry.created_at} holds a line that reads as an entry header of messages.md`
      );
    }
  }
  if (ordered.length === 0) return "";
  return `${ordered.map((entry) => `<!-- ${entry.kind} ${entry.created_at} -->\n${entry.text}`).join("\n\n")}\n`;
}

function sourceAsk(ask: Ask): SourceAsk {
  return {
    id: ask.id,
    kind: ask.kind,
    question: ask.question,
    options: ask.options.map((option) =>
      option.description === undefined || option.description === ""
        ? { label: option.label }
        : { label: option.label, description: option.description }
    ),
    multiple: ask.multiple,
    state: ask.state,
    created_at: ask.created_at,
    answer:
      ask.answer === null
        ? null
        : {
            selected: ask.answer.selected,
            text: ask.answer.text,
            user: ask.answer.user,
            at: ask.answer.at,
          },
  };
}

/** Every message of the issue: each root and each reply. */
async function issueMessages(dispatch: ProductionDispatch, key: string): Promise<Message[]> {
  return (await dispatch.messages(key)).flatMap(({ message, replies }) => [message, ...replies]);
}

/** The directory `name` under `out`, written again from nothing with `files`. */
function writeSource(out: string, name: string, files: Record<string, string>): string {
  const target = path.join(out, name);
  rmSync(target, { recursive: true, force: true });
  mkdirSync(target, { recursive: true });
  for (const [file, text] of Object.entries(files)) writeFileSync(path.join(target, file), text);
  return target;
}

async function fetchIssue(dispatch: ProductionDispatch, out: string, key: string): Promise<string> {
  const [issue, messages, comments, asks] = await Promise.all([
    dispatch.issue(key),
    issueMessages(dispatch, key),
    dispatch.comments(key),
    dispatch.asks(key),
  ]);
  const spec = await dispatch.artifactText(issue.primary_artifact_id);
  const entries: Entry[] = [
    ...messages
      .filter(({ author }) => author.kind === "user")
      .map((message) => ({
        kind: "message" as const,
        created_at: message.created_at,
        text: message.body.trimEnd(),
      })),
    ...comments
      .filter(({ author }) => author.kind === "user")
      .map((comment) => ({
        kind: "comment" as const,
        created_at: comment.created_at,
        text: commentText(comment),
      })),
  ];
  const sourceAsks = asks.toSorted(byCreatedAt).map(sourceAsk);
  const target = writeSource(out, key.toLowerCase(), {
    "title.txt": `${issue.title}\n`,
    "messages.md": messagesMarkdown(key, entries),
    "asks.json": `${JSON.stringify(sourceAsks, null, 2)}\n`,
    "old-spec.md": spec.markdown,
  });
  return `${target}: title.txt, messages.md (${entries.length} entries), asks.json (${sourceAsks.length} asks), old-spec.md`;
}

async function fetchDocument(
  dispatch: ProductionDispatch,
  out: string,
  project: string,
  slug: string
): Promise<string> {
  const [artifact, text] = await Promise.all([
    dispatch.projectArtifact(project, slug),
    dispatch.projectArtifactText(project, slug),
  ]);
  const latest = artifact.versions.toSorted((a, b) => b.number - a.number)[0];
  const at = latest?.created_at ?? artifact.created_at;
  const source = `dispatch://${project}/artifact/${slug}`;
  const target = writeSource(out, `${project.toLowerCase()}-${slug}`, {
    "title.txt": `${artifact.name}\n`,
    "messages.md": messagesMarkdown(source, [
      { kind: "message", created_at: at, text: text.markdown.trimEnd() },
    ]),
    "asks.json": "[]\n",
  });
  return `${target}: title.txt, messages.md (1 entries), asks.json (0 asks)`;
}

/** An ask as one text: its question, then each option's label and description, one per line. */
function askBody(ask: Ask): string {
  return [
    ask.question,
    ...ask.options.flatMap((option) =>
      option.description === undefined || option.description === ""
        ? [option.label]
        : [option.label, option.description]
    ),
  ].join("\n");
}

function sessionTitle(author: Actor): string | null {
  return author.kind === "session" ? (author.origin?.session_title ?? null) : null;
}

async function agentTexts(
  dispatch: ProductionDispatch,
  window: Window
): Promise<{ texts: AgentText[]; issues: number }> {
  const issues = await dispatch.issues();
  const perIssue = await mapLimit(issues, CONCURRENCY, async ({ key }) => {
    const [messages, comments, asks] = await Promise.all([
      issueMessages(dispatch, key),
      dispatch.comments(key),
      dispatch.asks(key),
    ]);
    const texts: AgentText[] = [
      ...messages.map((message) => ({
        kind: "message" as const,
        author: message.author,
        row: message,
        body: message.body,
      })),
      ...comments.map((comment) => ({
        kind: "comment" as const,
        author: comment.author,
        row: comment,
        body: comment.body,
      })),
      ...asks.map((ask) => ({
        kind: "ask" as const,
        author: ask.author,
        row: ask,
        body: askBody(ask),
      })),
    ]
      .filter(({ author, row }) => author.kind === "session" && inWindow(window, row.created_at))
      .map(({ kind, author, row, body }) => ({
        issue: key,
        id: row.id,
        kind,
        body,
        origin_session_title: sessionTitle(author),
        created_at: row.created_at,
      }));
    return texts;
  });
  return { texts: perIssue.flat().toSorted(byCreatedAt), issues: issues.length };
}

interface SourcesArguments {
  readonly mode: "sources";
  readonly out: string;
  readonly references: Reference[];
}

interface AgentTextsArguments {
  readonly mode: "agent-texts";
  readonly out: string;
  readonly window: Window;
}

/** The arguments, or the refusal that names the bad one. */
function parseArguments(argv: readonly string[]): SourcesArguments | AgentTextsArguments | string {
  let out: string | undefined;
  let since: string | undefined;
  let until: string | undefined;
  let agent = false;
  const references: Reference[] = [];
  for (let index = 0; index < argv.length; index++) {
    const word = argv[index] ?? "";
    if (word === "--agent-texts") {
      agent = true;
      continue;
    }
    if (word === "--out" || word === "--since" || word === "--until") {
      const value = argv[++index];
      if (value === undefined || value === "") return `${word} needs a value. ${USAGE}`;
      if (word === "--out") out = value;
      else if (word === "--since") since = value;
      else until = value;
      continue;
    }
    if (word.startsWith("--")) return `unknown argument ${word}. ${USAGE}`;
    const reference = parseReference(word);
    if (reference === undefined) {
      return `${word} is neither an issue key (ACME-1) nor a project document (dispatch://ACME/artifact/<slug>)`;
    }
    references.push(reference);
  }
  if (out === undefined) return `--out is required. ${USAGE}`;
  if (agent) {
    if (references.length > 0) return `--agent-texts takes no reference. ${USAGE}`;
    if (since === undefined || until === undefined) {
      return `--agent-texts needs --since and --until. ${USAGE}`;
    }
    const window = parseWindow(since, until);
    if (typeof window === "string") return window;
    return { mode: "agent-texts", out, window };
  }
  if (since !== undefined || until !== undefined) {
    return `--since and --until go only with --agent-texts. ${USAGE}`;
  }
  if (references.length === 0) return `name at least one reference. ${USAGE}`;
  return { mode: "sources", out, references };
}

/** The whole run. Returns the exit status: 0, 1 (a read failed) or 2 (refused). */
export async function main(argv: readonly string[], io: CliIo): Promise<number> {
  const refuse = (why: string) => {
    io.err(`fetch-spec-sources: ${why}`);
    return 2;
  };
  const args = parseArguments(argv);
  if (typeof args === "string") return refuse(args);
  const outside = outRefusal(args.out);
  if (outside !== undefined) return refuse(outside);
  let dispatch: ProductionDispatch;
  try {
    dispatch = new ProductionDispatch(resolveDispatch(io.env, io.cwd), io.fetch, io.sleep);
  } catch (error) {
    return refuse(messageFor(error));
  }
  const out = path.resolve(args.out);
  try {
    if (args.mode === "agent-texts") {
      const { texts, issues } = await agentTexts(dispatch, args.window);
      mkdirSync(out, { recursive: true });
      const file = path.join(out, "agent-texts.json");
      writeFileSync(file, `${JSON.stringify(texts, null, 2)}\n`);
      const count = (kind: AgentText["kind"]) => texts.filter((text) => text.kind === kind).length;
      io.out(
        `${file}: ${texts.length} texts by sessions (${count("message")} messages, ${count("comment")} comments, ${count("ask")} asks) read from ${issues} issues`
      );
      return 0;
    }
    mkdirSync(out, { recursive: true });
    for (const reference of args.references) {
      io.out(
        reference.kind === "issue"
          ? await fetchIssue(dispatch, out, reference.key)
          : await fetchDocument(dispatch, out, reference.project, reference.slug)
      );
    }
    return 0;
  } catch (error) {
    io.err(`fetch-spec-sources: ${messageFor(error)}`);
    return 1;
  }
}

if (import.meta.main) {
  process.exit(await main(Bun.argv.slice(2), processIo()));
}
