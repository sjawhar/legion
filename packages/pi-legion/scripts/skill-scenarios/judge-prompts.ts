// The ten judge prompts of the spec-writing and spec-rewrite scenarios, and a zod schema for the
// answer of each. judge.ts writes each prompt to prompt.txt in a directory that holds only that
// prompt's inputs, starts a fresh `omp -p` there (rig.sh judge-omp) and validates the answer with
// the schema. Each prompt names the one file the judge writes. These texts were written before
// any run: never change one to make a result appear. A change that corrects a fact is recorded,
// with its old and new text, in the pull request that makes it.
import path from "node:path";
import { z } from "zod";

/** The five fault rows of the spec's Problem table, which the faults judge counts. */
export const FAULT_ROWS = [
  "The reader cannot follow a sentence",
  "A term that nobody explained",
  "Too much text, or facts that do not matter",
  "Vague phrasing that hides who did not act",
  "History in place of design",
] as const;

/** faults.md: the five fault rows, each with what a fault of that row is. */
export const FAULTS_MD = `# The five faults

Count each fault of these five rows. Write each row's name exactly as this table writes it.

| Row | A fault of this row |
| --- | --- |
| ${FAULT_ROWS[0]} | A sentence the reader cannot follow. Its structure, its length or its words hide what it says. |
| ${FAULT_ROWS[1]} | A term nobody explained. The text uses a word or a name that a reader may not know, and it does not explain the term where the term first appears. |
| ${FAULT_ROWS[2]} | Too much text, or facts that do not matter. A passage that does not help a reader understand the problem, make a decision, build the work or check it. |
| ${FAULT_ROWS[3]} | Vague phrasing that hides who did not act. A sentence that does not say which person or program must act, or did not act. An example: "Nothing does the last step." |
| ${FAULT_ROWS[4]} | History in place of design. A passage that tells how people reached the design, such as who said what and when, in place of what the design is. |
`;

/** rule.md: the rule the shared-runs judge applies to one shared run. */
export const SHARED_RUNS_RULE = `# The rule

The verdict is \`names\` when the shared words only name what the design must name too. Such words are items that the person's input also lists, or the name of a product, a file, a command or a field.

Else the verdict is \`copies\`.
`;

/** The profile a judge runs in: `ste` has one user skill, asd-ste100. `bare` has no skills. Neither
 * has plugins. */
export type JudgeProfile = "ste" | "bare";

export interface JudgePrompt<T> {
  /** The base name of the judge's directory. */
  name: string;
  /** The file in that directory that the judge writes its answer to. */
  file: string;
  profile: JudgeProfile;
  schema: z.ZodType<T>;
  /** The prompt, for a judge whose directory is `dir`. */
  text: (dir: string) => string;
}

const count = z.number().int().min(0);
const words = z.string().min(1);

/** The last lines of every prompt: the one file to write, and the answer's shape. */
function ending(dir: string, file: string, shape: string): string {
  return [
    "Write your answer as one JSON object to this file:",
    "",
    path.join(dir, file),
    "",
    "Write no other file. Change no file in this directory. The object has this shape:",
    "",
    shape,
  ].join("\n");
}

const MESSAGES =
  "- `messages.md`: what the people wrote on the issue, oldest first. Each entry starts with a line `<!-- message <time> -->` or `<!-- comment <time> -->`. The `>` lines directly after a `<!-- comment <time> anchored -->` line quote the spec that the comment is about. They are not the person's words. Every other line is the person's words.";
const SOURCE_ASKS =
  "- `asks.json`: the questions that the people answered before the agent started. Each question holds its options and the person's answer (`answer.selected` and `answer.text`).";

