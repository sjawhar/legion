import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";

// envoy-session-delivery.sh's compare_issue_order is the run's only judgement of the order the
// followed issue's comments reach each session. These rows run the script's own function against
// the transcripts a run can produce, so a comparison that holds nothing, falls short or skips a
// comment it cannot find fails the run instead of printing the order as held.

const script = join(import.meta.dir, "..", "envoy-session-delivery.sh");
const fn = scriptFunctions(script);

interface Case {
  /** The comment markers in the order they were written. */
  readonly written: readonly string[];
  /** The markers in the order the stream stored them (published_order). */
  readonly published: readonly string[];
  /** Markers of published the stream holds no copy of, so they get no stream sequence. */
  readonly unstored?: readonly string[];
  /** Each marker's first transcript line in pi and in cc; a marker absent from a session is left out. */
  readonly lines: Readonly<Record<"pi" | "cc", Readonly<Record<string, number>>>>;
  /**
   * The run's event record (events.jsonl) as the file's own text, which the function reads to
   * tell a run that publishes no comment from one whose comments went missing. Left out, `events`
   * stays unset, as it is for a harness with no run behind it.
   */
  readonly record?: string;
  /** The run's --disruptions list, which the exemption line names. Left out, it stays unset. */
  readonly disruptions?: string;
}

/**
 * Runs the function over `run`, and reports what it set order_ok to, what it printed, and the
 * path of the event record it was given (the empty string when it was given none).
 */
function compare(run: Case): {
  readonly ok: string;
  readonly output: string;
  readonly record: string;
} {
  const dir = mkdtempSync(join(tmpdir(), "session-delivery-order-compare."));
  const record = run.record === undefined ? "" : join(dir, "events.jsonl");
  if (run.record !== undefined) writeFileSync(record, run.record);
  // A harness with no run behind it sets neither, as the function's contract has it: each is
  // read with a default, and the row that gives one says so.
  const environment = [
    record === "" ? "" : `events=${JSON.stringify(record)}`,
    run.disruptions === undefined ? "" : `disruptions=${JSON.stringify(run.disruptions)}`,
  ].join("\n");
  const seq = run.published
    .map((marker, i) =>
      run.unstored?.includes(marker) === true ? "" : `stream_seq[${marker}]=${i + 10}`
    )
    .join("\n");
  const lines = (["pi", "cc"] as const)
    .flatMap((label) =>
      Object.entries(run.lines[label]).map(
        ([marker, line]) => `first_line[${label}:${marker}]=${line}`
      )
    )
    .join("\n");
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `${nameMissingHelpers(script)}
set -u
table=/dev/null
followed_issue=LIVE-1
declare -A stream_seq=() first_line=()
${environment}
${seq}
${lines}
published_order=(${run.published.join(" ")})
written_order="${run.written.map((marker) => `${marker} `).join("")}"
${fn("compare_issue_order")}
compare_issue_order
printf 'order_ok=%s\\n' "$order_ok"`,
  ]);
  rmSync(dir, { force: true, recursive: true });
  const output = result.stdout.toString();
  expectNoMissingHelpers(output, result.stderr.toString());
  expect(result.exitCode).toBe(0);
  const ok = /^order_ok=(\d)$/m.exec(output)?.[1] ?? "";
  return { ok, output, record };
}

const both = (lines: Readonly<Record<string, number>>) => ({ pi: lines, cc: lines });

describe("compare_issue_order", () => {
  test("holds when both transcripts show every comment in stream order, and names what it compared", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c1", "c2"],
      lines: both({ c1: 5, c2: 9 }),
    });
    expect(ok).toBe("1");
    expect(output).toContain(
      "issue order compared in pi (2 comments): c1@seq10:line5 c2@seq11:line9"
    );
    expect(output).toContain(
      "issue order compared in cc (2 comments): c1@seq10:line5 c2@seq11:line9"
    );
    expect(output).not.toContain("note: Dispatch published");
  });

  test("fails a transcript that shows two comments out of stream order", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c1", "c2"],
      lines: { pi: { c1: 5, c2: 9 }, cc: { c1: 9, c2: 5 } },
    });
    expect(ok).toBe("0");
    expect(output).toContain("order break in cc: c2");
  });

  test("fails a comment missing from a transcript rather than skipping it", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c1", "c2"],
      lines: { pi: { c1: 5, c2: 9 }, cc: { c1: 5 } },
    });
    expect(ok).toBe("0");
    expect(output).toContain("issue order in cc: c2 (stream seq 11) is in no transcript line");
  });

  test("fails a comment the stream does not hold", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c2", "c1"],
      unstored: ["c2"],
      lines: both({ c1: 5, c2: 9 }),
    });
    expect(ok).toBe("0");
    expect(output).toContain("the stream holds no comment carrying c2");
  });

  test("fails a comparison that holds nothing", () => {
    const { ok, output } = compare({ written: [], published: [], lines: both({}) });
    expect(ok).toBe("0");
    expect(output).toContain("nothing was compared");
  });

  // The one answer's other side: the run published comments, so the function compares them, and
  // the same place names a published order that differs from the written one.
  test("names a published order that differs from the order the comments were written in", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c2", "c1"],
      lines: both({ c2: 5, c1: 9 }),
    });
    expect(ok).toBe("1");
    expect(output).toContain(
      "note: Dispatch published LIVE-1's comments in another order than they were written: " +
        "written c1 c2 ; stored c2 c1"
    );
    expect(output).toContain(
      "issue order compared in pi (2 comments): c2@seq10:line5 c1@seq11:line9"
    );
  });

  // An empty comparison with a record behind it: the record says which of the three it is.
  test("exempts a run whose event record was read and holds no comment", () => {
    const { ok, output } = compare({
      written: [],
      published: [],
      lines: both({}),
      record: `${JSON.stringify({ marker: "LDT1-001Z", disruption: "g", kind: "agent" })}\n`,
      disruptions: "g",
    });
    expect(ok).toBe("1");
    expect(output).toContain(
      "issue order: no window of this run (--disruptions g) publishes a comment of LIVE-1, " +
        "so there is nothing to compare"
    );
  });

  test("fails a run whose event record holds a comment none of which reached the comparison", () => {
    const { ok, output } = compare({
      written: [],
      published: [],
      lines: both({}),
      record: `${JSON.stringify({ marker: "LDT1-001Z", disruption: "f", kind: "issue" })}\n`,
      disruptions: "f",
    });
    expect(ok).toBe("0");
    expect(output).toContain(
      "issue order: no comment of LIVE-1 was recorded, so nothing was compared"
    );
  });

  test("fails a run whose event record cannot be read, naming it, rather than exempting it", () => {
    const { ok, output, record } = compare({
      written: [],
      published: [],
      lines: both({}),
      record: '{"marker": "LDT1-001Z", "kind": "issue"\n',
      disruptions: "g",
    });
    expect(ok).toBe("0");
    expect(output).toContain(`issue order: the event record ${record} could not be read (jq exit `);
    expect(output).toContain("whether this run published a comment of LIVE-1 is unknown");
    expect(output).not.toContain("nothing to compare");
  });
});
