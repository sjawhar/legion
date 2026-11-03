/**
 * The headless engine's default mode (without `readOnly`) draws a typed block whose content
 * breaks its ProseMirror content rule as its literal lines, where Milkdown used to drop it
 * silently with all its text (`createNodeInParserFail`), and keeps the parser's directive
 * refusals (INFRA-565). The live editor draws such a block the same way, and decides typed
 * blocks as the read-only mode does (`liveTypedBlocksRemarkPlugin`): a container directive the
 * block schema refuses is drawn as its lines too, and a paragraph line that opens like a
 * directive stays the text it is, where a refusal thrown from a paste let the browser insert the
 * plain text itself, its lines joined (sjawhar/legion#2433).
 */

import { expect, spyOn, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { BLOCK_ID_ATTR, createHeadlessProof } from "../src/lib-headless.js";
import { serverBlockSchema as blockSchema, blocks, runs } from "./block-schema-fixture";
import { withMarksEditor } from "./marks-editor";
import { pastePlainText, withHTMLElementGlobal } from "./paste";

/** An ask holding a heading, which its `paragraph+ bullet_list?` rule refuses. */
const HEADING_ASK = ":::ask{#a1}\n# Heading words\n:::\n";
/** An ask whose paragraph comes after its list, which its rule refuses too. */
const LIST_THEN_PARAGRAPH_ASK = "Lead\n\n:::ask{#a1}\nQ?\n\n- a\n\nAfter list\n:::\n";
/** An ask whose heading sits inside a block-level mark span. */
const MARK_SPAN_HEADING_ASK =
  ':::ask{#a1}\n<span data-dispatch="ask" data-id="x">\n\n# Heading words\n\n</span>\n:::\n';
/** A valid ask whose question sits inside a block-level mark span. */
const MARK_SPAN_VALID_ASK =
  ':::ask{#a1}\n<span data-dispatch="ask" data-id="x">\n\nQ?\n\n</span>\n:::\n';

async function defaultParse(markdown: string): Promise<ProseMirrorNode> {
  const proof = await createHeadlessProof({ blockSchema });
  return proof.parseMarkdown(markdown);
}

/** The names of every node type in `doc`, at any depth. */
function nodeTypes(doc: ProseMirrorNode): string[] {
  const types: string[] = [];
  doc.descendants((node) => {
    types.push(node.type.name);
  });
  return types;
}

type NodeJson = { type: string; attrs?: Record<string, unknown>; content?: NodeJson[] };

/**
 * `doc` as JSON without the ids either engine mints on its own: every block id, which each parse
 * mints afresh, and a heading's `id`, which the live editor's heading-id plugin fills in from
 * its text after a load and the headless engine, which has no view, leaves empty.
 */
function withoutMintedIds(doc: ProseMirrorNode): NodeJson {
  const strip = (node: NodeJson): NodeJson => {
    const attrs = { ...node.attrs };
    delete attrs[BLOCK_ID_ATTR];
    if (node.type === "heading") delete attrs.id;
    return { ...node, attrs, content: node.content?.map(strip) };
  };
  return strip(doc.toJSON() as NodeJson);
}

/** Runs `run` with `console.error` stubbed, and asserts Milkdown logged no parser failure. */
async function withoutParserErrors(run: () => void | Promise<void>): Promise<void> {
  const consoleError = spyOn(console, "error").mockImplementation(() => {});
  try {
    await run();
    expect(consoleError).not.toHaveBeenCalled();
  } finally {
    consoleError.mockRestore();
  }
}

/** Asserts `doc` holds an `ask` whose only block is `Q?`, under the `dispatchAsk` mark. */
function expectMarkSpanAsk(doc: ProseMirrorNode): void {
  let ask: ProseMirrorNode | undefined;
  doc.descendants((node) => {
    if (node.type.name === "ask") ask = node;
    return ask === undefined;
  });
  expect(ask).toBeDefined();
  if (ask === undefined) return;
  expect(blocks(ask)).toEqual([["paragraph", "Q?"]]);
  expect(runs(ask.child(0))).toEqual([{ text: "Q?", marks: ["dispatchAsk"] }]);
}

test("default mode draws an ask holding a heading as its lines", async () => {
  await withoutParserErrors(async () => {
    expect(blocks(await defaultParse(HEADING_ASK))).toEqual([
      ["paragraph", ":::ask{#a1}"],
      ["heading", "Heading words"],
      ["paragraph", ":::"],
    ]);
  });
});

test("default mode keeps every line of an ask with a paragraph after its list", async () => {
  await withoutParserErrors(async () => {
    const doc = await defaultParse(LIST_THEN_PARAGRAPH_ASK);
    expect(nodeTypes(doc)).not.toContain("ask");
    expect(blocks(doc)).toEqual([
      ["paragraph", "Lead"],
      ["paragraph", ":::ask{#a1}"],
      ["paragraph", "Q?"],
      ["bullet_list", "a"],
      ["paragraph", "After list"],
      ["paragraph", ":::"],
    ]);
  });
});

test("default mode checks the blocks inside a block-level mark span against the rule", async () => {
  await withoutParserErrors(async () => {
    const doc = await defaultParse(MARK_SPAN_HEADING_ASK);
    expect(nodeTypes(doc)).not.toContain("ask");
    expect(blocks(doc)).toEqual([
      ["paragraph", ":::ask{#a1}"],
      ["heading", "Heading words"],
      ["paragraph", ":::"],
    ]);
  });
});

test("default mode keeps an ask holding only a list typed", async () => {
  const doc = await defaultParse(":::ask{#a1}\n- only list\n:::\n");
  expect(blocks(doc).map(([type]) => type)).toEqual(["ask"]);
  expect(blocks(doc.child(0))).toEqual([
    ["paragraph", ""],
    ["bullet_list", "only list"],
  ]);
});

test("default mode keeps a valid ask typed", async () => {
  const doc = await defaultParse(":::ask{#a1}\nQ?\n\n- a\n- b\n:::\n");
  expect(blocks(doc).map(([type]) => type)).toEqual(["ask"]);
  expect(blocks(doc.child(0))).toEqual([
    ["paragraph", "Q?"],
    ["bullet_list", "ab"],
  ]);
});

test("default mode keeps a callout holding a heading typed", async () => {
  const doc = await defaultParse(":::callout{}\n# Heading words\n:::\n");
  expect(blocks(doc)).toEqual([["callout", "Heading words"]]);
  expect(blocks(doc.child(0))).toEqual([["heading", "Heading words"]]);
});

test("default mode keeps a valid ask in a block-level mark span typed, under its mark", async () => {
  expectMarkSpanAsk(await defaultParse(MARK_SPAN_VALID_ASK));
});

test("default mode still throws the parser's directive refusals", async () => {
  const proof = await createHeadlessProof({ blockSchema });
  expect(() => proof.parseMarkdown(":::note\nA note line\n:::\n")).toThrow(
    'unknown typed block "note"'
  );
  expect(() => proof.parseMarkdown("hello\n::a")).toThrow(
    "line 2: leaf directives (::name) are not supported"
  );
});

test("default mode refuses a leaf directive after a leading byte-order mark", async () => {
  const proof = await createHeadlessProof({ blockSchema });
  expect(() => proof.parseMarkdown("\ufeffhello\n::a")).toThrow(
    "line 2: leaf directives (::name) are not supported"
  );
});

test("default mode names a refused line's own number after a leading byte-order mark", async () => {
  const proof = await createHeadlessProof({ blockSchema });
  expect(() => proof.parseMarkdown("\ufeffx\n\n::: warning")).toThrow("line 3: ");
});

test("a drawn ask serializes to markdown that parses back to the same document", async () => {
  await withoutParserErrors(async () => {
    const proof = await createHeadlessProof({ blockSchema });
    const drawn = proof.parseMarkdown(HEADING_ASK);
    const again = proof.parseMarkdown(proof.serializeMarkdown(drawn));
    expect(nodeTypes(again)).not.toContain("ask");
    expect(withoutMintedIds(again)).toEqual(withoutMintedIds(drawn));
  });
});

test("the live editor keeps every word of a pasted ask holding a heading", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ view }) => {
        withHTMLElementGlobal(() => pastePlainText(view, HEADING_ASK));
        const { doc } = view.state;
        expect(nodeTypes(doc)).not.toContain("ask");
        expect(doc.textContent).toContain(":::ask{#a1}");
        expect(doc.textContent).toContain("Heading words");
        expect(blocks(doc)).toContainEqual(["heading", "Heading words"]);
        expect(blocks(doc)).toContainEqual(["paragraph", ":::"]);
      },
      { blockSchema }
    );
  });
});

