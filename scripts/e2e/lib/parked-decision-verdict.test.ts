import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { runJq } from "./run-jq.ts";

// The parked-decision verdict (INFRA-214): each person's spec decision the outbox finished
// undelivered, paired by its row with the line that relayed it, found it relayed, or relayed nothing
// because no person made it. Judged on a daemon log as the daemon writes it, one JSON object a line.

const program = fileURLToPath(new URL("./parked-decision-verdict.jq", import.meta.url));
const root = join(import.meta.dir, "..", "..", "..");

interface Verdict {
  undelivered: { time: string; row: number; kind: string; issue: string }[];
  sent: {
    time: string;
    msg: string;
    row: number;
    key: string;
    kind: string;
    issue: string;
    root: string;
  }[];
  unrelayed: { time: string; row: number; key: string; kind: string; issue: string; ref: string }[];
  unmatched: { time: string; row: number; kind: string; issue: string }[];
}

const stopped = "outbox notice finished undelivered: its architect stopped with its finished tree";
const ended = "outbox notice finished undelivered: its architect will not hold its role again";
const noArchitect = "outbox notice finished undelivered: it has no architect";
const sentText = "outbox parked decision sent to the controller";
const alreadySent = "outbox parked decision already sent to the controller";
const notRelayed = "outbox parked decision not relayed: it names no person";

type Line = Record<string, unknown>;
const at = (seconds: number) =>
  new Date(Date.UTC(2026, 9, 24, 12, 0, 0) + seconds * 1000).toISOString();
const undelivered = (t: number, row: number, kind: string, msg = stopped): Line => ({
  time: at(t),
  level: "INFO",
  msg,
  row,
  kind,
  issue: "ACME-110",
  architect: "legion-acme-110-architect",
});
const relay = (t: number, msg: string, row: number, extra: Line = {}): Line => ({
  time: at(t),
  level: "INFO",
  msg,
  row,
  kind: "design-approved",
  issue: "ACME-110",
  root: "ACME-110",
  key: `outbox:${row}`,
  ...extra,
});

function verdict(log: (Line | string)[]): Verdict {
  const input = log
    .map((line) => (typeof line === "string" ? line : JSON.stringify(line)))
    .join("\n");
  return JSON.parse(runJq(["-R", "-s", "-c", "-f", program], `${input}\n`));
}

describe("parked-decision-verdict.jq", () => {
  test("an undelivered approval and the line that sent it pair, so nothing is unmatched", () => {
    const got = verdict([undelivered(1, 7, "design-approved"), relay(2, sentText, 7)]);
    expect(got.undelivered).toEqual([
      { time: at(1), row: 7, kind: "design-approved", issue: "ACME-110" },
    ]);
    expect(got.sent).toEqual([
      {
        time: at(2),
        msg: sentText,
        row: 7,
        key: "outbox:7",
        kind: "design-approved",
        issue: "ACME-110",
        root: "ACME-110",
      },
    ]);
    expect(got.unrelayed).toEqual([]);
    expect(got.unmatched).toEqual([]);
  });

  test("an undelivered approval with no sent line is unmatched by its row", () => {
    const got = verdict([undelivered(1, 7, "design-approved", ended), relay(2, sentText, 8)]);
    expect(got.unmatched).toEqual([
      { time: at(1), row: 7, kind: "design-approved", issue: "ACME-110" },
    ]);
  });

  test("an already-sent line pairs, and a not-relayed line pairs and is listed with its key and ref", () => {
    const got = verdict([
      undelivered(1, 7, "design-approved"),
      relay(2, alreadySent, 7),
      undelivered(3, 8, "design-changes-requested"),
      relay(4, notRelayed, 8, { kind: "design-changes-requested", ref: "" }),
      undelivered(5, 9, "carried-gate-opened", ended),
      relay(6, notRelayed, 9, { kind: "carried-gate-opened", ref: "dispatch://ACME-110/spec" }),
    ]);
    expect(got.unmatched).toEqual([]);
    expect(got.sent.map((line) => [line.msg, line.row])).toEqual([[alreadySent, 7]]);
    expect(got.unrelayed).toEqual([
      {
        time: at(4),
        row: 8,
        key: "outbox:8",
        kind: "design-changes-requested",
        issue: "ACME-110",
        ref: "",
      },
      {
        time: at(6),
        row: 9,
        key: "outbox:9",
        kind: "carried-gate-opened",
        issue: "ACME-110",
        ref: "dispatch://ACME-110/spec",
      },
    ]);
  });

  test("a notice that is no spec decision, and a row with no architect, are not counted", () => {
    const got = verdict([
      undelivered(1, 10, "child-status"),
      undelivered(2, 11, "worker-died", ended),
      undelivered(3, 12, "design-approved", noArchitect),
    ]);
    expect(got.undelivered).toEqual([]);
    expect(got.unmatched).toEqual([]);
  });

  test("a line that is not a JSON object is skipped", () => {
    const got = verdict([
      "panic: not json",
      "[1,2]",
      '"a string"',
      undelivered(1, 7, "design-approved"),
      "{truncated",
    ]);
    expect(got.undelivered.map((line) => line.row)).toEqual([7]);
    expect(got.unmatched.map((line) => line.row)).toEqual([7]);
  });
});

describe("the texts the verdict counts are the daemon's own", () => {
  test("the outbox writes each line the verdict matches, with the text it matches", () => {
    const daemon = join(root, "packages", "daemon", "internal", "daemon");
    const outbox = readFileSync(join(daemon, "outbox.go"), "utf8");
    const jq = readFileSync(program, "utf8");
    const prefix = "outbox notice finished undelivered: its architect";
    expect(outbox).toContain(`"${prefix}`);
    expect(jq).toContain(JSON.stringify(prefix));
    const parked = readFileSync(join(daemon, "parked_decision.go"), "utf8");
    for (const text of [sentText, alreadySent, notRelayed]) {
      expect(parked).toContain(JSON.stringify(text));
      expect(jq).toContain(JSON.stringify(text));
    }
  });
});
