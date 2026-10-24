#!/usr/bin/env bun
// Runs the judges of the spec-writing and spec-rewrite scenarios (judge-prompts.ts). Each judge is a
// fresh `omp -p` that rig.sh judge-omp starts in a directory of its own,
// <work>/judging/<run>/<prompt>[-<n>]/, which holds only that prompt's inputs and prompt.txt. The
// judge writes its answer as JSON into that directory. judge.ts validates it and copies it into the
// run directory. A judge that writes no answer, or one that does not parse, fails the command,
// naming the file.
//
//   judge.ts answer <run dir> <turn>
//     The person stand-in: one `answer` judge for each open ask in state-<turn>.json, given the
//     source's messages.md and asks.json. Writes answers-<turn>.jsonl, one line per ask
//     ({"ask","selected","text"}), and an empty file when no ask is open.
//   judge.ts run <run dir>...
//     The nine judges of each run (faults, decisions, findings, checkable, questions, numbers,
//     preamble, proposal, and shared-runs once for each shared run that score.ts's sharedRuns
//     lists). Writes faults.json, decisions.json, findings.json, checkable.json, questions.json,
//     numbers.json, preamble.json, proposal.json and shared-runs.json into each run directory.
//
// <work> is the directory that holds the run's `runs/` directory, as score.ts reads it. Seeded asks
// (seeded.json) never reach a judge's inputs.
import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  answerPrompt,
  CheckableFile,
  checkablePrompt,
  decisionsPrompt,
  FAULTS_MD,
  faultsPrompt,
  findingsPrompt,
  type JudgeProfile,
  type JudgePrompt,
  numbersPrompt,
  preamblePrompt,
  proposalPrompt,
  questionsPrompt,
  SHARED_RUNS_RULE,
  SharedRunsFile,
  sharedRunPrompt,
} from "./judge-prompts";
import {
  AgentAsks,
  AgentTexts,
  approval,
  checkouts,
  SpecVersions,
  State,
  seededIds,
  sharedRuns,
  specWork,
} from "./score";
import { session, sessionId } from "./transcript";

/** Starts one judge in `dir` under `profile` and resolves to its exit status. */
export type JudgeRunner = (profile: JudgeProfile, dir: string) => Promise<number>;
export interface JudgeOptions {
  runner?: JudgeRunner;
  /** How many judges of a run run at once. */
  parallel?: number;
}

const here = import.meta.dir;
/** The production runner: `bash rig.sh judge-omp <profile> <dir>`, with this process's
 * environment, so the rig's work directory and the operator's model route reach it. */
const rigRunner: JudgeRunner = async (profile, dir) => {
  const child = Bun.spawn(["bash", path.join(here, "rig.sh"), "judge-omp", profile, dir], {
    stdout: "inherit",
    stderr: "inherit",
  });
  return await child.exited;
};

/** The work directory of a run under <work>/runs/<run>. */
function workOf(runDir: string): string {
  const runs = path.dirname(path.resolve(runDir));
  if (path.basename(runs) !== "runs")
    throw new Error(`${runDir} is not under a runs/ directory of the rig's work directory`);
  return path.dirname(runs);
}
function read<T>(file: string, schema: z.ZodType<T>): T {
  return schema.parse(JSON.parse(readFileSync(file, "utf8")));
}

/** One judge to run: its directory's name, its prompt and its input files (relative path to
 * content). */
interface Job<T> {
  dir: string;
  prompt: JudgePrompt<T>;
  inputs: Record<string, string>;
}
/** Builds the job's directory afresh with only its inputs and prompt.txt, runs the judge in it, and
 * returns its validated answer. */
async function runJob<T>(job: Job<T>, runner: JudgeRunner): Promise<T> {
  rmSync(job.dir, { recursive: true, force: true });
  mkdirSync(job.dir, { recursive: true });
  for (const [file, text] of Object.entries(job.inputs)) {
    mkdirSync(path.dirname(path.join(job.dir, file)), { recursive: true });
    writeFileSync(path.join(job.dir, file), text);
  }
  writeFileSync(path.join(job.dir, "prompt.txt"), job.prompt.text(job.dir));
  const status = await runner(job.prompt.profile, job.dir);
  const answer = path.join(job.dir, job.prompt.file);
  if (!existsSync(answer))
    throw new Error(`${answer} is missing: the judge wrote no answer (exit ${status})`);
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(answer, "utf8"));
  } catch (error) {
    throw new Error(`${answer} does not parse: ${error instanceof Error ? error.message : error}`);
  }
  const result = job.prompt.schema.safeParse(parsed);
  if (!result.success)
    throw new Error(`${answer} does not parse: ${z.prettifyError(result.error)}`);
  return result.data;
}
/** Runs every job, at most `parallel` at once, and resolves to their answers in job order. */
async function runAll<T>(jobs: Job<T>[], runner: JudgeRunner, parallel: number): Promise<T[]> {
  const answers: T[] = new Array(jobs.length);
  let next = 0;
  const lanes = Array.from({ length: Math.min(parallel, jobs.length) }, async () => {
    while (next < jobs.length) {
      const index = next++;
      const job = jobs[index];
      if (job !== undefined) answers[index] = await runJob(job, runner);
    }
  });
  await Promise.all(lanes);
  return answers;
}

