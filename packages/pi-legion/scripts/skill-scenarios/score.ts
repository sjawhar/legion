#!/usr/bin/env bun
// Scores what rig.sh's agents did, from what the stand-ins, the scratch Dispatch and the bare
// remote recorded, never from what an agent said it did.
//
//   score.ts live-read <run dir> <skills dir> <omp logs dir>
//     One row per `read` the session made: the characters it got back (less the `[Skill file:
//     <path>]` line that Oh My Pi puts first), the file's own character count less its trailing
//     newline, whether the result spilled to an artifact, and so whether the file arrived whole;
//     then the plugin paths Oh My Pi's log says it loaded.
//   score.ts runs <runs dir> [<scenario>]
//     One row per run, then counts per scenario and label, and a legend saying what each count
//     is. Each scenario's rule is on the function that scores it: askOnMessage,
//     measureBeforeAsk, testerProof, brainstormSurface, and specRow for spec-writing and
//     spec-rewrite. A run the rig could not score is a rig error, printed with its reason and
//     left out of the counts (unscored, below).
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";
import { z } from "zod";
import { ENTRY_HEADER } from "./fetch-spec-sources";
import {
  CheckableFile,
  DecisionsOutput,
  FaultsOutput,
  FindingsOutput,
  NumbersOutput,
  PreambleOutput,
  ProposalOutput,
  QUESTION_KINDS,
  QuestionsOutput,
  SharedRunsFile,
} from "./judge-prompts";
import { CREATED, type SessionEntry, session, sessionId, toolResults } from "./transcript";

/** `GET /api/v1/issues/{key}/asks`: every ask on the issue, whatever its state. */
const IssueAsks = z.array(
  z.looseObject({
    question: z.string(),
    options: z
      .array(z.looseObject({ label: z.string(), description: z.string().nullish() }))
      .nullish(),
  })
);
/** `capture-brainstorm`'s output: the one issue the run created in its project (null when it
 *  opened none, which scores the same as any other failure to put the design in the spec), and
 *  that issue's asks. */
const BrainstormCapture = z.looseObject({
  issue: z.string().nullable(),
  asks: z.array(z.looseObject({})),
});
/** `GET /api/v1/issues/{key}/events`, every page `seed.ts capture` read, joined: a bare array. */
const IssueEvents = z.array(
  z.looseObject({
    type: z.string(),
    actor: z.looseObject({ kind: z.string(), id: z.string().nullish() }).nullish(),
  })
);
/** `GET /api/v1/issues/{key}/messages/{id}`: the message and its replies. */
const MessageRead = z.looseObject({ message: z.looseObject({ body: z.string() }) });
/** One line gh-standin.ts or the `bun` stand-in records: when the call started, its exit, and (a
 * gh call that edited the pull request's body) the body the stand-in kept and serves from then on. */
const Call = z.looseObject({
  at: z.string(),
  as: z.string(),
  argv: z.array(z.string()),
  exit: z.number(),
  stdin: z.string().optional(),
  files: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});
/** rig.sh's worker_fixture: the tester's world, `head` the PR's head and `code` the commit under it. */
const World = z.object({
  key: z.string(),
  repo: z.string(),
  pr: z.number(),
  branch: z.string(),
  head: z.string(),
  code: z.string(),
});
const nonEmpty = z.string().min(1);
/** The tester's handoff as the tester role's prompt spells it (the `test` shape; nothing in the
 * daemon holds it to one), as far as the score reads it: the stamps the tester writes itself
 * (`schemaVersion` 1, `phase` test, its `issue`, `completed` an RFC 3339 time), the implementer's
 * proof verdict, the failures, and the tester's own proof, each entry's six fields non-empty.
 * Nothing stamps a handoff for the tester: it writes the whole file with `write`. */
const TestHandoff = z.looseObject({
  schemaVersion: z.literal(1),
  phase: z.literal("test"),
  issue: nonEmpty,
  completed: z.iso.datetime({ offset: true }),
  implementerProof: z.looseObject({ verdict: z.enum(["verified", "rejected"]), how: nonEmpty }),
  failures: z.array(z.looseObject({ criterion: z.string(), evidence: z.string() })).optional(),
  proof: z
    .array(
      z.looseObject({
        criterion: nonEmpty,
        surface: nonEmpty,
        command: nonEmpty,
        observed: nonEmpty,
        headSha: nonEmpty,
        negativeControl: nonEmpty,
      })
    )
    .optional(),
});

function lines(file: string): string[] {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "");
}
/** Each line of a JSONL file, parsed as `schema`; a line that does not parse is the rig's fault. */
function parsed<T>(file: string, schema: z.ZodType<T>): T[] {
  return lines(file).map((line, index) => {
    const result = schema.safeParse(JSON.parse(line));
    if (!result.success)
      throw new Error(`${file}:${index + 1} is not a record: ${result.error.message}`);
    return result.data;
  });
}
function json<T>(file: string, schema: z.ZodType<T>): T {
  return schema.parse(JSON.parse(readFileSync(file, "utf8")));
}

/** Every tool call the session made, in order: its id, its tool, and its whole arguments as JSON
 * and their `path`. */
function toolCalls(entries: SessionEntry[]) {
  return entries.flatMap((entry) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => {
            const args = part.arguments;
            const target =
              typeof args === "object" &&
              args !== null &&
              "path" in args &&
              typeof args.path === "string"
                ? args.path
                : "";
            return {
              id: part.id ?? "",
              tool: part.name ?? "",
              path: target,
              args: JSON.stringify(args ?? null),
            };
          })
  );
}
/** Every `read` call the session made, in order. */
function readCalls(entries: SessionEntry[]): { id: string; target: string }[] {
  return toolCalls(entries)
    .filter((call) => call.tool === "read")
    .map((call) => ({ id: call.id, target: call.path }));
}

function liveRead(runDir: string, skillsDir: string, logsDir: string) {
  const entries = session(runDir);
  const calls = new Map(readCalls(entries).map((call) => [call.id, call.target]));
  console.log("read\tchars\tfile chars\tspilled\twhole");
  let whole = 0;
  for (const { message } of entries) {
    if (message?.role !== "toolResult" || message.toolName !== "read") continue;
    const target = calls.get(message.toolCallId ?? "") ?? "";
    // The pinned Oh My Pi starts a skill read's result with one line, `[Skill file: <absolute
    // path>]`, and then gives the file's text. That line is not the file's. Any other first line
    // stays in the count.
    const got = (message.content ?? [])
      .filter((part) => part.type === "text")
      .map((part) => part.text ?? "")
      .join("")
      .replace(/^\[Skill file: \/[^\n]*\]\n/, "");
    const match = /^skill:\/\/([^/]+)(?:\/(.+))?$/.exec(target);
    const file = match ? path.join(skillsDir, match[1] ?? "", match[2] ?? "SKILL.md") : "";
    // Characters, as `wc -m` counts them, less the file's trailing newline, which a read drops.
    const expected =
      file && existsSync(file) ? [...readFileSync(file, "utf8").replace(/\n$/, "")].length : -1;
    const spilled = got.includes("artifact://");
    const ok = !spilled && [...got].length === expected;
    if (ok) whole += 1;
    console.log(`${target}\t${[...got].length}\t${expected}\t${spilled}\t${ok}`);
  }
  console.log(`${whole} of ${calls.size} reads arrived whole`);
  const loaded = new Set<string>();
  for (const log of existsSync(logsDir) ? readdirSync(logsDir) : []) {
    const logText = readFileSync(path.join(logsDir, log), "utf8");
    for (const hit of logText.matchAll(/"extension":"([^"]*)"/g)) loaded.add(hit[1] ?? "");
  }
  console.log(`extensions loaded: ${[...loaded].sort().join(" ")}`);
}

// ask-on-message: an ask points at the message instead of carrying it when its text sends the
// reader elsewhere in prose. "as described" alone is not one: an option says it of the ask's own
// text.
const POINTER =
  /\b(see|per|in|from|described in|listed in|posted in|outlined in)\s+(the|my|that|this|his|its)\s+(latest\s+|last\s+|earlier\s+|previous\s+|above\s+)?(message|comment|post|note)\b|\b(message|comment)\s+(above|below)\b|\bsee above\b|\bas (posted|attached)\b/i;
const COMMON: Record<string, true> = Object.fromEntries(
  "about after again against their there these those which while would could should other every first never itself shipped taken tonight"
    .split(" ")
    .map((word) => [word, true])
);
/** The distinctive words of a text: five letters or more, not a common word. */
function terms(body: string): Set<string> {
  const words = body.toLowerCase().match(/[a-z][a-z0-9_-]{4,}/g) ?? [];
  return new Set(words.filter((word) => !COMMON[word]));
}

interface Row {
  run: string;
  scenario: string;
  label: string;
  pass: boolean;
  ref: boolean;
  notes: string;
  /** spec-writing and spec-rewrite: the run's source and its faults.json counts. */
  source?: string;
  faults?: number;
  steBreaks?: number;
}

/** A Dispatch scenario's row: its asks, each printed whole under the row because a person judges
 * it, anything else the agent wrote on the issue (seed.ts writes as the session `dispatch-owner`),
 * whether each question gives a recommendation, which the dispatch skill asks for and no count
 * checks, and the skill files the agent read. The run passes the count when it opened an ask and
 * `flag` passes every one; `ref`: the agent read skill://dispatch, where both rules are. */
