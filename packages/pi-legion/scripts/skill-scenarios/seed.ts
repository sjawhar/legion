// Seeds rig.sh's scratch Dispatch through its API, and reads back what a run left there.
// DISPATCH_E2E_PORT names the server, which e2e/api.ts addresses. e2e/api.ts addresses
// PLAYWRIGHT_BASE_URL instead, with E2E_AGENT_TOKEN as its bearer, whenever it is set, so seed.ts
// refuses to run while either is set. Each mode is also an exported function, which seed.test.ts
// calls against a stubbed fetch.
//
//   seed.ts project                        project LWEVAL, which each run's own issue is filed in
//                                          (brainstorm's aside); once per batch
//   seed.ts tester-proof <file>            one run's own issue, the greet() issue the tester's world
//                                          implements; writes {issue} to <file>
//   seed.ts implement-handoff <file> <body file>
//                                          records <body file>, one JSON object, as the implement
//                                          handoff of the issue <file> names, under the session
//                                          IMPLEMENTER (never the run's); writes {issue, implement:
//                                          {id, version}} to <file>
//   seed.ts capture-handoffs <file> <run dir>
//                                          every handoff record of the issue <file> names, newest
//                                          first as Dispatch lists them, each with the body it
//                                          stores, as score.ts's testerProof reads them
//                                          (handoffs.json in <run dir>)
//   seed.ts ask-on-message <file>          one run's own issue and the owner's message posting its
//                                          plan; writes {issue, message} to <file>
//   seed.ts measure-before-ask <file>      one run's own issue and the owner's message naming three
//                                          ways to finish, one of which the export rig.sh writes
//                                          beside the agent rules out; writes {issue, message}
//   seed.ts capture <file> <run dir>       what the run left on the issue <file> names: the message,
//                                          every ask and the whole event log, as score.ts reads
//                                          them (message.json, asks.json, events.json in <run dir>)
//   seed.ts brainstorm                     project TODO, the to-do CLI rig.sh's run_brainstorm
//                                          writes into each agent's working directory, with two
//                                          unrelated issues; once per batch, as `project` is
//   seed.ts capture-brainstorm <file> <run dir>
//                                          the issue <run dir>'s own transcript created (Dispatch's
//                                          `Created <KEY>:` answer in a tool result; null when it
//                                          created none) and that issue's asks, as score.ts's
//                                          brainstormSurface reads them (brainstorm.json in
//                                          <run dir>); <file> is unread, kept for dispatch_session's
//                                          one capture call shape
//   seed.ts spec-writing <file> <source dir>
//                                          one run's own issue in LWEVAL, filed by the person alice
//                                          with the source's title.txt and an empty spec. Then the
//                                          source's input in time order: each entry of messages.md
//                                          as a message by alice, and each answered question of
//                                          asks.json as an ask that the agent bearer opens (as the
//                                          session SEED_ASKER, never the run's) and alice answers
//                                          with its selection and text. Writes {issue,
//                                          spec_artifact} to <file> and the seeded asks' ids, as a
//                                          JSON array, to seeded.json beside <file>
//   seed.ts spec-rewrite <file> <source dir>
//                                          spec-writing on an issue whose spec is the source's
//                                          old-spec.md; refuses a source without one, naming it.
//                                          Dispatch opens each ask block of the old spec as an open
//                                          ask, whatever state it held; alice answers each, in time
//                                          order, with the source's answer to that question
//                                          (asks.json, matched by question text), that source ask is
//                                          not opened again, and its id goes to seeded.json. A block
//                                          the source holds no answer to fails the seeding, naming
//                                          the block and the source
//   seed.ts state <file> <turn> <run dir>  the open questions on the issue, no seeded ask among
//                                          them, and the spec's open approval request; writes them
//                                          to <run dir>/state-<turn>.json and prints the same JSON
//   seed.ts answer <file> <answers file>   answers, as alice, the ask of each JSONL line
//                                          {ask, selected, text} of <answers file>; refuses a line
//                                          that names a seeded ask
//   seed.ts approve <file>                 answers, as alice, the spec's open approval request with
//                                          Approve, which is the review
//   seed.ts capture-spec <file> <run dir>  what the run wrote on the issue, the seeded asks left
//                                          out: spec.md, spec-versions.json and spec-versions/<n>.md,
//                                          agent-asks.json, agent-texts.json, documents/<slug>.md and
//                                          plan.md (the run directory table of judge.ts and score.ts)
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  type Actor,
  type ArtifactBlock,
  ASK_QUESTION_MAX,
  type Ask,
  type AskOption,
  type Handoff,
  type HandoffSummary,
} from "@legion/contracts";
import { z } from "zod";
import {
  answerAsk,
  baseUrl,
  createAsk,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  e2eAgentToken,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssue,
  getIssueEvents,
  getMessage,
  listComments,
  listIssueAsks,
  listIssueMessages,
} from "../../../dispatch/e2e/api";
import { ENTRY_HEADER } from "./fetch-spec-sources";
import { parseInstant } from "./production-dispatch";
import { CREATED, session, sessionId, toolResults } from "./transcript";