export const AnswerOutput = z.object({ selected: z.array(z.string()), text: words });
export const answerPrompt: JudgePrompt<z.infer<typeof AnswerOutput>> = {
  name: "answer",
  file: "answer.json",
  profile: "ste",
  schema: AnswerOutput,
  text: (dir) =>
    [
      "You stand in for a person in a design conversation. An agent designs a change with this person on a Dispatch issue. The agent asked the person one question. Answer the question as the person.",
      "",
      "Read only the files in this directory:",
      MESSAGES,
      SOURCE_ASKS,
      "- `ask.json`: the question to answer now, with its options. `multiple` is true when the person can choose more than one option.",
      "",
      "Choose the answer in this order:",
      "1. If `messages.md` or `asks.json` supports one option, choose that option.",
      "2. Else, if the question recommends an option, choose the recommended option.",
      "3. Else, choose the first option.",
      "",
      "Choose more than one option only when `multiple` is true and the input supports each option that you choose. Copy each label exactly as `ask.json` writes it. If the question has no options, write an empty `selected` list and answer in `text`.",
      "",
      "`text` is one sentence that gives the reason for the choice. Do not add a fact that the input does not give. Do not ask a question back.",
      "",
      ending(dir, "answer.json", '{"selected":["<option label>"],"text":"<one sentence>"}'),
    ].join("\n"),
};

/** faults.json. Each row's count equals the items that name the row: score.ts `coverage` holds
 * both judge.ts and the score to that. */
export const FaultsOutput = z.object({
  faults: z.record(z.enum(FAULT_ROWS), count),
  ste_breaks: count,
  items: z.array(z.object({ row: z.enum(FAULT_ROWS), quote: words })),
});
export const faultsPrompt: JudgePrompt<z.infer<typeof FaultsOutput>> = {
  name: "faults",
  file: "faults.json",
  profile: "ste",
  schema: FaultsOutput,
  text: (dir) =>
    [
      "You count the faults in the writing of a design spec. Use the asd-ste100 skill: read `skill://asd-ste100` before you start.",
      "",
      "Read only the asd-ste100 skill and the files in this directory:",
      "- `spec.md`: the spec.",
      "- `faults.md`: the five rows of faults to count.",
      "",
      "Read `spec.md` whole, as a person reads it who did not take part in the design. For each passage that has a fault of a row in `faults.md`, add one item. The item holds the row's name exactly as `faults.md` writes it, and a quote of the passage copied from `spec.md`. A passage can have faults of more than one row. Then each `faults` value is the number of items of that row.",
      "",
      'The `From:` part of a requirement names its source, and the spec\'s outline requires it. It is not a fault of any row. An approved spec writes its sources as "From: your answer on how far this goes".',
      "",
      "`ste_breaks` is the number of sentences in `spec.md` that break a structural rule of the asd-ste100 skill. The skill's table of structural rules lists these rules.",
      "",
      ending(
        dir,
        "faults.json",
        `{"faults":{${FAULT_ROWS.map((row) => `"${row}":<n>`).join(",")}},"ste_breaks":<n>,"items":[{"row":"<row>","quote":"<passage>"}]}`
      ),
    ].join("\n"),
};

export const DecisionsOutput = z.object({
  rows: z.array(
    z.object({
      choice: words,
      recorded: z.boolean(),
      reason: z.boolean(),
      options: z.boolean(),
      parts: z.boolean(),
    })
  ),
});
export const decisionsPrompt: JudgePrompt<z.infer<typeof DecisionsOutput>> = {
  name: "decisions",
  file: "decisions.json",
  profile: "ste",
  schema: DecisionsOutput,
  text: (dir) =>
    [
      "You check how a design spec records the choices that its agent made alone.",
      "",
      "Read only `spec.md` in this directory. It is the spec. Its `## Requirements` part lists requirements. Each requirement names its source after `From:`, which is a person's answer, comment or message. Its `## Design` part says how the work meets the requirements. Its `## Decisions` part lists the agent's own decisions.",
      "",
      "1. Find each choice that `## Design` makes and that no requirement states. A choice is a point where the design could be different, such as a value, a format, a limit, a tool, an owner, an order or a name.",
      "2. For each choice, find whether an entry of `## Decisions` holds it (`recorded`).",
      "3. For a recorded choice, find whether that entry gives the reason for the choice (`reason`), the other options that the agent weighed (`options`) and the parts of the spec that the choice shaped (`parts`). For a choice that is not recorded, write false for all three.",
      "",
      "Write one row for each choice. If the Design makes no such choice, write an empty list.",
      "",
      ending(
        dir,
        "decisions.json",
        '{"rows":[{"choice":"<the choice, in a few words>","recorded":true|false,"reason":true|false,"options":true|false,"parts":true|false}]}'
      ),
    ].join("\n"),
};