/** The source's input files a judge reads: messages.md and asks.json. */
function sourceInputs(runDir: string): Record<string, string> {
  return {
    "messages.md": readFileSync(path.join(runDir, "source", "messages.md"), "utf8"),
    "asks.json": readFileSync(path.join(runDir, "source", "asks.json"), "utf8"),
  };
}
const pretty = (value: unknown) => `${JSON.stringify(value, null, 2)}\n`;

/** `judge.ts answer`: the stand-in answers each open ask of the turn, one judge per ask. */
export async function answerTurn(runDir: string, turn: number, options: JudgeOptions = {}) {
  const runner = options.runner ?? rigRunner;
  const run = path.basename(path.resolve(runDir));
  const judging = path.join(workOf(runDir), "judging", run);
  const seeded = seededIds(runDir);
  const open = read(path.join(runDir, `state-${turn}.json`), State).open_asks.filter(
    (ask) => !seeded.has(ask.id)
  );
  const jobs = open.map((ask, index) => {
    const labels = (ask.options ?? []).map((option) => option.label);
    // The stand-in's label must be one the ask offers, and one label unless the ask takes several.
    const schema = answerPrompt.schema.superRefine((value, ctx) => {
      for (const label of value.selected)
        if (!labels.includes(label))
          ctx.addIssue({ code: "custom", message: `"${label}" is not a label of ask ${ask.id}` });
      if (labels.length > 0 && value.selected.length === 0)
        ctx.addIssue({ code: "custom", message: `no label of ask ${ask.id} is selected` });
      if (ask.multiple !== true && value.selected.length > 1)
        ctx.addIssue({ code: "custom", message: `ask ${ask.id} takes one label` });
    });
    return {
      dir: path.join(judging, `answer-${turn}-${index + 1}`),
      prompt: { ...answerPrompt, schema },
      inputs: {
        ...sourceInputs(runDir),
        "ask.json": pretty({
          question: ask.question,
          options: ask.options ?? [],
          multiple: ask.multiple ?? false,
        }),
      },
    };
  });
  const answers = await runAll(jobs, runner, options.parallel ?? 5);
  writeFileSync(
    path.join(runDir, `answers-${turn}.jsonl`),
    answers
      .map((answer, index) =>
        JSON.stringify({ ask: open[index]?.id, selected: answer.selected, text: answer.text })
      )
      .map((line) => `${line}\n`)
      .join("")
  );
}

/** The spec version each spec-only check led to: the first version the run's session wrote after
 * the write that followed the check. */
function specAfter(runDir: string, writeAt: string, own: string): string {
  const versions = read(path.join(runDir, "spec-versions.json"), SpecVersions)
    .filter((version) =>
      (version.authors ?? (version.actor ? [version.actor] : [])).some(
        (author) => author.kind === "session" && author.id === own
      )
    )
    .filter((version) => Date.parse(version.created_at) >= Date.parse(writeAt))
    .sort((a, b) => a.version - b.version);
  const version = versions[0];
  if (version === undefined)
    throw new Error(`spec-versions.json holds no version the session wrote at or after ${writeAt}`);
  return readFileSync(path.join(runDir, "spec-versions", `${version.version}.md`), "utf8");
}