/** What `ask-on-message` writes and `capture` reads. */
const Fixture = z.object({ issue: z.string(), message: z.string() });
/** The most events one `GET /api/v1/issues/{key}/events` page holds. */
const EVENT_PAGE = 200;

/** What `spec-writing` and `spec-rewrite` write, and every later spec mode reads. */
const SpecFixture = z.object({ issue: z.string(), spec_artifact: z.string() });
type SpecFixture = z.infer<typeof SpecFixture>;
/** The session a seeded ask is opened as: another agent's, never the run's own. */
export const SEED_ASKER = { kind: "session", id: "spec-source-agent" } as const;
/** What `tester-proof` writes, and `implement-handoff` and `capture-handoffs` read: the run's issue,
 * and once `implement-handoff` recorded it, the implementer's record. */
const TesterFixture = z.object({
  issue: z.string(),
  implement: z.object({ id: z.string(), version: z.number().int() }).optional(),
});
/** The session the tester's predecessor wrote its implement handoff as: the implementer's, never
 * the run's own. */
export const IMPLEMENTER = { kind: "session", id: "rig-implementer" } as const;
/** The person who files the run's issue, posts the source's input and answers every ask. */
const PERSON = "alice";
/** Dispatch's approval answer (packages/envoy/internal/dispatch/docs/approval_review.go). */
const APPROVE = "Approve";
/** How long spec-rewrite waits for Dispatch's settlement to open the old spec's ask blocks as asks
 * (it settles a seeded document 2 s after the issue is created), and how often it looks. */
const BLOCK_ASKS_WAIT_MS = 30_000;
const BLOCK_ASKS_POLL_MS = 250;

/** Every entry header of a source's messages.md: fetch-spec-sources.ts's ENTRY_HEADER, every match. */
const ENTRY = new RegExp(ENTRY_HEADER.source, "gm");
/** One ask of a source's asks.json, as fetch-spec-sources.ts writes it. */
const SourceAsk = z.object({
  id: z.string(),
  kind: z.enum(["question", "approval"]),
  question: z.string(),
  options: z.array(z.object({ label: z.string(), description: z.string().optional() })),
  multiple: z.boolean(),
  state: z.enum(["open", "answered", "resolved"]),
  created_at: z.string(),
  answer: z
    .object({
      selected: z.array(z.string()),
      text: z.string().nullable(),
      user: z.string(),
      at: z.string(),
    })
    .nullable(),
});
/** One line of an answers file, as judge.ts answer writes it. */
const StandInAnswer = z.object({
  ask: z.string(),
  selected: z.array(z.string()),
  text: z.string(),
});

/** One write of a source's input, at the time the source gives it. */
interface Step {
  at: bigint;
  write: () => Promise<void>;
}

/** The time `value` names, in nanoseconds since the epoch (production-dispatch.ts's
 * parseInstant), so the microseconds Dispatch stores keep their order; a value that is no RFC 3339
 * time is the source's fault. */
function time(value: string, where: string): bigint {
  const at = parseInstant(value);
  if (at === undefined)
    throw new Error(`${where}: ${JSON.stringify(value)} is not an RFC 3339 time`);
  return at;
}

/** Orders two times in nanoseconds for a stable sort. They are compared, not subtracted: a bigint
 * difference is no sort key. */
function byTime(a: bigint, b: bigint): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** A source question as an ask on the run's issue can hold it. The ask route refuses a question
 * over ASK_QUESTION_MAX characters (UTF-16 units, as JavaScript counts them), but an ask block in a
 * spec has no cap, so a source can hold a longer one. Such a question is cut at its last space
 * before the cap and ends with "…"; any other question is posted unchanged. */
export function seededQuestion(question: string): string {
  if (question.length <= ASK_QUESTION_MAX) return question;
  const room = question.slice(0, ASK_QUESTION_MAX - 1);
  const space = room.lastIndexOf(" ");
  return `${space > 0 ? room.slice(0, space + 1) : room}…`;
}

