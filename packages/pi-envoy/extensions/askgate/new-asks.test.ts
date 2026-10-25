import { describe, expect, test } from "bun:test";

import { askBlocks, type NewAsk, newAsks } from "./new-asks";

/** A row of the Dispatch server's own table of inserted markdown and the ask blocks it adds. */
interface DecisionBlockInsert {
  readonly name: string;
  readonly markdown: string;
  readonly asks: readonly string[];
}

// The table the server's TestDocumentEditAdviceCountsTheAskBlocksTheEditAdded reads, which checks
// each row's ids against the document the server stored.
const table: DecisionBlockInsert[] = JSON.parse(
  await Bun.file(
    new URL(
      "../../../envoy/internal/dispatch/api/testdata/decision-block-inserts.json",
      import.meta.url
    )
  ).text()
);

const ids = (asks: readonly NewAsk[]) => asks.map((ask) => ask.id);

describe("an insert opens the ask blocks the Dispatch server adds for it", () => {
  test("the server's table has rows", () => {
    expect(table.length).toBeGreaterThan(0);
  });
  for (const row of table) {
    test(row.name, () => {
      const input = {
        issue: "DSP-1",
        artifact: "spec",
        ops: [{ op: "insert", markdown: row.markdown, after: "end" }],
      };
      expect(ids(newAsks("dispatch_doc_edit", input))).toEqual([...row.asks]);
    });
  }
});

describe("an ask block's text is its body, read past its containers", () => {
  test("at the top level", () => {
    expect(askBlocks(":::ask{#window}\nWhich deployment window?\n:::")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?",
        opening: "Which deployment window?",
      },
    ]);
  });

  test("in a blockquote, its options included", () => {
    const markdown = [
      '> :::ask{#window urgency="high"}',
      "> Which deployment window?",
      ">",
      "> - Friday night: fewer users.",
      "> - Monday morning: staff on hand.",
      "> :::",
    ].join("\n");
    expect(askBlocks(markdown)).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?\n\n- Friday night: fewer users.\n- Monday morning: staff on hand.",
        opening: "Which deployment window? - Friday night: fewer users. - Mond",
      },
    ]);
  });

  test("in an ordered list item, at the item's content column", () => {
    expect(askBlocks("1. :::ask{#window}\n   Which deployment window?\n   :::")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?",
        opening: "Which deployment window?",
      },
    ]);
  });

  test("a block a blockquote's end closes ends there", () => {
    expect(askBlocks("> :::ask{#window}\n> Which deployment window?\n\nAfter the quote.")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?",
        opening: "Which deployment window?",
      },
    ]);
  });

  test("a block a list item's end closes ends there", () => {
    expect(askBlocks("- :::ask{#window}\n  Which deployment window?\n\nAfter the list.")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?",
        opening: "Which deployment window?",
      },
    ]);
  });

  test("only as many colons as opened a block close it", () => {
    expect(askBlocks("::::ask{#window}\nWhich?\n:::\nStill the ask.\n::::")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which?\n:::\nStill the ask.",
        opening: "Which? ::: Still the ask.",
      },
    ]);
  });

  test("a block inside a callout opens", () => {
    expect(ids(askBlocks("::::callout{}\n:::ask{#window}\nWhich?\n:::\n::::"))).toEqual(["window"]);
  });

  test("a block inside a footnote definition opens", () => {
    expect(
      ids(askBlocks("Text.[^1]\n\n[^1]: Note.\n\n    :::ask{#window}\n    Which?\n    :::"))
    ).toEqual(["window"]);
  });

  test("a CR LF line break reads as a line feed", () => {
    expect(askBlocks(":::ask{#window}\r\nWhich deployment window?\r\n:::\r\n")).toEqual([
      {
        kind: "block",
        id: "window",
        text: "Which deployment window?",
        opening: "Which deployment window?",
      },
    ]);
  });

  test("a block with no id has none", () => {
    expect(askBlocks(':::ask{urgency="high"}\nWhich?\n:::')).toEqual([
      { kind: "block", text: "Which?", opening: "Which?" },
    ]);
  });

  test("the opening is the text's first 60 code points", () => {
    const question = `${"🚀".repeat(59)}ab`;
    expect(askBlocks(`:::ask{#window}\n${question}\n:::`)[0]?.opening).toBe(`${"🚀".repeat(59)}a`);
  });
});

// A tab spans the columns to the next multiple of four, wherever it stands; each expectation is what
// the server's parser read from the same markdown.
describe("a tab in a line's indentation reads as the columns it spans", () => {
  test("after a quote's marker", () => {
    expect(ids(askBlocks(">\t:::ask{#window}\n>\tWhich?\n>\t:::"))).toEqual(["window"]);
  });

  test("after a list item's marker, and as the item's continuation", () => {
    expect(askBlocks("-\t:::ask{#window}\n\tWhich?\n\t:::")).toEqual([
      { kind: "block", id: "window", text: "Which?", opening: "Which?" },
    ]);
  });

  test("at the top level, four columns in, as code", () => {
    expect(askBlocks("\t:::ask{#window}\n\tWhich?\n\t:::")).toEqual([]);
  });
});

