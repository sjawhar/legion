import { describe, expect, it } from "bun:test";

import {
  buildAnswerMarkerComment,
  buildUrgencyMarkerComment,
  effectiveUrgency,
  parseAnswerMarker,
  parseMetaMarker,
  parseUrgencyMarker,
  stripMetaMarker,
} from "../markers";

describe("markers — parseMetaMarker", () => {
  it("returns null when the body has no frontmatter", () => {
    expect(parseMetaMarker("plain body, no metadata")).toBeNull();
  });

  it("reads urgency from leading YAML frontmatter", () => {
    const body = "---\nurgency: high\nrequestId: R\n---\n";
    expect(parseMetaMarker(body)?.urgency).toBe("high");
  });
});

describe("markers — parseMetaMarker requestId", () => {
  it("reads requestId from leading YAML frontmatter", () => {
    const body = "---\nurgency: med\nrequestId: req-7\n---\n";
    expect(parseMetaMarker(body)?.requestId).toBe("req-7");
  });
});

describe("markers — parseMetaMarker urgency validation", () => {
  it("returns null for an unknown urgency value", () => {
    const body = "---\nurgency: nuclear\nrequestId: R\n---\n";
    expect(parseMetaMarker(body)).toBeNull();
  });
});

describe("markers — parseMetaMarker ask payload", () => {
  it("reads a nested ask array from frontmatter", () => {
    const body = [
      "---",
      "urgency: med",
      "requestId: R",
      "ask:",
      "  - question: Color?",
      "    header: Color",
      "    options:",
      "      - label: blue",
      "      - label: red",
      "---",
      "",
    ].join("\n");
    const parsed = parseMetaMarker(body);
    expect(parsed?.ask as unknown).toEqual([
      {
        question: "Color?",
        header: "Color",
        options: [{ label: "blue" }, { label: "red" }],
      },
    ]);
  });
});

describe("markers — parseMetaMarker origin", () => {
  it("reads a nested origin block from frontmatter", () => {
    const body = [
      "---",
      "urgency: med",
      "requestId: R",
      "origin:",
      "  host: omp",
      "  machine: example-host",
      "  cwd: /home/ubuntu/legion",
      "  tmux: main:3.0",
      "---",
      "",
    ].join("\n");
    expect(parseMetaMarker(body)?.origin).toEqual({
      host: "omp",
      machine: "example-host",
      cwd: "/home/ubuntu/legion",
      tmux: "main:3.0",
    });
  });

  it("reads colon-bearing origin values as the Go marshaller emits them", () => {
    // tmux targets are plain scalars (no space after the colon); a cwd with
    // ": " or "#" arrives single-quoted from yaml.v3.
    const body = [
      "---",
      "urgency: med",
      "requestId: R",
      "origin:",
      "  host: omp",
      "  cwd: '/home/ubuntu/notes: issue #42'",
      "  tmux: legion-2.0:12.1",
      "---",
      "",
    ].join("\n");
    expect(parseMetaMarker(body)?.origin).toEqual({
      host: "omp",
      cwd: "/home/ubuntu/notes: issue #42",
      tmux: "legion-2.0:12.1",
    });
  });

  it("is undefined when the marker has no origin block", () => {
    const body = "---\nurgency: med\nrequestId: R\n---\n";
    expect(parseMetaMarker(body)?.origin).toBeUndefined();
  });

  it("keeps the marker parseable when origin carries unrecognized keys", () => {
    const body = [
      "---",
      "urgency: med",
      "requestId: R",
      "origin:",
      "  host: omp",
      "  sessionId: abc123",
      "---",
      "",
    ].join("\n");
    const parsed = parseMetaMarker(body);
    expect(parsed?.urgency).toBe("med");
    expect(parsed?.origin).toEqual({ host: "omp" });
  });

  it("drops an origin block that is not an object", () => {
    const body = "---\nurgency: med\nrequestId: R\norigin: not-an-object\n---\n";
    expect(parseMetaMarker(body)?.origin).toBeUndefined();
  });

  it("drops a host outside the union envoy-client can emit", () => {
    const body = "---\nurgency: med\nrequestId: R\norigin:\n  host: pirate\n  cwd: /tmp\n---\n";
    expect(parseMetaMarker(body)?.origin).toEqual({ cwd: "/tmp" });
  });
});

describe("markers — parseMetaMarker ask validation", () => {
  it("drops an ask block whose entries are not questions", () => {
    const body = "---\nurgency: med\nrequestId: R\nask:\n  - just a string\n---\n";
    const parsed = parseMetaMarker(body);
    expect(parsed?.urgency).toBe("med");
    expect(parsed?.ask).toBeUndefined();
  });

  it("drops a question whose options are not a list of labels", () => {
    const body =
      "---\nurgency: med\nrequestId: R\nask:\n  - question: Color?\n    options: red\n---\n";
    expect(parseMetaMarker(body)?.ask).toBeUndefined();
  });

  it("keeps a question that omits header and options, as the Go marshaller emits it", () => {
    const body = "---\nurgency: med\nrequestId: R\nask:\n  - question: Color?\n---\n";
    expect(parseMetaMarker(body)?.ask).toEqual([{ question: "Color?" }]);
  });
});

