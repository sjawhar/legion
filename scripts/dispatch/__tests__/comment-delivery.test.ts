import { afterAll, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
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
const BET15_LEAD = "notifications.agent.01a14313-f0d7-765d-91c1-f2e043218727";

const fixture = async (name: string): Promise<string> => readFile(join(FIXTURES, name), "utf8");
const input: ReportInput = {
  comments: await fixture("comments.csv"),
  received: await fixture("received.tsv"),
  sessions: await fixture("sessions.tsv"),
};
const scratch = await mkdtemp(join(tmpdir(), "comment-delivery-"));
afterAll(() => rm(scratch, { recursive: true, force: true }));

function judged(report: { readonly comments: readonly Judged[] }, id: string): Judged {
  const found = report.comments.find((comment) => comment.comment.id === id);
  if (found === undefined) throw new Error(`no comment ${id}`);
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

/** One comment row of comments.csv, every column given or empty. */
function commentRow(fields: Partial<Record<string, string>>): string {
  const columns = (input.comments.split("\n")[0] ?? "").split(",");
  return columns.map((column) => fields[column] ?? "").join(",");
}

interface Run {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

let written = 0;
async function cli(files: Partial<ReportInput> = {}, traces: readonly string[] = []): Promise<Run> {
  const paths: Record<string, string> = {};
  for (const name of ["comments", "received", "sessions"] as const) {
    const text = files[name];
    if (text === undefined) {
      paths[name] = join(FIXTURES, name === "comments" ? "comments.csv" : `${name}.tsv`);
    } else {
      paths[name] = join(scratch, `${written++}-${name}`);
      await writeFile(paths[name], text);
    }
  }
  const child = Bun.spawn(
    [
      "bun",
      SCRIPT,
      "--comments",
      paths.comments ?? "",
      "--received",
      paths.received ?? "",
      "--sessions",
      paths.sessions ?? "",
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

describe("the classes", () => {
  const report = judge(input);
  const verdict = (id: string) => judged(report, id).verdict;

  test("the BET-15 reply owed to the held claimant and never addressed is not-addressed:claimant", () => {
    const bet15 = judged(report, BET15);
    expect(bet15.verdict).toEqual({ class: "not-delivered", cause: "not-addressed:claimant" });
    expect(bet15.targets.map(({ target, verdict: its }) => [target.target, its.cause])).toEqual([
      [BET15_LEAD, "not-addressed:claimant"],
    ]);
  });

  test("the control comment, addressed and received, is delivered", () => {
    expect(verdict(CONTROL)).toEqual({ class: "delivered", cause: "" });
    expect(judged(report, CONTROL).targets[0]?.verdict.class).toBe("delivered");
  });

  test("a receipt by a fork of the target counts as the target's receipt", () => {
    const forked = judged(report, "0a000007-0000-4000-8000-000000000007");
    expect(forked.verdict.class).toBe("delivered");
    expect(forked.targets[0]?.verdict.class).toBe("delivered");
    expect(forked.receipts.map((receipt) => receipt.session)).toEqual([
      "01a1f001-0000-7000-8000-00000000000f",
    ]);
  });

  test("a target the store does not hold is unknown, never delivered", () => {
    const unheld = judged(report, "0a000006-0000-4000-8000-000000000006");
    expect(unheld.verdict).toEqual({ class: "unknown", cause: "" });
    expect(unheld.targets[0]?.verdict).toEqual({
      class: "unknown",
      cause: "",
      why: "the store does not hold this session",
    });
  });

  test("a role target is unknown", () => {
    const role = judged(report, "0a000005-0000-4000-8000-000000000005");
    expect(role.verdict.class).toBe("unknown");
    expect(role.targets[0]?.verdict.why).toBe("a role");
  });

  test("an event never published, a comment with no target, and each last-hop cause", () => {
    expect(verdict("0a000003-0000-4000-8000-000000000003").cause).toBe("never-published");
    expect(verdict("0a000004-0000-4000-8000-000000000004").cause).toBe("no-target");
    expect(verdict("0a000008-0000-4000-8000-000000000008").cause).toBe(
      "mention-failed:session-not-live"
    );
    // Its two copies ran from 2026-10-01 to 2026-10-03; Dispatch published on 2026-10-10.
    expect(verdict("0a000009-0000-4000-8000-000000000009").cause).toBe("not-running");
    expect(verdict("0a000010-0000-4000-8000-000000000010").cause).toBe("running-no-row");
  });

  test("an ask-thread receipt that names only its event, and an issue subscriber's receipt, deliver", () => {
    expect(verdict("0a000011-0000-4000-8000-000000000011").class).toBe("delivered");
    const subscriber = judged(report, "0a000012-0000-4000-8000-000000000012");
    expect(subscriber.verdict.class).toBe("delivered");
    // Alone, its owed target is still one the store does not hold.
    expect(subscriber.targets[0]?.verdict.class).toBe("unknown");
  });

  test("a comment takes the first cause of its targets; a sent mention counts as addressed", () => {
    const two = judged(report, "0a000013-0000-4000-8000-000000000013");
    expect(two.verdict.cause).toBe("not-addressed:claimant");
    // A sent steer with no transcript row is still a last-hop loss.
    expect(two.targets.map(({ verdict: its }) => its.cause)).toEqual([
      "not-addressed:claimant",
      "running-no-row",
    ]);
  });

  test("a mention's delivery of an ask-thread reply, named by its envelope only, delivers", () => {
    const reply = judged(report, "0a000016-0000-4000-8000-000000000016");
    expect(reply.verdict).toEqual({ class: "delivered", cause: "" });
    expect(reply.targets[0]?.verdict).toEqual({ class: "delivered", cause: "" });
    expect(reply.receipts.map((receipt) => [receipt.envelopeId, receipt.eventId])).toEqual([
      ["01a1aaaa-1111-7222-8333-444455550000", ""],
    ]);
  });

  test("a mention sent as a BTW with no receipt is delivered on Dispatch's record", () => {
    const btw = judged(report, "0a000017-0000-4000-8000-000000000017");
    expect(btw.receipts).toEqual([]);
    expect(btw.verdict).toEqual({ class: "delivered", cause: "", note: "btw-sent" });
    expect(btw.targets[0]?.verdict).toEqual({ class: "delivered", cause: "", note: "btw-sent" });
  });

  test("a mention sent as a BTW that failed is mention-failed, not btw-sent", () => {
    const failed = judged(report, "0a000008-0000-4000-8000-000000000008");
    expect(failed.targets[0]?.target.mentionDelivery).toBe("btw");
    expect(failed.verdict).toEqual({
      class: "not-delivered",
      cause: "mention-failed:session-not-live",
    });
  });

  test("receipts of comments outside the list, and receipts that link to no comment, are set apart", () => {
    expect(report.unlisted.map((receipt) => receipt.eventId)).toEqual(["999"]);
    expect(report.untied.map((receipt) => receipt.envelopeId)).toEqual([
      "01a1eeee-4444-7555-8666-777788880000",
    ]);
  });

  test("the diverged copies of one session merge, so their union covers the publish time", () => {
    const comments = `${input.comments}${commentRow({
      comment_id: "0a000014-0000-4000-8000-000000000014",
      event_id: "1014",
      owner: "INFRA-12",
      created_at: "2026-10-02T06:00:00.000000Z",
      published_at: "2026-10-02T06:00:00.100000Z",
      author: "sami@theorem.dev",
      shape: "conversation",
      reopened: "false",
      target: "notifications.agent.01a1d001-0000-7000-8000-00000000000d",
      target_kind: "session",
      owed_reasons: "claimant",
      addressed: "true",
    })}\n`;
    const merged = judge({ ...input, comments });
    expect(judged(merged, "0a000014-0000-4000-8000-000000000014").verdict.cause).toBe(
      "running-no-row"
    );
  });

  test("a mention's error names its class", () => {
    expect(mentionErrorClass("no live session 01a1a001-0000-7000-8000-000000000001")).toBe(
      "session-not-live"
    );
    expect(mentionErrorClass(RECEIPT_TIMEOUT_CAUSE)).toBe("receipt-timeout");
    expect(mentionErrorClass("session 01a1 (lead) does not advertise btw")).toBe("other");
  });

  test("a quoted field with a comma, a quote and a newline is one field", () => {
    const comments = `${input.comments}${commentRow({
      comment_id: "0a000015-0000-4000-8000-000000000015",
      event_id: "1015",
      owner: "INFRA-13",
      created_at: "2026-10-21T00:00:00.000000Z",
      published_at: "2026-10-21T00:00:00.100000Z",
      author: "sami@theorem.dev",
      shape: "conversation",
      reopened: "false",
      target: "notifications.agent.01a1a001-0000-7000-8000-000000000001",
      target_kind: "session",
      owed_reasons: "mention",
      addressed: "false",
      mention_state: "failed",
      mention_delivery: "steer",
      mention_error: '"listener: 502, ""bad gateway""\nretry later"',
    })}\n`;
    const target = judged(judge({ ...input, comments }), "0a000015-0000-4000-8000-000000000015")
      .targets[0];
    expect(target?.target.mentionError).toBe('listener: 502, "bad gateway"\nretry later');
    expect(target?.verdict.cause).toBe("mention-failed:other");
  });
});

describe("the document", () => {
  test("opens with the identity, includes each trace verbatim, and is the same bytes on a re-run", async () => {
    const trace = join(FIXTURES, "trace.csv");
    const first = await cli({}, [trace]);
    const second = await cli({}, [trace]);
    expect(first.code).toBe(0);
    expect(first.stderr).toBe("");
    expect(second.stdout).toBe(first.stdout);
    const lines = first.stdout.split("\n");
    expect(lines[2]).toBe(
      "Total 15 = delivered 6 + not delivered 7 (never-published 1, no-target 1, " +
        "not-addressed:claimant 2, not-addressed:route 0, not-addressed:follower 0, " +
        "not-addressed:asker 0, not-addressed:mention 0, mention-failed:session-not-live 1, " +
        "mention-failed:receipt-timeout 0, mention-failed:other 0, not-running 1, " +
        "running-no-row 1) + unknown 2"
    );
    expect(lines[5]).toStartWith("- Delivered: 6, of which btw-sent 1: Dispatch's record");
    expect(first.stdout).toContain(
      "| 0a000017-0000-4000-8000-000000000017 | notifications.agent.01a1e001-0000-7000-8000-00000000000e | " +
        "mention | no | sent (btw) | delivered (btw-sent) |  |"
    );
    expect(first.stdout).toContain(
      "| 0a000017-0000-4000-8000-000000000017 | INFRA-13 | 2026-10-20T11:00:00.000000Z | " +
        "sami@theorem.dev | conversation | delivered (btw-sent) |  |"
    );
    expect(first.stdout).toContain(
      `### trace.csv\n\n\`\`\`text\n${await fixture("trace.csv")}\`\`\``
    );
  });
});

describe("refusals", () => {
  const append = (text: string, line: string) => `${text}${line}\n`;
  const lineOf = (text: string, index: number) => text.split("\n")[index] ?? "";

  test("a duplicated row of any file is refused, naming both lines, and prints nothing", async () => {
    const run = await cli({ comments: append(input.comments, lineOf(input.comments, 2)) });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain(`comments.csv lines 3 and 18: comment ${BET15} and target`);
    expect(
      refusal({ ...input, received: append(input.received, lineOf(input.received, 3)) })
    ).toEqual([
      "received.tsv lines 4 and 10: session 01a1f001-0000-7000-8000-00000000000f, " +
        "/home/sami/.omp/agent/sessions/a/01a1f001-0000-7000-8000-00000000000f.jsonl:40 twice",
    ]);
    expect(
      refusal({ ...input, sessions: append(input.sessions, lineOf(input.sessions, 2)) })
    ).toEqual(["sessions.tsv lines 3 and 8: session 01a1a001-0000-7000-8000-000000000001 twice"]);
  });

  test("a header that is not the expected columns is refused", async () => {
    const run = await cli({ received: input.received.replace("comment_id", "comment") });
    expect(run.code).toBe(1);
    expect(run.stdout).toBe("");
    expect(run.stderr).toContain('received.tsv: the header is "session_id,host,ts');
    expect(refusal({ ...input, comments: input.comments.replace("owner,", "") })[0]).toStartWith(
      'comments.csv: the header is "comment_id,event_id,created_at'
    );
    // The header before mention_envelope_id and mention_delivery.
    const old = input.comments.replace(",mention_envelope_id,mention_delivery\n", "\n");
    expect(refusal({ ...input, comments: old })[0]).toBe(
      'comments.csv: the header is "comment_id,event_id,owner,created_at,published_at,author,' +
        "shape,reopened,target,target_kind,owed_reasons,addressed,mention_state,mention_error," +
        'route_used,claim_used,route_source", not "comment_id,event_id,owner,created_at,' +
        "published_at,author,shape,reopened,target,target_kind,owed_reasons,addressed," +
        "mention_state,mention_error,route_used,claim_used,route_source,mention_envelope_id," +
        'mention_delivery"'
    );
    expect(refusal({ ...input, sessions: `x\n${input.sessions}` })[0]).toStartWith(
      'sessions.tsv: the header is "x"'
    );
  });

  test("a receipt whose comment and event comments.csv does not pair is refused", () => {
    const received = input.received.replace(
      "dispatch-1007\t1007\t0a000007",
      "dispatch-1010\t1010\t0a000007"
    );
    expect(refusal({ ...input, received })).toEqual([
      "received.tsv: /home/sami/.omp/agent/sessions/a/01a1f001-0000-7000-8000-00000000000f.jsonl:40 " +
        "names comment 0a000007-0000-4000-8000-000000000007 and event 1010, which comments.csv " +
        "does not pair",
    ]);
  });

  test("rows of one comment that disagree are refused", () => {
    const changed = input.comments.replace(
      "2026-10-20T10:00:00.100000Z,sami@theorem.dev,reply,false,notifications.agent.01a1a001",
      "2026-10-20T10:00:09.100000Z,sami@theorem.dev,reply,false,notifications.agent.01a1a001"
    );
    expect(refusal({ ...input, comments: changed })).toEqual([
      "comments.csv lines 14 and 15: comment 0a000013-0000-4000-8000-000000000013 has two values of published_at",
    ]);
  });

  test("a comment in no class, or in two, is refused", () => {
    // Without the delivered rule, the five comments a receipt delivered fall in none. The one that
    // only Dispatch's record of a BTW mention delivers still falls in its own class.
    expect(refusal(input, { rules: CLASS_RULES.slice(1) })).toEqual(
      [
        CONTROL,
        "0a000007-0000-4000-8000-000000000007",
        "0a000011-0000-4000-8000-000000000011",
        "0a000016-0000-4000-8000-000000000016",
        "0a000012-0000-4000-8000-000000000012",
      ].map((id) => `comment ${id} falls in no class`)
    );
    const always: ClassRule = {
      name: "always",
      applies: () => true,
      verdict: () => ({ class: "delivered", cause: "" }),
    };
    const two = refusal(input, { rules: [...CLASS_RULES, always] });
    expect(two).toHaveLength(15);
    expect(two).toContain(
      "comment 0a000017-0000-4000-8000-000000000017 falls in 2 classes: delivered on " +
        "Dispatch's record of a BTW mention, always"
    );
    expect(two).toContain(
      `comment ${BET15} falls in 2 classes: not delivered, with a cause, always`
    );
  });

  test("a not-delivered comment with no cause, and an unknown one naming no unheld target, are refused", () => {
    const noCause: ClassRule[] = CLASS_RULES.map((rule) =>
      rule.name === "never-published"
        ? { ...rule, verdict: () => ({ class: "not-delivered", cause: "" }) }
        : rule
    );
    expect(refusal(input, { rules: noCause })).toEqual([
      "comment 0a000003-0000-4000-8000-000000000003 is not delivered and has no cause",
    ]);
    const unknownDelivered: ClassRule[] = CLASS_RULES.map((rule) =>
      rule.name === "delivered"
        ? { ...rule, verdict: () => ({ class: "unknown", cause: "" }) }
        : rule
    );
    expect(refusal(input, { rules: unknownDelivered })).toEqual(
      [
        CONTROL,
        "0a000007-0000-4000-8000-000000000007",
        "0a000011-0000-4000-8000-000000000011",
        "0a000016-0000-4000-8000-000000000016",
      ].map((id) => `comment ${id} is unknown and names no role or session the store does not hold`)
    );
  });

  test("an identity that does not add up is refused, with its numbers", () => {
    const counts: Counts = {
      total: 10,
      delivered: 6,
      notDelivered: 3,
      unknown: 2,
      causes: new Map([
        ["no-target", 1],
        ["not-running", 1],
      ]),
    };
    expect(identityProblems(counts, "comments")).toEqual([
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
