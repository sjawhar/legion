import { describe, expect, test } from "bun:test";
import fixture from "../__fixtures__/display-read-census.json" with { type: "json" };
import {
  CLASSES,
  canonicalTree,
  censusReport,
  classify,
  type Group,
  isSubsequence,
  MIN_STORED_PARAGRAPH,
  type PmNode,
  paragraphs,
  readingMode,
  type StoredText,
  storedParagraphs,
  storedTextIn,
  type TextClass,
  versionOrder,
  visibleText,
} from "../display-read-census.ts";

/** One fixture case: the two readings of one text and the class the census puts it in. */
interface FixtureCase {
  readonly id: string;
  readonly group: Group;
  readonly markdown: string;
  /** `the readers` for a reading this repository's own two readers made of `markdown`, `written`
   * for a pair written by hand because no text produced it. */
  readonly from: string;
  readonly why: string;
  readonly go: PmNode | null;
  readonly browser: PmNode | null;
  readonly browserError?: string;
  readonly class: TextClass;
}

const cases = fixture.cases as unknown as readonly FixtureCase[];

function reading(one: FixtureCase) {
  return {
    go: one.go,
    browser: one.browser,
    ...(one.browserError === undefined ? {} : { browserError: one.browserError }),
  };
}

describe("the display read census classes a text", () => {
  for (const one of cases) {
    test(`${one.class}: ${one.id}`, () => {
      expect(classify(reading(one))).toBe(one.class);
    });
  }

  test("every class the census can report has a case", () => {
    expect([...new Set(cases.map((one) => one.class))].sort()).toEqual([...CLASSES].sort());
  });

  test("the readers themselves made every case but the two the fixture says are written", () => {
    expect(cases.filter((one) => one.from === "written").map((one) => one.class)).toEqual([
      "format differs",
      "Go cannot read it",
    ]);
  });

  test("a document version is held to the editing engine, every other text to the read-only one", () => {
    expect(readingMode("document-version")).toBe("version");
    expect(readingMode("comment")).toBe("latest");
    expect(readingMode("ask-answer")).toBe("latest");
  });
});

describe("the comparison the classes rest on", () => {
  test("block ids, null attributes and mark order do not make two trees differ", () => {
    const left: PmNode = {
      type: "doc",
      content: [
        {
          type: "paragraph",
          attrs: { blockId: "b-1" },
          content: [
            {
              type: "text",
              text: "x",
              marks: [{ type: "strong" }, { type: "emphasis", attrs: { marker: "*" } }],
            },
          ],
        },
      ],
    };
    const right: PmNode = {
      type: "doc",
      content: [
        {
          type: "paragraph",
          attrs: { blockId: "b-9", title: null },
          content: [
            {
              type: "text",
              text: "x",
              marks: [{ type: "emphasis", attrs: { marker: "*" } }, { type: "strong" }],
            },
          ],
        },
      ],
    };
    expect(canonicalTree(left)).toBe(canonicalTree(right));
  });

  test("an attribute that is not the block id does make them differ", () => {
    expect(canonicalTree({ type: "doc", attrs: { a: 1 } })).not.toBe(
      canonicalTree({ type: "doc", attrs: { a: 2 } })
    );
  });

  test("a short table body row is padded to the header's width", () => {
    const cell = (alignment: string): PmNode => ({
      type: "table_cell",
      attrs: { alignment, colspan: 1, colwidth: null, rowspan: 1 },
      content: [{ type: "paragraph" }],
    });
    const table = (row: readonly PmNode[]): PmNode => ({
      type: "doc",
      content: [
        {
          type: "table",
          content: [
            { type: "table_header_row", content: [cell("left"), cell("right")] },
            { type: "table_row", content: [...row] },
          ],
        },
      ],
    });
    // The padded cell takes its header cell's alignment, which is `right` here, so a row padded
    // with a `left` cell would not compare equal.
    expect(canonicalTree(table([cell("left")]))).toBe(
      canonicalTree(table([cell("left"), cell("right")]))
    );
    expect(canonicalTree(table([cell("left")]))).not.toBe(
      canonicalTree(table([cell("left"), cell("left")]))
    );
    // A row holding no cell is a table with no body row to either reader, and stays empty.
    expect(canonicalTree(table([]))).not.toBe(canonicalTree(table([cell("left"), cell("right")])));
  });

  test("the visible text is every text node's text, run together", () => {
    expect(
      visibleText({
        type: "doc",
        content: [
          { type: "heading", content: [{ type: "text", text: "Head" }] },
          {
            type: "paragraph",
            content: [
              { type: "text", text: "a" },
              { type: "hardbreak" },
              { type: "text", text: "b" },
            ],
          },
        ],
      })
    ).toBe("Headab");
  });

  test("a subsequence is the whole with characters deleted, equality included", () => {
    expect(isSubsequence("ace", "abcde")).toBe(true);
    expect(isSubsequence("abcde", "abcde")).toBe(true);
    expect(isSubsequence("aec", "abcde")).toBe(false);
    expect(isSubsequence("", "abc")).toBe(true);
    expect(isSubsequence("abc", "")).toBe(false);
  });

  test("versions are read newest first, one document at a time round the documents", () => {
    expect(
      versionOrder([
        { artifact: "long", number: 1 },
        { artifact: "long", number: 2 },
        { artifact: "long", number: 3 },
        { artifact: "short", number: 1 },
      ])
    ).toEqual([
      { artifact: "long", number: 3 },
      { artifact: "short", number: 1 },
      { artifact: "long", number: 2 },
      { artifact: "long", number: 1 },
    ]);
    expect(versionOrder([])).toEqual([]);
  });
});