describe("an opener the server would not read as an ask block opens nothing", () => {
  test("a fenced opener", () => {
    expect(askBlocks("```\n:::ask{#example}\nWhich?\n:::\n```")).toEqual([]);
  });

  test("an opener four columns into its container", () => {
    expect(askBlocks("> Quote:\n>\n>     :::ask{#example}\n>     Which?\n>     :::")).toEqual([]);
  });

  test("an opener with two ids, which the server refuses", () => {
    expect(askBlocks(":::ask{#one #two}\nWhich?\n:::")).toEqual([]);
  });

  test("an opener of another typed block", () => {
    expect(askBlocks(":::callout{}\nA note.\n:::")).toEqual([]);
  });

  test("a paragraph's line that only looks like an opener", () => {
    expect(askBlocks("Some text\n    :::ask{#example}\n    Which?")).toEqual([]);
  });
});

// Dispatch's parser lets an empty item, or one numbered other than 1, open no list where it would
// interrupt a block the browser editor holds open (pmdoc's emptyItemGuard and
// interruptsOpenBlock). Each expectation is what the server's parser read from the same markdown.
describe("an item numbered 10 opens a list only where the browser editor's parser lets it", () => {
  for (const [name, markdown, asks] of [
    ["right after indented code", "    code\n10. :::ask{#window}\n    Which?\n    :::", []],
    [
      "after indented code and a blank line",
      "    code\n\n10. :::ask{#window}\n    Which?\n    :::",
      [],
    ],
    [
      "in an item opened on a paragraph's next line",
      "Some text.\n- 10. :::ask{#window}\n      Which?\n      :::",
      [],
    ],
    [
      "after a blank line under the paragraph",
      "Some text.\n\n- 10. :::ask{#window}\n      Which?\n      :::",
      ["window"],
    ],
    [
      "under a table",
      "| a | b |\n| - | - |\n- 10. :::ask{#window}\n      Which?\n      :::",
      ["window"],
    ],
    [
      "after indented code right after a list",
      "- a\n\n      code\n10. :::ask{#window}\n    Which?\n    :::",
      ["window"],
    ],
    [
      "after indented code right after a quote",
      "> Quote.\n    code\n10. :::ask{#window}\n    Which?\n    :::",
      ["window"],
    ],
  ] as const) {
    test(name, () => {
      expect(ids(askBlocks(markdown))).toEqual([...asks]);
    });
  }
});

describe("a document's front matter is no markdown", () => {
  const closed = "---\n:::ask{#quoted}\nWhich?\n:::\n---\n\n:::ask{#window}\nWhich?\n:::";

  test("a spec's front matter holds no ask block", () => {
    expect(ids(askBlocks(closed))).toEqual(["window"]);
  });

  test("an insert at the document's start reads its front matter", () => {
    const ops = [{ op: "insert", markdown: closed, before: "start" }];
    expect(ids(newAsks("dispatch_doc_edit", { issue: "DSP-1", artifact: "spec", ops }))).toEqual([
      "window",
    ]);
  });

  test("an insert anywhere else reads a leading --- as a rule", () => {
    const ops = [{ op: "insert", markdown: closed, after: "end" }];
    expect(ids(newAsks("dispatch_doc_edit", { issue: "DSP-1", artifact: "spec", ops }))).toEqual([
      "quoted",
      "window",
    ]);
  });

  test("after an opener nothing closes, no quote opens at the document's level", () => {
    const markdown = "---\n> :::ask{#quoted}\n> Which?\n> :::\n\n:::ask{#window}\nWhich?\n:::";
    expect(ids(askBlocks(markdown))).toEqual(["window"]);
  });
});

describe("an id is read as the server reads attributes", () => {
  test("a # inside a quoted value is no id", () => {
    expect(askBlocks(':::ask{answer="see #12"}\nWhich?\n:::')).toEqual([
      { kind: "block", text: "Which?", opening: "Which?" },
    ]);
  });

  test("the id beside a quoted value holding a #", () => {
    expect(ids(askBlocks(':::ask{answer="see #12" #window}\nWhich?\n:::'))).toEqual(["window"]);
  });
});