describe("markers — parseUrgencyMarker", () => {
  it("reads urgency from a kind=urgency frontmatter comment", () => {
    const body = "---\nkind: urgency\nurgency: high\n---\n";
    expect(parseUrgencyMarker(body)).toBe("high");
  });

  it("returns null for unrelated frontmatter (no urgency kind)", () => {
    const body = "---\nkind: other\nurgency: high\n---\n";
    expect(parseUrgencyMarker(body)).toBeNull();
  });

  it("returns null for non-frontmatter bodies", () => {
    expect(parseUrgencyMarker("plain comment")).toBeNull();
  });
});

describe("markers — parseAnswerMarker", () => {
  it("reads forThread + answers from a kind=answer frontmatter comment", () => {
    const body = [
      "---",
      "kind: answer",
      "forThread: 641",
      "answers:",
      "  - [ship]",
      "  - [north, east]",
      "---",
      "",
    ].join("\n");
    expect(parseAnswerMarker(body)).toEqual({
      forThread: 641,
      answers: [["ship"], ["north", "east"]],
    });
  });

  it("returns null for unrelated frontmatter", () => {
    expect(parseAnswerMarker("---\nkind: urgency\nurgency: high\n---\n")).toBeNull();
  });

  it("returns null for non-frontmatter bodies", () => {
    expect(parseAnswerMarker("plain comment")).toBeNull();
  });

  it("returns null when the answers are not lists of strings", () => {
    const body = "---\nkind: answer\nforThread: 641\nanswers:\n  - ship\n---\n";
    expect(parseAnswerMarker(body)).toBeNull();
  });
});

describe("markers — stripMetaMarker", () => {
  it("returns the body text after the frontmatter block", () => {
    const body = "---\nurgency: med\nrequestId: R\n---\n\n**Subject**\n\nBody";
    expect(stripMetaMarker(body)).toBe("**Subject**\n\nBody");
  });

  it("returns the original body when there is no frontmatter", () => {
    expect(stripMetaMarker("plain text")).toBe("plain text");
  });
});

describe("markers — buildAnswerMarkerComment", () => {
  it("round-trips through parseAnswerMarker", () => {
    const out = buildAnswerMarkerComment(641, [["blue"], ["free text"]], "Color: blue");
    const parsed = parseAnswerMarker(out);
    expect(parsed?.forThread).toBe(641);
    expect(parsed?.answers).toEqual([["blue"], ["free text"]]);
  });

  it("emits the human-readable summary below the frontmatter", () => {
    const out = buildAnswerMarkerComment(1, [["a"]], "Q1: a");
    expect(out).toContain("Q1: a");
    expect(out.startsWith("---\n")).toBe(true);
  });
});

describe("markers — buildUrgencyMarkerComment", () => {
  it("round-trips through parseUrgencyMarker", () => {
    expect(parseUrgencyMarker(buildUrgencyMarkerComment("blocking"))).toBe("blocking");
    expect(parseUrgencyMarker(buildUrgencyMarkerComment("low"))).toBe("low");
  });
});

describe("markers — effectiveUrgency", () => {
  it("returns body urgency when no marker comments are present", () => {
    expect(effectiveUrgency("med", [])).toBe("med");
    expect(effectiveUrgency("med", [{ body: "plain reply" }])).toBe("med");
  });

  it("uses the latest urgency-marker comment, per spec §5.4", () => {
    const high = buildUrgencyMarkerComment("high");
    const low = buildUrgencyMarkerComment("low");
    expect(effectiveUrgency("med", [{ body: high }])).toBe("high");
    expect(effectiveUrgency("med", [{ body: high }, { body: "noise" }, { body: low }])).toBe("low");
  });
});

import { summarizeAnswers } from "../components/ask-form";
import type { MarkerQuestion } from "../markers";

describe("ask-form — summarizeAnswers", () => {
  it("includes the question prompt alongside the header and answer values", () => {
    const ask: MarkerQuestion[] = [
      {
        header: "Sanity check",
        question: "Did the YAML migration land cleanly?",
        options: [{ label: "yes", description: "All good" }],
      },
    ];
    const out = summarizeAnswers(ask, [["yes"]]);
    expect(out).toContain("Sanity check");
    expect(out).toContain("Did the YAML migration land cleanly?");
    expect(out).toContain("yes");
  });
});