function askRow(
  runDir: string,
  run: string,
  scenario: string,
  label: string,
  flag: (all: string, options: { label: string }[]) => { pass: boolean; notes: string }
): Row {
  const asks = json(path.join(runDir, "asks.json"), IssueAsks);
  const other = json(path.join(runDir, "events.json"), IssueEvents)
    .filter((event) => event.actor?.id !== "dispatch-owner" && !event.type.startsWith("ask."))
    .map((event) => event.type);
  const verdicts = asks.map((ask) => {
    const options = ask.options ?? [];
    const all = [ask.question, ...options.map((o) => `${o.label} ${o.description ?? ""}`)].join(
      "\n"
    );
    return { question: ask.question, options, ...flag(all, options) };
  });
  const pass = verdicts.length > 0 && verdicts.every((verdict) => verdict.pass);
  const skillReads = readCalls(session(runDir))
    .map((call) => call.target)
    .filter((target) => target.startsWith("skill://"));
  const ref = skillReads.some((target) => /^skill:\/\/dispatch(\/SKILL\.md)?$/.test(target));
  const notes = [
    `asks=${asks.length}`,
    ...verdicts.map(
      (v, i) =>
        `ask${i + 1}: ${v.notes} options=${v.options.length} recommends=${/\brecommend/i.test(v.question)} chars=${v.question.length}`
    ),
    `otherWrites=[${other.join(", ")}]`,
    `reads=[${skillReads.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  for (const [i, v] of verdicts.entries()) {
    const shown = v.options.map((o) => `  - ${o.label}: ${o.description ?? ""}`);
    notes.push(`\n  ask${i + 1} question: ${v.question}\n${shown.join("\n")}`);
  }
  return { run, scenario, label, pass, ref, notes: notes.join(" ") };
}

/** ask-on-message: the dispatch skill's rule is that the ask carries the plan and its options in
 * its own text and never points at the message in prose. A person judges that. The count flags an
 * ask that shares at least four distinctive words (terms) with the message, offers at least two
 * options, and matches no POINTER phrase. */
function askOnMessage(runDir: string, run: string, label: string): Row {
  const wanted = terms(json(path.join(runDir, "message.json"), MessageRead).message.body);
  return askRow(runDir, run, "ask-on-message", label, (all, options) => {
    const carried = [...terms(all)].filter((word) => wanted.has(word));
    const pointer = POINTER.exec(all)?.[0];
    return {
      pass: carried.length >= 4 && options.length >= 2 && pointer === undefined,
      notes: `carried=${carried.length}[${carried.join(",")}] pointer=${JSON.stringify(pointer ?? null)}`,
    };
  });
}

/** A value that carries Dispatch's credential: the variable's own name, or a bearer header in any
 * quoting (`Authorization: Bearer …`, `"Authorization": "Bearer …"`). */
const LEAK = /DISPATCH_TOKEN|Authorization\W{1,6}Bearer\s+\S/i;

/** Where a brainstorm run put the design: "spec" when Dispatch created it an issue and it never
 * asked through the interactive `ask` tool, "chat" the other way round, else "mixed" or "neither". */
function surfaceOf(createdIssue: boolean, askedChat: boolean): string {
  if (createdIssue && askedChat) return "mixed";
  if (createdIssue) return "spec";
  if (askedChat) return "chat";
  return "neither";
}

/** brainstorm: whether a bare `/brainstorming` prompt, with no word of Dispatch in the prompt and
 * no Dispatch configuration anywhere in the run's environment, puts the design in the issue's spec
 * (`skill://dispatch-brainstorming`) or asks in chat (main, before the skill existed); `surface`
 * is `surfaceOf`'s answer. `blocks` is the created issue's own ask count, from
 * `capture-brainstorm`'s read of Dispatch, never the agent's own claim in chat. `leak` flags a
 * session that went looking for its project next to the credential
 * (`skills/dispatch-brainstorming/SKILL.md` "Where the spec lives"): `LEAK` matching a tool call's
 * arguments, or a tool's result, where a bare `env` or `printenv` dump lands in the model's
 * context without its command naming anything. `ref`: the run's first `read` was
 * `skill://dispatch-brainstorming` — this scenario's own instrument, since a main run has no such
 * skill to read. The run passes when it put the design in the spec and leaked nothing looking for
 * the project. */
function brainstormSurface(runDir: string, run: string, label: string): Row {
  const entries = session(runDir);
  const calls = toolCalls(entries);
  // Whether Dispatch created an issue is its own answer in a tool's result, whatever reached it: a
  // `dispatch issue` command in a `bash` call, the same run from inside an `eval` cell, or a host
  // that calls the tool by name. A call Dispatch refused as a duplicate created nothing.
  const results = toolResults(entries);
  const surface = surfaceOf(
    results.some((text) => CREATED.test(text)),
    calls.some((call) => call.tool === "ask")
  );
  const leak =
    calls.some((call) => LEAK.test(call.args)) || results.some((text) => LEAK.test(text));
  const captured = json(path.join(runDir, "brainstorm.json"), BrainstormCapture);
  const reads = calls.filter((call) => call.tool === "read").map((call) => call.path);
  const ref = reads[0] === "skill://dispatch-brainstorming";
  const pass = surface === "spec" && !leak;
  return {
    run,
    scenario: "brainstorm",
    label,
    pass,
    ref,
    notes: `surface=${surface} issue=${captured.issue ?? "none"} blocks=${captured.asks.length} leak=${leak} reads=[${reads.join(",")}]`,
  };
}

/** measure-before-ask: gate 2 of the dispatch skill's "Before you ask" (skills/dispatch/SKILL.md).
 * rig.sh's export lists 430 stranded issues, 412 of them without the owner that fix A (email each
 * issue's owner) needs, while fixes B and C repair all 430. So the ask gate 2 calls for names the
 * 430 and that measurement, offers B and C, and drops A rather than offering it cut down to the 18
 * it reaches. A person judges each ask. The count flags an ask that names the population
 * (430), names the measurement (the 412 without an owner, or the 18 with one), offers at least two
 * options, none of whose labels mentions emailing or owners, and matches no POINTER phrase. The
 * notes also say whether a tool call named the export (`readExport`).
 *
 * What the count cannot see:
 *   - it reads option labels only, for "email" or "owner": option A kept under another label
 *     passes, and A cut down to the 18 it reaches counts as kept, which the rule also forbids;
 *   - it reads only the seeded issue's asks: an ask opened on another issue scores as a fail, not
 *     a rig error;
 *   - `readExport` is a substring match on the tool calls' arguments, not a record of a read;
 *   - the fixture gives agents a second reason to drop A: every owner in the export is at
 *     example.invalid, and an agent that notices those addresses cannot receive mail has grounds
 *     to drop A that have nothing to do with gate 2. Split the runs on whether the ask says so, and
 *     read a drop against that split before crediting the rule with it. */
function measureBeforeAsk(runDir: string, run: string, label: string): Row {
  const row = askRow(runDir, run, "measure-before-ask", label, (all, options) => {
    const population = /\b430\b/.test(all);
    const measured = /\b(412|18)\b/.test(all);
    const dropped = !options.some((option) => /e-?mail|owner/i.test(option.label));
    const pointer = POINTER.exec(all)?.[0];
    return {
      pass: population && measured && dropped && options.length >= 2 && pointer === undefined,
      notes: `population=${population} measured=${measured} dropped=${dropped} pointer=${JSON.stringify(pointer ?? null)}`,
    };
  });
  const readExport = toolCalls(session(runDir)).some((call) => call.args.includes("stranded.csv"));
  return { ...row, notes: `readExport=${readExport} ${row.notes}` };
}

/** A commit this run can name: the PR's head, or one its tester pushed. A seven-or-more-digit
 * prefix counts. The implementer's code commit does not, since the fixture's own `E2E
 * (implementer)` line names it. */
function namesOwnCommit(text: string, commits: string[]): boolean {
  return [...text.matchAll(/\b[0-9a-f]{7,40}\b/g)].some(([sha]) =>
    commits.some((commit) => commit.startsWith(sha))
  );
}

/** tester-proof: the legion-worker skill's rule for a tester whose predecessor's proof holds.
 * worker_fixture writes a valid implement handoff whose proof reproduces, and its CLI meets all
 * three acceptance criteria, so the skill's answer is `verified` with a proof of the tester's own
 * and no red test to push (a handoff that failed validation would read as missing, and the answer
 * would be `rejected`). The handoff write is the session's `write` tool call of
 * .legion/<key>/test.json and the completion its `legion` tool call with `op: "handoff_complete"`
 * that the tool answered without error, each timed at the call; the pushes are the run's own
 * remote's record. A run passes when all of these hold:
 *   1. before the handoff write, the run's `bun` stand-in recorded a run of greet.ts (the first
 *      argument that is not a flag, after an optional `run`): the tester drove the CLI, whatever
 *      command line it wrote to do so. The stand-in records only a bun reached through PATH:
 *      `mise exec bun@… -- bun greet.ts` and a bun named by its absolute path bypass it, and such
 *      a run reads as having run nothing;
 *   2. that write comes before the first push carrying .legion/<key>/test.json, which comes before
 *      the accepted completion: a push before you complete is the rule, since the `legion` tool's
 *      handoff_complete reports the pushed commit carrying the handoff and refuses an unpushed one;
 *   3. the branch as the tester completed it (its last push before the completion) changes nothing
 *      under the PR's head but .legion/<key>/test.json, which is a test handoff (TestHandoff) of
 *      this issue with `implementerProof.verdict` `verified`, no failures, and a proof whose every
 *      `headSha` names a commit of this run's own (namesOwnCommit);
 *   4. the PR body the gh stand-in kept last has an `E2E (tester)` line, up to the next field, that
 *      names a commit of this run's own.
 * A line naming another run's PR head instead is two runs meeting in the shared /tmp, where agents
 * draft the body whatever TMPDIR says: a rig error, thrown, not a failure.
 * `ref`: the agent read skill://legion-worker/references/pr-body.md, where the line is defined. */
function testerProof(runDir: string, run: string, label: string, heads: string[]): Row {
  const world = json(path.join(runDir, "world.json"), World);
  const calls = parsed(path.join(runDir, "calls.jsonl"), Call);
  const entries = session(runDir);
  const handoffPath = `.legion/${world.key}/test.json`;
  // Each tool result, by the call it answers: whether the tool refused it.
  const refusedResults = new Map<string, boolean>();
  for (const { message } of entries) {
    if (message?.role === "toolResult" && message.toolCallId !== undefined)
      refusedResults.set(message.toolCallId, message.isError === true);
  }
  // The session's tool calls, each timed at the assistant message that made it.
  const timedCalls = entries.flatMap((entry) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => {
            const args =
              typeof part.arguments === "object" && part.arguments !== null
                ? (part.arguments as Record<string, unknown>)
                : {};
            return { at: entry.timestamp ?? "", id: part.id ?? "", tool: part.name ?? "", args };
          })
  );
  const write = timedCalls.find(
    (c) =>
      c.tool === "write" && typeof c.args.path === "string" && c.args.path.endsWith(handoffPath)
  );
  const completions = timedCalls
    .filter((c) => c.tool === "legion" && c.args.op === "handoff_complete")
    .map((c) => ({ at: c.at, accepted: refusedResults.get(c.id) === false }));
  // A completion the tool refused never counts.
  const complete = completions.find((c) => c.accepted);
  const refused = completions.filter((c) => !c.accepted).length;
  const remote = path.join(runDir, "remote.git");
  const git = (...args: string[]) => Bun.spawnSync(["git", "-C", remote, ...args]);
  const pushes = lines(path.join(runDir, "pushes.log"))
    .map((line) => {
      const [at = "", ref = "", , sha = ""] = line.split(" ");
      return { at, ref, sha };
    })
    .filter((p) => p.ref === `refs/heads/${world.branch}`);
  const own = [world.head, ...pushes.map((p) => p.sha)];
  const drove = calls.find((c) => {
    if (c.as !== "bun") return false;
    const args = c.argv.filter((arg) => !arg.startsWith("-"));
    return (args[0] === "run" ? args[1] : args[0])?.endsWith("greet.ts") ?? false;
  });
  const push = pushes.find((p) => git("cat-file", "-e", `${p.sha}:${handoffPath}`).exitCode === 0);
  const ran = drove !== undefined && write !== undefined && drove.at < write.at;
  const ordered =
    write !== undefined &&
    push !== undefined &&
    complete !== undefined &&
    write.at < push.at &&
    push.at < complete.at;
  const tip = pushes.filter((p) => complete === undefined || p.at < complete.at).at(-1);
  const problems: string[] = [];
  if (tip === undefined) problems.push("nothing pushed");
  else {
    const changed = git("diff", "--name-only", world.head, tip.sha).stdout.toString().trim();
    if (changed !== handoffPath) problems.push(`the push changed [${changed.split("\n")}]`);
    const shown = git("show", `${tip.sha}:${handoffPath}`);
    const read = TestHandoff.safeParse(
      shown.exitCode === 0 ? JSON.parse(shown.stdout.toString()) : undefined
    );
    if (!read.success) problems.push(`not a test handoff: ${z.prettifyError(read.error)}`);
    else {
      const { issue, implementerProof, failures = [], proof = [] } = read.data;
      if (issue !== world.key) problems.push(`issue is ${issue}, not ${world.key}`);
      if (implementerProof.verdict !== "verified")
        problems.push(`implementerProof.verdict is ${implementerProof.verdict}`);
      if (failures.length > 0) problems.push(`${failures.length} failures`);
      if (proof.length === 0) problems.push("no proof of the tester's own");
      for (const [i, entry] of proof.entries())
        if (!namesOwnCommit(entry.headSha, own))
          problems.push(`proof.${i}.headSha ${entry.headSha} is not this run's`);
    }
  }
  const proof = problems.length === 0;
  const bodies = calls.filter((c) => c.as === "gh" && c.exit === 0 && c.body !== undefined);
  const edit = bodies.find((c) => c.body?.includes("E2E (tester)"));
  const testerLine =
    /\*\*E2E \(tester\):\*\*[\s\S]*?(?=\n\*\*|$)/.exec(bodies.at(-1)?.body ?? "")?.[0] ?? "";
  const ownHead = namesOwnCommit(testerLine, own);
  const foreign = heads.filter((head) => head !== world.head);
  if (!ownHead && namesOwnCommit(testerLine, foreign))
    throw new Error(
      `its PR body carries another run's E2E (tester) line: ${testerLine.slice(0, 160)}`
    );
  const pass = ran && ordered && proof && ownHead;
  const opened = readCalls(entries).map((call) => call.target);
  const ref = opened.some((target) =>
    target.includes("skill://legion-worker/references/pr-body.md")
  );
  const worker = opened
    .filter((target) => target.includes("legion-worker"))
    .map((target) => target.replace("skill://legion-worker", "") || "/");
  const notes = [
    `ran=${ran} write=${write !== undefined} proof=${proof} push=${push !== undefined} complete=${complete !== undefined} refusedCompletions=${refused} ordered=${ordered} testerLine=${testerLine !== ""} ownHead=${ownHead}`,
    `editBeforeWrite=${edit !== undefined && write !== undefined && edit.at < write.at}`,
    ...(proof ? [] : [`proofProblems=${JSON.stringify(problems)}`]),
    `refs=[${worker.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  return { run, scenario: "tester-proof", label, pass, ref, notes: notes.join(" ") };
}

// spec-writing and spec-rewrite: how a fresh agent writes a design spec with a person. A run is a
// design conversation between a fresh agent and a person stand-in on the run's own issue. Its
// directory holds what rig.sh, seed.ts and judge.ts recorded: the source (source/), the turns
// (turns.txt, state-<turn>.json), the captured issue (spec.md, spec-versions/, agent-asks.json,
// agent-texts.json, documents/, plan.md) and the judges' answers (faults.json and the rest).

/** The outline every spec must follow: its `##` headings, exactly and in this order. */
export const OUTLINE = [
  "Summary",
  "Problem",
  "Goals and non-goals",
  "Requirements",
  "Design",
  "Decisions",
  "Acceptance",
] as const;
/** The Dispatch skill the change removes. `routed` fails a run that reads it, so this name stays
 * when the skill is renamed everywhere else. */
const REMOVED_SKILL = "dispatch-brainstorming";
/** The fewest words a shared run holds. */
const RUN_WORDS = 8;
/** The checkout rig.sh runs from, which a run's agent can also reach. */
const RIG_ROOT = path.resolve(import.meta.dir, "../../../..");

const Actor = z.looseObject({ kind: z.string(), id: z.string().nullish() });
/** spec-versions.json: every version of the primary spec. A version lists its `authors`; `actor`
 * is the first of them. */
export const SpecVersions = z.array(
  z.looseObject({
    version: z.number().int(),
    created_at: z.string(),
    authors: z.array(Actor).nullish(),
    actor: Actor.nullish(),
  })
);
const AskOption = z.looseObject({ label: z.string(), description: z.string().nullish() });
/** state-<turn>.json: after the turn, the issue's open question asks (never the approval request)
 * and its open approval request. */
export const State = z.looseObject({
  turn: z.number().int(),
  open_asks: z.array(
    z.looseObject({
      id: z.string(),
      question: z.string(),
      options: z.array(AskOption).nullish(),
      multiple: z.boolean().nullish(),
    })
  ),
  approval: z.looseObject({ ask_id: z.string(), version: z.number().int() }).nullable(),
});
export type State = z.infer<typeof State>;
/** agent-asks.json: each ask on the issue that seed.ts did not seed, the approval request
 * included. */
export const AgentAsks = z.array(
  z.looseObject({
    id: z.string(),
    kind: z.string(),
    question: z.string(),
    options: z.array(AskOption).nullish(),
    created_at: z.string().nullish(),
  })
);
/** agent-texts.json: each message, comment and ask the run's session wrote. */
export const AgentTexts = z.array(
  z.looseObject({ id: z.string(), kind: z.string(), body: z.string() })
);
/** source/asks.json: the asks the people answered on the source issue. */
const SourceAsks = z.array(
  z.looseObject({ answer: z.looseObject({ text: z.string().nullish() }).nullish() })
);
/** fixture.json of a spec run: the run's issue and its primary spec's artifact id. */
export const SpecFixture = z.looseObject({ issue: z.string(), spec_artifact: z.string() });
export type SpecFixture = z.infer<typeof SpecFixture>;

/** The ids of the asks seed.ts seeded (seeded.json). Each is another agent's ask, which counts
 * neither against the run nor for it. */
export function seededIds(runDir: string): Set<string> {
  return new Set(json(path.join(runDir, "seeded.json"), z.array(z.string())));
}

/** Each turn's state-<turn>.json, one per line of turns.txt, in turn order. */
function states(runDir: string): State[] {
  return lines(path.join(runDir, "turns.txt")).map((line) =>
    json(path.join(runDir, `state-${line.split(" ")[0]}.json`), State)
  );
}

/** Whether the person approved the spec: turns.txt has an `approved` turn, the one the rig starts
 * with "The person approved the spec". `version` is the version the approval request named in
 * the last state before that turn. */
export function approval(runDir: string): {
  approved: boolean;
  version: number | null;
  turns: number;
} {
  const turns = lines(path.join(runDir, "turns.txt")).map((line) => line.split(" "));
  const approvedTurn = turns.find(([, kind]) => kind === "approved")?.[0];
  if (approvedTurn === undefined) return { approved: false, version: null, turns: turns.length };
  const requested = states(runDir)
    .filter((state) => state.turn < Number(approvedTurn) && state.approval !== null)
    .at(-1)?.approval;
  if (!requested)
    throw new Error(
      `turns.txt names turn ${approvedTurn} approved, but no state before it holds an approval request`
    );
  return { approved: true, version: requested.version, turns: turns.length };
}

/** Every checkout a run's agent can reach: each label's, as `rig.sh profile` recorded it under
 * <work>/profiles, and the one rig.sh runs from. */
export function checkouts(work: string): string[] {
  const profiles = path.join(work, "profiles");
  const labels = (existsSync(profiles) ? readdirSync(profiles) : [])
    .filter((label) => existsSync(path.join(profiles, label, "checkout")))
    .map((label) => readFileSync(path.join(profiles, label, "checkout"), "utf8").trim());
  return [...new Set([...labels, RIG_ROOT])];
}

const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** Every string in a value, depth first. */
function strings(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(strings);
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(strings);
  return [];
}

/** One tool call of the session, at the assistant message that made it. */
interface Step {
  index: number;
  at: string;
  id: string;
  tool: string;
  args: Record<string, unknown>;
}
/** One tool result: whether the tool refused the call, a `bash` call's exit code (`details`, else
 * the text's last line), its text and the tool's own record. */
interface Outcome {
  index: number;
  at: string;
  failed: boolean;
  exit: number | undefined;
  text: string;
  details: unknown;
}
function steps(entries: SessionEntry[]): Step[] {
  return entries.flatMap((entry, index) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => ({
            index,
            at: entry.timestamp ?? "",
            id: part.id ?? "",
            tool: part.name ?? "",
            args:
              typeof part.arguments === "object" && part.arguments !== null
                ? (part.arguments as Record<string, unknown>)
                : {},
          }))
  );
}
function outcomes(entries: SessionEntry[]): Map<string, Outcome> {
  const found = new Map<string, Outcome>();
  for (const [index, entry] of entries.entries()) {
    const message = entry.message;
    if (message?.role !== "toolResult" || message.toolCallId === undefined) continue;
    const text = (message.content ?? []).map((part) => part.text ?? "").join("");
    const details = message.details;
    const exit =
      typeof details === "object" &&
      details !== null &&
      "exitCode" in details &&
      typeof details.exitCode === "number"
        ? details.exitCode
        : Number(/Command exited with code (\d+)\s*$/.exec(text)?.[1] ?? Number.NaN);
    found.set(message.toolCallId, {
      index,
      at: entry.timestamp ?? "",
      failed: message.isError === true,
      exit: Number.isNaN(exit) ? undefined : exit,
      text,
      details,
    });
  }
  return found;
}

/** One `dispatch <command>` a tool call ran, with its flags. */
interface DispatchCall {
  command: string;
  text: string;
  flag: (name: string) => string | undefined;
}
/** Where a command starts in shell text: at a line's start, or after `;`, `&`, `|`, `(`, `$(` or a
 * backquote, past any variable assignments, the words that open a command inside a compound one
 * (`then`, `do`, `else`, `elif`, `!`, `{`), and `command`, `exec` or `env`. */
const AT_COMMAND = String.raw`(?:^|[;&|(\x60]|\$\()[ \t]*(?:[A-Za-z_]\w*=\S*[ \t]+)*(?:(?:then|do|else|elif|!|\{)[ \t]+)*(?:(?:command|exec|env)[ \t]+)?`;
/** A command's text after its first word, up to the next command's place. */
const COMMAND_REST = String.raw`[^\n;&|\x60)]*`;
/** A `dispatch` command at a command's place: its text from `dispatch` on, and its command. */
const DISPATCH_COMMAND = new RegExp(
  String.raw`${AT_COMMAND}(dispatch[ \t]+([a-z][a-z-]*)${COMMAND_REST})`,
  "gm"
);
/** Any command at a command's place, by the same parser: its first word and the rest. */
const SHELL_COMMAND = new RegExp(
  String.raw`${AT_COMMAND}([^\s;&|\x60()<>]+)(${COMMAND_REST})`,
  "gm"
);
/** A here-document: the rest of the line that opens it, then its body up to the line that ends it. */
const HEREDOC = /(<<-?[ \t]*(['"]?)(\w+)\2[^\n]*\n)[\s\S]*?^[ \t]*\3[ \t]*$/gm;
/** A skill file a command names: a skill:// target, or a `.md` file under `skills/<name>/`. */
const SKILL_PATH = /skill:\/\/[\w./-]+|[^\s"'`]*\/skills\/[\w.-]+\/[\w./-]*\.md/g;
/** A command whose output goes to a file, not to the tool's result. */
const TO_FILE = /(?:^|\s)1?>>?(?![&>])/;
/** The words of the shell's compound commands and tests, and commands that print nothing when
 * they succeed. */
const SILENT = new Set(
  "cd mkdir export set unset true false : rm cp mv touch chmod ln sleep local declare shopt trap wait umask exit return if then else elif for while until do in case [ [[ test { !".split(
    " "
  )
);
/** The words that close a compound command: its exit is the exit of the command it ran last. */
const CLOSERS = new Set(["fi", "done", "esac", "}"]);
/** What `dispatch <command>` prints first when Dispatch took the write: for doc-edit
 * `Applied <n> ops …`, for artifact `Uploaded <name> as version <v> …`, for issue
 * `Created <key>: …` (packages/envoy-client/src/dispatch-execute.ts). */
const ANSWER: Readonly<Record<string, RegExp>> = {
  "doc-edit": /^Applied \d+ ops\b/,
  artifact: /^Uploaded \S+ as version \d+/,
  issue: /^Created [A-Z][A-Z0-9]*-[1-9]\d*: /,
};

/** A `dispatch` command's flags, read from its text. */
function shellCall(text: string, command: string): DispatchCall {
  return {
    command,
    text,
    flag: (name: string) => {
      const found = new RegExp(`(?:^|\\s)--${name}(?:=|\\s+)(?:'([^']*)'|"([^"]*)"|(\\S+))`).exec(
        text
      );
      if (found !== null) return found[1] ?? found[2] ?? found[3];
      return new RegExp(`(?:^|\\s)--${name}(?:\\s|$)`).test(text) ? "" : undefined;
    },
  };
}
/** A Dispatch tool a host calls by name (`dispatch_doc_edit`), as the `dispatch` command it is. */
function nativeCall(step: Step, tool: string): DispatchCall {
  return {
    command: tool.replace(/_/g, "-"),
    text: `${step.tool} ${JSON.stringify(step.args)}`,
    flag: (name: string) => {
      const value = step.args[name.replace(/-/g, "_")];
      return value === undefined
        ? undefined
        : typeof value === "string"
          ? value
          : JSON.stringify(value);
    },
  };
}
/** Shell text with its lines joined where a backslash continues them, and its here-document
 * bodies taken out (a body is data for the command whose `<<` opens it), with each body and the
 * place of its `<<` in that text. */
function withoutBodies(source: string): { code: string; bodies: { at: number; text: string }[] } {
  const joined = source.replace(/\\\r?\n/g, " ");
  const bodies: { at: number; text: string }[] = [];
  let code = "";
  let last = 0;
  for (const hit of joined.matchAll(HEREDOC)) {
    const opener = hit[1] ?? "";
    code += joined.slice(last, hit.index) + opener;
    bodies.push({ at: code.length - opener.length, text: hit[0].slice(opener.length) });
    last = (hit.index ?? 0) + hit[0].length;
  }
  return { code: code + joined.slice(last), bodies };
}
/** How the command at `at` follows the one before it. */
function separator(code: string, at: number): string {
  if (code.startsWith("$(", at)) return "$(";
  const char = code[at];
  if (char === "|") return code[at - 1] === "|" ? "||" : "|";
  if (char === "&") return code[at - 1] === "&" ? "&&" : "&";
  return char === ";" || char === "(" || char === "`" ? char : "\n";
}
/** The line an `echo` prints, when its arguments are a literal: no option, expansion or escape. */
function echoed(args: string): string | undefined {
  const text = args.trim();
  if (text.startsWith("-")) return undefined;
  const quoted = /^(["'])([^"']*)\1$/.exec(text);
  if (quoted !== null) {
    const line = quoted[2] ?? "";
    return quoted[1] === '"' && /[$`\\]/.test(line) ? undefined : line;
  }
  return /["'$`\\*?[\]{}~]/.test(text) ? undefined : text.replace(/[ \t]+/g, " ");
}
/** The refusal the dispatch CLI printed for `command` at `lines[at]`: that line, a
 * `dispatch <command> was not called: …` line or a usage refusal (which names no command), and
 * the `- ` lines under it. */
function refusalAt(
  lines: string[],
  at: number,
  command: string
): { text: string; end: number } | undefined {
  const [found] = cliRefusals(lines[at] ?? "");
  if (found === undefined || (found.command !== null && found.command !== command))
    return undefined;
  let end = at + 1;
  while (lines[end]?.startsWith("- ")) end++;
  return { text: lines.slice(at, end).join("\n"), end };
}

/** One thing a tool call did, in the order it did them: a read of a skill file, or one `dispatch`
 * command, with what the call's result shows of it. `answer` is the line of Dispatch's answer to a
 * write (ANSWER), or "". `refusal` is the refusal the result shows for the command, or undefined.
 * `succeeded` is true when the command ran and was not refused. */
type Operation =
  | { kind: "read"; step: Step; target: string }
  | {
      kind: "dispatch";
      step: Step;
      call: DispatchCall;
      answer: string;
      refusal: string | undefined;
      succeeded: boolean;
    };
/** A shell command of a call, in order, as operationsOf reads it. */
type ShellCommand =
  | { kind: "dispatch"; at: number; sep: string; call: DispatchCall; quiet: boolean }
  | { kind: "label"; at: number; sep: string; line: string }
  | { kind: "silent"; at: number; sep: string }
  | { kind: "closer"; at: number; sep: string }
  | { kind: "other"; at: number; sep: string };

/**
 * What a tool call did, in order (Operation).
 * - A `task` call does nothing here, and a `read` of a skill:// target or of a file under
 *   `skills/<name>/` reads it.
 * - A Dispatch tool a host calls by name is one `dispatch` command. Its result is that command's
 *   output, and the tool's failure is its refusal.
 * - A `bash` command or an `eval` cell runs each `dispatch` command DISPATCH_COMMAND finds in it
 *   (here-document bodies left out), and reads each skill file it names outside them: in its text,
 *   or in a here-document a command other than `dispatch` reads, at that command's place.
 * - Another tool reads the first skill file its arguments name.
 *
 * One result holds the output of every command of the call, with nothing to tell one command's
 * output from the next. So a command's output is attributed only where the output shows it. The
 * commands are walked in order with the line where the next output starts, while it is known:
 * the call's first line, then the line after a literal `echo` label the output shows, or after a
 * refusal attributed to the command before. A `dispatch` command's refusal is the refusal line at
 * the start of its own output (refusalAt). A write takes the next line of its kind of answer
 * (ANSWER), unless a refusal naming its command comes first. The call's failure (a `bash` exit of
 * 1 or 2, any failure of another tool) is the refusal of its last `dispatch` command when that
 * command ends the call, except one after `&&` that follows a shown refusal: the shell never ran
 * it.
 *
 * What cannot be attributed:
 * - a refusal after the output of another command (`cat`, `sed`, a `dispatch` command that
 *   succeeded), whose end the output does not show: it could be that output's text;
 * - output a `$(…)` or a redirect to a file takes, which is not in the result where the command
 *   runs;
 * - a command in a loop, which runs more than once and is one command here;
 * - text another command prints that holds a line of Dispatch's answer before a write's own;
 * - which command of a failed call set its exit, when the call does not end with a `dispatch`
 *   command.
 */
function operationsOf(step: Step, outcome: Outcome | undefined): Operation[] {
  if (step.tool === "task") return [];
  if (step.tool === "read") {
    const target = typeof step.args.path === "string" ? step.args.path : "";
    return /^skill:\/\//.test(target) || /(^|\/)skills\/[^/]+\//.test(target)
      ? [{ kind: "read", step, target }]
      : [];
  }
  const text = outcome?.text ?? "";
  const output = text.replace(/\s*Command exited with code \d+\s*$/, "");
  const lines = output.split("\n");
  const failed =
    outcome?.failed === true && (step.tool !== "bash" || outcome.exit === 1 || outcome.exit === 2);
  const native = /^dispatch_([a-z_]+)$/.exec(step.tool)?.[1];
  if (native !== undefined) {
    const call = nativeCall(step, native);
    const answer = lines.find((line) => ANSWER[call.command]?.test(line)) ?? "";
    const refusal = failed ? output : undefined;
    const succeeded = outcome !== undefined && !outcome.failed;
    return [{ kind: "dispatch", step, call, answer, refusal, succeeded }];
  }
  if (step.tool !== "bash" && step.tool !== "eval") {
    const target = new RegExp(SKILL_PATH.source).exec(strings(step.args).join("\n"))?.[0];
    return target === undefined ? [] : [{ kind: "read", step, target }];
  }
  const source = (step.tool === "bash" ? strings(step.args.command) : strings(step.args)).join(
    "\n"
  );
  const { code, bodies } = withoutBodies(source);
  const spans = [...code.matchAll(DISPATCH_COMMAND)].map((hit) => ({
    at: hit.index ?? 0,
    end: (hit.index ?? 0) + hit[0].length,
    command: hit[2] ?? "",
    text: (hit[1] ?? "").trim(),
  }));
  const inside = (at: number) => spans.some((span) => at >= span.at && at < span.end);
  const commands: ShellCommand[] = spans.map((span) => {
    const sep = separator(code, span.at);
    const call = shellCall(span.text, span.command);
    const quiet = sep === "$(" || sep === "`" || TO_FILE.test(span.text);
    return { kind: "dispatch", at: span.at, sep, call, quiet };
  });
  for (const hit of code.matchAll(SHELL_COMMAND)) {
    const at = hit.index ?? 0;
    const end = at + hit[0].length;
    const word = hit[1] ?? "";
    // The `1` of a `2>&1`, a comment, or a command that overlaps a `dispatch` one.
    if (/^\d+$/.test(word) || word.startsWith("#")) continue;
    if (spans.some((span) => at < span.end && end > span.at)) continue;
    const sep = separator(code, at);
    const args = hit[2] ?? "";
    const line = word === "echo" && !TO_FILE.test(args) ? echoed(args) : undefined;
    if (line !== undefined) commands.push({ kind: "label", at, sep, line });
    else if (CLOSERS.has(word)) commands.push({ kind: "closer", at, sep });
    else if (SILENT.has(word) || TO_FILE.test(args)) commands.push({ kind: "silent", at, sep });
    else commands.push({ kind: "other", at, sep });
  }
  commands.sort((a, b) => a.at - b.at);

  const operations: { at: number; operation: Operation }[] = [];
  for (const hit of code.matchAll(SKILL_PATH))
    if (!inside(hit.index ?? 0))
      operations.push({ at: hit.index ?? 0, operation: { kind: "read", step, target: hit[0] } });
  for (const body of bodies)
    if (!inside(body.at))
      for (const [target] of body.text.matchAll(SKILL_PATH))
        operations.push({ at: body.at, operation: { kind: "read", step, target } });

  const invocations: Extract<Operation, { kind: "dispatch" }>[] = [];
  let cursor: number | undefined = 0;
  let anchor = 0;
  for (const [place, command] of commands.entries()) {
    if (command.kind === "dispatch") {
      const start: number | undefined = command.quiet ? undefined : cursor;
      const shown: { text: string; end: number } | undefined =
        start === undefined ? undefined : refusalAt(lines, start, command.call.command);
      let answer = "";
      if (shown !== undefined) {
        cursor = shown.end;
        anchor = shown.end;
      } else {
        const kind = ANSWER[command.call.command];
        const writes =
          kind !== undefined &&
          command.call.flag("help") === undefined &&
          command.call.flag("dry-run") === undefined;
        if (writes && !command.quiet)
          for (let line = start ?? anchor; line < lines.length; line++) {
            const own = cliRefusals(lines[line] ?? "")[0];
            if (own?.command === command.call.command) {
              anchor = line + 1;
              break;
            }
            if (kind.test(lines[line] ?? "")) {
              answer = lines[line] ?? "";
              anchor = line + 1;
              break;
            }
          }
        if (!command.quiet) cursor = undefined;
      }
      const invocation = {
        kind: "dispatch" as const,
        step,
        call: command.call,
        answer,
        refusal: shown?.text,
        succeeded: false,
      };
      invocations.push(invocation);
      operations.push({ at: command.at, operation: invocation });
      continue;
    }
    // A command whose output goes into a pipe prints nothing itself.
    if (command.kind === "silent" || command.kind === "closer") continue;
    if (commands[place + 1]?.sep === "|") continue;
    if (command.kind === "other") {
      cursor = undefined;
      continue;
    }
    const label = command.line;
    if (cursor !== undefined && lines[cursor] === label) {
      cursor += 1;
      anchor = Math.max(anchor, cursor);
      continue;
    }
    const found = label === "" ? -1 : lines.indexOf(label, anchor);
    cursor = found < 0 ? undefined : found + 1;
    if (found >= 0) anchor = found + 1;
  }
  const last = invocations.at(-1);
  const ending = commands.filter((command) => command.kind !== "closer").at(-1);
  if (failed && last !== undefined && ending?.kind === "dispatch" && ending.call === last.call) {
    const skipped =
      ending.sep === "&&" && invocations.slice(0, -1).some((other) => other.refusal !== undefined);
    if (!skipped) last.refusal ??= output;
  }
  for (const invocation of invocations)
    invocation.succeeded =
      outcome !== undefined &&
      invocation.refusal === undefined &&
      !(outcome.failed && invocation === last);
  return operations.sort((a, b) => a.at - b.at).map(({ operation }) => operation);
}
/** A reference to an issue or to one of its documents, as `--ref` takes it: `dispatch://<issue>`
 * or a dashboard URL (`…/issues/<issue>`), then `/spec` or `/artifact/<id>` (`/artifacts/<id>` in
 * a URL). It gives the issue, and the document: `spec`, or the artifact's id. */
const ISSUE_REF =
  /^(?:dispatch:\/\/|https?:\/\/[^/\s]+\/issues\/)([A-Z][A-Z0-9]{1,9}-[1-9]\d*)(?:\/(spec)|\/artifacts?\/([^/@?#\s]+)(?:@v\d+)?)?(?:[?#]\S*)?$/;
/** Whether one `dispatch` command wrote the run's primary spec: what it names, and the line of
 * Dispatch's answer operationsOf attributed to it (`answer`). The answer, not the exit, says
 * whether Dispatch made a version, so a command the CLI refused (`was not called`), an edit that
 * changed nothing (`no new version`) and a failed command give none, and a shell that exits
 * non-zero after the write does not take it back. A `--help` or `--dry-run` command writes nothing.
 * - `dispatch issue` with a spec, answered with the run's issue made (`Created <issue>:`).
 * - `dispatch doc-edit` of the run's issue (`--issue`, or the issue of a `--ref`) and of its spec
 *   (`--artifact`, or the document of the `--ref`; no document named is the issue's primary
 *   document, as the CLI resolves it), answered `Applied <n> ops (version <v>)`.
 * - `dispatch artifact` of the run's issue and of the spec's file, answered
 *   `Uploaded <name> as version <v>`.
 * A command of another issue, another document or a project's document writes no spec. */
function writesSpec(call: DispatchCall, fixture: SpecFixture, answer: string): boolean {
  if (call.flag("help") !== undefined || call.flag("dry-run") !== undefined) return false;
  const isSpec = (name: string | undefined) =>
    name !== undefined &&
    ["spec", "spec.md", "spec-md", fixture.spec_artifact.toLowerCase()].includes(
      name.toLowerCase()
    );
  if (call.command === "issue")
    return (
      (call.flag("spec") !== undefined || call.flag("spec-file") !== undefined) &&
      answer.startsWith(`Created ${fixture.issue}: `)
    );
  if (call.command === "doc-edit") {
    const named = ISSUE_REF.exec(call.flag("ref") ?? "");
    const document = call.flag("artifact") ?? named?.[2] ?? named?.[3];
    return (
      (named?.[1] ?? call.flag("issue")) === fixture.issue &&
      (document === undefined || isSpec(document)) &&
      /^Applied \d+ ops[^\n]*\(version \d+\)/.test(answer)
    );
  }
  if (call.command === "artifact")
    return (
      call.flag("issue") === fixture.issue &&
      isSpec(call.flag("name")) &&
      /^Uploaded \S+ as version \d+/.test(answer)
    );
  return false;
}
/** Every operation of a transcript's tool calls, in order: each call's (operationsOf), in the
 * order the calls were made. */
function operations(entries: SessionEntry[]): Operation[] {
  const results = outcomes(entries);
  return steps(entries).flatMap((step) => operationsOf(step, results.get(step.id)));
}
/** Whether a skill file read is skill `name` itself. */
const readsSkill = (target: string, name: string) =>
  new RegExp(`^skill://${escaped(name)}(/SKILL\\.md)?$|/skills/${escaped(name)}/SKILL\\.md$`).test(
    target
  );

/** A spec-only check: a `task` call made before a spec write, its assignment (every string of its
 * arguments), its report and when the last part of the report arrived. `write` is the place of
 * the spec write that follows it, in SpecWork.writes. */
export interface SpecCheck {
  assignment: string;
  report: string;
  reportAt: string;
  write: number;
}
export interface SpecWork {
  /** Each spec write the tool accepted, in order, and whether a spec-only check came after the
   * write before it (or the session's start) and before it. */
  writes: { index: number; at: string; text: string; checked: boolean }[];
  checks: SpecCheck[];
}
/** A task result's report: the `<task-result id="…">` blocks in the call's own result and, for a
 * task that runs in the background, in each later message that delivers one of its tasks; else the
 * outputs the result's `details` keep; else the result's text. */
function checkReport(
  entries: SessionEntry[],
  step: Step,
  own: Outcome | undefined
): { report: string; reportAt: string } {
  const block = /<task-result\b[^>]*?\bid="([^"]+)"[^>]*>([\s\S]*?)<\/task-result>/g;
  const tasks = Array.isArray(step.args.tasks) ? step.args.tasks : [];
  const names = new Set<string>(
    tasks.flatMap((task) =>
      typeof task === "object" && task !== null && "name" in task && typeof task.name === "string"
        ? [task.name]
        : []
    )
  );
  const text = own?.text ?? "";
  for (const [, name] of text.matchAll(/`([^`\s]+)` \(job/g)) if (name) names.add(name);
  const parts: string[] = [];
  let reportAt = own?.at || step.at;
  for (const [, name, body] of text.matchAll(block)) {
    if (name) names.add(name);
    parts.push((body ?? "").trim());
  }
  const details = own?.details;
  if (parts.length === 0 && typeof details === "object" && details !== null && "results" in details)
    for (const result of Array.isArray(details.results) ? details.results : [])
      if (typeof result === "object" && result !== null && "output" in result)
        if (typeof result.output === "string") parts.push(result.output.trim());
  for (const [index, entry] of entries.entries()) {
    if (index <= step.index || index === own?.index) continue;
    const delivered =
      entry.type === "custom_message"
        ? strings(entry.content).join("")
        : entry.message?.role === "toolResult"
          ? (entry.message.content ?? []).map((part) => part.text ?? "").join("")
          : "";
    for (const [, name, body] of delivered.matchAll(block)) {
      const report = (body ?? "").trim();
      if (!names.has(name ?? "") || parts.includes(report)) continue;
      parts.push(report);
      reportAt = entry.timestamp ?? reportAt;
    }
  }
  return { report: parts.length > 0 ? parts.join("\n\n") : text, reportAt };
}
/** The spec writes of a transcript (writesSpec, against the run's fixture.json) and the spec-only
 * checks before them. A check is a `task` call whose assignment holds the seven outline headings as
 * `##` lines and names no path under a checkout in `reach`; a check counts for the first spec write
 * after it. Each `dispatch` command of a call is a write of its own (operationsOf), so two writes
 * in one call with no check between them leave the second unchecked. */
export function specWork(entries: SessionEntry[], reach: string[], fixture: SpecFixture): SpecWork {
  const results = outcomes(entries);
  const writes: SpecWork["writes"] = [];
  const checks: SpecCheck[] = [];
  let pending: { step: Step; assignment: string }[] = [];
  for (const step of steps(entries)) {
    if (step.tool === "task") {
      const assignment = strings(step.args).join("\n\n");
      const outlined = OUTLINE.every((heading) =>
        new RegExp(`^[ \\t]*## ${escaped(heading)}[ \\t]*$`, "m").test(assignment)
      );
      const local = reach.some((dir) => new RegExp(`${escaped(dir)}(?![\\w.-])`).test(assignment));
      if (outlined && !local) pending.push({ step, assignment });
      continue;
    }
    for (const operation of operationsOf(step, results.get(step.id))) {
      if (operation.kind !== "dispatch" || !writesSpec(operation.call, fixture, operation.answer))
        continue;
      for (const check of pending)
        checks.push({
          assignment: check.assignment,
          ...checkReport(entries, check.step, results.get(check.step.id)),
          write: writes.length,
        });
      writes.push({
        index: step.index,
        at: step.at,
        text: operation.call.text,
        checked: pending.length > 0,
      });
      pending = [];
    }
  }
  return { writes, checks };
}

/** Each `**R<n>.**` or `**A<n>.**` paragraph of a spec: its mark and its text, up to a blank line,
 * a heading or the next list item. */
function marked(spec: string): { mark: string; body: string }[] {
  const found: { mark: string; body: string }[] = [];
  let current: { mark: string; body: string } | undefined;
  for (const line of spec.split("\n")) {
    const mark = /^\s*(?:[-*+]\s+|\d+[.)]\s+)?\*\*([RA]\d+)\.\*\*/.exec(line)?.[1];
    if (mark !== undefined) {
      current = { mark, body: line };
      found.push(current);
    } else if (
      current === undefined ||
      line.trim() === "" ||
      /^\s*(#|[-*+]\s|\d+[.)]\s)/.test(line)
    )
      current = undefined;
    else current.body += `\n${line}`;
  }
  return found;
}
/** The requirement marks of a spec (`R1`, `R2`, …), in order. */
export function requirements(spec: string): string[] {
  return marked(withoutFences(spec))
    .map((paragraph) => paragraph.mark)
    .filter((mark) => mark.startsWith("R"));
}
/** Replaces each match of `pattern` with spaces and keeps the line breaks, so an offset into the
 * result still points into the text. */
function blank(text: string, pattern: RegExp): string {
  return text.replace(pattern, (hit) => hit.replace(/[^\n]/g, " "));
}
const withoutFences = (text: string) => blank(text, /^(```|~~~)[^\n]*\n[\s\S]*?^\1[^\n]*$/gm);
/** outline: what keeps a spec from the outline. Its `##` headings are exactly OUTLINE's, in order.
 * It holds a requirement and a check. Each `**R<n>.**` paragraph names its source after `From:`,
 * and each `**A<n>.**` paragraph names at least one `R<n>`. */
function outlineProblems(spec: string): string[] {
  const text = withoutFences(spec);
  const headings = [...text.matchAll(/^##[ \t]+(.+?)[ \t#]*$/gm)].map((hit) => hit[1] ?? "");
  const problems: string[] = [];
  if (headings.join("\n") !== OUTLINE.join("\n"))
    problems.push(
      `headings are ${headings.map((heading) => `## ${heading}`).join(", ") || "none"}`
    );
  const paragraphs = marked(text);
  const reqs = paragraphs.filter((paragraph) => paragraph.mark.startsWith("R"));
  const checks = paragraphs.filter((paragraph) => paragraph.mark.startsWith("A"));
  if (reqs.length === 0) problems.push("no **R<n>.** requirement");
  if (checks.length === 0) problems.push("no **A<n>.** check");
  for (const req of reqs)
    if (!/From:\s*\S/.test(req.body)) problems.push(`${req.mark} names no source (From:)`);
  for (const check of checks)
    if (!/\bR\d+\b/.test(check.body)) problems.push(`${check.mark} names no requirement`);
  return problems;
}

/** A text the `quoted` score compares: `display` is what a reader sees of it, and `compare` the
 * same text with what the score leaves out blanked. */
interface Piece {
  source: string;
  display: string;
  compare: string;
}
/** What `quoted` leaves out of a text: fenced code, code spans, links and images whole, URLs and
 * dispatch:// references, HTML comments, character references, and typed-block lines, whose
 * attributes Dispatch writes (an answered ask's line holds the person's answer). */
function piece(source: string, display: string): Piece {
  let compare = withoutFences(display);
  for (const pattern of [
    /``[^`]*``|`[^`\n]*`/g,
    /!?\[[^\]\n]*\]\([^)\n]*\)/g,
    /<[a-z][a-z0-9+.-]*:[^>\s]*>/gi,
    /\b[a-z][a-z0-9+.-]*:\/\/\S+/gi,
    /<!--[\s\S]*?-->/g,
    /&#?\w+;/g,
    /^:::[\w-]*(\{.*\})?[ \t]*$/gm,
  ])
    compare = blank(compare, pattern);
  return { source, display, compare };
}
interface Word {
  word: string;
  piece: number;
  start: number;
  end: number;
}
/** The words of pieces, with case and punctuation folded: letters and digits, an apostrophe inside
 * a word dropped. A line break is a space like any other. */
function wordsOf(pieces: Piece[]): Word[] {
  return pieces.flatMap((text, index) =>
    [...text.compare.matchAll(/[\p{L}\p{N}]+(?:['’][\p{L}\p{N}]+)*/gu)].map((hit) => ({
      word: hit[0].toLowerCase().replace(/['’]/g, ""),
      piece: index,
      start: hit.index ?? 0,
      end: (hit.index ?? 0) + hit[0].length,
    }))
  );
}
/** The sentences of `display` that hold [from, to), with one sentence on each side. */
function context(display: string, from: number, to: number): string {
  const bounds = [
    0,
    ...[...display.matchAll(/[.!?]+["')\]*_]*\s+|\n+/g)].map(
      (hit) => (hit.index ?? 0) + hit[0].length
    ),
    display.length,
  ];
  const sentences = bounds
    .slice(0, -1)
    .map((start, index) => ({ start, end: bounds[index + 1] ?? display.length }))
    .filter((sentence) => display.slice(sentence.start, sentence.end).trim() !== "");
  const first = Math.max(
    0,
    sentences.findIndex((sentence) => sentence.end > from)
  );
  const last = sentences.findLastIndex((sentence) => sentence.start < to);
  const begin = sentences[Math.max(0, first - 1)]?.start ?? 0;
  const end = sentences[Math.min(sentences.length - 1, last + 1)]?.end ?? display.length;
  return display.slice(begin, end).replace(/\s+/g, " ").trim();
}
/** Where `words` sit in `pieces`: each piece's sentences that hold them, with one on each side. */
function contextOf(pieces: Piece[], words: Word[]): string {
  return [...new Set(words.map((word) => word.piece))]
    .map((index) => {
      const inPiece = words.filter((word) => word.piece === index);
      return context(
        pieces[index]?.display ?? "",
        inPiece[0]?.start ?? 0,
        inPiece.at(-1)?.end ?? 0
      );
    })
    .join(" … ");
}

/** One shared run: the words a text of the run and the person's input both hold, where the text
 * is, and the context of the run in the text and in the input. */
export interface SharedRun {
  source: string;
  run: string;
  words: number;
  text: string;
  input: string;
}
/** The `>` lines fetch-spec-sources.ts writes for an anchored comment's quote: the lines right
 * after a `<!-- comment <time> anchored -->` header, up to its first line that is not a `>` line.
 * The quote is the old spec's text, not the person's. Every other `>` line of messages.md is the
 * person's own: a block quote in a message, one after a comment's quote and body, or one an
 * unanchored comment starts with. */
const ANCHORED_QUOTE = /^(<!-- comment \S+ anchored -->[ \t]*\n)((?:>[^\n]*(?:\n|$))+)/gm;
/** Every entry header of messages.md (fetch-spec-sources.ts's ENTRY_HEADER), which is no input. */
const ENTRY_HEADERS = new RegExp(ENTRY_HEADER.source, "gm");
/** The shared runs the `quoted` score finds. The run's texts are spec.md, each text of
 * agent-texts.json (seeded asks left out) and each file of documents/. The person's input is
 * source/messages.md less its entry headers and each comment's anchored quote (ANCHORED_QUOTE),
 * and the text of each answer in source/asks.json. A shared run is a longest stretch of RUN_WORDS
 * words or more that a text and the input both hold, so a copied paragraph is one run: a stretch
 * inside a longer one, or a repeat of one, is not a run of its own. */
export function sharedRuns(runDir: string): SharedRun[] {
  const seeded = seededIds(runDir);
  const messages = readFileSync(path.join(runDir, "source", "messages.md"), "utf8");
  const quotesBlanked = messages.replace(
    ANCHORED_QUOTE,
    (_, header: string, quote: string) => header + quote.replace(/[^\n]/g, " ")
  );
  const input = [
    piece("messages.md", blank(quotesBlanked, ENTRY_HEADERS)),
    ...json(path.join(runDir, "source", "asks.json"), SourceAsks).flatMap((ask, index) =>
      ask.answer?.text ? [piece(`asks.json#${index}`, ask.answer.text)] : []
    ),
  ];
  const documents = path.join(runDir, "documents");
  const texts = [
    piece("spec.md", readFileSync(path.join(runDir, "spec.md"), "utf8")),
    ...json(path.join(runDir, "agent-texts.json"), AgentTexts)
      .filter((text) => !seeded.has(text.id))
      .map((text) => piece(`agent-texts.json#${text.id}`, text.body)),
    ...(existsSync(documents) ? readdirSync(documents).sort() : []).map((file) =>
      piece(`documents/${file}`, readFileSync(path.join(documents, file), "utf8"))
    ),
  ];
  const held = wordsOf(input);
  const grams = new Map<string, number[]>();
  const gram = (words: Word[], at: number) =>
    words
      .slice(at, at + RUN_WORDS)
      .map((word) => word.word)
      .join(" ");
  for (let at = 0; at + RUN_WORDS <= held.length; at++)
    grams.set(gram(held, at), [...(grams.get(gram(held, at)) ?? []), at]);
  const runs: SharedRun[] = [];
  for (const text of texts) {
    const words = wordsOf([text]);
    const spans: { start: number; end: number; from: number }[] = [];
    for (let at = 0; at + RUN_WORDS <= words.length; at++)
      for (const from of grams.get(gram(words, at)) ?? []) {
        // A stretch that the word before both continues starts earlier.
        if (at > 0 && from > 0 && words[at - 1]?.word === held[from - 1]?.word) continue;
        let length = RUN_WORDS;
        while (
          words[at + length] !== undefined &&
          words[at + length]?.word === held[from + length]?.word
        )
          length++;
        spans.push({ start: at, end: at + length, from });
      }
    const longest = spans.filter(
      (span, index) =>
        !spans.some(
          (other, o) =>
            other.start <= span.start &&
            other.end >= span.end &&
            (other.end - other.start > span.end - span.start || o < index)
        )
    );
    for (const span of longest.sort((a, b) => a.start - b.start)) {
      const own = words.slice(span.start, span.end);
      runs.push({
        source: text.source,
        run: own.map((word) => word.word).join(" "),
        words: own.length,
        text: contextOf([text], own),
        input: contextOf(input, held.slice(span.from, span.from + own.length)),
      });
    }
  }
  return runs;
}

/** A judge's answer in the run directory, as judge.ts wrote it; a file missing or malformed is a
 * rig error that `rig.sh judge` fixes. */
function judged<T>(runDir: string, file: string, schema: z.ZodType<T>): T {
  const full = path.join(runDir, file);
  if (!existsSync(full)) throw new Error(`${file} is missing: run rig.sh judge ${runDir}`);
  const result = schema.safeParse(JSON.parse(readFileSync(full, "utf8")));
  if (!result.success)
    throw new Error(`${file} is not a judge's answer: ${z.prettifyError(result.error)}`);
  return result.data;
}
/** Each count of `values` in `kinds`, as `kind:n,…`. */
const tally = (kinds: readonly string[], values: string[]) =>
  kinds.map((kind) => `${kind}:${values.filter((value) => value === kind).length}`).join(",");
/** Lines under a row, each indented past the row's own notes. */
const indented = (text: string, depth = 6) =>
  text
    .split("\n")
    .map((line) => `${" ".repeat(depth)}${line}`)
    .join("\n");

/** spec-writing and spec-rewrite: one row per run, with the eight scores below.
 * The row passes when its eight scores pass; `judged` gives the judges' counts, which pass or fail
 * nothing here: the acceptance run of the skills that replace dispatch-brainstorming holds each
 * count to its own bound.
 *   - outline: outlineProblems finds nothing.
 *   - checked: the transcript holds at least one spec write (writesSpec: a write Dispatch made of
 *     the run's own primary spec, each `dispatch` command of a call apart), a spec-only check
 *     (specWork) came before every one, the first included, and findings.json has no `neither`
 *     row.
 *   - refusals: each refused `dispatch` command (operationsOf: the refusal at the start of its own
 *     output, or the call's failure with exit 1 or 2 when the command ends the call) is followed
 *     by a `dispatch` command that succeeded in a later tool call, with no skill file read between
 *     them, in the order of each call's commands and reads. A command of the refused one's own call
 *     is no fix: the agent wrote it before it saw the refusal, so it cannot show that the agent
 *     fixed the call from the refusal alone. Each failing pair is printed with its refusal text.
 *   - plan: the person approved the spec, the issue holds plan.md (capture-spec runs after the
 *     approval), and the transcript read skill://writing-plans.
 *   - routed: the transcript read skill://dispatch-writing-specs before its first spec write, and
 *     never read REMOVED_SKILL. `ref` is the first half.
 *   - chat: no call of the interactive `ask` tool (surfaceOf's chat), and every state-<turn>.json
 *     but the last lists an open approval request, or an open ask whose id is not in seeded.json.
 *   - quoted: every run sharedRuns finds has the verdict `names` in shared-runs.json. Each run is
 *     printed with its context in the text and in the input.
 *   - self: spec.md and each text of agent-texts.json hold neither the session id the
 *     transcript's header names nor its first 8 characters.
 *   - judged: faults.json's faults and STE breaks; decisions.json's choices not recorded, or
 *     recorded without a reason, the options or the parts; findings.json's findings and `neither`
 *     rows; checkable.json (`checkable`: the approved version's every requirement has a check);
 *     questions.json's `dominated`, `settled` and kinds; numbers.json's `names_number`;
 *     preamble.json's passages by kind; proposal.json's verdicts. Each row whose id is in
 *     seeded.json is left out. */
function specRow(
  runDir: string,
  run: string,
  scenario: string,
  label: string,
  reach: string[]
): Row {
  const entries = session(runDir);
  const seeded = seededIds(runDir);
  const spec = readFileSync(path.join(runDir, "spec.md"), "utf8");
  const source = readFileSync(path.join(runDir, "source.txt"), "utf8").trim();
  const fixture = json(path.join(runDir, "fixture.json"), SpecFixture);
  const { approved, version, turns } = approval(runDir);
  const all = steps(entries);
  const work = specWork(entries, reach, fixture);
  const ops = operations(entries);
  // Places in `ops`, so a read and a write of one call keep their order.
  const firstWrite = ops.findIndex(
    (op) => op.kind === "dispatch" && writesSpec(op.call, fixture, op.answer)
  );
  const reads = ops.flatMap((op, place) =>
    op.kind === "read" ? [{ place, target: op.target }] : []
  );
  const details: string[] = [];

  const outline = outlineProblems(spec);
  details.push(...outline.map((problem) => `outline: ${problem}`));

  const findings = judged(runDir, "findings.json", FindingsOutput);
  const neither = findings.rows.filter((row) => row.verdict === "neither");
  const unchecked = work.writes.filter((write) => !write.checked);
  const checked = work.writes.length > 0 && unchecked.length === 0 && neither.length === 0;
  if (work.writes.length === 0) details.push("checked: the transcript holds no spec write");
  if (unchecked.length > 0)
    details.push(
      `unchecked writes: ${unchecked.length} of ${work.writes.length}`,
      ...unchecked.map((write) => indented(write.text, 2))
    );
  for (const row of neither)
    details.push(`finding of check ${row.check} neither fixed nor recorded: ${row.finding}`);

  const refused: string[] = [];
  for (const [place, op] of ops.entries()) {
    if (op.kind !== "dispatch" || op.refusal === undefined) continue;
    let read: string | undefined;
    let fixed = false;
    for (const next of ops.slice(place + 1)) {
      if (next.kind === "read") read ??= next.target;
      else if (next.succeeded && next.step !== op.step) {
        fixed = true;
        break;
      }
    }
    if (fixed && read === undefined) continue;
    refused.push(
      [
        `refused call: ${op.call.text}`,
        indented(op.refusal, 4),
        fixed
          ? `    then read ${read} before the next dispatch call succeeded`
          : "    no later dispatch call succeeded",
      ].join("\n")
    );
  }
  details.push(...refused);

  const planned = reads.some((read) => readsSkill(read.target, "writing-plans"));
  const plan = approved && existsSync(path.join(runDir, "plan.md")) && planned;
  const routedRead = reads.some(
    (read) =>
      (firstWrite < 0 || read.place < firstWrite) &&
      readsSkill(read.target, "dispatch-writing-specs")
  );
  const removed = reads.filter(
    (read) =>
      read.target.startsWith(`skill://${REMOVED_SKILL}`) ||
      read.target.includes(`/skills/${REMOVED_SKILL}/`)
  );
  const routed = routedRead && removed.length === 0;
  details.push(...removed.map((read) => `routed: read ${read.target}`));

  const chatAsks = all.filter((step) => step.tool === "ask").length;
  const idle = states(runDir)
    .slice(0, -1)
    .filter(
      (state) => state.approval === null && !state.open_asks.some((ask) => !seeded.has(ask.id))
    )
    .map((state) => state.turn);
  const chat = chatAsks === 0 && idle.length === 0;
  if (chatAsks > 0) details.push(`chat: ${chatAsks} calls of the interactive ask tool`);
  if (idle.length > 0) details.push(`turns with nothing open for the person: ${idle.join(", ")}`);

  const shared = sharedRuns(runDir);
  const verdicts = judged(runDir, "shared-runs.json", SharedRunsFile).rows;
  const key = (row: { source: string; run: string }) => `${row.source}\n${row.run}`;
  if (
    verdicts.length !== shared.length ||
    shared.some((found) => !verdicts.some((row) => key(row) === key(found)))
  )
    throw new Error(
      `shared-runs.json judges ${verdicts.length} runs, and sharedRuns finds ${shared.length} others: run rig.sh judge ${runDir}`
    );
  let copies = 0;
  for (const [index, found] of shared.entries()) {
    const verdict = verdicts.find((row) => key(row) === key(found));
    if (verdict?.verdict === "copies") copies++;
    details.push(
      `shared run ${index + 1} in ${found.source} (${found.words} words, ${verdict?.verdict}: ${verdict?.reason}): ${found.run}`,
      `    text: ${found.text}`,
      `    input: ${found.input}`
    );
  }
  const quoted = copies === 0;

  const id = sessionId(entries);
  if (id === undefined) throw new Error("the transcript has no session header");
  const selfTexts = [
    { source: "spec.md", body: spec },
    ...json(path.join(runDir, "agent-texts.json"), AgentTexts)
      .filter((text) => !seeded.has(text.id))
      .map((text) => ({ source: `agent-texts.json#${text.id}`, body: text.body })),
  ].filter((text) => text.body.toLowerCase().includes(id.slice(0, 8).toLowerCase()));
  const self = selfTexts.length === 0;
  details.push(...selfTexts.map((text) => `self: ${text.source} names the session`));

  const faults = judged(runDir, "faults.json", FaultsOutput);
  const faultCount = Object.values(faults.faults).reduce((sum, n) => sum + n, 0);
  const decisions = judged(runDir, "decisions.json", DecisionsOutput).rows;
  const checkable = judged(runDir, "checkable.json", CheckableFile);
  if (approved && checkable.version !== version)
    throw new Error(
      `checkable.json judged version ${checkable.version}, and the person approved version ${version}: run rig.sh judge ${runDir}`
    );
  const uncheckable = checkable.rows.filter((row) => row.uncheckable);
  for (const row of uncheckable) details.push(`uncheckable ${row.requirement}: ${row.check}`);
  const asks = judged(runDir, "questions.json", QuestionsOutput).rows.filter(
    (row) => !seeded.has(row.id)
  );
  const texts = judged(runDir, "numbers.json", NumbersOutput).rows.filter(
    (row) => !seeded.has(row.id)
  );
  const passages = judged(runDir, "preamble.json", PreambleOutput).rows.filter(
    (row) => !seeded.has(/^agent-texts\.json#(.+)$/.exec(row.source)?.[1] ?? "")
  );
  for (const row of passages) details.push(`preamble ${row.kind} in ${row.source}: ${row.passage}`);
  const proposals = judged(runDir, "proposal.json", ProposalOutput).rows.filter(
    (row) => !seeded.has(row.ask ?? "")
  );

  const scores = {
    outline: outline.length === 0,
    checked,
    refusals: refused.length === 0,
    plan,
    routed,
    chat,
    quoted,
    self,
  };
  const notes = [
    `source=${source} turns=${turns} approved=${approved}`,
    ...Object.entries(scores).map(([name, ok]) => `${name}=${ok}`),
    `faults=${faultCount} ste_breaks=${faults.ste_breaks}`,
    `decisions=${decisions.length} unrecorded=${decisions.filter((row) => !row.recorded).length}`,
    `incomplete=${decisions.filter((row) => row.recorded && !(row.reason && row.options && row.parts)).length}`,
    `findings=${findings.rows.length} neither=${neither.length}`,
    `checkable=${approved && checkable.rows.length > 0 && uncheckable.length === 0} uncheckable=${uncheckable.length}`,
    `asks=${asks.length} dominated=${asks.filter((row) => row.dominated).length}`,
    `settled=${asks.filter((row) => row.settled).length}`,
    `kinds=${tally(
      QUESTION_KINDS,
      asks.map((row) => row.kind)
    )}`,
    `texts=${texts.length} names_number=${texts.filter((row) => row.names_number).length}`,
    `preamble=${tally(
      ["retold", "dated", "self"],
      passages.map((row) => row.kind)
    )}`,
    `proposal=${tally(
      ["proposed", "asked", "missing"],
      proposals.map((row) => row.verdict)
    )}`,
    `writes=${work.writes.length} shared=${shared.length} copies=${copies}`,
  ].join(" ");
  return {
    run,
    scenario,
    label,
    pass: Object.values(scores).every(Boolean),
    ref: routedRead,
    notes: [notes, ...details.map((line) => `  ${line}`)].join("\n"),
    source,
    faults: faultCount,
    steBreaks: faults.ste_breaks,
  };
}

/** The median of numbers: the middle one, or the mean of the two middle ones. */
function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1
    ? (sorted[middle] ?? 0)
    : ((sorted[middle - 1] ?? 0) + (sorted[middle] ?? 0)) / 2;
}

/** Why the rig cannot score a run, or undefined when it can:
 *   - it never finished: out.txt has no `exit=` line;
 *   - its agent got no model turn: it left no transcript, or one with no assistant message, as when
 *     the profile's gateway key command ran past its budget. The key command's own record of each
 *     call, which scripts/e2e/lib/model-gateway-unserved.sh reads, is a better signal this rig does
 *     not set up;
 *   - it compared the wrong text: a tool call's arguments name the other label's checkout, or a
 *     skill file (a path to skills/dispatch, skills/dispatch-first, skills/dispatch-brainstorming
 *     or skills/legion-worker)
 *     anywhere but its own run directory (its HOME is there) or its label's profile or checkout.
 *     An agent reads the whole filesystem, and `legion` on its PATH resolves into the checkout
 *     rig.sh runs from.
 * A record missing or malformed is a rig error too, thrown by the scenario's function. */
function unscored(runDir: string, label: string, labels: Map<string, string>): string | undefined {
  if (!lines(path.join(runDir, "out.txt")).at(-1)?.startsWith("exit="))
    return "out.txt has no exit= line: the run never finished";
  const entries = session(runDir);
  if (!entries.some((entry) => entry.message?.role === "assistant"))
    return "the agent got no model turn: no transcript, or one with no assistant message";
  const profile = path.join(path.dirname(path.dirname(runDir)), "profiles", label);
  const allowed = [runDir, profile, labels.get(label) ?? profile].map((dir) => `${dir}/`);
  for (const call of toolCalls(entries)) {
    for (const [other, checkout] of labels)
      if (other !== label && call.args.includes(checkout))
        return `a ${call.tool} call names ${other}'s checkout: ${call.args.slice(0, 200)}`;
    for (const [skillPath] of call.args.matchAll(
      /[^\s"'`=:;|&<>()]*skills\/(?:dispatch-first|dispatch-brainstorming|dispatch|legion-worker)\b/g
    ))
      if (!allowed.some((dir) => skillPath.startsWith(dir)))
        return `a ${call.tool} call names a skill outside ${label}'s own: ${skillPath}`;
  }
  return undefined;
}

function scoreRuns(runsDir: string, only: string | undefined) {
  const rows: Row[] = [];
  const errors: { run: string; key: string; reason: string }[] = [];
  // Each label's checkout, as `rig.sh profile` recorded it.
  const profiles = path.join(path.dirname(runsDir), "profiles");
  const labels = new Map(
    (existsSync(profiles) ? readdirSync(profiles) : [])
      .filter((label) => existsSync(path.join(profiles, label, "checkout")))
      .map((label) => [label, readFileSync(path.join(profiles, label, "checkout"), "utf8").trim()])
  );
  // Every tester-proof run's PR head, to tell another run's line from a wrong one. A world.json
  // that does not parse is its own run's rig error, below.
  const heads = (existsSync(runsDir) ? readdirSync(runsDir) : []).flatMap((run) => {
    try {
      return [json(path.join(runsDir, run, "world.json"), World).head];
    } catch {
      return [];
    }
  });
  // Every checkout a spec run's agent can reach, so that a check naming one is no spec-only check.
  const reach = checkouts(path.dirname(runsDir));
  // Each scenario's scorer, keyed by the name its runs' directories start with.
  const scorers: Record<string, (dir: string, run: string, label: string) => Row> = {
    "ask-on-message": askOnMessage,
    "measure-before-ask": measureBeforeAsk,
    "tester-proof": (dir, run, label) => testerProof(dir, run, label, heads),
    brainstorm: brainstormSurface,
    "spec-writing": (dir, run, label) => specRow(dir, run, "spec-writing", label, reach),
    "spec-rewrite": (dir, run, label) => specRow(dir, run, "spec-rewrite", label, reach),
  };
  const name = new RegExp(`^(${Object.keys(scorers).join("|")})-(.+)-(\\d+)$`);
  for (const run of existsSync(runsDir) ? readdirSync(runsDir).sort() : []) {
    const match = name.exec(run);
    const score = scorers[match?.[1] ?? ""];
    if (!match || !score) continue;
    const [, scenario = "", label = ""] = match;
    if (only && scenario !== only) continue;
    const dir = path.join(runsDir, run);
    const key = `${scenario}\t${label}`;
    let reason: string | undefined;
    try {
      reason = unscored(dir, label, labels);
      if (reason === undefined) rows.push(score(dir, run, label));
    } catch (error) {
      reason = error instanceof Error ? error.message : String(error);
    }
    if (reason !== undefined) errors.push({ run, key, reason });
  }
  for (const row of rows) console.log(`${row.run}\tpass=${row.pass}\tref=${row.ref}\t${row.notes}`);
  for (const error of errors) console.log(`${error.run}\trig error: ${error.reason}`);
  console.log("\nscenario\tlabel\tpass\tpass+ref\trig errors");
  const keys = [
    ...new Set([
      ...rows.map((row) => `${row.scenario}\t${row.label}`),
      ...errors.map((e) => e.key),
    ]),
  ];
  for (const key of keys) {
    const group = rows.filter((row) => `${row.scenario}\t${row.label}` === key);
    const passed = group.filter((row) => row.pass);
    const withRef = passed.filter((row) => row.ref);
    const failed = errors.filter((error) => error.key === key).length;
    console.log(
      `${key}\t${passed.length}/${group.length}\t${withRef.length}/${group.length}\t${failed}`
    );
  }
  // For each source and label, the median fault count of its spec runs (faults.json): the count
  // that compares two versions of the skills on the same source.
  const specKeys = [
    ...new Set(
      rows.flatMap((row) =>
        row.source === undefined ? [] : [`${row.source}\t${row.scenario}\t${row.label}`]
      )
    ),
  ];
  if (specKeys.length > 0) {
    console.log("\nsource\tscenario\tlabel\truns\tmedian faults\tmedian ste breaks");
    for (const key of specKeys) {
      const group = rows.filter((row) => `${row.source}\t${row.scenario}\t${row.label}` === key);
      console.log(
        `${key}\t${group.length}\t${median(group.map((row) => row.faults ?? 0))}\t${median(group.map((row) => row.steBreaks ?? 0))}`
      );
    }
  }
  console.log(
    [
      "",
      "pass       tester-proof: the four conditions on testerProof. ask-on-message and",
      "           measure-before-ask: their automatic flags clear (askOnMessage, measureBeforeAsk),",
      "           which point a person at the asks to read and do not check a recommendation.",
      "           brainstorm: the design went to the issue's spec and nothing leaked the project",
      "           lookup (brainstormSurface); a person still reads notes for unagreed points.",
      "           spec-writing and spec-rewrite: the eight scores of specRow pass (outline,",
      "           checked, refusals, plan, routed, chat, quoted, self); the judges' counts follow",
      "           them, and the source table gives each source and label's median fault count",
      "pass+ref   passed, and the agent read the file the rule is in",
      "rig errors runs the rig could not score (unscored), left out of both counts",
      "ran=false  a tester-proof run the bun stand-in saw no greet.ts run in: read its transcript",
      "           before believing it, since the stand-in sees only a bun reached through PATH",
    ].join("\n")
  );
}

// judge.ts imports sharedRuns from this file: the command line runs only when bun runs score.ts.
if (import.meta.main) {
  const [command, ...rest] = Bun.argv.slice(2);
  if (command === "live-read" && rest.length === 3) {
    liveRead(rest[0] ?? "", rest[1] ?? "", rest[2] ?? "");
  } else if (command === "runs" && rest.length >= 1) {
    scoreRuns(rest[0] ?? "", rest[1]);
  } else {
    console.error(
      "usage: score.ts live-read <run dir> <skills dir> <omp logs dir> | runs <runs dir> [<scenario>]"
    );
    process.exit(2);
  }
}
