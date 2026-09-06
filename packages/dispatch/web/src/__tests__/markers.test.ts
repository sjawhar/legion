import { describe, expect, it } from "bun:test";

import { summarizeAnswer } from "../components/ask-form";
import {
  buildAnswerMarkerComment,
  buildUrgencyMarkerComment,
  effectiveUrgency,
  parseAnswerMarker,
  parseAskMarker,
  parseThreadMarker,
  parseUrgencyMarker,
  stripMarker,
} from "../markers";

const htmlThread = [
  "<!-- dispatch:thread",
  "requestId: R",
  "urgency: high",
  "origin:",
  "    host: opencode",
  "    cwd: '/home/ubuntu/notes: issue #42'",
  "    tmux: legion-2.0:12.1",
  "    pane: '%15'",
  "    sessionId: ses_abc",
  "    sessionTitle: 'pm: e2e submitter identity'",
  "ask:",
  "    - askId: R",
  "      question: Color?",
  "      header: Color",
  "      options:",
  "        - label: blue",
  "        - label: red",
  "-->",
  "",
  "**Subject**",
  "",
  "## Context",
  "",
  "Body",
].join("\n");

const legacyThread =
  "---\nurgency: med\nrequestId: L\norigin:\n  host: omp\n  tmux: main:3.0\n  pane: '%840'\nask:\n  - question: Color?\n---\n\n**Subject**\n\nBody";

describe("markers — parseThreadMarker", () => {
  it("reads the HTML-comment encoding with session identity and askIds", () => {
    expect(parseThreadMarker(htmlThread)).toEqual({
      requestId: "R",
      urgency: "high",
      origin: {
        host: "opencode",
        cwd: "/home/ubuntu/notes: issue #42",
        tmux: "legion-2.0:12.1",
        pane: "%15",
        sessionId: "ses_abc",
        sessionTitle: "pm: e2e submitter identity",
      },
      ask: [
        {
          askId: "R",
          question: "Color?",
          header: "Color",
          options: [{ label: "blue" }, { label: "red" }],
        },
      ],
    });
  });

  it("still reads legacy front matter", () => {
    expect(parseThreadMarker(legacyThread)).toEqual({
      requestId: "L",
      urgency: "med",
      origin: { host: "omp", tmux: "main:3.0", pane: "%840" },
      ask: [{ question: "Color?" }],
    });
  });

  it("returns null for other kinds, invalid urgency, missing requestId, and markers not at the start", () => {
    expect(parseThreadMarker("plain body")).toBeNull();
    expect(parseThreadMarker("---\nkind: answer\nforThread: 1\nanswers: [[a]]\n---\n")).toBeNull();
    expect(parseThreadMarker("<!-- dispatch:ask\nrequestId: R\nask: []\n-->")).toBeNull();
    expect(
      parseThreadMarker("<!-- dispatch:thread\nrequestId: R\nurgency: nuclear\n-->")
    ).toBeNull();
    expect(parseThreadMarker("<!-- dispatch:thread\nurgency: med\n-->")).toBeNull();
    expect(parseThreadMarker("\n<!-- dispatch:thread\nrequestId: R\nurgency: med\n-->")).toBeNull();
    expect(parseThreadMarker("<!-- dispatch:thread\nrequestId: R\nurgency: med\n")).toBeNull();
  });

  it("drops a host outside the union, unknown origin keys, and malformed ask blocks", () => {
    const body =
      "<!-- dispatch:thread\nrequestId: R\nurgency: med\norigin:\n    host: pirate\n    cwd: /tmp\n    rogue: 1\nask:\n    - just a string\n-->";
    expect(parseThreadMarker(body)).toEqual({
      requestId: "R",
      urgency: "med",
      origin: { cwd: "/tmp" },
    });
  });
});

describe("markers — parseAskMarker", () => {
  it("reads a follow-up ask comment", () => {
    const body =
      "<!-- dispatch:ask\nrequestId: F\norigin:\n    host: omp\n    sessionId: ses_2\nask:\n    - askId: F\n      question: Which?\n    - askId: F.1\n      question: How many?\n-->\n\n## Context\n\nc";
    expect(parseAskMarker(body)).toEqual({
      requestId: "F",
      origin: { host: "omp", sessionId: "ses_2" },
      ask: [
        { askId: "F", question: "Which?" },
        { askId: "F.1", question: "How many?" },
      ],
    });
  });

  it("yields an empty ask list when the turn carries no structured question", () => {
    expect(parseAskMarker("<!-- dispatch:ask\nrequestId: F\n-->\n\n## Context")).toEqual({
      requestId: "F",
      ask: [],
    });
  });

  it("returns null for thread markers, missing requestId, and prose", () => {
    expect(parseAskMarker(htmlThread)).toBeNull();
    expect(parseAskMarker("<!-- dispatch:ask\nask: []\n-->")).toBeNull();
    expect(parseAskMarker("a reply that mentions dispatch:ask")).toBeNull();
  });
});