/** The entries of a source's messages.md, oldest first as the file holds them. */
function sourceEntries(file: string): { kind: string; at: bigint; body: string }[] {
  const text = readFileSync(file, "utf8");
  const headers = [...text.matchAll(ENTRY)];
  const before = text.slice(0, headers[0]?.index ?? text.length);
  if (before.trim() !== "") throw new Error(`${file}: text comes before its first entry header`);
  return headers.map((header, index) => {
    const start = (header.index ?? 0) + header[0].length + 1;
    const body = text.slice(start, headers[index + 1]?.index ?? text.length).trimEnd();
    if (body.trim() === "") throw new Error(`${file}: the entry ${header[0]} has no text`);
    return { kind: header[1] ?? "", at: time(header[2] ?? "", file), body };
  });
}

/** The asks Dispatch opened for the ask blocks of the spec `spec` of `issue`, once its settlement
 * opened one for every block: the spec's blocks of type `ask` (`GET …/blocks`, as the agent bearer),
 * then the issue's asks, read again until each such block has its ask. */
async function specBlockAsks(issue: string, spec: string): Promise<Ask[]> {
  const response = await fetch(
    new URL(`/api/v1/artifacts/${encodeURIComponent(spec)}/blocks`, baseUrl),
    {
      headers: { Authorization: `Bearer ${e2eAgentToken}` },
    }
  );
  if (!response.ok)
    throw new Error(
      `GET the blocks of ${spec} failed: ${response.status} ${await response.text()}`
    );
  const blocks = ((await response.json()) as ArtifactBlock[]).filter(
    (block) => block.type === "ask"
  );
  if (blocks.length === 0) return [];
  const deadline = Date.now() + BLOCK_ASKS_WAIT_MS;
  for (;;) {
    const asks = await listIssueAsks(issue, { as: "agent" });
    const opened = blocks.map((block) => asks.find((ask) => ask.block_id === block.id));
    if (opened.every((ask) => ask !== undefined)) return opened as Ask[];
    if (Date.now() > deadline) {
      const missing = blocks.filter((_, index) => opened[index] === undefined);
      throw new Error(
        `${issue}'s spec holds ask blocks ${missing.map((block) => block.id).join(", ")} that Dispatch opened no ask for within ${BLOCK_ASKS_WAIT_MS / 1000} s`
      );
    }
    await Bun.sleep(BLOCK_ASKS_POLL_MS);
  }
}

/** Creates the run's issue as the person, posts the source's input in time order, and writes the
 * fixture and seeded.json. `spec` is the issue's first spec; an empty spec when it is absent. Each
 * ask block of `spec` is an ask Dispatch opens (an upload resets the block to open), answered here
 * as the source answered its question. */