describe("newAsks", () => {
  test("a dispatch ask is one ask: its question, then its options", () => {
    expect(
      newAsks("dispatch_ask", {
        issue: "DSP-1",
        question: "Which window?\nRecommendation: keep Friday.",
        options: [{ label: "Friday", description: "Fewer users." }, { label: "Monday" }],
      })
    ).toEqual([
      {
        kind: "ask",
        text: "Which window?\nRecommendation: keep Friday.\n\n- Friday: Fewer users.\n- Monday",
        opening: "Which window? Recommendation: keep Friday.",
      },
    ]);
  });

  test("a dispatch ask with no options is its question", () => {
    expect(newAsks("dispatch_ask", { issue: "DSP-1", question: " Which window? " })).toEqual([
      { kind: "ask", text: "Which window?", opening: "Which window?" },
    ]);
  });

  test("a retype into an ask is an ask whose text the document holds", () => {
    expect(
      newAsks("dispatch_doc_edit", {
        issue: "DSP-1",
        artifact: "spec",
        ops: [
          { op: "retype", block: "b-7", type: "ask", attributes: { urgency: "high" } },
          { op: "retype", block: "b-8", type: "callout" },
        ],
      })
    ).toEqual([{ kind: "retype", id: "b-7", text: "", opening: "" }]);
  });

  test("a doc-edit's asks in the order of its operations", () => {
    expect(
      newAsks("dispatch_doc_edit", {
        issue: "DSP-1",
        artifact: "spec",
        ops: [
          { op: "insert", markdown: ":::ask{#first}\nFirst?\n:::", after: "end" },
          { op: "retype", block: "b-7", type: "ask" },
          { op: "insert", markdown: "> :::ask{#third}\n> Third?\n> :::", before: "start" },
        ],
      }).map((ask) => [ask.kind, ask.id])
    ).toEqual([
      ["block", "first"],
      ["retype", "b-7"],
      ["block", "third"],
    ]);
  });

  test("a replace's with opens no block", () => {
    expect(
      newAsks("dispatch_doc_edit", {
        issue: "DSP-1",
        artifact: "spec",
        ops: [{ op: "replace", find: "Old", with: ":::ask{#window}\nWhich?\n:::" }],
      })
    ).toEqual([]);
  });

  test("a new issue's spec opens each ask block in it", () => {
    const spec = [
      "# Spec",
      "",
      "```md",
      ":::ask{#example}",
      "Which?",
      ":::",
      "```",
      "",
      ":::ask{#window}",
      "Which deployment window?",
      ":::",
      "",
      "> :::ask{#region}",
      "> Which region?",
      "> :::",
    ].join("\n");
    expect(ids(newAsks("dispatch_issue", { project: "DSP", title: "Spec", spec }))).toEqual([
      "window",
      "region",
    ]);
  });

  test("a new issue without a spec opens nothing", () => {
    expect(newAsks("dispatch_issue", { project: "DSP", title: "Spec" })).toEqual([]);
  });

  test("an upload from --content-file opens the ask blocks of its content", () => {
    expect(
      ids(
        newAsks("dispatch_artifact", {
          issue: "DSP-1",
          name: "brief.md",
          content: "# Brief\n\n:::ask{#window}\nWhich deployment window?\n:::\n",
        })
      )
    ).toEqual(["window"]);
  });

  test("an upload from --path opens the ask blocks of the content the gate read from it", () => {
    expect(
      ids(
        newAsks("dispatch_artifact", {
          issue: "DSP-1",
          name: "brief.md",
          path: "docs/brief.md",
          content: "> :::ask{#window}\n> Which deployment window?\n> :::\n",
        })
      )
    ).toEqual(["window"]);
  });

  test("an upload with no content opens nothing", () => {
    expect(
      newAsks("dispatch_artifact", { issue: "DSP-1", name: "brief.md", path: "docs/brief.md" })
    ).toEqual([]);
  });

  test("an upload skips an ask block whose id the document already holds", () => {
    const content = [
      ':::ask{#window state="answered"}',
      "Which deployment window?",
      ":::",
      "",
      ":::ask{#region}",
      "Which region?",
      ":::",
      "",
      ":::ask{}",
      "Which team?",
      ":::",
    ].join("\n");
    expect(
      newAsks(
        "dispatch_artifact",
        { issue: "DSP-1", name: "brief.md", content },
        new Set(["window", "intro"])
      ).map((ask) => ask.opening)
    ).toEqual(["Which region?", "Which team?"]);
  });

  test("only an upload skips the ids the document holds", () => {
    expect(
      ids(
        newAsks(
          "dispatch_doc_edit",
          {
            issue: "DSP-1",
            artifact: "spec",
            ops: [{ op: "insert", markdown: ":::ask{#window}\nWhich?\n:::", after: "end" }],
          },
          new Set(["window"])
        )
      )
    ).toEqual(["window"]);
  });

  for (const [tool, input] of [
    ["dispatch_edit_ask", { ask: "a-1", question: "Which window?" }],
    ["dispatch_comment", { issue: "DSP-1", body: ":::ask{#window}\nWhich?\n:::" }],
    ["dispatch_message", { issue: "DSP-1", body: ":::ask{#window}\nWhich?\n:::" }],
    ["dispatch_request_approval", { issue: "DSP-1", summary: "Approve the spec." }],
  ] as const) {
    test(`${tool} opens nothing`, () => {
      expect(newAsks(tool, input)).toEqual([]);
    });
  }
});