test("the live editor draws a pasted ask whose heading sits in a block-level mark span", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ view }) => {
        withHTMLElementGlobal(() => pastePlainText(view, MARK_SPAN_HEADING_ASK));
        const { doc } = view.state;
        expect(nodeTypes(doc)).not.toContain("ask");
        expect(doc.textContent).toContain(":::ask{#a1}");
        expect(blocks(doc)).toContainEqual(["heading", "Heading words"]);
        expect(blocks(doc)).toContainEqual(["paragraph", ":::"]);
      },
      { blockSchema }
    );
  });
});

test("the live editor decides typed blocks after the mark plugins have run", async () => {
  // Decided before the mark plugins, the span would still be raw HTML around the question, and
  // a valid ask would be drawn as text.
  await withMarksEditor(
    "",
    ({ handle, view }) => {
      withHTMLElementGlobal(() => pastePlainText(view, MARK_SPAN_VALID_ASK));
      expectMarkSpanAsk(view.state.doc);
      handle.setMarkdown(MARK_SPAN_VALID_ASK);
      expectMarkSpanAsk(view.state.doc);
    },
    { blockSchema }
  );
});

/** An unknown typed block's source, as a reader copies it from another document. */
const NOTE_DIRECTIVE = ":::note\nhello world\n:::\n";

