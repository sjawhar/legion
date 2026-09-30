#!/usr/bin/env bun
// Scores what rig.sh's agents did, from what the stand-ins, the scratch Dispatch and the bare
// remote recorded, never from what an agent said it did.
//
//   score.ts live-read <run dir> <skills dir> <omp logs dir>
//     One row per `read` the session made: the characters it got back, the file's own character
//     count less its trailing newline, whether the result spilled to an artifact, and so whether
//     the file arrived whole; then the plugin paths Oh My Pi's log says it loaded.
//   score.ts runs <runs dir> [<scenario>]
//     One row per run, then pass counts per scenario and label. Each ask-on-message ask is printed
//     whole, since the pass rule is read by a person: the automatic flags only point at it. A run
//     the rig failed (no `exit=` line in its out.txt, or a record missing or malformed) is a rig
//     error: printed with its reason and left out of the pass counts.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describePhaseHandoffWriteProblems } from "@legion/contracts";
import { z } from "zod";

/** An Oh My Pi session transcript line: a tool call starting, or a tool's result. */
const SessionEntry = z.looseObject({
  type: z.string().optional(),
  customType: z.string().optional(),
  data: z
    .looseObject({
      toolName: z.string().optional(),
      toolCallId: z.string().optional(),
      args: z.looseObject({ path: z.string().optional() }).optional(),
    })
    .optional(),
  message: z
    .looseObject({
      role: z.string().optional(),
      toolName: z.string().optional(),
      toolCallId: z.string().optional(),
      content: z.array(z.looseObject({ type: z.string(), text: z.string().optional() })).optional(),
    })
    .optional(),
});
type SessionEntry = z.infer<typeof SessionEntry>;
/** `GET /api/v1/issues/{key}/asks`: every ask on the issue, whatever its state. */
const IssueAsks = z.array(
  z.looseObject({
    question: z.string(),
    options: z
      .array(z.looseObject({ label: z.string(), description: z.string().nullish() }))
      .nullish(),
  })
);
/** `GET /api/v1/issues/{key}/events`, every page `seed.ts capture` read, joined: a bare array. */
const IssueEvents = z.array(
  z.looseObject({
    type: z.string(),
    actor: z.looseObject({ kind: z.string(), id: z.string().nullish() }).nullish(),
  })
);
/** `GET /api/v1/issues/{key}/messages/{id}`: the message and its replies. */
const MessageRead = z.looseObject({ message: z.looseObject({ body: z.string() }) });
/** One line gh-standin.ts or legion-standin.sh records: when the call started, and its exit. */
const Call = z.looseObject({
  at: z.string(),
  as: z.string(),
  argv: z.array(z.string()),
  exit: z.number(),
  stdin: z.string().optional(),
  files: z.record(z.string(), z.string()).optional(),
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

function session(runDir: string): SessionEntry[] {
  const dir = path.join(runDir, "sessions");
  const file = existsSync(dir)
    ? readdirSync(dir).find((name) => name.endsWith(".jsonl"))
    : undefined;
  return file ? parsed(path.join(dir, file), SessionEntry) : [];
}
/** Every `read` call the session started, in order. */
function readCalls(entries: SessionEntry[]): { id: string; target: string }[] {
  return entries
    .filter(
      (entry) =>
        entry.type === "custom" &&
        entry.customType === "tool_execution_start" &&
        entry.data?.toolName === "read"
    )
    .map((entry) => ({ id: entry.data?.toolCallId ?? "", target: entry.data?.args?.path ?? "" }));
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

function askOnMessage(runDir: string, run: string, label: string): Row {
  const asks = json(path.join(runDir, "asks.json"), IssueAsks);
  // Anything else the agent wrote on the issue; seed.ts writes as the session `dispatch-owner`.
  const other = json(path.join(runDir, "events.json"), IssueEvents)
    .filter((event) => event.actor?.id !== "dispatch-owner" && !event.type.startsWith("ask."))
    .map((event) => event.type);
  const message = json(path.join(runDir, "message.json"), MessageRead);
  const wanted = terms(message.message.body);
  const verdicts = asks.map((ask) => {
    const question = ask.question;
    const options = ask.options ?? [];
    const all = [question, ...options.map((o) => `${o.label} ${o.description ?? ""}`)].join("\n");
    const carried = [...terms(all)].filter((word) => wanted.has(word));
    const pointer = POINTER.exec(all)?.[0];
    const pass = carried.length >= 4 && options.length >= 2 && pointer === undefined;
    return { question, options, carried, pointer, pass };
  });
  const pass = verdicts.length > 0 && verdicts.every((verdict) => verdict.pass);
  const skillReads = readCalls(session(runDir))
    .map((call) => call.target)
    .filter((target) => target.startsWith("skill://"));
  // The rule is in the dispatch skill's SKILL.md.
  const ref = skillReads.some((target) => /^skill:\/\/dispatch(\/SKILL\.md)?$/.test(target));
  const notes = [
    `asks=${asks.length}`,
    ...verdicts.map(
      (v, i) =>
        `ask${i + 1}: carried=${v.carried.length}[${v.carried.join(",")}] options=${v.options.length} pointer=${JSON.stringify(v.pointer ?? null)}`
    ),
    `otherWrites=[${other.join(", ")}]`,
    `reads=[${skillReads.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  for (const [i, v] of verdicts.entries()) {
    const shown = v.options.map((o) => `  - ${o.label}: ${o.description ?? ""}`);
    notes.push(`\n  ask${i + 1} question: ${v.question}\n${shown.join("\n")}`);
  }
  return { run, scenario: "ask-on-message", label, pass, ref, notes: notes.join(" ") };
}

/** The tester's handoff as the next phase reads it: `.legion/test.json` at a pushed commit, which
 * must pass the handoff CLI's own write rules and carry a proof of its own. */
function pushedProof(remote: string, sha: string): string[] {
  const shown = Bun.spawnSync(["git", "-C", remote, "show", `${sha}:.legion/test.json`]);
  if (shown.exitCode !== 0) return [`${sha} has no .legion/test.json`];
  const handoff: unknown = JSON.parse(shown.stdout.toString());
  const problems = describePhaseHandoffWriteProblems(handoff);
  if (problems.length > 0) return problems;
  const { phase, proof } = handoff as { phase?: unknown; proof?: unknown };
  if (phase !== "test") return [`phase is ${JSON.stringify(phase)}, not "test"`];
  return Array.isArray(proof) && proof.length > 0 ? [] : ["no proof of the tester's own"];
}

function testerProof(runDir: string, run: string, label: string): Row {
  const world = json(path.join(runDir, "world.json"), World);
  const calls = parsed(path.join(runDir, "calls.jsonl"), Call);
  const handoffs = calls.filter((c) => c.as === "legion" && c.argv[0] === "handoff");
  const writes = handoffs.filter((c) => c.argv[1] === "write" && c.argv.includes("test"));
  // A write or completion the CLI refused never counts.
  const write = writes.find((c) => c.exit === 0);
  const refused = writes.filter((c) => c.exit !== 0).length;
  const complete = handoffs.find((c) => c.argv[1] === "complete" && c.exit === 0);
  /** Everything a gh call handed GitHub: its argv, its stdin, and each file it read a body from. */
  const sent = (c: z.infer<typeof Call>) =>
    [c.argv.join(" "), c.stdin ?? "", ...Object.values(c.files ?? {})].join("\n");
  const edits = calls.filter(
    (c) =>
      c.as === "gh" &&
      c.exit === 0 &&
      new RegExp(`^pr edit|^api .*pulls/${world.pr}`).test(c.argv.join(" ")) &&
      sent(c).includes("E2E (tester)")
  );
  const edit = edits[0];
  // The PR is left with the last edit's body. Its E2E (tester) line, up to the next field, names a
  // head of this run's own; one that names neither is a wrong head or another run's line, as when
  // two concurrent agents write the body to one scratch file in the shared /tmp.
  const last = edits.at(-1);
  const testerLine =
    /\*\*E2E \(tester\):\*\*[\s\S]*?(?=\n\*\*|$)/.exec(last ? sent(last) : "")?.[0] ?? "";
  const ownHead = [...testerLine.matchAll(/\b[0-9a-f]{7,40}\b/g)].some(
    ([sha]) => world.head.startsWith(sha) || world.code.startsWith(sha)
  );
  const remote = path.join(runDir, "remote.git");
  const pushes = lines(path.join(runDir, "pushes.log"))
    .map((line) => {
      const [at = "", ref = "", , sha = ""] = line.split(" ");
      return { at, ref, sha };
    })
    .filter((p) => p.ref === `refs/heads/${world.branch}`);
  const push = pushes.find(
    (p) =>
      Bun.spawnSync(["git", "-C", remote, "cat-file", "-e", `${p.sha}:.legion/test.json`])
        .exitCode === 0
  );
  // The branch as the next phase finds it: its tip when the tester completed, else its last push.
  const tip = pushes.filter((p) => complete === undefined || p.at < complete.at).at(-1);
  const problems = tip === undefined ? ["nothing pushed"] : pushedProof(remote, tip.sha);
  const proof = problems.length === 0;
  const ordered =
    write !== undefined &&
    push !== undefined &&
    complete !== undefined &&
    write.at < push.at &&
    push.at < complete.at;
  const pass = ordered && proof && ownHead;
  const opened = readCalls(session(runDir)).map((call) => call.target);
  const ref = opened.some((target) =>
    target.includes("skill://legion-worker/references/pr-body.md")
  );
  const worker = opened
    .filter((target) => target.includes("legion-worker"))
    .map((target) => target.replace("skill://legion-worker", "") || "/");
  const notes = [
    `write=${write !== undefined} refusedWrites=${refused} proof=${proof} push=${push !== undefined} complete=${complete !== undefined} ordered=${ordered} testerLine=${edit !== undefined} ownHead=${ownHead}`,
    `editBeforeWrite=${edit !== undefined && write !== undefined && edit.at < write.at}`,
    ...(proof ? [] : [`proofProblems=${JSON.stringify(problems)}`]),
    `refs=[${worker.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  return { run, scenario: "tester-proof", label, pass, ref, notes: notes.join(" ") };
}

function scoreRuns(runsDir: string, only: string | undefined) {
  const rows: Row[] = [];
  const errors: { run: string; key: string; reason: string }[] = [];
  for (const run of existsSync(runsDir) ? readdirSync(runsDir).sort() : []) {
    const match = /^(ask-on-message|tester-proof)-(.+)-(\d+)$/.exec(run);
    if (!match) continue;
    const [, scenario = "", label = ""] = match;
    if (only && scenario !== only) continue;
    const dir = path.join(runsDir, run);
    const key = `${scenario}\t${label}`;
    // A run the rig failed never reached its agent's exit, or left a record missing or malformed.
    let reason = lines(path.join(dir, "out.txt")).at(-1)?.startsWith("exit=")
      ? undefined
      : "out.txt has no exit= line: the run never finished";
    if (reason === undefined) {
      try {
        rows.push(
          scenario === "ask-on-message"
            ? askOnMessage(dir, run, label)
            : testerProof(dir, run, label)
        );
      } catch (error) {
        reason = error instanceof Error ? error.message : String(error);
      }
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