export const FindingsOutput = z.object({
  rows: z.array(
    z.object({
      check: z.number().int().min(1),
      finding: words,
      verdict: z.enum(["fixed", "recorded", "neither"]),
    })
  ),
});
export const findingsPrompt: JudgePrompt<z.infer<typeof FindingsOutput>> = {
  name: "findings",
  file: "findings.json",
  profile: "ste",
  schema: FindingsOutput,
  text: (dir) =>
    [
      "An agent wrote a design spec. Before it wrote some versions of the spec, a separate agent checked the spec. You find whether the agent dealt with each finding of each check.",
      "",
      "Read only the files in this directory. Each directory `check-<n>` holds one check:",
      "- `assignment.md`: the text that the agent gave to the checking agent.",
      "- `report.md`: the checking agent's report.",
      "- `spec-after.md`: the version of the spec that the agent wrote after the check.",
      "",
      "For each check, list each finding of `report.md`. A finding is a gap, a conflict, a duplicate, a requirement that the checker cannot check, a term that the spec does not explain, a sentence that the checker cannot follow, a question that the spec does not ask, or a question that is not needed. A report that finds nothing gives no rows.",
      "",
      "For each finding, give one verdict from `spec-after.md`:",
      "- `fixed`: `spec-after.md` no longer has the fault that the finding names.",
      "- `recorded`: `spec-after.md` keeps the fault, and an entry of its `## Decisions` part names the finding and gives a reason.",
      "- `neither`: the finding is not fixed and not recorded.",
      "",
      "`check` is the number `<n>` of the check's directory.",
      "",
      ending(
        dir,
        "findings.json",
        '{"rows":[{"check":<n>,"finding":"<the finding, in a few words>","verdict":"fixed"|"recorded"|"neither"}]}'
      ),
    ].join("\n"),
};

export const CheckableOutput = z.object({
  rows: z.array(
    z.object({
      requirement: z.string().regex(/^R\d+$/),
      check: words,
      uncheckable: z.boolean(),
    })
  ),
});
export const checkablePrompt: JudgePrompt<z.infer<typeof CheckableOutput>> = {
  name: "checkable",
  file: "checkable.json",
  profile: "ste",
  schema: CheckableOutput,
  text: (dir) =>
    [
      "You are an agent that must check finished work against a design spec. A person approved the spec. You get the spec and nothing else.",
      "",
      "Read only `spec.md` in this directory. Its `## Requirements` part lists requirements, and the mark `**R<n>.**` starts each one.",
      "",
      "For each requirement, write how you will check that the finished work meets it: what you run, read or measure, and which result shows that the work meets it. If the spec does not let you say how to check a requirement, write `uncheckable` true and give the reason in `check`. Use only `spec.md`. Do not guess a fact that the spec does not give.",
      "",
      "Write one row for each requirement, in the order of the spec. Write the requirement's mark without its stars and its full stop, for example `R3`.",
      "",
      ending(
        dir,
        "checkable.json",
        '{"rows":[{"requirement":"R<n>","check":"<how you check it>","uncheckable":true|false}]}'
      ),
    ].join("\n"),
};