async function seedSpecIssue(file: string, source: string, spec?: string): Promise<void> {
  const title = readFileSync(path.join(source, "title.txt"), "utf8").trim();
  if (title === "") throw new Error(`${path.join(source, "title.txt")} holds no title`);
  const entries = sourceEntries(path.join(source, "messages.md"));
  const asksFile = path.join(source, "asks.json");
  const asks = z.array(SourceAsk).parse(JSON.parse(readFileSync(asksFile, "utf8")));
  const issue = await createIssue(
    { project: "LWEVAL", force: true, title, ...(spec === undefined ? {} : { spec }) },
    { login: PERSON }
  );
  const person = { as: "user", login: PERSON } as const;
  const steps: Step[] = entries.map((entry) => ({
    at: entry.at,
    write: async () => {
      await createMessage(issue.key, { body: entry.body }, person);
    },
  }));
  const seeded: string[] = [];
  const answers: Step[] = [];
  const answeredInSource = asks.filter(
    (ask) => ask.kind === "question" && ask.state === "answered" && ask.answer !== null
  );
  // The source asks an old-spec block holds: each is on the issue as its block's ask, so it is not
  // opened a second time below.
  const inSpec = new Set<string>();
  const blockAsks =
    spec === undefined ? [] : await specBlockAsks(issue.key, issue.primary_artifact_id);
  for (const opened of blockAsks) {
    const source = answeredInSource.find((ask) => ask.question.trim() === opened.question.trim());
    const answer = source?.answer;
    if (source === undefined || answer == null)
      throw new Error(
        `${asksFile} holds no answer to ${JSON.stringify(opened.question)}, the question of the old spec's ask block ${opened.block_id} (ask ${opened.id} on ${issue.key}); spec-rewrite does not make one up`
      );
    inSpec.add(source.id);
    seeded.push(opened.id);
    answers.push({
      at: time(answer.at, asksFile),
      write: async () => {
        await answerAsk(
          opened.id,
          {
            selected: answer.selected,
            ...(answer.text === null ? {} : { text: answer.text }),
            expected_edited_at: opened.edited_at,
          },
          { login: PERSON }
        );
      },
    });
  }
  // Only an answered question is the person's input: an open ask holds none, and an approval is a
  // review of an old document version, which the run's issue does not hold.
  for (const ask of asks) {
    const answer = ask.answer;
    if (ask.kind !== "question" || ask.state !== "answered" || answer === null) continue;
    if (inSpec.has(ask.id)) continue;
    let opened: Ask | undefined;
    if (seededQuestion(ask.question) !== ask.question)
      console.log(
        `seeded ask ${ask.id}: its ${ask.question.length}-character question is cut to Dispatch's ${ASK_QUESTION_MAX}-character cap`
      );
    steps.push({
      at: time(ask.created_at, asksFile),
      write: async () => {
        opened = await createAsk(
          issue.key,
          { question: seededQuestion(ask.question), options: ask.options, multiple: ask.multiple },
          { as: "agent", actor: SEED_ASKER }
        );
        seeded.push(opened.id);
      },
    });
    answers.push({
      at: time(answer.at, asksFile),
      write: async () => {
        if (opened === undefined)
          throw new Error(`the ask ${ask.id} was answered before it opened`);
        await answerAsk(
          opened.id,
          {
            selected: answer.selected,
            ...(answer.text === null ? {} : { text: answer.text }),
            expected_edited_at: opened.edited_at,
          },
          { login: PERSON }
        );
      },
    });
  }
  // A stable sort: at one time, the messages come first, then each ask opens before its answer.
  const ordered = [...steps, ...answers].sort((a, b) => byTime(a.at, b.at));
  for (const step of ordered) await step.write();
  const fixture: SpecFixture = { issue: issue.key, spec_artifact: issue.primary_artifact_id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  writeFileSync(path.join(path.dirname(file), "seeded.json"), `${JSON.stringify(seeded)}\n`);
  console.log(`seeded ${issue.key}: ${entries.length} messages, ${seeded.length} answered asks`);
}

/** `spec-writing <file> <source dir>`: the run's issue with an empty spec. */
export function specWriting(file: string, source: string): Promise<void> {
  return seedSpecIssue(file, source);
}

/** `spec-rewrite <file> <source dir>`: the run's issue with the source's old spec. It refuses a
 * source without old-spec.md before it writes anything. */
export async function specRewrite(file: string, source: string): Promise<void> {
  const oldSpec = path.join(source, "old-spec.md");
  if (!existsSync(oldSpec)) throw new Error(`the source has no ${oldSpec}: spec-rewrite needs it`);
  const spec = readFileSync(oldSpec, "utf8");
  if (spec.trim() === "") throw new Error(`${oldSpec} is empty: spec-rewrite needs an old spec`);
  await seedSpecIssue(file, source, spec);
}

/** The fixture `file` names and the ids in seeded.json beside it. */
function loadSpecFixture(file: string): SpecFixture & { seeded: Set<string> } {
  const fixture = SpecFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const seededFile = path.join(path.dirname(file), "seeded.json");
  const seeded = z.array(z.string()).parse(JSON.parse(readFileSync(seededFile, "utf8")));
  return { ...fixture, seeded: new Set(seeded) };
}

/** The spec's open approval request among `asks`: the open approval ask on the primary spec. */
function openSpecApproval(asks: Ask[], fixture: SpecFixture): Ask | undefined {
  return asks.find(
    (ask) =>
      ask.kind === "approval" &&
      ask.state === "open" &&
      ask.approval?.artifact_id === fixture.spec_artifact
  );
}

/** What `state` writes to state-<turn>.json. */
export interface TurnState {
  turn: number;
  open_asks: { id: string; question: string; options: Required<AskOption>[]; multiple: boolean }[];
  approval: { ask_id: string; version: number } | null;
}

/** `state <file> <turn> <run dir>`: the open questions no seed opened, and the spec's open
 * approval request. */
export async function state(file: string, turn: number, runDir: string): Promise<TurnState> {
  const fixture = loadSpecFixture(file);
  const asks = await listIssueAsks(fixture.issue, { as: "agent" });
  const approval = openSpecApproval(asks, fixture);
  const result: TurnState = {
    turn,
    open_asks: asks
      .filter((ask) => ask.kind === "question" && ask.state === "open")
      .filter((ask) => !fixture.seeded.has(ask.id))
      .map((ask) => ({
        id: ask.id,
        question: ask.question,
        options: ask.options.map((option) => ({
          label: option.label,
          description: option.description ?? "",
        })),
        multiple: ask.multiple,
      })),
    approval:
      approval?.approval === undefined
        ? null
        : { ask_id: approval.id, version: approval.approval.version },
  };
  const text = JSON.stringify(result);
  writeFileSync(path.join(runDir, `state-${turn}.json`), `${text}\n`);
  console.log(text);
  return result;
}

/** `answer <file> <answers file>`: the person answers each line's ask with the stand-in's choice.
 * It checks every line before it posts one. */
export async function answer(file: string, answersFile: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const lines = readFileSync(answersFile, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => StandInAnswer.parse(JSON.parse(line)));
  const seededLine = lines.find((line) => fixture.seeded.has(line.ask));
  if (seededLine !== undefined)
    throw new Error(
      `${answersFile} answers the seeded ask ${seededLine.ask}, which is not the run's`
    );
  const asks = new Map(
    (await listIssueAsks(fixture.issue, { as: "agent" })).map((ask) => [ask.id, ask])
  );
  for (const line of lines) {
    const ask = asks.get(line.ask);
    if (ask === undefined) throw new Error(`${fixture.issue} has no ask ${line.ask}`);
    await answerAsk(
      ask.id,
      { selected: line.selected, text: line.text, expected_edited_at: ask.edited_at },
      { login: PERSON }
    );
  }
  console.log(`answered ${lines.length} asks on ${fixture.issue}`);
}

/** `approve <file>`: the person approves the spec, answering its open approval request. */
export async function approve(file: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const request = openSpecApproval(await listIssueAsks(fixture.issue, { as: "agent" }), fixture);
  if (request === undefined)
    throw new Error(`${fixture.issue} has no open approval request on ${fixture.spec_artifact}`);
  await answerAsk(
    request.id,
    { selected: [APPROVE], expected_edited_at: request.edited_at },
    { login: PERSON }
  );
  console.log(`approved ${fixture.issue}'s spec at version ${request.approval?.version}`);
}

/** One row of agent-texts.json. */
interface AgentText {
  id: string;
  kind: "message" | "comment" | "ask";
  body: string;
  origin_session_title: string | null;
  created_at: string;
}

/** `capture-spec <file> <run dir>`: what the run left on its issue, for judge.ts and score.ts. */
export async function captureSpec(file: string, runDir: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const reader = { as: "agent" } as const;
  const out = (name: string, text: string) => writeFileSync(path.join(runDir, name), text);
  const json = (name: string, value: unknown) => out(name, `${JSON.stringify(value)}\n`);

  out("spec.md", (await getArtifactText(fixture.spec_artifact, reader)).markdown);
  const spec = await getArtifact(fixture.spec_artifact, reader);
  mkdirSync(path.join(runDir, "spec-versions"), { recursive: true });
  for (const version of spec.versions) {
    const text = await getArtifactVersion(spec.id, version.number, reader);
    out(path.join("spec-versions", `${version.number}.md`), text.markdown);
  }
  json(
    "spec-versions.json",
    spec.versions.map((version) => ({
      version: version.number,
      created_at: version.created_at,
      authors: version.authors,
      actor: version.authors[0] ?? null,
    }))
  );

  const asks = (await listIssueAsks(fixture.issue, reader)).filter(
    (ask) => !fixture.seeded.has(ask.id)
  );
  json(
    "agent-asks.json",
    asks.map(({ id, kind, question, options, state, answer, created_at }) => ({
      id,
      kind,
      question,
      options,
      state,
      answer,
      created_at,
    }))
  );

  // The run's session is the one its transcript's header names; a run with no transcript wrote
  // nothing.
  const runSession = sessionId(session(runDir));
  const texts: AgentText[] = [];
  const add = (id: string, kind: AgentText["kind"], author: Actor, body: string, at: string) => {
    if (author.kind !== "session" || author.id !== runSession) return;
    texts.push({
      id,
      kind,
      body,
      origin_session_title: author.origin?.session_title ?? null,
      created_at: at,
    });
  };
  for (const ask of asks) {
    const lines = [ask.question];
    for (const option of ask.options) {
      lines.push(option.label);
      if (option.description) lines.push(option.description);
    }
    add(ask.id, "ask", ask.author, lines.join("\n"), ask.created_at);
  }
  for (const thread of await listIssueMessages(fixture.issue, reader)) {
    for (const message of [thread.message, ...thread.replies]) {
      add(message.id, "message", message.author, message.body, message.created_at);
    }
  }
  for (const comment of await listComments(fixture.issue, undefined, reader)) {
    add(comment.id, "comment", comment.author, comment.body, comment.created_at);
  }
  // Dispatch's times carry microseconds, which Date.parse drops; two texts of one time keep the order
  // the capture read them in.
  const when = (text: AgentText) => time(text.created_at, `${fixture.issue}'s text ${text.id}`);
  json(
    "agent-texts.json",
    texts.sort((a, b) => byTime(when(a), when(b)))
  );

  // Every other markdown document: plan.md on its own, the rest under documents/.
  rmSync(path.join(runDir, "documents"), { recursive: true, force: true });
  rmSync(path.join(runDir, "plan.md"), { force: true });
  mkdirSync(path.join(runDir, "documents"));
  const documents = (await getIssue(fixture.issue, reader)).artifacts.filter(
    (artifact) => artifact.kind === "doc" && artifact.id !== fixture.spec_artifact
  );
  for (const document of documents) {
    const { markdown } = await getArtifactText(document.id, reader);
    out(
      document.name === "plan.md" ? "plan.md" : path.join("documents", `${document.slug}.md`),
      markdown
    );
  }
  console.log(
    `captured ${fixture.issue}: ${spec.versions.length} spec versions, ${asks.length} asks, ${texts.length} texts, ${documents.length} documents`
  );
}

async function seedProject(): Promise<void> {
  await createProject({ key: "LWEVAL", name: "skill scenarios" });
  console.log("seeded LWEVAL");
}

/** `tester-proof <file>`: the run's own greet() issue, whose spec lists the acceptance criteria
 * rig.sh's worker_fixture meets. A batch's runs share one Dispatch, and each run's implement and
 * test records go on its own issue alone, so every run files the same issue and `force` passes
 * Dispatch's duplicate check. */
export async function testerProofIssue(file: string): Promise<void> {
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "greet(): a greeting helper for the widgets CLI",
      spec: [
        "## Goal",
        "",
        "`greet(name)` returns `Hello, <name>!`, and `bun greet.ts <name>` prints it.",
        "",
        "## Acceptance",
        "",
        "1. `bun greet.ts Ada` prints `Hello, Ada!` and exits 0.",
        "2. `bun greet.ts` with no name, or a blank one, exits 2 and prints `usage: greet.ts <name>` to stderr.",
        "3. Surrounding whitespace in the name is trimmed.",
      ].join("\n"),
    },
    { as: "agent" }
  );
  const fixture: z.infer<typeof TesterFixture> = { issue: issue.key };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key}`);
}

/** `implement-handoff <file> <body file>`: the implementer's handoff on the run's issue, as
 * `dispatch handoff-write` records it under the implementer's session. A body that is no JSON
 * object is the fixture's fault, refused before anything is written. */
export async function implementHandoff(file: string, bodyFile: string): Promise<void> {
  const { issue } = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const body: unknown = JSON.parse(readFileSync(bodyFile, "utf8"));
  if (typeof body !== "object" || body === null || Array.isArray(body))
    throw new Error(`${bodyFile} holds no JSON object, which a handoff body is`);
  const recorded = await createHandoff(issue, "implement", body, IMPLEMENTER);
  const fixture: z.infer<typeof TesterFixture> = {
    issue,
    implement: { id: recorded.id, version: recorded.version },
  };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue}'s implement handoff, version ${recorded.version} (${recorded.id})`);
}

