// Seeds rig.sh's scratch Dispatch through its API, and reads back what a run left there.
// DISPATCH_E2E_PORT names the server, which e2e/api.ts addresses. e2e/api.ts addresses
// PLAYWRIGHT_BASE_URL instead, with E2E_AGENT_TOKEN as its bearer, whenever it is set, so seed.ts
// refuses to run while either is set.
//
//   seed.ts project                        project LWEVAL and LWEVAL-1, the issue the tester's world
//                                          implements
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
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  getIssueEvents,
  getMessage,
  listIssueAsks,
} from "../../../dispatch/e2e/api";

/** What `ask-on-message` writes and `capture` reads. */
const Fixture = z.object({ issue: z.string(), message: z.string() });
/** One transcript line, as far as `capture-brainstorm` reads one: each tool result's text. */
const TranscriptLine = z.looseObject({
  message: z
    .looseObject({
      role: z.string().optional(),
      content: z.array(z.looseObject({ text: z.string().optional() })).optional(),
    })
    .optional(),
});
/** The most events one `GET /api/v1/issues/{key}/events` page holds. */
const EVENT_PAGE = 200;

for (const name of ["PLAYWRIGHT_BASE_URL", "E2E_AGENT_TOKEN"]) {
  if (process.env[name] !== undefined) {
    console.error(
      `seed.ts: ${name} is set; seed.ts seeds only rig.sh's scratch Dispatch (DISPATCH_E2E_PORT), so it sends nothing`
    );
    process.exit(2);
  }
}

const [command, file, runDir] = Bun.argv.slice(2);
if (command === "project") {
  await createProject({ key: "LWEVAL", name: "skill scenarios" });
  const issue = await createIssue(
    {
      project: "LWEVAL",
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
  console.log(`seeded ${issue.key}`);
} else if (command === "ask-on-message" && file !== undefined) {
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
        "dispatch_issue_update, after re-checking each row against the live component model. Then",
        "read both trees back and confirm every child carries components or a not-architectural",
        "reason.",
      ].join(" "),
    },
    owner
  );
  const fixture: z.infer<typeof Fixture> = { issue: issue.key, message: message.id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
} else if (command === "measure-before-ask" && file !== undefined) {
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
} else if (command === "capture" && file !== undefined && runDir !== undefined) {
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
} else if (command === "brainstorm") {
  // The to-do CLI's Dispatch project, the batch's only one, with two issues unrelated to the
  // prompt's feature (a crash already understood and a display tweak). Each title names the "todo
  // CLI" in plain words: Postgres's English parser reads `todo.py` and `to-do` as single lexemes, so
  // a session's `dispatch_search` for "todo CLI" finds a title only where `todo` stands alone.
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
} else if (command === "capture-brainstorm" && runDir !== undefined) {
  // The run's own issue, from its own transcript, never from the project: a batch's runs share the
  // one project. Dispatch answers `Created <KEY>:` only when it creates the issue, whatever reached
  // it (a `write` to the `xd://dispatch_issue` device, the same inside an `eval` cell, or the tool by
  // name), as score.ts's CREATED reads it; a refused duplicate reads `Not created:`.
  const sessions = path.join(runDir, "sessions");
  const transcript = readdirSync(sessions).find((name) => name.endsWith(".jsonl"));
  let issue: string | null = null;
  for (const text of transcript
    ? readFileSync(path.join(sessions, transcript), "utf8").split("\n")
    : []) {
    if (text.trim() === "" || issue !== null) continue;
    const message = TranscriptLine.parse(JSON.parse(text)).message;
    if (message?.role !== "toolResult") continue;
    const created = /\bCreated ([A-Z][A-Z0-9]*-\d+):/.exec(
      (message.content ?? []).map((part) => part.text ?? "").join("")
    );
    issue = created?.[1] ?? null;
  }
  const asks = issue === null ? [] : await listIssueAsks(issue, { as: "agent" });
  writeFileSync(path.join(runDir, "brainstorm.json"), `${JSON.stringify({ issue, asks })}\n`);
  console.log(`captured ${issue ?? "no new issue"}: ${asks.length} asks`);
} else {
  console.error(
    "usage: seed.ts project | ask-on-message <file> | measure-before-ask <file> | capture <file> <run dir> | brainstorm | capture-brainstorm <file> <run dir>"
  );
  process.exit(2);
}
