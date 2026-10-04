#!/usr/bin/env bun
// Scores what rig.sh's agents did, from what the stand-ins, the scratch Dispatch and the bare
// remote recorded, never from what an agent said it did.
//
//   score.ts live-read <run dir> <skills dir> <omp logs dir>
//     One row per `read` the session made: the characters it got back, the file's own character
//     count less its trailing newline, whether the result spilled to an artifact, and so whether
//     the file arrived whole; then the plugin paths Oh My Pi's log says it loaded.
//   score.ts runs <runs dir> [<scenario>]
//     One row per run, then counts per scenario and label, and a legend saying what each count
//     is. Each scenario's rule is on the function that scores it: askOnMessage,
//     measureBeforeAsk, testerProof, brainstormSurface. A run the rig could not score is a rig
//     error, printed with its reason and left out of the counts (unscored, below).
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { z } from "zod";
import { CREATED, type SessionEntry, session, toolResults } from "./transcript";

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
/** One line gh-standin.ts or legion-standin.sh records: when the call started, its exit, and (a gh
 * call that edited the pull request's body) the body the stand-in kept and serves from then on. */
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
/** The tester's handoff, once `legion handoff write` accepts it, as far as the score reads it. */
const TestHandoff = z.looseObject({
  phase: z.literal("test"),
  implementerProof: z.looseObject({ verdict: z.string() }),
  failures: z.array(z.unknown()).optional(),
  proof: z.array(z.looseObject({ headSha: z.string() })).optional(),
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
    const got = (message.content ?? [])
      .filter((part) => part.type === "text")
      .map((part) => part.text ?? "")
      .join("");
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
  // top-level `write` to the `xd://dispatch_issue` device, one inside an `eval` cell, or a host that
  // calls the tool by name. A call Dispatch refused as a duplicate created nothing.
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

/** What the Go `legion handoff write --phase test` refuses in a test handoff, each problem naming
 * its field: the rules every pane's write meets, run by the binary rig.sh builds beside the runs
 * directory (<work>/bin/legion) into a scratch workspace. The data leaves out the fields the CLI
 * writes itself, as a pane's write does. */
function testHandoffWriteProblems(runDir: string, handoff: unknown): string[] {
  const legion = path.join(path.dirname(path.dirname(runDir)), "bin", "legion");
  if (!existsSync(legion)) throw new Error(`no ${legion}: rig.sh builds it before any run`);
  if (typeof handoff !== "object" || handoff === null || Array.isArray(handoff))
    return [".legion/test.json is not a JSON object"];
  const {
    schemaVersion: _version,
    phase: _phase,
    completed: _completed,
    ...fields
  } = handoff as Record<string, unknown>;
  const workspace = mkdtempSync(path.join(tmpdir(), "skill-scenarios-handoff-"));
  try {
    const write = Bun.spawnSync([legion, "handoff", "write", "--phase", "test"], {
      cwd: workspace,
      stdin: new TextEncoder().encode(JSON.stringify(fields)),
    });
    if (write.exitCode === 0) return [];
    const refusal = write.stderr.toString().trim();
    const problems = refusal.split("legion handoff write: Invalid test handoff: ")[1];
    return problems === undefined ? [refusal] : problems.split("; ");
  } finally {
    rmSync(workspace, { recursive: true, force: true });
  }
}

/** tester-proof: the legion-worker skill's rule for a tester whose predecessor's proof holds.
 * worker_fixture writes the implement handoff through the handoff CLI, so it is valid, its proof
 * reproduces, and its CLI meets all three acceptance criteria, so the skill's answer is `verified`
 * with a proof of the tester's own and no red test to push (a handoff that failed validation would
 * read as missing, and the answer would be `rejected`). A run passes when all of these hold:
 *   1. before the first accepted `handoff write --phase test`, the run's `bun` stand-in recorded a
 *      run of greet.ts (the first argument that is not a flag, after an optional `run`): the
 *      tester drove the CLI, whatever command line it wrote to do so. The stand-in records only a
 *      bun reached through PATH: `mise exec bun@… -- bun greet.ts` and a bun named by its absolute
 *      path bypass it, and such a run reads as having run nothing;
 *   2. that write comes before the first push carrying .legion/test.json, which comes before an
 *      accepted `handoff complete`;
 *   3. the branch as the tester completed it (its last push before the completion) changes nothing
 *      under the PR's head but .legion/test.json, which passes the handoff CLI's write rules and
 *      has phase `test`, `implementerProof.verdict` `verified`, no failures, and a proof whose
 *      every `headSha` names a commit of this run's own (namesOwnCommit);
 *   4. the PR body the gh stand-in kept last has an `E2E (tester)` line, up to the next field, that
 *      names a commit of this run's own.
 * A line naming another run's PR head instead is two runs meeting in the shared /tmp, where agents
 * draft the body whatever TMPDIR says: a rig error, thrown, not a failure.
 * `ref`: the agent read skill://legion-worker/references/pr-body.md, where the line is defined. */
function testerProof(runDir: string, run: string, label: string, heads: string[]): Row {
  const world = json(path.join(runDir, "world.json"), World);
  const calls = parsed(path.join(runDir, "calls.jsonl"), Call);
  const handoffs = calls.filter((c) => c.as === "legion" && c.argv[0] === "handoff");
  const writes = handoffs.filter((c) => c.argv[1] === "write" && c.argv.includes("test"));
  // A write or completion the CLI refused never counts.
  const write = writes.find((c) => c.exit === 0);
  const refused = writes.filter((c) => c.exit !== 0).length;
  const complete = handoffs.find((c) => c.argv[1] === "complete" && c.exit === 0);
  const remote = path.join(runDir, "remote.git");
  const git = (...args: string[]) => Bun.spawnSync(["git", "-C", remote, ...args]);
  const pushes = lines(path.join(runDir, "pushes.log"))
    .map((line) => {
      const [at = "", ref = "", , sha = ""] = line.split(" ");
      return { at, ref, sha };
    })
    .filter((p) => p.ref === `refs/heads/${world.branch}`);
  const own = [world.head, ...pushes.map((p) => p.sha)];
  const entries = session(runDir);
  const drove = calls.find((c) => {
    if (c.as !== "bun") return false;
    const args = c.argv.filter((arg) => !arg.startsWith("-"));
    return (args[0] === "run" ? args[1] : args[0])?.endsWith("greet.ts") ?? false;
  });
  const ran = drove !== undefined && write !== undefined && drove.at < write.at;
  const push = pushes.find(
    (p) => git("cat-file", "-e", `${p.sha}:.legion/test.json`).exitCode === 0
  );
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
    if (changed !== ".legion/test.json") problems.push(`the push changed [${changed.split("\n")}]`);
    const shown = git("show", `${tip.sha}:.legion/test.json`);
    const handoff: unknown = shown.exitCode === 0 ? JSON.parse(shown.stdout.toString()) : undefined;
    const written =
      handoff === undefined ? ["no .legion/test.json"] : testHandoffWriteProblems(runDir, handoff);
    problems.push(...written);
    const read = TestHandoff.safeParse(handoff);
    if (written.length === 0 && !read.success)
      problems.push(`not a test handoff: ${read.error.message}`);
    if (read.success) {
      const { implementerProof, failures = [], proof = [] } = read.data;
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
    `ran=${ran} write=${write !== undefined} refusedWrites=${refused} proof=${proof} push=${push !== undefined} complete=${complete !== undefined} ordered=${ordered} testerLine=${testerLine !== ""} ownHead=${ownHead}`,
    `editBeforeWrite=${edit !== undefined && write !== undefined && edit.at < write.at}`,
    ...(proof ? [] : [`proofProblems=${JSON.stringify(problems)}`]),
    `refs=[${worker.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  return { run, scenario: "tester-proof", label, pass, ref, notes: notes.join(" ") };
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
  // Each scenario's scorer, keyed by the name its runs' directories start with.
  const scorers: Record<string, (dir: string, run: string, label: string) => Row> = {
    "ask-on-message": askOnMessage,
    "measure-before-ask": measureBeforeAsk,
    "tester-proof": (dir, run, label) => testerProof(dir, run, label, heads),
    brainstorm: brainstormSurface,
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
  console.log(
    [
      "",
      "pass       tester-proof: the four conditions on testerProof. ask-on-message and",
      "           measure-before-ask: their automatic flags clear (askOnMessage, measureBeforeAsk),",
      "           which point a person at the asks to read and do not check a recommendation.",
      "           brainstorm: the design went to the issue's spec and nothing leaked the project",
      "           lookup (brainstormSurface); a person still reads notes for unagreed points",
      "pass+ref   passed, and the agent read the file the rule is in",
      "rig errors runs the rig could not score (unscored), left out of both counts",
      "ran=false  a tester-proof run the bun stand-in saw no greet.ts run in: read its transcript",
      "           before believing it, since the stand-in sees only a bun reached through PATH",
    ].join("\n")
  );
}

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