/** `GET route` as the agent bearer, its JSON answer; a refusal names the route and Dispatch's answer. */
async function agentGet<T>(route: string): Promise<T> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

/** `capture-handoffs <file> <run dir>`: every handoff record of the run's issue, the seeded
 * implement record among them, newest first as `GET …/handoffs` lists them, each read whole by id
 * (`GET /api/v1/handoffs/{id}`) for its body. */
export async function captureHandoffs(file: string, runDir: string): Promise<void> {
  const { issue } = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const summaries = await agentGet<HandoffSummary[]>(
    `/api/v1/issues/${encodeURIComponent(issue)}/handoffs`
  );
  const records: Handoff[] = [];
  for (const summary of summaries)
    records.push(await agentGet<Handoff>(`/api/v1/handoffs/${encodeURIComponent(summary.id)}`));
  writeFileSync(path.join(runDir, "handoffs.json"), `${JSON.stringify(records)}\n`);
  console.log(`captured ${issue}: ${records.length} handoff records`);
}

async function askOnMessage(file: string): Promise<void> {
  // The owner session's world: an issue whose backfill was proposed and never run, the proposal
  // as an attached table, and the message that posts the plan for the rest of the work. Every run
  // seeds the same issue, so `force` passes Dispatch's duplicate check.
  const owner = { as: "agent", actor: { kind: "session", id: "dispatch-owner" } } as const;
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "Make the Architecture page tell the truth: every issue attached to its components",
      spec: [
        "## Summary",
        "",
        "The Architecture page colours each component by the issues attached to it, but most issues",
        "in the two proving-ground trees (the Go refactor and the roadmap) are attached to nothing,",
        "so the page shows one gray box. Every issue in both trees should carry the components it",
        "changes, or a stated reason it is not architectural.",
      ].join("\n"),
    },
    owner
  );
  await createIssueArtifact(
    issue.key,
    {
      name: "attachment-dry-run.md",
      content: [
        "| issue | proposed components | why |",
        "| --- | --- | --- |",
        "| Go refactor: state store | daemon | changes the coordinator's store |",
        "| Go refactor: pod runtime | daemon, worker-image | changes the sandbox runtime |",
        "| Roadmap: hiring pipeline | none (not architectural) | hiring process work |",
        "| Roadmap: search ranking | dispatch-server | changes search |",
      ].join("\n"),
    },
    owner
  );
  const message = await createMessage(
    issue.key,
    {
      body: [
        "Taken by the Dispatch owner session, from tonight's owner audit. What shipped: the",
        "unattached-issue guidance and the coordinator's architecture model. What never ran: the",
        "backfill itself. The dry-run table (attachment-dry-run.md, attached here) only proposed",
        "attachments for the Go refactor's and the roadmap's trees. Next: apply that table through",
        "`dispatch issue-update`, after re-checking each row against the live component model. Then",
        "read both trees back and confirm every child carries components or a not-architectural",
        "reason.",
      ].join(" "),
    },
    owner
  );
  const fixture: z.infer<typeof Fixture> = { issue: issue.key, message: message.id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
}