describe("markers — parseAnswerMarker", () => {
  it("reads an HTML-comment answer naming its ask", () => {
    const body =
      '<!-- dispatch:answer\nforThread: 641\nforAsk: "R.1"\nanswers:\n  - - "ship"\n-->\n\n**Q** — ship';
    expect(parseAnswerMarker(body)).toEqual({ forThread: 641, forAsk: "R.1", answers: [["ship"]] });
  });

  it("reads a legacy answer as naming no ask", () => {
    const body =
      "---\nkind: answer\nforThread: 641\nanswers:\n  - [ship]\n  - [north, east]\n---\n";
    expect(parseAnswerMarker(body)).toEqual({
      forThread: 641,
      forAsk: null,
      answers: [["ship"], ["north", "east"]],
    });
  });

  it("returns null for other kinds and non-string answers", () => {
    expect(parseAnswerMarker("<!-- dispatch:urgency\nurgency: high\n-->")).toBeNull();
    expect(
      parseAnswerMarker("---\nkind: answer\nforThread: 641\nanswers:\n  - ship\n---\n")
    ).toBeNull();
    expect(parseAnswerMarker("plain comment")).toBeNull();
  });
});

describe("markers — parseUrgencyMarker / effectiveUrgency", () => {
  it("reads both encodings", () => {
    expect(
      parseUrgencyMarker("<!-- dispatch:urgency\nurgency: high\n-->\n\nUrgency set to **high**.")
    ).toBe("high");
    expect(parseUrgencyMarker("---\nkind: urgency\nurgency: low\n---\n")).toBe("low");
    expect(parseUrgencyMarker("---\nkind: other\nurgency: high\n---\n")).toBeNull();
    expect(parseUrgencyMarker("plain comment")).toBeNull();
  });

  it("latest urgency marker wins across encodings", () => {
    const legacyHigh = "---\nkind: urgency\nurgency: high\n---\n";
    expect(
      effectiveUrgency("med", [
        { body: legacyHigh },
        { body: "noise" },
        { body: buildUrgencyMarkerComment("low") },
      ])
    ).toBe("low");
    expect(effectiveUrgency("med", [])).toBe("med");
  });
});

describe("markers — writers", () => {
  it("writes the answer as an HTML comment naming the ask, with the summary below", () => {
    const out = buildAnswerMarkerComment(
      641,
      "R.1",
      [["blue", "free text"]],
      "**Color** — Color?\nblue, free text"
    );
    expect(out.startsWith("<!-- dispatch:answer\n")).toBe(true);
    expect(out).not.toContain("---");
    expect(out).toContain("\n-->\n\n**Color** — Color?\nblue, free text");
    expect(parseAnswerMarker(out)).toEqual({
      forThread: 641,
      forAsk: "R.1",
      answers: [["blue", "free text"]],
    });
  });

  it("writes urgency as an HTML comment with a one-line summary", () => {
    const out = buildUrgencyMarkerComment("blocking");
    expect(out.startsWith("<!-- dispatch:urgency\n")).toBe(true);
    expect(out.endsWith("-->\n\nUrgency set to **blocking**.")).toBe(true);
    expect(parseUrgencyMarker(out)).toBe("blocking");
  });

  it("keeps the marker a single comment when an answer contains comment delimiters", () => {
    const out = buildAnswerMarkerComment(1, "R", [["A --> B", "<!-- x", "y --!> z"]], "summary");
    const markerEnd = out.indexOf("\n-->");
    expect(markerEnd).toBeGreaterThan(0);
    const inside = out.slice("<!-- dispatch:answer\n".length, markerEnd);
    expect(inside).not.toContain("-->");
    expect(inside).not.toContain("<!--");
    expect(inside).not.toContain("--!>");
    expect(parseAnswerMarker(out)?.answers).toEqual([["A --> B", "<!-- x", "y --!> z"]]);
  });
});

describe("markers — stripMarker", () => {
  it("returns the body after an HTML-comment marker", () => {
    expect(stripMarker(htmlThread)).toBe("**Subject**\n\n## Context\n\nBody");
  });

  it("returns the body after legacy front matter", () => {
    expect(stripMarker(legacyThread)).toBe("**Subject**\n\nBody");
  });

  it("returns the original body when there is no marker", () => {
    expect(stripMarker("plain text")).toBe("plain text");
  });
});

describe("ask-form — summarizeAnswer", () => {
  it("includes header, prompt, and values", () => {
    const out = summarizeAnswer(
      { header: "Sanity check", question: "Did it land?", options: [{ label: "yes" }] },
      ["yes"]
    );
    expect(out).toBe("**Sanity check** — Did it land?\nyes");
    expect(summarizeAnswer({ question: "Q" }, [], 2)).toBe("**Question 3** — Q\nNo answer");
  });
});
