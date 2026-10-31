import { describe, expect, test } from "bun:test";
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
  /** Each marker's first transcript line in pi and in cc; a marker absent from a session is left out. */
  readonly lines: Readonly<Record<"pi" | "cc", Readonly<Record<string, number>>>>;
}

function compare(run: Case): { readonly ok: string; readonly output: string } {
  const seq = run.published.map((marker, i) => `stream_seq[${marker}]=${i + 10}`).join("\n");
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
${seq}
${lines}
published_order=(${run.published.join(" ")})
written_order="${run.written.map((marker) => `${marker} `).join("")}"
${fn("compare_issue_order")}
compare_issue_order
printf 'order_ok=%s\\n' "$order_ok"`,
  ]);
  const output = result.stdout.toString();
  expectNoMissingHelpers(output, result.stderr.toString());
  expect(result.exitCode).toBe(0);
  const ok = /^order_ok=(\d)$/m.exec(output)?.[1] ?? "";
  return { ok, output };
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

  test("fails a stream order shorter than the comments written", () => {
    const { ok, output } = compare({
      written: ["c1", "c2"],
      published: ["c1"],
      lines: both({ c1: 5 }),
    });
    expect(ok).toBe("0");
    expect(output).toContain("2 comments of LIVE-1 were written and 1 read back in stream order");
  });

  test("fails a comparison that holds nothing", () => {
    const { ok, output } = compare({ written: [], published: [], lines: both({}) });
    expect(ok).toBe("0");
    expect(output).toContain("nothing was compared");
  });
});
