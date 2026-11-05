import { describe, expect, test } from "bun:test";
import {
  proseOf,
  renderSourcingNotice,
  SOURCING_FLAG_CAP,
  sentences,
  sourcingLint,
} from "./sourcing-lint";

// Every sentence below is taken verbatim from a Dispatch write AskGate actually reviewed, read out
// of the `advisor-gate` entries of this machine's session files (the `reviewedText` field, which
// holds the arguments the gate judged). They are the lint's contract: the first set is what it
// exists to catch, the second is text it must leave alone, and both were hand-read before being
// written down here. The measured behaviour over the wider sample of those writes is in the pull
// request.

/** Real sentences that assert a measurement and name nothing a reader can go and check. */
const MUST_FLAG: ReadonlyArray<[string, string]> = [
  ["a ratio", "53 of 65 attempts were graded by 23:45Z on 3 November."],
  ["a percentage", "OSS Verify holds 10,784 vCPUs and keeps 1,363 busy, which is 13%."],
  ["a count with a unit", "Slot-a is at 98,402 of 100,240 jobs."],
  ["a correction to an earlier number", "The right numbers are 298 and 10.3 percent."],
  ["a result reported as a pass", "Two later manual runs failed."],
  ["a suite's result", "Suite on the swapped head: 22 files, 446 tests, 0 failed."],
  [
    "a rate across a population",
    "82.4% of BET CI minutes go to the shared suite, and a one-family change spends 82-95% of its run there.",
  ],
  ["a count of findings", "The red team found 8 errors in this tick's own draft."],
];

/**
 * Real text the lint must stay silent on. Four reasons it must: the sentence names its source, it
 * is already labelled as something other than a measurement, it describes a design rather than
 * reporting a run, or it carries no measurement at all.
 */
const MUST_NOT_FLAG: ReadonlyArray<[string, string]> = [
  [
    "a count whose source is a command",
    "`dispatch read --issue INFRA-515` lists 47 comments on the issue.",
  ],
  [
    "a count beside the file it was read from",
    "packages/pi-envoy/extensions/askgate/askgate-core.ts holds 3,740 lines.",
  ],
  ["a count beside a dispatch key", "INFRA-1239 traced 8 instances of exactly this step."],
  ["a count beside a sha", "At tree-pw 3fedd12 the suite is 627 tests."],
  ["a count beside a pull request", "theorem-labs/machcsl-riscv#52 removes 5 lines per diff."],
  ["an estimate the writer labelled", "Cost: 3-5 agent-days [estimate]."],
  ["an approximation", "The work is about 163 agent-days in 20 parts."],
  ["an inference the writer labelled", "[INFERENCE] 9 of the 12 sessions never read the rule."],
  ["a conditional", "If each attempt scores 0 or 1, a gap of 0.3 or more appears in 11 of 64."],
  ["a requirement", "The branch must keep 3 reviewers on every round."],
  ["an instruction", "Run the full ladder of seven rungs: 35 attempts."],
  [
    "a design described in the present tense",
    "A target whose proof fails scores 0 while the rest of the submission still counts.",
  ],
  ["a question", "How many of the 247 fractional-proof claims are machine-checked today?"],
  ["prose with no measurement in it", "The person cannot tell where the claim comes from."],
];

describe("sourcingLint", () => {
  test.each(MUST_FLAG)("flags %s", (_what, sentence) => {
    expect(sourcingLint(sentence)).toEqual([
      { sentence, kind: expect.stringMatching(/^(?:number|status)$/) },
    ]);
  });

  test.each(MUST_NOT_FLAG)("leaves %s alone", (_what, sentence) => {
    expect(sourcingLint(sentence)).toEqual([]);
  });

  test("the same claim is flagged without its source and silent with it", () => {
    const bare = "The regrade moved 15 of 79 rewrites.";
    expect(sourcingLint(bare)).toHaveLength(1);
    expect(sourcingLint(`${bare.slice(0, -1)}, per dispatch://INFRA-515.`)).toEqual([]);
  });

  test("each flagged sentence of a write is returned, in the order it was written", () => {
    const text = [
      "The suite passed on the first head.",
      "Then 19 of 23 sessions minted a turn on the capped class.",
    ].join(" ");
    expect(sourcingLint(text).map((flag) => flag.sentence)).toEqual([
      "The suite passed on the first head.",
      "Then 19 of 23 sessions minted a turn on the capped class.",
    ]);
  });
});

describe("sentences", () => {
  test("a claim wrapped over several lines is one sentence, not three fragments", () => {
    const wrapped = "The census counts 563 backup files of the 1,370 in-scope\nfiles on the box.";
    expect(sentences(wrapped)).toEqual([
      "The census counts 563 backup files of the 1,370 in-scope files on the box.",
    ]);
  });

  test("a line break written as the two characters of an escape also splits", () => {
    expect(
      sentences("The first run failed on the box.\\nThe second run failed too, later.")
    ).toEqual(["The first run failed on the box.", "The second run failed too, later."]);
  });

  test("fenced code, indented code, a quotation, a heading and a table row are not claims", () => {
    const text = [
      "## 12 of 16 rejections were named",
      "```",
      "19 of 23 sessions minted a turn",
      "```",
      "    19 of 23 sessions minted a turn",
      "> 19 of 23 sessions minted a turn",
      "| level 8 | 2,354 of 2,354 |",
      "Outside all of that, 19 of 23 sessions minted a turn.",
    ].join("\n");
    expect(sentences(text)).toEqual(["Outside all of that, 19 of 23 sessions minted a turn."]);
  });

  test("a list item is its own sentence and keeps none of its marker", () => {
    expect(sentences("- 36 of 40 runs resumed here.\n- 0 of them ran twice overall.")).toEqual([
      "36 of 40 runs resumed here.",
      "0 of them ran twice overall.",
    ]);
  });
});

describe("proseOf", () => {
  test("reads every string of a device's arguments, nested ones included", () => {
    expect(
      proseOf({
        issue: "INFRA-515",
        ops: [{ op: "replace", with: "the suite passed" }],
        count: 3,
        flag: true,
      })
    ).toBe("INFRA-515\nreplace\nthe suite passed");
  });
});

describe("renderSourcingNotice", () => {
  test("nothing flagged, nothing said", () => {
    expect(renderSourcingNotice([])).toBeUndefined();
  });

  test("the note names the sentences, says it blocked nothing, and offers the label", () => {
    const notice = renderSourcingNotice(sourcingLint("The regrade moved 15 of 79 rewrites.")) ?? "";
    expect(notice).toContain('<advisor-gate advisor="AskGate" check="sourcing">');
    expect(notice).toContain("The regrade moved 15 of 79 rewrites.");
    expect(notice).toContain("This blocks nothing");
    expect(notice).toContain("an inference, not an observation");
    expect(notice.endsWith("</advisor-gate>")).toBe(true);
  });

  test("past the cap the note lists the first few and counts the rest", () => {
    const text = Array.from(
      { length: SOURCING_FLAG_CAP + 2 },
      (_unused, index) => `Round ${index} moved ${index + 2} of 79 rewrites in that pass.`
    ).join("\n");
    const flags = sourcingLint(text);
    expect(flags).toHaveLength(SOURCING_FLAG_CAP + 2);
    const notice = renderSourcingNotice(flags) ?? "";
    expect(notice.split("\n").filter((line) => line.startsWith("- "))).toHaveLength(
      SOURCING_FLAG_CAP
    );
    expect(notice).toContain("and 2 more like them");
  });
});
