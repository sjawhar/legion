import { afterAll, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { RECEIPT_TIMEOUT_CAUSE } from "../../../packages/contracts/src/dispatch-api.ts";
import {
  CAUSES,
  CLASS_RULES,
  type ClassRule,
  type Counts,
  identityProblems,
  type Judged,
  judge,
  mentionErrorClass,
  type ReportInput,
  ReportRefusal,
} from "../comment-delivery/report.ts";

const FIXTURES = join(import.meta.dir, "..", "__fixtures__", "comment-delivery");
const SCRIPT = join(import.meta.dir, "..", "comment-delivery", "report.ts");
const BET15 = "03207432-749f-4211-9607-e378dc636807";
const CONTROL = "c1283587-7929-4a34-9048-b83f82e4bfd2";
const BET15_LEAD = "session:01a14313-f0d7-765d-91c1-f2e043218727";
const SESSION_A = "01a1a001-0000-7000-8000-000000000001";
const id = (n: number) =>
  `0a${String(n).padStart(6, "0")}-0000-4000-8000-${String(n).padStart(12, "0")}`;

const FILES = { comments: "comments.csv", receipts: "receipts.tsv", counts: "receipts-counts.txt" };
const fixture = async (name: string): Promise<string> => readFile(join(FIXTURES, name), "utf8");
const input: ReportInput = {
  comments: await fixture(FILES.comments),
  receipts: await fixture(FILES.receipts),
  counts: await fixture(FILES.counts),
};
const scratch = await mkdtemp(
  join(process.env.TMPDIR ?? "/legion/scratch/tmp", "comment-delivery-")
);
afterAll(() => rm(scratch, { recursive: true, force: true }));

function judged(report: { readonly comments: readonly Judged[] }, commentId: string): Judged {
  const found = report.comments.find((comment) => comment.comment.id === commentId);
  if (found === undefined) throw new Error(`no comment ${commentId}`);
  return found;
}

/** The problems judge refuses `changed` with; fails when it does not refuse. */
function refusal(changed: ReportInput, options: Parameters<typeof judge>[1] = {}): string[] {
  try {
    judge(changed, options);
  } catch (error) {
    if (error instanceof ReportRefusal) return [...error.problems];
    throw error;
  }
  throw new Error("judge did not refuse");
}

/** One row of comments.csv or receipts.tsv, every column given or empty. */
function row(text: string, separator: string, fields: Partial<Record<string, string>>): string {
  const columns = (text.split("\n")[0] ?? "").split(separator);
  return columns.map((column) => fields[column] ?? "").join(separator);
}

/** receipts-counts.txt with the value of `key` replaced. */
function counts(key: string, value: string): string {
  return input.counts.replace(new RegExp(`^${key}\t.*$`, "m"), `${key}\t${value}`);
}

interface Run {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

let written = 0;
async function cli(files: Partial<ReportInput> = {}, traces: readonly string[] = []): Promise<Run> {
  const paths: Record<string, string> = {};
  for (const name of ["comments", "receipts", "counts"] as const) {
    const text = files[name];
    if (text === undefined) {
      paths[name] = join(FIXTURES, FILES[name]);
    } else {
      paths[name] = join(scratch, `${written++}-${FILES[name]}`);
      await writeFile(paths[name], text);
    }
  }
  const child = Bun.spawn(
    [
      "bun",
      SCRIPT,
      "--comments",
      paths.comments ?? "",
      "--receipts",
      paths.receipts ?? "",
      "--counts",
      paths.counts ?? "",
      ...traces.flatMap((trace) => ["--trace", trace]),
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
}

describe("the pairs and the comments", () => {
  const report = judge(input);
  const verdict = (commentId: string) => judged(report, commentId).verdict;
  const pairVerdicts = (commentId: string) =>
    judged(report, commentId).pairs.map(({ pair, verdict: its }) => [pair.target, its]);

  test("the BET-15 reply owed to the claimant, not addressed and not received, is not-addressed:claimant", () => {
    expect(verdict(BET15)).toEqual({ class: "not-delivered", cause: "not-addressed:claimant" });
    expect(pairVerdicts(BET15)).toEqual([
      [BET15_LEAD, { class: "not-delivered", cause: "not-addressed:claimant" }],
    ]);
  });

  test("the control comment, addressed and received, is delivered", () => {
    expect(verdict(CONTROL)).toEqual({ class: "delivered", cause: "" });
    expect(pairVerdicts(CONTROL)).toEqual([
      [BET15_LEAD, { class: "delivered", cause: "", evidence: "transcript" }],
    ]);
  });

  test("an unlisted receipt makes the comment delivered; its pair alone keeps its cause", () => {
    expect(verdict(id(7))).toEqual({ class: "delivered", cause: "" });
    expect(pairVerdicts(id(7))).toEqual([
      [`session:${SESSION_A}`, { class: "not-delivered", cause: "running-no-row" }],
    ]);
    expect(report.unlisted.map((receipt) => [receipt.commentId, receipt.target])).toEqual([
      [id(7), "session:01a1f001-0000-7000-8000-00000000000f"],
    ]);
  });

  test("an unlisted row with no checked_session is read as one with it", () => {
    const receipts = input.receipts.replace(
      "\tfalse\t01a1f001-0000-7000-8000-00000000000f\t",
      "\tfalse\t\t"
    );
    expect(judged(judge({ ...input, receipts }), id(7)).verdict).toEqual({
      class: "delivered",
      cause: "",
    });
  });

  test("each unknown reason stays unknown, never delivered, and the comment names the pair", () => {
    expect(verdict(id(6))).toEqual({
      class: "unknown",
      cause: "",
      why: "session:01a1cccc-0000-7000-8000-00000000000c: session_not_in_store",
    });
    expect(verdict(id(5))).toEqual({
      class: "unknown",
      cause: "",
      why: "role:legion-infra-architect: role",
    });
    expect(verdict(id(12))).toEqual({
      class: "unknown",
      cause: "",
      why: "session:01a1e001-0000-7000-8000-00000000000e: btw",
    });
    for (const commentId of [id(5), id(6), id(12)]) {
      expect(judged(report, commentId).pairs[0]?.verdict.class).toBe("unknown");
    }
  });

  test("a BTW reply is delivered evidence", () => {
    expect(verdict(id(11))).toEqual({ class: "delivered", cause: "" });
    expect(judged(report, id(11)).pairs[0]?.verdict).toEqual({
      class: "delivered",
      cause: "",
      evidence: "btw_reply:0b000011-0000-4000-8000-000000000011",
    });
  });

  test("a role mention that resolved to a session, received there, is delivered", () => {
    const role = judged(report, id(14));
    expect(role.verdict).toEqual({ class: "delivered", cause: "" });
    expect(role.pairs[0]?.pair.mentionSessionId).toBe(SESSION_A);
    expect(role.pairs[0]?.pair.receipt.checkedSession).toBe(SESSION_A);
  });

  test("each pair cause, and each not-delivered comment class", () => {
    expect(verdict(id(3))).toEqual({ class: "not-delivered", cause: "never-published" });
    expect(judged(report, id(3)).pairs[0]?.verdict.cause).toBe("never-published");
    expect(verdict(id(4))).toEqual({ class: "not-delivered", cause: "no-target" });
    expect(judged(report, id(4)).pairs).toEqual([]);
    expect(verdict(id(8)).cause).toBe("mention-failed:session-not-live");
    // The session's times run from 2026-10-01 to 2026-10-03; Dispatch published on 2026-10-10.
    expect(verdict(id(9)).cause).toBe("not-running");
    expect(verdict(id(10)).cause).toBe("running-no-row");
  });

  test("a mention's attempts keep their envelope ids and modes aligned, an empty id for none", () => {
    const pair = judged(report, id(8)).pairs[0]?.pair;
    expect(pair?.mentionModes).toEqual(["steer", "steer"]);
    expect(pair?.mentionEnvelopeIds).toEqual(["", ""]);
  });

  test("a comment takes the first cause of its pairs; a sent mention counts as addressed", () => {
    expect(verdict(id(13)).cause).toBe("not-addressed:claimant");
    expect(judged(report, id(13)).pairs.map(({ verdict: its }) => its.cause)).toEqual([
      "not-addressed:claimant",
      "running-no-row",
    ]);
  });

  test("a pending mention not addressed is not-addressed; a pair owed twice names both reasons", () => {
    const comments = input.comments.replace(
      `${BET15_LEAD},claimant,false,,,,,,,`,
      `${BET15_LEAD},claimant;mention,false,${BET15_LEAD.slice(8)},,,steer,pending,,`
    );
    expect(judged(judge({ ...input, comments }), BET15).verdict.cause).toBe(
      "not-addressed:claimant+mention"
    );
  });

  test("not-running reads the checked session's start too, and an end left empty means still running", () => {
    const early = input.receipts.replace(
      `${id(10)}\t1010\tsession:${SESSION_A}\ttrue\t${SESSION_A}\tnot_received\t\t\t\t\t\t2026-10-01T00:00:00.000Z\t2026-10-31T00:00:00.000Z`,
      `${id(10)}\t1010\tsession:${SESSION_A}\ttrue\t${SESSION_A}\tnot_received\t\t\t\t\t\t2026-10-21T00:00:00.000Z\t`
    );
    expect(judged(judge({ ...input, receipts: early }), id(10)).verdict.cause).toBe("not-running");
    const running = input.receipts.replace(
      "2026-10-01T00:00:00.000Z\t2026-10-03T00:00:00.000Z",
      "2026-10-01T00:00:00.000Z\t"
    );
    expect(judged(judge({ ...input, receipts: running }), id(9)).verdict.cause).toBe(
      "running-no-row"
    );
  });

  test("a mention's error names its class", () => {
    expect(mentionErrorClass(`no live session ${SESSION_A}`)).toBe("session-not-live");
    expect(mentionErrorClass(RECEIPT_TIMEOUT_CAUSE)).toBe("receipt-timeout");
    expect(mentionErrorClass("session 01a1 (lead) does not advertise btw")).toBe("other");
  });

  test("a quoted field with a comma, a quote and a newline is one field", () => {
    const comments = input.comments.replace(
      `no live session ${SESSION_A}`,
      '"listener: 502, ""bad gateway""\nretry later"'
    );
    const pair = judged(judge({ ...input, comments }), id(8)).pairs[0];
    expect(pair?.pair.mentionError).toBe('listener: 502, "bad gateway"\nretry later');
    expect(pair?.verdict.cause).toBe("mention-failed:other");
  });
});

describe("the document", () => {
  test("opens with the identity and the window, includes each trace verbatim, and is the same bytes on a re-run", async () => {
    const trace = join(FIXTURES, "trace.csv");
    const first = await cli({}, [trace]);
    const second = await cli({}, [trace]);
    expect(first.code).toBe(0);
    expect(first.stderr).toBe("");
    expect(second.stdout).toBe(first.stdout);
    const lines = first.stdout.split("\n");
    expect(lines[2]).toBe(
      "Total 14 = delivered 4 + not delivered 7 (never-published 1, no-target 1, " +
        "not-addressed:claimant 2, not-addressed:route 0, not-addressed:follower 0, " +
        "not-addressed:asker 0, not-addressed:mention 0, mention-failed:session-not-live 1, " +
        "mention-failed:receipt-timeout 0, mention-failed:other 0, not-running 1, " +
        "running-no-row 1) + unknown 3"
    );
    expect(lines[4]).toBe(
      "- Window: comments that people created from 2026-10-01T00:00:00Z (included) to " +
        "2026-10-27T05:00:00Z (not included), UTC."
    );
    expect(first.stdout).toContain(
      "Total 15 = delivered 3 + not delivered 9 (never-published 1, no-target 1, " +
        "not-addressed:claimant 2, not-addressed:route 0, not-addressed:follower 0, " +
        "not-addressed:asker 0, not-addressed:mention 0, mention-failed:session-not-live 1, " +
        "mention-failed:receipt-timeout 0, mention-failed:other 0, not-running 1, " +
        "running-no-row 3) + unknown 3"
    );
    expect(first.stdout).toContain(
      `| ${id(11)} | session:01a1e001-0000-7000-8000-00000000000e | mention | no | ` +
        "1 btw 01a1eeee-1111-7222-8333-000000000011; newest sent | " +
        "01a1e001-0000-7000-8000-00000000000e | received | " +
        "delivered (btw_reply:0b000011-0000-4000-8000-000000000011) |  |"
    );
    expect(first.stdout).toContain(
      `| ${BET15} | BET-15 | 2026-10-19T01:31:21.247610Z | sami@theorem.dev | reply | ` +
        `not delivered | \`not-addressed:claimant\` | ${BET15_LEAD} (claimant): not-addressed:claimant |`
    );
    expect(first.stdout).toContain(
      "## What tq receipts counted\n\n" +
        "- Listed pairs: 14 = received 3 + not_received 8 + unknown 3 (session_not_in_store 1, " +
        "role 1, btw 1).\n" +
        "- Pairs with no target (a comments.csv row with no target, which has no receipts.tsv " +
        "row): 1.\n" +
        "- Unlisted receipt rows: 1.\n" +
        "- Comments: 14 = with a receipt 4 + without 10. A comment with a receipt has a row with " +
        "the status `received` in receipts.tsv, listed or unlisted.\n" +
        "- A person's comment rows from which no id was read: 0.\n"
    );
    expect(first.stdout).toContain(
      `- Comment ${id(7)}: session:01a1f001-0000-7000-8000-00000000000f, first at ` +
        "2026-10-20T04:00:01.000Z, sami-rocqperf:/home/sami/.omp/agent/sessions/a/" +
        "01a1f001-0000-7000-8000-00000000000f.jsonl:40"
    );
    expect(first.stdout).toContain(
      `### trace.csv\n\n\`\`\`text\n${await fixture("trace.csv")}\`\`\``
    );
  });
});

describe("refusals", () => {
  const append = (text: string, line: string) => `${text}${line}\n`;
  const lineOf = (text: string, index: number) => text.split("\n")[index] ?? "";
  /** `text` with `line` put in as line `index + 1`, so that receipts.tsv stays sorted. */
  const insert = (text: string, index: number, line: string) => {
    const lines = text.split("\n");
    lines.splice(index, 0, line);
    return lines.join("\n");
  };

  test("a duplicated listed row is refused, naming both lines, and prints nothing", async () => {
    const run = await cli({ receipts: insert(input.receipts, 2, lineOf(input.receipts, 1)) });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain(
      `receipts.tsv lines 2 and 3: comment ${BET15} and target "${BET15_LEAD}" twice`
    );
  });

  test("a duplicated unlisted row is refused: comment_id and target repeat", () => {
    const receipts = insert(
      input.receipts,
      7,
      lineOf(input.receipts, 6).replace(
        "\t01a1f001-0000-7000-8000-00000000000f\treceived",
        "\t\treceived"
      )
    );
    expect(refusal({ ...input, receipts })).toContain(
      `receipts.tsv lines 7 and 8: comment ${id(7)} and unlisted target ` +
        '"session:01a1f001-0000-7000-8000-00000000000f" twice'
    );
  });

  test("an unlisted row whose target is not session:<id> is refused", () => {
    const receipts = input.receipts.replace(
      `${id(7)}\t1007\tsession:01a1f001-0000-7000-8000-00000000000f\tfalse`,
      `${id(7)}\t1007\t\tfalse`
    );
    expect(refusal({ ...input, receipts })).toEqual([
      `receipts.tsv line 7 (comment ${id(7)}): an unlisted row with target "", not session:<id>`,
    ]);
  });

  test("receipts.tsv out of order is refused, naming the first row out of order", () => {
    const lines = input.receipts.split("\n");
    const swap = (first: number, second: number) => {
      const swapped = [...lines];
      [swapped[first], swapped[second]] = [lines[second] ?? "", lines[first] ?? ""];
      return refusal({ ...input, receipts: swapped.join("\n") });
    };
    const order =
      "out of order: the rows are not sorted by comment_id, then listed (true first), then target";
    // By comment_id.
    expect(swap(1, 2)).toEqual([`receipts.tsv line 3 (comment ${BET15}): ${order}`]);
    // By listed: the unlisted row of a comment before its listed row.
    expect(swap(5, 6)).toEqual([`receipts.tsv line 7 (comment ${id(7)}): ${order}`]);
    // By target.
    expect(swap(12, 13)).toEqual([`receipts.tsv line 14 (comment ${id(13)}): ${order}`]);
  });

  test("a time in receipts.tsv that is not YYYY-MM-DDTHH:MM:SS.mmmZ is refused", () => {
    const receipts = input.receipts
      .replace("2026-10-20T04:00:01.000Z", "2026-10-20T04:00:01Z")
      .replace(
        "2026-10-01T00:00:00.000Z\t2026-10-03T00:00:00.000Z",
        "2026-10-01 00:00:00\t2026-10-03T00:00:00.000000Z"
      );
    expect(refusal({ ...input, receipts })).toEqual([
      `receipts.tsv line 7 (comment ${id(7)}): first_received_at "2026-10-20T04:00:01Z" is not ` +
        "YYYY-MM-DDTHH:MM:SS.mmmZ",
      `receipts.tsv line 9 (comment ${id(9)}): target_started_at "2026-10-01 00:00:00" is not ` +
        "YYYY-MM-DDTHH:MM:SS.mmmZ",
      `receipts.tsv line 9 (comment ${id(9)}): target_ended_at "2026-10-03T00:00:00.000000Z" is ` +
        "not YYYY-MM-DDTHH:MM:SS.mmmZ",
    ]);
  });

  test("a duplicated comments.csv pair is refused", () => {
    const comments = append(input.comments, lineOf(input.comments, 2));
    expect(refusal({ ...input, comments })).toContain(
      `comments.csv lines 3 and 17: comment ${BET15} and target "${BET15_LEAD}" twice`
    );
  });

  test("a pair with no listed row is refused", async () => {
    const receipts = input.receipts.replace(`${lineOf(input.receipts, 1)}\n`, "");
    const run = await cli({ receipts });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain(
      `comments.csv line 3: comment ${BET15} and target ${BET15_LEAD} have no listed row in receipts.tsv`
    );
  });

  test("a listed row that comments.csv does not hold is refused", () => {
    const receipts = insert(
      input.receipts,
      3,
      row(input.receipts, "\t", {
        comment_id: id(4),
        event_id: "1004",
        target: `session:${SESSION_A}`,
        listed: "true",
        checked_session: SESSION_A,
        status: "unknown",
        unknown_reason: "session_not_in_store",
      })
    );
    expect(refusal({ ...input, receipts })).toContain(
      `receipts.tsv line 4: comment ${id(4)} and target "session:${SESSION_A}" is a listed ` +
        "row that comments.csv does not hold"
    );
  });

  test("a row whose comment or event comments.csv does not hold is refused", () => {
    const receipts = input.receipts.replace(
      `${id(7)}\t1007\tsession:01a1f001`,
      `${id(7)}\t1010\tsession:01a1f001`
    );
    expect(refusal({ ...input, receipts })).toContain(
      `receipts.tsv line 7: comment ${id(7)} with event "1010", but comments.csv gives event "1007"`
    );
    const stranger = insert(
      input.receipts,
      15,
      row(input.receipts, "\t", {
        comment_id: id(999),
        event_id: "999",
        target: `session:${SESSION_A}`,
        listed: "false",
        checked_session: SESSION_A,
        status: "received",
        evidence: "transcript",
      })
    );
    expect(refusal({ ...input, receipts: stranger })).toContain(
      `receipts.tsv line 16: comment ${id(999)} is not in comments.csv`
    );
  });

  test("a header that is not the expected columns is refused", async () => {
    const run = await cli({ receipts: input.receipts.replace("evidence", "proof") });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain('receipts.tsv: the header is "comment_id\tevent_id\ttarget');
    expect(refusal({ ...input, comments: input.comments.replace("owner,", "") })[0]).toStartWith(
      'comments.csv: the header is "comment_id,event_id,created_at'
    );
    // The header before INFRA-608's contract, with target_kind and one mention attempt.
    const old = input.comments.replace("target,owed_reasons", "target,target_kind,owed_reasons");
    expect(refusal({ ...input, comments: old })[0]).toStartWith(
      'comments.csv: the header is "comment_id,event_id,owner,created_at,published_at,author,' +
        "shape,reopened,target,target_kind,"
    );
  });

  test("a status, reason or evidence outside its values, or one that disagrees with another, is refused", () => {
    const receipts = input.receipts
      .replace("\tnot_received\t", "\tmissing\t")
      .replace("\tsession_not_in_store\t", "\tpod\t")
      .replace("btw_reply:0b000011", "reply:0b000011");
    expect(refusal({ ...input, receipts })).toEqual(
      expect.arrayContaining([
        `receipts.tsv line 2 (comment ${BET15}): status "missing"`,
        `receipts.tsv line 5 (comment ${id(6)}): unknown_reason "pod"`,
        `receipts.tsv line 11 (comment ${id(11)}): evidence "reply:0b000011-0000-4000-8000-000000000011"`,
      ])
    );
    const unknownNoReason = input.receipts.replace("\tunknown\tbtw\t", "\tunknown\t\t");
    expect(refusal({ ...input, receipts: unknownNoReason })).toContain(
      `receipts.tsv line 12 (comment ${id(12)}): status "unknown" with unknown_reason ""`
    );
    const listed = input.receipts.replace(
      `${BET15}\t1002\t${BET15_LEAD}\ttrue`,
      `${BET15}\t1002\t${BET15_LEAD}\tyes`
    );
    expect(refusal({ ...input, receipts: listed })).toContain(
      `receipts.tsv line 2 (comment ${BET15}): listed "yes"`
    );
  });

  test("mention envelope ids that do not align with the modes are refused", () => {
    const comments = input.comments.replace('",",,"steer,steer"', '",",,steer');
    expect(refusal({ ...input, comments })).toContain(
      `comments.csv line 9 (comment ${id(8)}): mention_envelope_ids "," has 2 attempts, ` +
        'mention_modes "steer" 1'
    );
  });

  test("receipts-counts.txt with a sum that its own numbers do not make is refused", async () => {
    const run = await cli({ counts: counts("received", "4") });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain(
      "receipts-counts.txt line 3: listed_pairs is 14, but received 4 + not_received 8 + " +
        "unknown 3 is 15"
    );
    expect(refusal({ ...input, counts: counts("received", "4") })).toEqual([
      "receipts-counts.txt line 3: listed_pairs is 14, but received 4 + not_received 8 + " +
        "unknown 3 is 15",
      "receipts-counts.txt line 4: received is 4, but receipts.tsv and comments.csv give 3",
    ]);
    expect(refusal({ ...input, counts: counts("unknown_btw", "0") })).toEqual([
      "receipts-counts.txt line 6: unknown is 3, but unknown_session_not_in_store 1 + " +
        "unknown_role 1 + unknown_btw 0 is 2",
      "receipts-counts.txt line 9: unknown_btw is 0, but receipts.tsv and comments.csv give 1",
    ]);
    expect(refusal({ ...input, counts: counts("comments_without_receipt", "9") })).toEqual([
      "receipts-counts.txt line 11: comments is 14, but comments_with_receipt 4 + " +
        "comments_without_receipt 9 is 13",
      "receipts-counts.txt line 13: comments_without_receipt is 9, but receipts.tsv and " +
        "comments.csv give 10",
    ]);
  });

  test("receipts-counts.txt with a number that is not what the report counts is refused", () => {
    // A received row of either evidence, listed or unlisted, is a receipt: id(11) has only a
    // btw_reply row and id(7) only an unlisted row.
    const firstReceiptOnly = input.counts
      .replace("comments_with_receipt\t4", "comments_with_receipt\t3")
      .replace("comments_without_receipt\t10", "comments_without_receipt\t11");
    expect(refusal({ ...input, counts: firstReceiptOnly })).toEqual([
      "receipts-counts.txt line 12: comments_with_receipt is 3, but receipts.tsv and " +
        "comments.csv give 4",
      "receipts-counts.txt line 13: comments_without_receipt is 11, but receipts.tsv and " +
        "comments.csv give 10",
    ]);
    expect(refusal({ ...input, counts: counts("unlisted_rows", "0") })).toEqual([
      "receipts-counts.txt line 10: unlisted_rows is 0, but receipts.tsv and comments.csv give 1",
    ]);
  });

  test("receipts-counts.txt whose empty_target_pairs is not comments.csv's is refused", () => {
    expect(refusal({ ...input, counts: counts("empty_target_pairs", "2") })).toEqual([
      "receipts-counts.txt line 3: listed_pairs is 14, but comments.csv's 15 rows - " +
        "empty_target_pairs 2 is 13",
      "receipts-counts.txt line 14: empty_target_pairs is 2, but receipts.tsv and comments.csv " +
        "give 1",
    ]);
  });

  test("receipts-counts.txt with rows_without_id not 0 is refused", () => {
    expect(refusal({ ...input, counts: counts("rows_without_id", "1") })).toEqual([
      "receipts-counts.txt line 15: rows_without_id is 1: tq receipts read no event id, " +
        "envelope id or comment id from that many of a person's comment.created rows, so " +
        "receipts.tsv can miss receipts",
    ]);
  });

  test("receipts-counts.txt with keys out of order, missing or extra is refused", () => {
    const swapped = input.counts.replace(
      "received\t3\nnot_received\t8\n",
      "not_received\t8\nreceived\t3\n"
    );
    expect(refusal({ ...input, counts: swapped })).toEqual([
      'receipts-counts.txt line 4: the key is "not_received", not "received"',
      'receipts-counts.txt line 5: the key is "received", not "not_received"',
    ]);
    const short = input.counts.replace("rows_without_id\t0\n", "");
    expect(refusal({ ...input, counts: short })).toEqual([
      'receipts-counts.txt line 15: missing; the "rows_without_id" line comes here',
    ]);
    expect(refusal({ ...input, counts: `${input.counts}extra\t1\n` })).toEqual([
      'receipts-counts.txt line 16: "extra\t1" is after the last key, rows_without_id',
    ]);
    const old =
      "from: 2026-10-01T00:00:00Z\nto: 2026-10-27T05:00:00Z\n" +
      "listed pairs: 14 = received 3 + not_received 8 + unknown 3 (session_not_in_store 1, " +
      "role 1, btw 1)\nunlisted receipt rows: 1\ncomments: 14 = with receipt 4 + without 10\n" +
      "no id read: 0\n";
    expect(refusal({ ...input, counts: old })).toContain(
      'receipts-counts.txt line 1: the key is "from: 2026-10-01T00:00:00Z", not "from"'
    );
  });

  test("receipts-counts.txt with a malformed value is refused", () => {
    expect(refusal({ ...input, counts: counts("from", "2026-10-01") })).toEqual([
      'receipts-counts.txt line 1: from "2026-10-01" is not a time YYYY-MM-DDTHH:MM:SSZ',
    ]);
    expect(refusal({ ...input, counts: counts("to", "2026-10-27T05:00:00.000Z") })).toEqual([
      'receipts-counts.txt line 2: to "2026-10-27T05:00:00.000Z" is not a time ' +
        "YYYY-MM-DDTHH:MM:SSZ",
    ]);
    expect(refusal({ ...input, counts: counts("received", "03") })).toEqual([
      'receipts-counts.txt line 4: received "03" is not a plain integer',
    ]);
    expect(refusal({ ...input, counts: counts("unknown", "-3") })).toEqual([
      'receipts-counts.txt line 6: unknown "-3" is not a plain integer',
    ]);
    expect(refusal({ ...input, counts: input.counts.replace("received\t3", "received") })).toEqual([
      'receipts-counts.txt line 4: "received" has no tab and value',
    ]);
    expect(refusal({ ...input, counts: input.counts.slice(0, -1) })).toEqual([
      "receipts-counts.txt: the last line has no newline",
    ]);
  });

  test("rows of one comment that disagree are refused", () => {
    const changed = input.comments.replace(
      `2026-10-20T10:00:00.100000Z,sami@theorem.dev,reply,false,session:${SESSION_A}`,
      `2026-10-20T10:00:09.100000Z,sami@theorem.dev,reply,false,session:${SESSION_A}`
    );
    expect(refusal({ ...input, comments: changed })).toEqual([
      `comments.csv lines 14 and 15: comment ${id(13)} has two values of published_at`,
    ]);
  });

  test("a comment in no class, or in two, is refused", () => {
    // Without the delivered rule, the four comments a received row delivers fall in none.
    expect(refusal(input, { rules: CLASS_RULES.slice(1) })).toEqual(
      [CONTROL, id(7), id(11), id(14)].map((commentId) => `comment ${commentId} falls in no class`)
    );
    const always: ClassRule = {
      name: "always",
      applies: () => true,
      verdict: () => ({ class: "delivered", cause: "" }),
    };
    const two = refusal(input, { rules: [...CLASS_RULES, always] });
    expect(two).toHaveLength(14);
    expect(two).toContain(
      `comment ${BET15} falls in 2 classes: not delivered, with a cause, always`
    );
    expect(two).toContain(`comment ${id(12)} falls in 2 classes: unknown, always`);
  });

  test("a not-delivered comment with no cause is refused", () => {
    const noCause: ClassRule[] = CLASS_RULES.map((rule) =>
      rule.name === "never-published"
        ? { ...rule, verdict: () => ({ class: "not-delivered", cause: "" }) }
        : rule
    );
    expect(refusal(input, { rules: noCause })).toEqual([
      `comment ${id(3)} is not delivered and has no cause`,
    ]);
  });

  test("an identity that does not add up is refused, with its numbers", () => {
    const broken: Counts = {
      total: 10,
      delivered: 6,
      notDelivered: 3,
      unknown: 2,
      causes: new Map([
        ["no-target", 1],
        ["not-running", 1],
      ]),
    };
    expect(identityProblems(broken, "comments")).toEqual([
      "comments: the identity fails: Total 10 = delivered 6 + not delivered 3 (no-target 1, " +
        "not-running 1) + unknown 2, and 6 + 3 + 2 is not 10",
      "comments: the identity fails: the causes add up to 2, not 3",
    ]);
  });

  test("a cause with no fix is refused, naming it", async () => {
    const { "not-running": _, ...withoutNotRunning } = CAUSES;
    expect(refusal(input, { causes: withoutNotRunning })).toEqual([
      "the cause not-running has no fix in the cause-to-fix table",
    ]);
  });
});