test("the live editor draws a pasted unknown typed block as its lines", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ view }) => {
        expect(() => pastePlainText(view, NOTE_DIRECTIVE)).not.toThrow();
        expect(blocks(view.state.doc)).toEqual([
          ["paragraph", ":::note"],
          ["paragraph", "hello world"],
          ["paragraph", ":::"],
        ]);
      },
      { blockSchema }
    );
  });
});

test("the live editor draws a pasted ask with a refused attribute as its lines", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ view }) => {
        expect(() => pastePlainText(view, ':::ask{#a1 bogus="x"}\nQ?\n:::')).not.toThrow();
        expect(nodeTypes(view.state.doc)).not.toContain("ask");
        expect(blocks(view.state.doc)).toEqual([
          ["paragraph", ':::ask{#a1 bogus="x"}'],
          ["paragraph", "Q?"],
          ["paragraph", ":::"],
        ]);
      },
      { blockSchema }
    );
  });
});

test("the live editor keeps a pasted line that opens like a leaf directive as text", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ view }) => {
        expect(() => pastePlainText(view, "text\n::note\nmore")).not.toThrow();
        expect(blocks(view.state.doc)).toEqual([["paragraph", "text\n::note\nmore"]]);
      },
      { blockSchema }
    );
  });
});

test("the live editor's markdown import draws an unknown typed block as its lines", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ handle, view }) => {
        expect(() => handle.setMarkdown(NOTE_DIRECTIVE)).not.toThrow();
        expect(blocks(view.state.doc)).toEqual([
          ["paragraph", ":::note"],
          ["paragraph", "hello world"],
          ["paragraph", ":::"],
        ]);
      },
      { blockSchema }
    );
  });
});

test("the live editor's markdown import keeps every line of an ask after its list", async () => {
  await withoutParserErrors(async () => {
    await withMarksEditor(
      "",
      ({ handle, view }) => {
        handle.setMarkdown(LIST_THEN_PARAGRAPH_ASK);
        const { doc } = view.state;
        expect(nodeTypes(doc)).not.toContain("ask");
        expect(blocks(doc)).toEqual([
          ["paragraph", "Lead"],
          ["paragraph", ":::ask{#a1}"],
          ["paragraph", "Q?"],
          ["bullet_list", "a"],
          ["paragraph", "After list"],
          ["paragraph", ":::"],
        ]);
      },
      { blockSchema }
    );
  });
});

test("the headless default mode and the live editor read a drawn ask alike", async () => {
  await withoutParserErrors(async () => {
    const headless = withoutMintedIds(await defaultParse(HEADING_ASK));
    await withMarksEditor(
      "",
      ({ handle, view }) => {
        handle.setMarkdown(HEADING_ASK);
        expect(withoutMintedIds(view.state.doc)).toEqual(headless);
      },
      { blockSchema }
    );
  });
});

/** An answered decision as a past version's text carries it: the server writes every attribute. */
const ANSWERED_ASK =
  ':::ask{#a1 urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-01T00:00:00Z" selected="[&#x22;Option A&#x22;]"}\nWhich one?\n\n- Option A\n- Option B\n:::\n';

test("the live editor's version view keeps an answered decision's server attributes", async () => {
  await withMarksEditor(
    "",
    ({ handle, view }) => {
      handle.setMarkdown(ANSWERED_ASK);
      const ask = view.state.doc.child(0);
      expect(ask.type.name).toBe("ask");
      expect(ask.attrs.state).toBe("answered");
      expect(ask.attrs.answered_by).toBe("alice");
      expect(ask.attrs.selected).toEqual(["Option A"]);
    },
    { blockSchema }
  );
});