describe("the report", () => {
  const classed = cases.map((one) => ({
    id: one.id,
    group: one.group,
    class: one.class,
  }));
  const spent = [
    { unit: "issue-comments/A-1", texts: [], requests: 3, at: 10_000, ms: 1_000 },
    { unit: "issue-asks/A-1", texts: [], requests: 2, at: 12_000, ms: 500, error: "404" },
  ];
  const report = censusReport(classed, spent, 4);

  test("every class is printed, a class with nothing in it as zero", () => {
    expect(report.classes.map((count) => count.class)).toEqual([...CLASSES]);
    expect(report.classes.reduce((total, count) => total + count.texts, 0)).toBe(classed.length);
  });

  test("each class splits by group", () => {
    const agree = report.classes.find((count) => count.class === "the two agree");
    expect(agree?.groups).toEqual({ comment: 2, "document-version": 1 });
  });

  test("the groups count the same texts as the classes", () => {
    expect(report.groups.reduce((total, row) => total + row.texts, 0)).toBe(classed.length);
  });

  test("a class that must be empty is reported text by text", () => {
    expect(report.defects.map((row) => row.class).sort()).toEqual([
      "Go cannot read it",
      "Go loses text",
      "the text differs both ways",
    ]);
  });

  test("the requests, the reading time and the refused units come from the units read", () => {
    expect(report.requests).toBe(5);
    expect(report.readSeconds).toBe(3);
    expect(report.classSeconds).toBe(4);
    expect(report.failedUnits).toBe(1);
  });
});

describe("--check-no-stored-text", () => {
  const stored = fixture.guard.stored as readonly StoredText[];
  const index = storedParagraphs(stored);

  test("a paragraph shorter than the guard's length is not indexed", () => {
    expect(paragraphs("Short.")).toEqual([]);
    expect(paragraphs("x".repeat(MIN_STORED_PARAGRAPH))).toEqual([
      "x".repeat(MIN_STORED_PARAGRAPH),
    ]);
  });

  test("a file holding a stored paragraph is refused, and the stored text named", () => {
    const hits = storedTextIn("findings.md", fixture.guard.holds, index);
    expect(hits.map((hit) => hit.id)).toEqual(["version/guard-2/1"]);
    expect(hits[0].group).toBe("document-version");
  });

  test("a file holding none is accepted", () => {
    expect(storedTextIn("findings.md", fixture.guard.clean, index)).toEqual([]);
  });

  test("re-wrapping a stored paragraph does not hide it", () => {
    const wrapped = fixture.guard.holds.replace(/ /gu, "\n");
    expect(storedTextIn("wrapped.md", wrapped, index).map((hit) => hit.id)).toEqual([
      "version/guard-2/1",
    ]);
  });
});
