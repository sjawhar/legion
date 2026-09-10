// interim.ts generates a small fixture corpus until @sjawhar/proof-editor/headless is published.
// It deliberately uses a hand-built schema with the Proof attributes exercised by this corpus.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { Schema, type Mark as ProseMirrorMark, type Node as ProseMirrorNode } from "prosemirror-model";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

const here = import.meta.dir;
const corpus = join(here, "..", "testdata", "corpus");
const out = join(here, "..", "testdata", "fixtures.json");

export const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { group: "block", content: "inline*" },
    heading: {
      group: "block",
      content: "inline*",
      attrs: { level: { default: 1 }, id: { default: "" } },
    },
    bullet_list: { group: "block", content: "list_item+", attrs: { spread: { default: false } } },
    ordered_list: {
      group: "block",
      content: "list_item+",
      attrs: { order: { default: 1 }, spread: { default: false } },
    },
    list_item: {
      content: "paragraph block*",
      attrs: { label: { default: "•" }, checked: { default: null }, spread: { default: false } },
    },
    code_block: {
      group: "block",
      content: "text*",
      marks: "",
      attrs: { language: { default: "" } },
    },
    table: { group: "block", content: "table_header_row table_row+" },
    table_header_row: { content: "table_header+" },
    table_row: { content: "table_cell+" },
    table_header: { content: "inline*", attrs: { alignment: { default: "left" } } },
    table_cell: { content: "inline*", attrs: { alignment: { default: "left" } } },
    hardbreak: { inline: true, group: "inline", attrs: { isInline: { default: false } } },
    text: { group: "inline" },
  },
  marks: {
    strong: { attrs: { marker: { default: "*" } } },
    emphasis: { attrs: { marker: { default: "*" } } },
    strike_through: {},
    inlineCode: {},
    link: { attrs: { href: {}, title: { default: null } } },
    proofComment: { attrs: { id: {}, by: {} }, inclusive: false },
    proofSuggestion: { attrs: { id: {}, by: {}, kind: {} }, inclusive: false },
    dispatchAsk: { attrs: { id: {}, by: {} }, inclusive: false },
  },
});

const node = (type: string, attrs?: Record<string, unknown> | null, content?: ProseMirrorNode[]) =>
  schema.node(type, attrs, content);
const text = (value: string, marks?: ProseMirrorMark[]) => schema.text(value, marks);
const mark = (type: string, attrs?: Record<string, unknown> | null) => schema.mark(type, attrs);
const paragraph = (...content: ProseMirrorNode[]) => node("paragraph", null, content);
const listItem = (...content: ProseMirrorNode[]) => node("list_item", null, content);

function documentFor(name: string): ProseMirrorNode {
  switch (name) {
    case "paragraphs":
      return node("doc", null, [
        paragraph(text("First paragraph with a hard break."), node("hardbreak"), text("Continues here.")),
        paragraph(text("Second paragraph.")),
      ]);
    case "headings":
      return node("doc", null, [
        node("heading", { level: 1 }, [text("One")]),
        node("heading", { level: 2 }, [text("Two")]),
        node("heading", { level: 3 }, [text("Three")]),
      ]);
    case "emphasis":
      return node("doc", null, [paragraph(
        text("strong", [mark("strong")]),
        text(" "),
        text("emphasis", [mark("emphasis")]),
        text(" "),
        text("strike", [mark("strike_through")]),
        text(" "),
        text("code", [mark("inlineCode")]),
        text(" and "),
        text("nested", [mark("strong"), mark("emphasis")]),
      )]);
    case "links":
      return node("doc", null, [paragraph(
        text("untitled", [mark("link", { href: "https://example.com", title: null })]),
        text(" and "),
        text("titled", [mark("link", { href: "https://example.org", title: "Title" })]),
        text(" and "),
        text("https://example.net", [mark("link", { href: "https://example.net", title: null })]),
      )]);
    case "lists":
      return node("doc", null, [
        node("bullet_list", null, [
          listItem(paragraph(text("one")), node("bullet_list", null, [listItem(paragraph(text("nested")))])),
          listItem(paragraph(text("two"))),
        ]),
        node("ordered_list", { order: 3 }, [
          listItem(paragraph(text("three"))),
          listItem(paragraph(text("four"))),
        ]),
      ]);
    case "tasks":
      return node("doc", null, [node("bullet_list", null, [
        node("list_item", { checked: false }, [paragraph(text("unchecked"))]),
        node("list_item", { checked: true }, [paragraph(text("checked"))]),
      ])]);
    case "table":
      return node("doc", null, [node("table", null, [
        node("table_header_row", null, [
          node("table_header", { alignment: "left" }, [text("Left")]),
          node("table_header", { alignment: "center" }, [text("Center")]),
          node("table_header", { alignment: "right" }, [text("Right")]),
        ]),
        node("table_row", null, [
          node("table_cell", { alignment: "left" }, [text("a", [mark("strong")])]),
          node("table_cell", { alignment: "center" }, [text("b", [mark("emphasis")])]),
          node("table_cell", { alignment: "right" }, [text("c", [mark("inlineCode")])]),
        ]),
      ])]);
    case "code":
      return node("doc", null, [
        node("code_block", { language: "go" }, [text("fmt.Println(\"hi\")")]),
        node("code_block", null, [text("plain")]),
        node("code_block", null, [text("indent")]),
      ]);
    case "unicode":
      return node("doc", null, [paragraph(
        text("😀 before "),
        text("marked", [mark("strong")]),
        text(" 漢字"),
      )]);
    case "marks":
      return node("doc", null, [
        paragraph(
          text("A sentence with "),
          text("a commented span", [mark("proofComment", { id: "c1", by: "user:sami" })]),
          text(" and "),
          text("an ask span", [mark("dispatchAsk", { id: "a1", by: "session:01a0" })]),
          text("."),
        ),
        paragraph(
          text("Proposed "),
          text("replacement", [mark("proofSuggestion", { id: "s1", by: "session:01a0", kind: "replace" })]),
          text(" here."),
        ),
      ]);
    default:
      throw new Error(`unknown interim fixture ${name}`);
  }
}

if (import.meta.main) {
  const fixtures = readdirSync(corpus)
    .filter((file) => file.endsWith(".md"))
    .sort()
    .map((file) => {
      const name = basename(file, ".md");
      const markdown = readFileSync(join(corpus, file), "utf8");
      const doc = documentFor(name);
      const ydoc = new Y.Doc();
      ydoc.clientID = 1;
      prosemirrorToYXmlFragment(doc, ydoc.getXmlFragment("prosemirror"));
      return {
        name,
        markdown,
        pm_json: doc.toJSON(),
        yjs_update_v1_b64: Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString("base64"),
        // The browser serializer is unavailable until the concurrently published package lands.
        rendered_by_milkdown: markdown,
      };
    });

  writeFileSync(out, `${JSON.stringify({ source: "interim-hand-schema", fixtures }, null, 2)}\n`);
  console.log(`wrote ${fixtures.length} interim fixtures`);
}
