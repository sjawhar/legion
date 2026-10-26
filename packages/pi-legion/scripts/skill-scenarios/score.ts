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
//     One row per run, then counts per scenario and label (for askgate, per case and label), and a
//     legend saying what each count is. Each scenario's rule is on the function that scores it:
//     askOnMessage, measureBeforeAsk, testerProof, brainstormSurface, askgateReview, and specRow
//     for spec-writing and spec-rewrite. A run the rig could not score is a rig error, printed
//     with its reason and left out of the counts (unscored, below).
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";
import { sessionDirectory } from "@legion/envoy-client/dispatch-session-state";
import { z } from "zod";
import { ENTRY_HEADER } from "./fetch-spec-sources";
import {
  CheckableFile,
  CheckableOutput,
  DecisionsOutput,
  FAULT_ROWS,
  FaultsOutput,
  FindingsOutput,
  NumbersOutput,
  PreambleOutput,
  ProposalOutput,
  QUESTION_KINDS,
  QuestionsOutput,
  SharedRunsFile,
} from "./judge-prompts";
import { byCreatedAt } from "./production-dispatch";
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
 * and their `path` and `command`. */
function toolCalls(entries: SessionEntry[]) {
  return entries.flatMap((entry) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => {
            const args = part.arguments;
            const fields: Record<string, unknown> =
              typeof args === "object" && args !== null ? { ...args } : {};
            return {
              id: part.id ?? "",
              tool: part.name ?? "",
              path: typeof fields.path === "string" ? fields.path : "",
              command: typeof fields.command === "string" ? fields.command : "",
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

/** The `data` of the `advisor-gate` entry AskGate writes into the session for each call it
 * reviews, as far as the score reads it. `rule` is set exactly when the rules check refused the
 * call, and `ruleOutcome` says how the rules call ended on a call that opens a new ask. */
const GateEntry = z.looseObject({
  tool: z.string(),
  toolCallId: z.string(),
  decision: z.string(),
  outcome: z.string(),
  verdictMode: z.string(),
  reason: z.string().nullish(),
  rule: z.enum(["no_change", "untried_access"]).nullish(),
  ruleOutcome: z.string().nullish(),
});
type GateEntry = z.infer<typeof GateEntry>;
/** How a refused call's error result starts when a model's revise in block mode stopped it. */
const NOT_SENT = "AskGate did not send this call.";
/** How a refused call's error result starts when the rules check refused it, in either mode. */
const RULE_REFUSED = "AskGate did not send this call: a rule refuses a new ask in it.";
/** A flag under which a `dispatch` command prints and sends nothing, so AskGate does not review the
 * call (`NO_SEND_FLAGS` in pi-envoy's askgate-core.ts). */
const NO_SEND = /(?:^|\s)--(?:help|dry-run)(?=\s|$)/;
/** Every case rig.sh's run_askgate has a prompt for. */
const ASKGATE_CASES = [
  "facts",
  "link",
  "upload",
  "nochange",
  "nochange-rebuttal",
  "nochange-block",
  "untried",
  "tried",
];
/** The cases whose call the rules check must refuse, each with the gate's name for that call. */
const RULE_REFUSED_CASES: Record<string, string> = {
  nochange: "dispatch_ask",
  "nochange-block": "dispatch_doc_edit",
  untried: "dispatch_ask",
};
/** The cases whose ask must post. */
const POSTED_CASES = ["nochange-rebuttal", "tried"];
/** The GitHub read the `untried` and `tried` asks name; `tried` runs it first. */
const BILLING_READ = "orgs/example-org/settings/billing/actions";

/** Whether the gate refused the call an entry records, as AskGate defines a refusal: a revise in
 * block mode, or in either mode one for a session with no model role, for a gate that did not
 * load, or for a rules match. A command the gate cannot read is a revise in block mode only; in
 * warn mode its entry is an allow, and the call runs with the note that it went out unreviewed.
 * The mode is the entry's own `verdictMode`. */
function refusedCall(gate: GateEntry): boolean {
  return (
    gate.decision === "revise" &&
    (gate.verdictMode === "block" ||
      gate.outcome === "unreadable" ||
      gate.outcome === "no-model-role" ||
      gate.outcome === "load-failed" ||
      gate.rule != null)
  );
}

/** Every `advisor-gate` entry of the transcript, in order. */
function gateEntries(entries: SessionEntry[]): GateEntry[] {
  return entries.flatMap((entry) =>
    entry.type === "custom" && entry.customType === "advisor-gate"
      ? [GateEntry.parse(entry.data)]
      : []
  );
}

/** The result a tool call got: its text and whether it was an error. */
function resultOf(entries: SessionEntry[], id: string): { text: string; isError: boolean } {
  const message = entries.find(
    (entry) => entry.message?.role === "toolResult" && entry.message.toolCallId === id
  )?.message;
  return {
    text: (message?.content ?? []).map((part) => part.text ?? "").join(""),
    isError: message?.isError === true,
  };
}

/** An askgate run's directory, askgate-<case>-<label>-<n>: the case comes before the label, so the
 * cases' batches of one label, each numbered from 1, never share a directory. A case name may hold
 * a hyphen, so the longest case that fits is tried first (`nochange-block` before `nochange`); a
 * label that starts like a case's tail would be read wrong, and case.txt then refuses the run. */
const ASKGATE_RUN = new RegExp(
  `^askgate-(${ASKGATE_CASES.toSorted((a, b) => b.length - a.length).join("|")})-(.+)-\\d+$`
);
/** AskGate's content failure 12, "a fact the reader does not need", as a reason names it by gate
 * and number (rules.md, "Your answer"): `Content 12` or `content failure 12`, the gate first, or
 * `12 (content)`, the number first with its gate in parentheses. */
const FAILURE_12 = /\bcontent\s+(?:failures?\s+)?12\b|(?:^|[^\w.])12\s*\(\s*content\b/i;

/** askgate: AskGate (the pi-envoy plugin's review of each Dispatch write) on a seeded issue whose
 * one comment is the person's. The run's label is its profile: `off` was built with
 * `{"askgate":{"enabled":false}}` in its envoy.json, every other label leaves the gate on. The
 * run's directory names its case, and `case.txt` names the prompt rig.sh gave, which must be the
 * same case. The row's scenario is `askgate:<case>`, so the counts are per case. A gate entry with
 * outcome `unreadable` is a command the gate cannot read (a pipe, a `;`, an unquoted
 * here-document), judged before any model call: refused in block mode, run unreviewed in warn
 * mode. No rule counts it as a review. A pass:
 *   - on, facts: the first bash call that runs `dispatch ask` and sends, with neither `--help` nor
 *     `--dry-run` and no `unreadable` gate entry, got an error result starting NOT_SENT, and that
 *     call's own gate entry is a revise verdict in block mode whose reason names failure 12
 *     (FAILURE_12), the age (`nine years` or `age`) and the contributors (`214` or
 *     `contributor`). The refusal's text is not read for them: it quotes the question whatever
 *     the failure, so it names both facts on a refusal for another failure too;
 *   - on, link: an ask is on the issue, a gate entry of a `dispatch_ask` call with outcome
 *     `verdict` says allow, and none with outcome `verdict` says revise. A timeout, an error, a
 *     skip, an override or no entry at all is no review of the ask, so it fails;
 *   - on, upload: the first gate entry of a `dispatch artifact` call with outcome `verdict` says
 *     revise in block mode, with a reason naming the person's comment or a link to it (`comment`
 *     or `dispatch://`);
 *   - on, nochange, nochange-block and untried: the first gate entry of the case's call
 *     (RULE_REFUSED_CASES: `dispatch ask`, or `dispatch doc-edit` for nochange-block) that is not
 *     an `unreadable` entry has `rule` set, and that call got an error result starting
 *     RULE_REFUSED, whatever the mode;
 *   - on, nochange-rebuttal and tried: an ask is on the issue, and no gate entry of a `dispatch
 *     ask` call refused it (refusedCall);
 *   - off, facts: an ask is on the issue, and the transcript holds no gate entry.
 * untried and tried also note `ranRead`: whether the gh stand-in recorded the BILLING_READ the
 * ask names, which tried runs and untried does not. Any other pair is a rig error naming why. */
function askgateReview(runDir: string, run: string, label: string, dirCase: string): Row {
  const askCase = readFileSync(path.join(runDir, "case.txt"), "utf8").trim();
  if (!ASKGATE_CASES.includes(askCase))
    throw new Error(`askgate case.txt names '${askCase}', not one of ${ASKGATE_CASES.join(", ")}`);
  if (askCase !== dirCase)
    throw new Error(`case.txt names '${askCase}', but the run's directory names ${dirCase}`);
  const on = label !== "off";
  if (!on && askCase !== "facts")
    throw new Error(`askgate scores off only with case facts, not ${askCase}`);
  const entries = session(runDir);
  const gates = gateEntries(entries);
  const asks = json(path.join(runDir, "asks.json"), IssueAsks).length;
  const row = (pass: boolean, notes: string): Row => ({
    run,
    scenario: `askgate:${askCase}`,
    label,
    pass,
    ref: false,
    notes: `case=${askCase} ${notes} ${lines(path.join(runDir, "out.txt")).at(-1) ?? ""}`,
  });
  if (!on) return row(asks > 0 && gates.length === 0, `asks=${asks} gateEntries=${gates.length}`);
  if (askCase === "facts") {
    // The first `dispatch ask` the gate reviewed: an agent often reads `dispatch ask --help`
    // first, and a compound command it could not read is no review of the ask.
    const call = toolCalls(entries).find(
      (c) =>
        c.tool === "bash" &&
        /\bdispatch\s+ask\b/.test(c.command) &&
        !NO_SEND.test(c.command) &&
        gates.find((g) => g.toolCallId === c.id)?.outcome !== "unreadable"
    );
    if (call === undefined) return row(false, "askCall=none");
    const result = resultOf(entries, call.id);
    const refused = result.isError && result.text.startsWith(NOT_SENT);
    const gate = gates.find((g) => g.toolCallId === call.id);
    const verdict = gate?.decision === "revise" && gate.outcome === "verdict";
    const reason = gate?.reason ?? "";
    const failure12 = FAILURE_12.test(reason);
    const age = /nine years|\bage\b/i.test(reason);
    const contributors = /\b214\b|contributor/i.test(reason);
    const mode = gate?.verdictMode ?? "none";
    return row(
      refused && verdict && failure12 && age && contributors && mode === "block",
      `refused=${refused} failure12=${failure12} age=${age} contributors=${contributors} verdictMode=${mode} entry=${gate ? `${gate.decision}/${gate.outcome}` : "none"} reason=${JSON.stringify(reason)}`
    );
  }
  // An `unreadable` entry is a command the gate could not read, judged before any model reads the
  // ask (refused in block mode, run unreviewed in warn mode), so it says nothing of the review this
  // case measures; it is counted apart, as the refused cases skip it.
  const unreadable = gates.filter(
    (g) => g.tool === "dispatch_ask" && g.outcome === "unreadable"
  ).length;
  if (askCase === "link") {
    const reviews = gates.filter((g) => g.tool === "dispatch_ask");
    const verdicts = reviews.filter((g) => g.outcome === "verdict");
    const allowed = verdicts.filter((g) => g.decision === "allow").length;
    const revised = verdicts.filter((g) => g.decision === "revise");
    return row(
      asks > 0 && allowed > 0 && revised.length === 0,
      `asks=${asks} allowed=${allowed} revised=${revised.length} outcomes=${JSON.stringify(reviews.map((g) => g.outcome))} reasons=${JSON.stringify(revised.map((g) => g.reason ?? ""))}`
    );
  }
  // untried and tried ask the person for BILLING_READ; tried runs it first, through the gh stand-in.
  const read =
    askCase === "untried" || askCase === "tried"
      ? ` ranRead=${parsed(path.join(runDir, "calls.jsonl"), Call).some(
          (call) => call.as === "gh" && call.argv.some((arg) => arg.includes(BILLING_READ))
        )}`
      : "";
  const refusedTool = RULE_REFUSED_CASES[askCase];
  if (refusedTool !== undefined) {
    // The first review of the case's call, past an `unreadable` entry, which no model read.
    const reviewed = gates.find((g) => g.tool === refusedTool && g.outcome !== "unreadable");
    if (reviewed === undefined) return row(false, `reviewed=none${read}`);
    const result = resultOf(entries, reviewed.toolCallId);
    const byRule = result.isError && result.text.startsWith(RULE_REFUSED);
    return row(
      byRule && reviewed.rule != null,
      `refusedByRule=${byRule} rule=${reviewed.rule ?? "none"} ruleOutcome=${reviewed.ruleOutcome ?? "none"} verdictMode=${reviewed.verdictMode}${read} result=${JSON.stringify(result.text.slice(0, 300))}`
    );
  }
  if (POSTED_CASES.includes(askCase)) {
    const gated = gates.filter((g) => g.tool === "dispatch_ask");
    const refusals = gated.filter((g) => g.outcome !== "unreadable" && refusedCall(g)).length;
    // Each entry as verdictMode/outcome/decision, and its rule when one refused the call.
    const shapes = gated.map((g) =>
      [g.verdictMode, g.outcome, g.decision, ...(g.rule == null ? [] : [g.rule])].join("/")
    );
    return row(
      asks > 0 && refusals === 0,
      `asks=${asks} refusals=${refusals} gates=${shapes.join(",") || "none"}${read} unreadable=${unreadable}`
    );
  }
  const reviewed = gates.find((g) => g.tool === "dispatch_artifact" && g.outcome === "verdict");
  if (reviewed === undefined) return row(false, "reviewedUpload=none");
  const reason = reviewed.reason ?? "";
  const fix = /comment|dispatch:\/\//i.test(reason);
  return row(
    reviewed.decision === "revise" && reviewed.verdictMode === "block" && fix,
    `decision=${reviewed.decision} verdictMode=${reviewed.verdictMode} fix=${fix} reason=${JSON.stringify(reason)}`
  );
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

/** rig.sh's own_dispatch record, <run>/dispatch.json: the run's own Dispatch, a database in the
 * batch's Postgres container (its id) and the server on it. */
const RunDispatch = z.object({
  postgres: z.string().min(1),
  database: z.string().min(1),
  port: z.number(),
  url: z.string(),
});
/** The run's own Dispatch as `postgres` and `database`, one tab between them, or undefined when the
 * run records none (no dispatch.json). */
function dispatchOf(runDir: string): string | undefined {
  const file = path.join(runDir, "dispatch.json");
  if (!existsSync(file)) return undefined;
  const { postgres, database } = json(file, RunDispatch);
  return `${postgres}\t${database}`;
}

/** The scenarios whose runs the rig scores with no dispatch.json: spec-writing and spec-rewrite.
 * Before each run had a Dispatch of its own, rig.sh ran each batch of these as one run of one label
 * (cmd_batch refused more from the first spec run on), so that run had its batch's Dispatch to
 * itself. Today's rig.sh writes dispatch.json (own_dispatch) before it seeds the run's issue or
 * starts its agent, so a run of today that has none also has no fixture.json and no `exit=` line,
 * and is a rig error for that. An older batch of ask-on-message, measure-before-ask or tester-proof
 * could hold several runs on its one Dispatch, so a run of another scenario with no dispatch.json
 * stays a rig error. */
const BATCH_DISPATCH: Record<string, true> = { "spec-writing": true, "spec-rewrite": true };
const BATCH_DISPATCH_NOTE =
  "no dispatch.json: an older rig's spec run, on its batch's Dispatch, alone in its batch";

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
 * Whether Dispatch took the command is the CLI's ledger's to say (refusalBlocks). */
type Operation =
  | { kind: "read"; step: Step; target: string }
  | {
      kind: "dispatch";
      step: Step;
      call: DispatchCall;
      answer: string;
      refusal: string | undefined;
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
 * The refusals score does not rest on this attribution: the CLI logs each call it ran, refused or
 * not, in its ledger (refusalBlocks), and the attribution only says which command of a call a
 * ledger line belongs to. The output does not show:
 * - the end of another command's output (`cat`, `sed`, a `dispatch` command that succeeded), so a
 *   refusal after it could be that output's text;
 * - output a `$(…)` or a redirect to a file takes;
 * - how often a command in a loop ran: it is one command here;
 * - a write's answer, when another command prints a line of that answer before the write's own;
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
    return [{ kind: "dispatch", step, call, answer, refusal }];
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
/** One line of the dispatch CLI's ledger (dispatch-session-state.ts `ResultEntry`): when the CLI
 * ended a call, the call's tool, and the refusal (`error`) when Dispatch or the call's own check
 * refused it. A line with no `error` is a call Dispatch took. */
const LedgerLine = z.looseObject({
  at: z.string(),
  tool: z.string(),
  error: z.string().optional(),
});
type LedgerLine = z.infer<typeof LedgerLine>;
/** The run's dispatch CLI ledger: `results.jsonl` in the CLI's state directory for the session the
 * transcript's header names, under the run's HOME (rig.sh base_env sets no other state directory).
 * The CLI logs every call it ran, refused or not, whatever the shell did with its output: a pipe,
 * `$(…)`, a loop or another program (Python's subprocess.run) hides nothing from it. A refusal the
 * CLI makes before the call reaches Dispatch is logged too whenever the call knows its session: a
 * refusal of a command's own arguments (`dispatch <command> was not called`, exit 1) under its
 * tool, and a usage refusal (`dispatch: …`, exit 2) under its tool, or under `dispatch` when the
 * CLI could not name the command. It logs no `--help` call that printed help and no dry run that
 * passed; a `--help` of an unknown command is a usage refusal and is logged. A run with
 * no ledger is the rig's fault: its CLI kept no record where the rig reads it. */
function ledgerOf(runDir: string, session: string): LedgerLine[] {
  const state = { XDG_STATE_HOME: path.join(runDir, "home", ".local", "state") };
  const file = path.join(sessionDirectory(state, session), "results.jsonl");
  if (!existsSync(file))
    throw new Error(
      `${file} is missing: the run's dispatch CLI logged no call where the rig reads`
    );
  return parsed(file, LedgerLine);
}
/** The ledger's name of a `dispatch` command's tool. */
const toolOf = (call: DispatchCall) => `dispatch_${call.command.replace(/-/g, "_")}`;
/** refusals: one block per refused call that no later call fixed from its refusal alone.
 * - The refusals: each ledger line with an `error`, and each refusal a call's output shows
 *   (operationsOf) that no ledger line belongs to, which is a refusal the CLI printed and did not
 *   log.
 * - A ledger line belongs to the tool call whose call time is at or before the line's time and
 *   whose result time is at or after it; of two such calls (one message's parallel calls), to the
 *   one whose result came first. Within that call it belongs to the next `dispatch` command of its
 *   tool, in the call's order, that the CLI logs. A line that no command takes (a command in a
 *   loop, one Python ran) belongs to the call alone, and one outside every call's time to no call.
 * - A refusal is fixed by the first ledger line with no `error` whose tool call the agent made after
 *   the refused call's result came back (after the line's time, for a line of no call): a call
 *   made before then cannot show that the agent fixed the refusal from its text.
 * - Each skill file read between the two breaks the fix: a read of the refused call that
 *   operationsOf does not order before the refused command, a read of the fixing call that it does
 *   not order after the fixing command, and every read of a call between them, or of another call
 *   in the message of either.
 * Each block names the call, the ledger's tool and time when the ledger holds it, its refusal text,
 * and the read or the missing fix. */
function refusalBlocks(
  runDir: string,
  entries: SessionEntry[],
  ops: Operation[],
  session: string
): string[] {
  const ledger = ledgerOf(runDir, session);
  const results = outcomes(entries);
  const calls = steps(entries).map((step) => {
    const result = results.get(step.id);
    return {
      step,
      start: Date.parse(step.at),
      end: Date.parse(result?.at ?? ""),
      seen: result?.index ?? Number.POSITIVE_INFINITY,
    };
  });
  type Call = (typeof calls)[number];
  const owner = (line: LedgerLine): Call | undefined => {
    const at = Date.parse(line.at);
    return calls
      .filter((call) => call.start <= at && at <= call.end)
      .sort((a, b) => a.end - b.end)[0];
  };
  // Each ledger line's call and, when a command of that call takes it, the command's place in ops.
  const lines = ledger.map((line) => ({
    line,
    call: owner(line),
    op: undefined as number | undefined,
  }));
  const logged = new Set<number>();
  for (const call of calls) {
    // The commands of the call the CLI logged: every one but a `--help` that printed help, and a
    // `--dry-run` only when its argument check refused it. The CLI checks a command's name before
    // its flags, so a help call of an unknown command is refused and logged like any usage
    // refusal. A usage refusal (`dispatch: …`) is logged under tool `dispatch` when the CLI could
    // not name the command, so the ledger line and the printed refusal are one refusal and match
    // here.
    const candidates = ops.flatMap((op, place) =>
      op.kind === "dispatch" &&
      op.step.id === call.step.id &&
      (op.call.flag("help") === undefined || op.refusal !== undefined) &&
      (op.call.flag("dry-run") === undefined || op.refusal !== undefined)
        ? [
            {
              place,
              tool: toolOf(op.call),
              refused: op.refusal !== undefined,
              usage: cliRefusals(op.refusal ?? "")[0]?.command === null,
            },
          ]
        : []
    );
    let next = 0;
    for (const entry of lines.filter((one) => one.call === call)) {
      // A line the CLI logged as a success is no command whose output shows a refusal.
      const found = candidates.findIndex(
        (candidate, index) =>
          index >= next &&
          (candidate.tool === entry.line.tool ||
            (entry.line.tool === "dispatch" && candidate.usage)) &&
          (entry.line.error !== undefined || !candidate.refused)
      );
      const candidate = candidates[found];
      if (candidate === undefined) continue;
      entry.op = candidate.place;
      logged.add(candidate.place);
      next = found + 1;
    }
  }
  const callOf = (step: Step) => calls.find((call) => call.step.id === step.id);
  const refusals = [
    ...lines.flatMap((entry) => {
      if (entry.line.error === undefined) return [];
      const op = entry.op === undefined ? undefined : ops[entry.op];
      const where =
        op?.kind === "dispatch"
          ? `${op.call.text} (${entry.line.tool}, logged at ${entry.line.at})`
          : `${entry.line.tool}, logged at ${entry.line.at}, ${entry.call ? `in tool call ${entry.call.step.id}` : "in no tool call's time"}`;
      return [
        {
          where,
          text: entry.line.error.trim(),
          call: entry.call,
          op: entry.op,
          at: Date.parse(entry.line.at),
        },
      ];
    }),
    ...ops.flatMap((op, place) => {
      if (op.kind !== "dispatch" || op.refusal === undefined || logged.has(place)) return [];
      const call = callOf(op.step);
      return [{ where: op.call.text, text: op.refusal, call, op: place, at: call?.start ?? 0 }];
    }),
    // An `eval` cell runs `dispatch` through its own language, which no shell parse reads, and Oh
    // My Pi gives its kernel an environment of allowlisted names alone, with no session id in it,
    // so the CLI knows no session and logs nothing. Such a cell's refusals are in its output
    // alone, and each counts once, for the cell.
    ...calls.flatMap((call) => {
      if (call.step.tool !== "eval") return [];
      if (lines.some((entry) => entry.call === call)) return [];
      if (ops.some((op) => op.kind === "dispatch" && op.step.id === call.step.id)) return [];
      return cliRefusals(results.get(call.step.id)?.text ?? "").map((found) => ({
        where: `eval cell ${call.step.id}, which the CLI logged no call of`,
        text: found.line,
        call: call as Call | undefined,
        op: undefined as number | undefined,
        at: call.start,
      }));
    }),
  ].sort((a, b) => a.at - b.at || (a.op ?? 0) - (b.op ?? 0));

  const blocks: string[] = [];
  for (const refusal of refusals) {
    const after = (call: Call) =>
      refusal.call === undefined ? call.start > refusal.at : call.step.index > refusal.call.seen;
    const fix = lines.find(
      (entry) => entry.line.error === undefined && entry.call !== undefined && after(entry.call)
    );
    type Read = Extract<Operation, { kind: "read" }>;
    const between = (op: Operation, place: number): op is Read => {
      if (op.kind !== "read" || fix?.call === undefined) return false;
      const call = callOf(op.step);
      if (call === undefined) return false;
      if (call === refusal.call) return refusal.op === undefined || place > refusal.op;
      if (call === fix.call) return fix.op === undefined || place < fix.op;
      const from =
        refusal.call === undefined
          ? call.start >= refusal.at
          : call.step.index >= refusal.call.step.index;
      return from && call.step.index <= fix.call.step.index;
    };
    const read = ops.find(between);
    if (fix !== undefined && read === undefined) continue;
    blocks.push(
      [
        `refused call: ${refusal.where}`,
        indented(refusal.text, 4),
        fix === undefined || read === undefined
          ? "    no later dispatch call succeeded"
          : `    then read ${read.target} before the next dispatch call succeeded`,
      ].join("\n")
    );
  }
  return blocks;
}
/** Whether a skill file read is skill `name` itself. */
const readsSkill = (target: string, name: string) =>
  new RegExp(`^skill://${escaped(name)}(/SKILL\\.md)?$|/skills/${escaped(name)}/SKILL\\.md$`).test(
    target
  );

/** A spec-only check that counts for a spec write: a `task` call whose every task completed with a
 * report that came back before the write (checkOutcome). `assignment` is every string of its
 * arguments, `report` each task's report, and `write` the place of the write in SpecWork.writes. */
export interface SpecCheck {
  assignment: string;
  report: string;
  write: number;
}
/** A spec-only check that counts for no spec write: its tasks, its call's id, the place of the
 * write after it, and why. */
export interface RejectedCheck {
  tasks: string;
  call: string;
  write: number;
  problem: string;
}
export interface SpecWork {
  /** Each spec write the tool accepted, in order, and whether a spec-only check that counts came
   * after the write before it (or the session's start) and before it. */
  writes: { index: number; at: string; text: string; checked: boolean }[];
  checks: SpecCheck[];
  rejected: RejectedCheck[];
}
/** The opening tag of Oh My Pi's envelope of one task's end (prompts/tools/task-summary.md). Its
 * attributes give the task's id. taskNames reads the opening tags alone, so a closing tag that a
 * report quotes cannot make its list shorter. A greedy capture of the body would merge the
 * envelopes of a blocking result of several tasks into one match and drop every name after the
 * first, so a task that failed would not be required.
 * The scan lists names conservatively; it does not tell which task is which. An opening tag that a
 * report quotes can add a name, and no tag can remove one. An added name with no completed record
 * rejects the check (`no report of task <name> arrived`). An added name that has a completed record
 * of its own, such as another task that finished after this call, brings that task's unrelated
 * report into the check, and the check counts. A task of the call that failed still rejects it,
 * since its name stays in the list. To close the gap, take the call's task ids from its structured
 * results, not from the text. */
const TASK_RESULT_OPEN = /<task-result\b([^>]*)>/g;
const attribute = (attributes: string, name: string) =>
  new RegExp(`\\b${name}="([^"]*)"`).exec(attributes)?.[1];
/** A task's report in its envelope's body: the `<output>` or `<preview>` part, else the body less
 * its `<meta>` and `<abort-reason>` lines. Oh My Pi's `(no output)` placeholder is no report. */
function reportOf(body: string): string {
  const inner =
    /<(output|preview)\b[^>]*>([\s\S]*?)<\/\1>/.exec(body)?.[2] ??
    body.replace(/<meta\b[^>]*\/>|<abort-reason>[\s\S]*?<\/abort-reason>/g, "");
  const report = inner.trim();
  return /^\(no output\)/.test(report) ? "" : report;
}
/** A record's field, when the record is an object. */
const field = (value: unknown, name: string): unknown =>
  typeof value === "object" && value !== null
    ? (value as Record<string, unknown>)[name]
    : undefined;
/** The status Oh My Pi's envelope gives a task from its `details.results` entry
 * (task/result-summary.ts). */
function statusOf(result: unknown): string {
  const exit = field(result, "exitCode");
  if (field(result, "aborted") === true) return "cancelled";
  if (exit === 0) return field(result, "error") ? "merge failed" : "completed";
  return `failed (exit ${typeof exit === "number" ? exit : "?"})`;
}
/** The names of a `task` call's tasks: each one its arguments name, each a background spawn's
 * result names (`` `<name>` (job ``), and each a blocking result's envelope names. */
function taskNames(step: Step, own: Outcome | undefined): string[] {
  const names = new Set<string>();
  const tasks = Array.isArray(step.args.tasks) ? step.args.tasks : [step.args];
  for (const task of tasks) {
    const name = field(task, "name");
    if (typeof name === "string" && name !== "") names.add(name);
  }
  const text = own?.text ?? "";
  for (const [, name] of text.matchAll(/`([^`\s]+)` \(job/g)) if (name) names.add(name);
  for (const [, attributes = ""] of text.matchAll(TASK_RESULT_OPEN)) {
    const id = attribute(attributes, "id");
    if (id) names.add(id);
  }
  return [...names];
}
/** The text Oh My Pi delivered for job `name` in the body of an `async-result` message that
 * delivers the jobs `names`. Its task jobs carry no `resultText`, so the body is the only report.
 * A message of one job is a `Background job <name> has completed…` line, then the job's envelope;
 * a message of several is a count line, then each job under a `── Job <id> (<label>) ──` line.
 * Only a line naming a job this message delivers frames a section, since a report may quote such a
 * line of its own. A job's section runs to the next framing line of another job, or to the end. A
 * message that frames the job's own section other than once gives it no text: which section is the
 * job's own cannot be told. */
function jobSection(body: string, name: string, names: string[]): string {
  const text = body.replace(/^\s*<system-notice>\s*/, "").replace(/\s*<\/system-notice>\s*$/, "");
  if (names.length === 1) {
    const lead = new RegExp(`^Background job ${escaped(name)} has completed\\.[^\\n]*\\n`).exec(
      text
    );
    return lead === null ? "" : text.slice(lead[0].length);
  }
  const marks = [...text.matchAll(/^── Job (\S+) \(.*\) ──$/gm)]
    .map((mark) => ({
      id: mark[1],
      start: mark.index ?? 0,
      end: (mark.index ?? 0) + mark[0].length,
    }))
    .filter((mark) => mark.id !== undefined && names.includes(mark.id));
  const own = marks.filter((mark) => mark.id === name);
  const mark = own[0];
  if (own.length !== 1 || mark === undefined) return "";
  const after = marks.find((other) => other.start > mark.start && other.id !== name);
  return text.slice(mark.end, after?.start ?? text.length);
}
/** What a spec-only check came to before the spec write whose assistant message is entry `before`:
 * its tasks' reports, or why it does not count. Every task the call started must have ended
 * `completed` with a report that is not empty, in an entry before the write. A report counts only
 * from Oh My Pi's structured task delivery: a blocking task call's own `details.results`, or the
 * `details.jobs` entry of a `wait` result or `async-result` message whose job is that task. A
 * blocking result's envelope is read only when Oh My Pi recorded no `details.results`. A `wait` that
 * returned a message, or an async delivery of another job, delivers text and never a task's end.
 * A report body is opaque: no envelope inside it counts as another task's report. A report that
 * arrives after the write is no report for it. A task call the tool failed (`isError`) checked
 * nothing. */
function checkOutcome(
  entries: SessionEntry[],
  step: Step,
  own: Outcome | undefined,
  before: number
): { report: string } | { problem: string } {
  if (own === undefined) return { problem: "the task call has no result" };
  if (own.index > before) return { problem: "the task call's result arrived after the spec write" };
  if (own.failed)
    return { problem: `the task call failed: ${own.text.trim().split("\n")[0] ?? ""}` };
  const ended: { name: string; status: string; report: string; index: number }[] = [];
  const results = field(own.details, "results");
  if (Array.isArray(results) && results.length > 0)
    for (const result of results) {
      const output = field(result, "output");
      ended.push({
        name: String(field(result, "id") ?? ""),
        status: statusOf(result),
        report: typeof output === "string" ? output.trim() : "",
        index: own.index,
      });
    }
  // As in the delivered branch below: no end anchor, and a greedy capture. A blocking result has no
  // sections, so only reportOf bounds the report, and the same limit applies.
  else {
    const envelope = /^\s*<task-result\b([^>]*)>([\s\S]*)<\/task-result>/.exec(own.text);
    if (envelope !== null)
      ended.push({
        name: attribute(envelope[1] ?? "", "id") ?? "",
        status: attribute(envelope[1] ?? "", "status") ?? "no status",
        report: reportOf(envelope[2] ?? ""),
        index: own.index,
      });
  }
  for (const [index, entry] of entries.entries()) {
    if (index <= step.index || index === own.index) continue;
    const details =
      entry.type === "custom_message" && entry.customType === "async-result"
        ? entry.details
        : entry.message?.role === "toolResult" && entry.message.toolName === "wait"
          ? entry.message.details
          : undefined;
    const jobs = field(details, "jobs");
    if (!Array.isArray(jobs)) continue;
    // The jobs this message delivers, by the name each is recorded under: a section's framing line
    // names one of these, and nothing else frames a section.
    const delivered = jobs
      .map((job) => field(job, "id") ?? field(job, "jobId") ?? field(job, "agentId"))
      .filter((name): name is string => typeof name === "string");
    for (const job of jobs) {
      if (field(job, "type") !== "task") continue;
      const name = field(job, "id") ?? field(job, "jobId") ?? field(job, "agentId");
      const status = field(job, "status");
      if (typeof name !== "string" || typeof status !== "string") continue;
      const resultText = field(job, "resultText");
      const text =
        typeof resultText === "string"
          ? resultText
          : jobSection(strings(entry.content).join(""), name, delivered);
      // Oh My Pi writes text of its own after the closing tag of most deliveries (the agent is
      // idle, how to message it, where its transcript is), so the envelope is not the text's end.
      // The capture is greedy. A lazy capture would end at the first `</task-result>` that the
      // report quotes and drop every finding after it; the agent being scored writes its checker's
      // prompt, so it could ask for exactly that. The greedy body can run past the real closing
      // tag: Oh My Pi escapes no `task-result` tag, and text it writes after the envelope can hold
      // one. Two rules bound the report. In a message that delivers several jobs, jobSection ends
      // this job's section at the framing line of the next job the message delivers. reportOf
      // takes the first complete `<output>` or `<preview>` part, and task-summary.md always writes
      // one, so text after that part is not in the report.
      // Limit: two envelopes with neither part, with text between them, put that text and the
      // second body into the report. The pinned task-summary.md always writes one of the two parts,
      // so no supported producer gives that shape.
      const envelope = /^\s*<task-result\b([^>]*)>([\s\S]*)<\/task-result>/.exec(text);
      const body =
        envelope !== null && attribute(envelope[1] ?? "", "id") === name ? (envelope[2] ?? "") : "";
      ended.push({ name, status, report: reportOf(body), index });
    }
  }
  const names = taskNames(step, own);
  if (names.length === 0)
    return { problem: "the call names no task, and no task's report arrived" };
  const problems: string[] = [];
  const reports: string[] = [];
  for (const name of names) {
    // A job snapshot of a task still running is no end: a `wait` answers with every job it waited
    // on, the running ones included, and the completion follows in a later entry. Taking a
    // completed record over a running one is safe only because Oh My Pi gives a repeated task name
    // a new id (`C`, then `C-2`), so every record of one name belongs to one run of that task.
    // This code ties no record to the call that started it: were two runs to share a name, a
    // second run's completion could answer for the first.
    const seen = ended.filter((one) => one.name === name && one.index < before);
    const end = seen.find((one) => one.status === "completed") ?? seen[0];
    if (end === undefined)
      problems.push(
        ended.some((one) => one.name === name)
          ? `the report of task ${name} arrived after the spec write`
          : `no report of task ${name} arrived`
      );
    else if (end.status !== "completed") problems.push(`task ${name} ended ${end.status}`);
    else if (end.report === "") problems.push(`the report of task ${name} is empty`);
    else reports.push(end.report);
  }
  return problems.length > 0 ? { problem: problems.join("; ") } : { report: reports.join("\n\n") };
}
/** The spec writes of a transcript (writesSpec, against the run's fixture.json) and the spec-only
 * checks before them. A spec-only check is a `task` call whose assignment holds the seven outline
 * headings as `##` lines and names no path under a checkout in `reach`. It counts for the first
 * spec write after it when its tasks' reports came back before that write (checkOutcome); either
 * way it counts for no later write. Each `dispatch` command of a call is a write of its own
 * (operationsOf), so two writes in one call with no check between them leave the second
 * unchecked. Places are transcript order, never timestamps. */
export function specWork(entries: SessionEntry[], reach: string[], fixture: SpecFixture): SpecWork {
  const results = outcomes(entries);
  const writes: SpecWork["writes"] = [];
  const checks: SpecCheck[] = [];
  const rejected: RejectedCheck[] = [];
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
      let checked = false;
      for (const check of pending) {
        const own = results.get(check.step.id);
        const outcome = checkOutcome(entries, check.step, own, step.index);
        if ("problem" in outcome)
          rejected.push({
            tasks: taskNames(check.step, own).join(", ") || "(unnamed)",
            call: check.step.id,
            write: writes.length,
            problem: outcome.problem,
          });
        else {
          checks.push({
            assignment: check.assignment,
            report: outcome.report,
            write: writes.length,
          });
          checked = true;
        }
      }
      writes.push({ index: step.index, at: step.at, text: operation.call.text, checked });
      pending = [];
    }
  }
  return { writes, checks, rejected };
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

/** What judge.ts gives each judge of a run, and so what each judge's answer must cover:
 * - `requirements`: the `**R<n>.**` marks of the approved version's spec-versions/<v>.md, or none
 *   when the person approved none (then no checkable judge runs);
 * - `asks`: agent-asks.json less the approval request and the seeded asks, oldest first at full
 *   precision (Dispatch writes `…:00.5Z` before `…:00.51Z`, which text order reverses);
 * - `texts`: agent-texts.json less the seeded asks;
 * - `documents`: each file of documents/, as `documents/<file>`;
 * - `checks`: how many spec-only checks count for a spec write (specWork): with none, no findings
 *   judge runs and findings.json has no row. */
export interface JudgeInputs {
  requirements: string[];
  asks: z.infer<typeof AgentAsks>;
  texts: z.infer<typeof AgentTexts>;
  documents: string[];
  checks: number;
}
export function judgeInputs(runDir: string, work: SpecWork): JudgeInputs {
  const seeded = seededIds(runDir);
  const { approved, version } = approval(runDir);
  const documents = path.join(runDir, "documents");
  return {
    requirements:
      approved && version !== null
        ? requirements(readFileSync(path.join(runDir, "spec-versions", `${version}.md`), "utf8"))
        : [],
    asks: json(path.join(runDir, "agent-asks.json"), AgentAsks)
      .filter((ask) => ask.kind !== "approval" && !seeded.has(ask.id))
      .toSorted((a, b) =>
        byCreatedAt({ created_at: a.created_at ?? "" }, { created_at: b.created_at ?? "" })
      ),
    texts: json(path.join(runDir, "agent-texts.json"), AgentTexts).filter(
      (text) => !seeded.has(text.id)
    ),
    documents: (existsSync(documents) ? readdirSync(documents).sort() : []).map(
      (file) => `documents/${file}`
    ),
    checks: work.checks.length,
  };
}
/** What a judge file's ids leave out of, repeat from or add to the ids its judge was given:
 * `exact` holds the answer to one row per given id, else each id it names must be one given. */
function covered(what: string, given: string[], named: string[], exact: boolean): string[] {
  const problems: string[] = [];
  if (exact) {
    for (const id of given) {
      const count = named.filter((one) => one === id).length;
      if (count === 0) problems.push(`no row for ${what} ${id}`);
      if (count > 1) problems.push(`two or more rows for ${what} ${id}`);
    }
  }
  const unknown = [...new Set(named.filter((one) => !given.includes(one)))];
  if (unknown.length > 0) problems.push(`rows for ${what} it was not given: ${unknown.join(", ")}`);
  return problems;
}
/** What a judge's answer to `file` (a judge-prompts.ts output) leaves out of, repeats from or adds
 * to what judge.ts gave its judge (judgeInputs); empty when it covers its input exactly. Both
 * judge.ts, after each judge's answer parses, and specRow use it: judge.ts fails the run naming the
 * answer, and specRow reports the file as a rig error.
 * - checkable.json: one row for each requirement of the approved version;
 * - questions.json: one row for each ask given;
 * - numbers.json: one row for each text given;
 * - proposal.json: each `ask` a row names is one given;
 * - preamble.json: each `source` is spec.md, a given document, or agent-texts.json#<given id>;
 * - faults.json: each row's count is the number of its items;
 * - findings.json: each `check` is 1 to the number of checks given. */
export function coverage(file: string, answer: unknown, inputs: JudgeInputs): string[] {
  const asks = inputs.asks.map((ask) => ask.id);
  const texts = inputs.texts.map((text) => text.id);
  switch (file) {
    case "checkable.json":
      return covered(
        "requirement",
        inputs.requirements,
        CheckableOutput.parse(answer).rows.map((row) => row.requirement),
        true
      );
    case "questions.json":
      return covered(
        "ask",
        asks,
        QuestionsOutput.parse(answer).rows.map((row) => row.id),
        true
      );
    case "numbers.json":
      return covered(
        "text",
        texts,
        NumbersOutput.parse(answer).rows.map((row) => row.id),
        true
      );
    case "proposal.json":
      return covered(
        "ask",
        asks,
        ProposalOutput.parse(answer).rows.flatMap((row) => (row.ask ? [row.ask] : [])),
        false
      );
    case "preamble.json":
      return covered(
        "source",
        ["spec.md", ...inputs.documents, ...texts.map((id) => `agent-texts.json#${id}`)],
        PreambleOutput.parse(answer).rows.map((row) => row.source),
        false
      );
    case "faults.json": {
      const faults = FaultsOutput.parse(answer);
      return FAULT_ROWS.flatMap((row) => {
        const items = faults.items.filter((item) => item.row === row).length;
        return items === faults.faults[row]
          ? []
          : [`faults["${row}"] is ${faults.faults[row]}, but ${items} items name that row`];
      });
    }
    case "findings.json":
      return covered(
        "check",
        Array.from({ length: inputs.checks }, (_, index) => String(index + 1)),
        FindingsOutput.parse(answer).rows.map((row) => String(row.check)),
        false
      );
    default:
      return [];
  }
}
/** A judge's answer in the run directory, as judge.ts wrote it; a file missing, malformed, or not
 * covering what judge.ts gave its judge (coverage) is a rig error that `rig.sh judge` fixes. */
function judged<T>(runDir: string, file: string, schema: z.ZodType<T>, inputs: JudgeInputs): T {
  const full = path.join(runDir, file);
  if (!existsSync(full)) throw new Error(`${file} is missing: run rig.sh judge ${runDir}`);
  const result = schema.safeParse(JSON.parse(readFileSync(full, "utf8")));
  if (!result.success)
    throw new Error(`${file} is not a judge's answer: ${z.prettifyError(result.error)}`);
  const problems = coverage(file, result.data, inputs);
  if (problems.length > 0)
    throw new Error(
      `${file} does not cover what judge.ts gave its judge: ${problems.join("; ")}: run rig.sh judge ${runDir}`
    );
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
 *     (specWork) counts for every one, the first included, and findings.json has no `neither`
 *     row. A check counts for a write when its task call and every task's completed, non-empty
 *     report came after the write before it and before this write, in transcript order
 *     (checkOutcome). Each check that counts for no write is printed with why.
 *   - refusals: every refused `dispatch` call (refusalBlocks: an `error` line of the CLI's ledger,
 *     or a refusal a call's output shows that the ledger holds no line of: an `eval` cell's, whose
 *     kernel knows no session, or one the CLI made with no session) is followed by a call
 *     the ledger logs as a success, in a later tool call, with no skill file read between them. A
 *     call the agent made before the refusal came back is no fix: it cannot show that the agent
 *     fixed the call from the refusal alone. Each failing refusal is printed with its text.
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

  const inputs = judgeInputs(runDir, work);
  const findings = judged(runDir, "findings.json", FindingsOutput, inputs);
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
  for (const check of work.rejected)
    details.push(
      `check ${check.tasks} (call ${check.call}) for spec write ${check.write + 1} does not count: ${check.problem}`
    );

  const id = sessionId(entries);
  if (id === undefined) throw new Error("the transcript has no session header");
  const refused = refusalBlocks(runDir, entries, ops, id);
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
  const verdicts = judged(runDir, "shared-runs.json", SharedRunsFile, inputs).rows;
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

  const selfTexts = [
    { source: "spec.md", body: spec },
    ...json(path.join(runDir, "agent-texts.json"), AgentTexts)
      .filter((text) => !seeded.has(text.id))
      .map((text) => ({ source: `agent-texts.json#${text.id}`, body: text.body })),
  ].filter((text) => text.body.toLowerCase().includes(id.slice(0, 8).toLowerCase()));
  const self = selfTexts.length === 0;
  details.push(...selfTexts.map((text) => `self: ${text.source} names the session`));

  const faults = judged(runDir, "faults.json", FaultsOutput, inputs);
  const faultCount = Object.values(faults.faults).reduce((sum, n) => sum + n, 0);
  const decisions = judged(runDir, "decisions.json", DecisionsOutput, inputs).rows;
  const checkable = judged(runDir, "checkable.json", CheckableFile, inputs);
  if (approved && checkable.version !== version)
    throw new Error(
      `checkable.json judged version ${checkable.version}, and the person approved version ${version}: run rig.sh judge ${runDir}`
    );
  const uncheckable = checkable.rows.filter((row) => row.uncheckable);
  for (const row of uncheckable) details.push(`uncheckable ${row.requirement}: ${row.check}`);
  const asks = judged(runDir, "questions.json", QuestionsOutput, inputs).rows;
  const texts = judged(runDir, "numbers.json", NumbersOutput, inputs).rows;
  const passages = judged(runDir, "preamble.json", PreambleOutput, inputs).rows;
  for (const row of passages) details.push(`preamble ${row.kind} in ${row.source}: ${row.passage}`);
  const proposals = judged(runDir, "proposal.json", ProposalOutput, inputs).rows;

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
 *   - it did not have a Dispatch of its own: it records none (`dispatch` undefined: no
 *     dispatch.json) and is no spec run (BATCH_DISPATCH), or another run names the same database
 *     in the same Postgres container (`sharing`, each Dispatch's runs). A `dispatch search`
 *     searches every project of its server, so runs that share one can read each other's issue
 *     and asks, and one run's answer can be another's;
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
function unscored(
  runDir: string,
  scenario: string,
  dispatch: string | undefined,
  label: string,
  labels: Map<string, string>,
  sharing: Map<string, string[]>
): string | undefined {
  if (dispatch === undefined) {
    if (!BATCH_DISPATCH[scenario]) return "it records no Dispatch of its own (no dispatch.json)";
  } else {
    const others = (sharing.get(dispatch) ?? []).filter((run) => run !== path.basename(runDir));
    if (others.length > 0) {
      const [postgres, database] = dispatch.split("\t");
      return `it shares Dispatch database ${database} of Postgres ${postgres} with ${others.join(", ")}, so each could read the other's issue and asks`;
    }
  }
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
  // Every run's own Dispatch, keyed by its Postgres container and database, with the runs that
  // name it. A run that records none is scored or not by unscored, below; a dispatch.json that does
  // not parse is its own run's rig error, below.
  const sharing = new Map<string, string[]>();
  for (const run of existsSync(runsDir) ? readdirSync(runsDir) : []) {
    let dispatch: string | undefined;
    try {
      dispatch = dispatchOf(path.join(runsDir, run));
    } catch {
      continue;
    }
    if (dispatch !== undefined) sharing.set(dispatch, [...(sharing.get(dispatch) ?? []), run]);
  }
  // Every checkout a spec run's agent can reach, so that a check naming one is no spec-only check.
  const reach = checkouts(path.dirname(runsDir));
  // Each scenario's scorer, keyed by the name its runs' directories start with. An askgate run's
  // case comes from its directory (ASKGATE_RUN).
  const scorers: Record<string, (dir: string, run: string, label: string, askCase: string) => Row> =
    {
      "ask-on-message": askOnMessage,
      "measure-before-ask": measureBeforeAsk,
      "tester-proof": (dir, run, label) => testerProof(dir, run, label, heads),
      brainstorm: brainstormSurface,
      askgate: askgateReview,
      "spec-writing": (dir, run, label) => specRow(dir, run, "spec-writing", label, reach),
      "spec-rewrite": (dir, run, label) => specRow(dir, run, "spec-rewrite", label, reach),
    };
  const name = new RegExp(`^(${Object.keys(scorers).join("|")})-(.+)-(\\d+)$`);
  for (const run of existsSync(runsDir) ? readdirSync(runsDir).sort() : []) {
    const match = name.exec(run);
    const score = scorers[match?.[1] ?? ""];
    if (!match || !score) continue;
    const [, scenario = "", named = ""] = match;
    if (only && scenario !== only) continue;
    const dir = path.join(runsDir, run);
    // undefined for every other scenario, null for an askgate directory that names no case.
    const askgate = scenario === "askgate" ? ASKGATE_RUN.exec(run) : undefined;
    const [, askCase = "", label = named] = askgate ?? [];
    const key = `${askgate ? `askgate:${askCase}` : scenario}\t${label}`;
    let reason: string | undefined;
    try {
      if (askgate === null)
        throw new Error(
          "the directory names no case: an askgate run's directory is askgate-<case>-<label>-<n>"
        );
      const dispatch = dispatchOf(dir);
      reason = unscored(dir, scenario, dispatch, label, labels, sharing);
      if (reason === undefined) {
        const row = score(dir, run, label, askCase);
        rows.push(
          dispatch === undefined ? { ...row, notes: `${row.notes}\n  ${BATCH_DISPATCH_NOTE}` } : row
        );
      }
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
      "           askgate: the rule for the run's label and case (askgateReview)",
      "pass+ref   passed, and the agent read the file the rule is in (askgate: never counted)",
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