async function measureBeforeAsk(file: string): Promise<void> {
  // The owner session's world: the issues a retired component left stranded, and the message
  // naming three ways to finish. Option A needs each issue's owner, and rig.sh's stranded.csv
  // shows 412 of the 430 have none.
  const owner = { as: "agent", actor: { kind: "session", id: "dispatch-owner" } } as const;
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "Re-home the issues the retired billing-api component left stranded",
      spec: [
        "## Summary",
        "",
        "The last architecture re-import retired the `billing-api` component. The issues attached",
        "to it still point at it, so the Architecture page files them under a component that no",
        "longer exists. Every stranded issue should end up attached to a live component or marked",
        "not architectural.",
      ].join("\n"),
    },
    owner
  );
  const message = await createMessage(
    issue.key,
    {
      body: [
        "Taken by the Dispatch owner session. The billing-api issues are stranded, and there are",
        "three ways to finish: (A) email each issue's owner and have them re-attach it to the right",
        "component; (B) move every stranded issue to `platform-core`, which absorbed most of",
        "billing-api; (C) detach them all and mark each not architectural. The stranded issues are",
        "exported as stranded.csv (key, title, component, owner).",
      ].join(" "),
    },
    owner
  );
  const fixture: z.infer<typeof Fixture> = { issue: issue.key, message: message.id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
}