export const QUESTION_KINDS = ["authority", "taste", "risk", "answerable"] as const;
export const QuestionsOutput = z.object({
  rows: z.array(
    z.object({
      id: words,
      dominated: z.boolean(),
      settled: z.boolean(),
      kind: z.enum(QUESTION_KINDS),
      reason: words,
    })
  ),
});
export const questionsPrompt: JudgePrompt<z.infer<typeof QuestionsOutput>> = {
  name: "questions",
  file: "questions.json",
  profile: "ste",
  schema: QuestionsOutput,
  text: (dir) =>
    [
      "An agent designs a change with a person on a Dispatch issue. The agent asked the person questions. You judge each question.",
      "",
      "Read only the files in this directory:",
      "- `agent-asks.json`: the agent's questions, oldest first. Each holds its id, its options, its state and the person's answer when there is one.",
      MESSAGES,
      SOURCE_ASKS,
      "",
      "For each question in `agent-asks.json`, give:",
      "- `dominated`: true when one option is at least as good as every other option on every point that the person cares about. `messages.md` and `asks.json` show what the person cares about.",
      "- `settled`: true when an earlier answer settles the question. An earlier answer is an answer in `asks.json`, or an answer in `agent-asks.json` to a question that the agent asked before this one.",
      "- `kind`: the one kind that fits best:",
      "  - `authority`: only the person may decide it.",
      "  - `taste`: it turns on what the person likes.",
      "  - `risk`: it turns on how much risk the person accepts.",
      "  - `answerable`: the agent could propose the answer itself, from the input, the code or an estimate. A number is answerable in this way.",
      "- `reason`: one sentence that gives the reason for your verdicts.",
      "",
      "Write one row for each question, by its id.",
      "",
      ending(
        dir,
        "questions.json",
        '{"rows":[{"id":"<question id>","dominated":true|false,"settled":true|false,"kind":"authority"|"taste"|"risk"|"answerable","reason":"<one sentence>"}]}'
      ),
    ].join("\n"),
};

export const NumbersOutput = z.object({
  rows: z.array(z.object({ id: words, names_number: z.boolean(), quote: z.string().nullish() })),
});
export const numbersPrompt: JudgePrompt<z.infer<typeof NumbersOutput>> = {
  name: "numbers",
  file: "numbers.json",
  profile: "ste",
  schema: NumbersOutput,
  text: (dir) =>
    [
      "You read the texts that an agent sent to a person. You find each text that names a requirement or a check by its number.",
      "",
      "Read only `agent-texts.json` in this directory. It holds each message, comment and question that the agent sent, each with its id and its body.",
      "",
      'A text names a requirement or a check by its number when it writes a mark such as `R3`, `R2 to R5`, `A6` or "requirement 4" in place of saying what the requirement or the check is. A number that is not the number of a requirement or a check is not such a mark. Examples are a count, a version or an issue key such as `ACME-12`.',
      "",
      "Write one row for each text, by its id. For a text that names a requirement or a check by its number, `quote` holds the words that name it. Else `quote` is null.",
      "",
      ending(
        dir,
        "numbers.json",
        '{"rows":[{"id":"<text id>","names_number":true|false,"quote":"<the words>"|null}]}'
      ),
    ].join("\n"),
};

export const PREAMBLE_KINDS = ["retold", "dated", "self"] as const;
export const PreambleOutput = z.object({
  rows: z.array(z.object({ source: words, passage: words, kind: z.enum(PREAMBLE_KINDS) })),
});
export const preamblePrompt: JudgePrompt<z.infer<typeof PreambleOutput>> = {
  name: "preamble",
  file: "preamble.json",
  profile: "ste",
  schema: PreambleOutput,
  text: (dir) =>
    [
      "An agent wrote a design spec and other texts for a person. You find each passage that talks about the conversation in place of the design.",
      "",
      "Read only the files in this directory:",
      "- `spec.md`: the spec.",
      "- `agent-texts.json`: each message, comment and question that the agent sent, each with its id and its body.",
      "- `documents/`: each other document that the agent wrote on the issue. The directory is absent when there is none.",
      `${MESSAGES} These are the person's input.`,
      "",
      "Find each passage of these three kinds in `spec.md`, in `agent-texts.json` and in `documents/`:",
      "- `retold`: it quotes or retells the person's input to the person.",
      '- `dated`: it dates a person\'s words, as "On 20 October you said" does.',
      "- `self`: it says who the writing agent session is, by its id, its name or its title.",
      "",
      'The `From:` part of a requirement names its source. It is not a passage of any kind. An approved spec writes its sources as "From: your answer on how far this goes". A passage that states the design is not `retold`, also when the design does what the person asked.',
      "",
      "`source` names where the passage is: `spec.md`, `documents/<file>`, or `agent-texts.json#<id>`. `passage` is the passage, copied from its text. If you find no passage, write an empty list.",
      "",
      ending(
        dir,
        "preamble.json",
        '{"rows":[{"source":"<where>","passage":"<the passage>","kind":"retold"|"dated"|"self"}]}'
      ),
    ].join("\n"),
};