/** `judge.ts run`: the nine judges of one run. */
export async function judgeRun(runDir: string, options: JudgeOptions = {}) {
  const runner = options.runner ?? rigRunner;
  const parallel = options.parallel ?? 5;
  const work = workOf(runDir);
  const run = path.basename(path.resolve(runDir));
  const judging = path.join(work, "judging", run);
  const seeded = seededIds(runDir);
  const file = (name: string) => readFileSync(path.join(runDir, name), "utf8");
  const spec = file("spec.md");
  const entries = session(runDir);
  const own = sessionId(entries);
  if (own === undefined) throw new Error(`${runDir}: the transcript has no session header`);
  const fixture = read(
    path.join(runDir, "fixture.json"),
    z.looseObject({ spec_artifact: z.string() })
  );
  const asks = read(path.join(runDir, "agent-asks.json"), AgentAsks)
    .filter((ask) => ask.kind !== "approval" && !seeded.has(ask.id))
    .map((ask, index) => ({ ask, index }))
    .sort(
      (a, b) => (a.ask.created_at ?? "").localeCompare(b.ask.created_at ?? "") || a.index - b.index
    )
    .map(({ ask }) => ask);
  const texts = read(path.join(runDir, "agent-texts.json"), AgentTexts).filter(
    (text) => !seeded.has(text.id)
  );
  const documents = path.join(runDir, "documents");
  const documentFiles = existsSync(documents)
    ? Object.fromEntries(
        (await Array.fromAsync(new Bun.Glob("*").scan({ cwd: documents })))
          .sort()
          .map((name) => [`documents/${name}`, readFileSync(path.join(documents, name), "utf8")])
      )
    : {};
  const { checks, writes } = specWork(entries, checkouts(work), fixture.spec_artifact);
  const findingsInputs = Object.fromEntries(
    checks.flatMap((check, index) => [
      [`check-${index + 1}/assignment.md`, `${check.assignment}\n`],
      [`check-${index + 1}/report.md`, `${check.report}\n`],
      [`check-${index + 1}/spec-after.md`, specAfter(runDir, writes[check.write]?.at ?? "", own)],
    ])
  );
  const { approved, version } = approval(runDir);
  const approvedSpec =
    approved && version !== null ? file(path.join("spec-versions", `${version}.md`)) : undefined;

  const single: Job<unknown>[] = [
    { dir: "faults", prompt: faultsPrompt, inputs: { "spec.md": spec, "faults.md": FAULTS_MD } },
    { dir: "decisions", prompt: decisionsPrompt, inputs: { "spec.md": spec } },
    { dir: "findings", prompt: findingsPrompt, inputs: findingsInputs },
    ...(approvedSpec === undefined
      ? []
      : [{ dir: "checkable", prompt: checkablePrompt, inputs: { "spec.md": approvedSpec } }]),
    {
      dir: "questions",
      prompt: questionsPrompt,
      inputs: { "agent-asks.json": pretty(asks), ...sourceInputs(runDir) },
    },
    { dir: "numbers", prompt: numbersPrompt, inputs: { "agent-texts.json": pretty(texts) } },
    {
      dir: "preamble",
      prompt: preamblePrompt,
      inputs: {
        "spec.md": spec,
        "agent-texts.json": pretty(texts),
        ...documentFiles,
        "messages.md": file(path.join("source", "messages.md")),
      },
    },
    {
      dir: "proposal",
      prompt: proposalPrompt,
      inputs: { "spec.md": spec, "agent-asks.json": pretty(asks) },
    },
  ].map((job) => ({ ...job, dir: path.join(judging, job.dir) }) as Job<unknown>);
  const runs = sharedRuns(runDir);
  const shared = runs.map((found, index) => ({
    dir: path.join(judging, `shared-runs-${index + 1}`),
    prompt: sharedRunPrompt,
    inputs: {
      "run.txt": `${found.run}\n`,
      "context.md": `# The agent's text (${found.source})\n\n${found.text}\n\n# The person's input\n\n${found.input}\n`,
      "rule.md": SHARED_RUNS_RULE,
    },
  }));
  const [singleAnswers, sharedAnswers] = await Promise.all([
    runAll(single, runner, parallel),
    runAll(shared, runner, parallel),
  ]);
  for (const [index, job] of single.entries()) {
    const answer = singleAnswers[index];
    const out = path.join(runDir, job.prompt.file);
    if (job.prompt === checkablePrompt)
      writeFileSync(out, pretty(CheckableFile.parse({ ...(answer as object), version })));
    else cpSync(path.join(job.dir, job.prompt.file), out);
  }
  if (approvedSpec === undefined)
    writeFileSync(
      path.join(runDir, checkablePrompt.file),
      pretty(CheckableFile.parse({ rows: [], version: null }))
    );
  writeFileSync(
    path.join(runDir, "shared-runs.json"),
    pretty(
      SharedRunsFile.parse({
        rows: runs.map((found, index) => ({
          source: found.source,
          run: found.run,
          verdict: sharedAnswers[index]?.verdict,
          reason: sharedAnswers[index]?.reason,
        })),
      })
    )
  );
}

if (import.meta.main) {
  const [command, ...rest] = Bun.argv.slice(2);
  const parallel = Number(process.env.SKILL_SCENARIOS_PARALLEL ?? 5) || 5;
  if (command === "answer" && rest.length === 2 && /^\d+$/.test(rest[1] ?? "")) {
    await answerTurn(rest[0] ?? "", Number(rest[1]), { parallel });
  } else if (command === "run" && rest.length >= 1) {
    for (const runDir of rest) {
      await judgeRun(runDir, { parallel });
      console.log(`${path.basename(path.resolve(runDir))}: judged`);
    }
  } else {
    console.error("usage: judge.ts answer <run dir> <turn> | run <run dir>...");
    process.exit(2);
  }
}