async function capture(file: string, runDir: string): Promise<void> {
  // The event log is read page by page to its end, as the agent bearer the run's session holds.
  const { issue, message } = Fixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const reader = { as: "agent" } as const;
  const events = await getIssueEvents(issue, { limit: EVENT_PAGE }, reader);
  for (let page = events; page.length === EVENT_PAGE; ) {
    page = await getIssueEvents(issue, { after: page.at(-1)?.seq, limit: EVENT_PAGE }, reader);
    events.push(...page);
  }
  const records = {
    message: await getMessage(issue, message, reader),
    asks: await listIssueAsks(issue, reader),
    events,
  };
  for (const [name, record] of Object.entries(records)) {
    writeFileSync(path.join(runDir, `${name}.json`), `${JSON.stringify(record)}\n`);
  }
  console.log(`captured ${issue}: ${records.asks.length} asks, ${events.length} events`);
}

async function seedBrainstorm(): Promise<void> {
  // The to-do CLI's Dispatch project, the batch's only one, with two issues unrelated to the
  // prompt's feature (a crash already understood and a display tweak). Each title names the "todo
  // CLI" in plain words: Postgres's English parser reads `todo.py` and `to-do` as single lexemes, so
  // a session's `dispatch search` for "todo CLI" finds a title only where `todo` stands alone.
  await createProject({ key: "TODO", name: "todo CLI" });
  for (const [title, problem] of [
    [
      "The todo CLI's done command crashes on a task number past the end of the list",
      "`python todo.py done 9` on a list of three tasks raises IndexError and prints a traceback instead of saying the task does not exist.",
    ],
    [
      "The todo CLI's list command shows no task count",
      "A long list gives no total; `list` should end with a line such as `3 tasks`.",
    ],
  ]) {
    const issue = await createIssue(
      { project: "TODO", force: true, title, spec: `## Problem\n\n${problem}` },
      { as: "agent" }
    );
    console.log(`seeded ${issue.key}`);
  }
}