export const PROPOSAL_VERDICTS = ["proposed", "asked", "missing"] as const;
export const ProposalOutput = z.object({
  rows: z.array(
    z.object({ item: words, verdict: z.enum(PROPOSAL_VERDICTS), ask: z.string().nullish() })
  ),
});
export const proposalPrompt: JudgePrompt<z.infer<typeof ProposalOutput>> = {
  name: "proposal",
  file: "proposal.json",
  profile: "ste",
  schema: ProposalOutput,
  text: (dir) =>
    [
      "An agent designs a change with a person. You find whether the design gives each value that it needs.",
      "",
      "Read only the files in this directory:",
      "- `spec.md`: the spec.",
      "- `agent-asks.json`: the agent's questions to the person, each with its id.",
      "",
      "Find each number, limit, schedule or owner that the design needs. The design needs a value when another agent cannot build the work or check it without that value. For each one, give the first verdict that fits, in this order:",
      "- `asked`: a question in `agent-asks.json` asks the person for it. `ask` is that question's id.",
      "- `proposed`: `spec.md` gives a value and the reason for that value.",
      "- `missing`: neither. A value that the spec gives with no reason is missing.",
      "",
      "`ask` is null for a verdict other than `asked`. If the design needs no such value, write an empty list.",
      "",
      ending(
        dir,
        "proposal.json",
        '{"rows":[{"item":"<the value the design needs, in a few words>","verdict":"proposed"|"asked"|"missing","ask":"<question id>"|null}]}'
      ),
    ].join("\n"),
};

export const SharedRunOutput = z.object({
  run: z.string(),
  verdict: z.enum(["names", "copies"]),
  reason: words,
});
export const sharedRunPrompt: JudgePrompt<z.infer<typeof SharedRunOutput>> = {
  name: "shared-runs",
  file: "shared-run.json",
  profile: "bare",
  schema: SharedRunOutput,
  text: (dir) =>
    [
      "An agent wrote a text for a person. The text and the person's input share a run of words. You decide whether the text names what the design must name, or copies the person's words.",
      "",
      "Read only the files in this directory:",
      "- `run.txt`: the shared words, in lower case, with no punctuation.",
      "- `context.md`: the sentences that hold the run, with one sentence on each side. It gives them first in the agent's text and then in the person's input.",
      "- `rule.md`: the rule for your verdict.",
      "",
      "Apply the rule of `rule.md` to the run. Give one verdict, and one sentence that gives the reason. `run` holds the words of `run.txt`.",
      "",
      ending(
        dir,
        "shared-run.json",
        '{"run":"<the shared words>","verdict":"names"|"copies","reason":"<one sentence>"}'
      ),
    ].join("\n"),
};

/** The nine prompts `judge.ts run` asks for each run, in the order it writes their files. The
 * tenth, `answer`, runs during the conversation (`judge.ts answer`). */
export const RUN_PROMPTS = [
  faultsPrompt,
  decisionsPrompt,
  findingsPrompt,
  checkablePrompt,
  questionsPrompt,
  numbersPrompt,
  preamblePrompt,
  proposalPrompt,
  sharedRunPrompt,
] as const;

/** checkable.json in the run directory: the judge's rows and the approved version it read (null
 * when the run ended with no approval, and then the judge did not run). */
export const CheckableFile = CheckableOutput.extend({ version: z.number().int().nullable() });
/** shared-runs.json in the run directory: one row for each run that `sharedRuns` lists, its text
 * as `sharedRuns` gives it and the judge's verdict. */
export const SharedRunsFile = z.object({
  rows: z.array(
    z.object({
      source: words,
      run: words,
      verdict: z.enum(["names", "copies"]),
      reason: words,
    })
  ),
});