async function captureBrainstorm(runDir: string): Promise<void> {
  // The run's own issue, from its own transcript, never from the project: a batch's runs share the
  // one project. Dispatch answers `Created <KEY>:` only when it creates the issue, whatever
  // reached it (score.ts's CREATED); a refused duplicate reads `Not created:`.
  let issue: string | null = null;
  for (const text of toolResults(session(runDir))) {
    const match = CREATED.exec(text);
    if (match?.[1] !== undefined) {
      issue = match[1];
      break;
    }
  }
  const asks = issue === null ? [] : await listIssueAsks(issue, { as: "agent" });
  writeFileSync(path.join(runDir, "brainstorm.json"), `${JSON.stringify({ issue, asks })}\n`);
  console.log(`captured ${issue ?? "no new issue"}: ${asks.length} asks`);
}

const USAGE =
  "usage: seed.ts project | ask-on-message <file> | measure-before-ask <file> | capture <file> <run dir> | brainstorm | capture-brainstorm <file> <run dir> | spec-writing <file> <source dir> | spec-rewrite <file> <source dir> | state <file> <turn> <run dir> | answer <file> <answers file> | approve <file> | capture-spec <file> <run dir> | tester-proof <file> | implement-handoff <file> <body file> | capture-handoffs <file> <run dir>";

/** Runs the mode `argv` names; false when argv names no mode, with its arguments. The first six
 * modes take extra arguments and ignore them, as they always did; each new mode takes exactly its
 * own. */
async function main(argv: string[]): Promise<boolean> {
  const [command, file, second, third] = argv;
  const n = argv.length;
  if (command === "project") await seedProject();
  else if (command === "ask-on-message" && file !== undefined) await askOnMessage(file);
  else if (command === "measure-before-ask" && file !== undefined) await measureBeforeAsk(file);
  else if (command === "capture" && file !== undefined && second !== undefined)
    await capture(file, second);
  else if (command === "brainstorm") await seedBrainstorm();
  else if (command === "capture-brainstorm" && second !== undefined)
    await captureBrainstorm(second);
  else if (command === "spec-writing" && file !== undefined && second !== undefined && n === 3)
    await specWriting(file, second);
  else if (command === "spec-rewrite" && file !== undefined && second !== undefined && n === 3)
    await specRewrite(file, second);
  else if (command === "state" && file !== undefined && third !== undefined && n === 4) {
    if (!/^[1-9][0-9]*$/.test(second ?? "")) return false;
    await state(file, Number(second), third);
  } else if (command === "answer" && file !== undefined && second !== undefined && n === 3)
    await answer(file, second);
  else if (command === "approve" && file !== undefined && n === 2) await approve(file);
  else if (command === "capture-spec" && file !== undefined && second !== undefined && n === 3)
    await captureSpec(file, second);
  else if (command === "tester-proof" && file !== undefined && n === 2)
    await testerProofIssue(file);
  else if (command === "implement-handoff" && file !== undefined && second !== undefined && n === 3)
    await implementHandoff(file, second);
  else if (command === "capture-handoffs" && file !== undefined && second !== undefined && n === 3)
    await captureHandoffs(file, second);
  else return false;
  return true;
}

if (import.meta.main) {
  for (const name of ["PLAYWRIGHT_BASE_URL", "E2E_AGENT_TOKEN"]) {
    if (process.env[name] !== undefined) {
      console.error(
        `seed.ts: ${name} is set; seed.ts seeds only rig.sh's scratch Dispatch (DISPATCH_E2E_PORT), so it sends nothing`
      );
      process.exit(2);
    }
  }
  if (!(await main(Bun.argv.slice(2)))) {
    console.error(USAGE);
    process.exit(2);
  }
}
