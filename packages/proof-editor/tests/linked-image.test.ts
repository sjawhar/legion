import { expect, test } from "bun:test";
import {
  DOMSerializer,
  type Node as ProseMirrorNode,
  type ResolvedPos,
  type Slice,
} from "@milkdown/kit/prose/model";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import * as proseView from "@milkdown/kit/prose/view";
import { prosemirrorToYXmlFragment, yXmlFragmentToProseMirrorRootNode } from "y-prosemirror";
import * as Y from "yjs";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withDomWindow, withHTMLElementGlobal } from "./dom-window";
import { withMarksEditor } from "./marks-editor";

// INFRA-190: an image that is also a link keeps its link, as the image's `link` and `linkTitle`
// attributes, through the editor's markdown reader and writer, its Yjs store, its DOM and paste.

/** Every image of `doc`, with the names of the marks it carries. */
function images(doc: ProseMirrorNode): { attrs: Record<string, unknown>; marks: string[] }[] {
  const found: { attrs: Record<string, unknown>; marks: string[] }[] = [];
  doc.descendants((node) => {
    if (node.type.name === "image") {
      found.push({ attrs: node.attrs, marks: node.marks.map((mark) => mark.type.name) });
    }
    return true;
  });
  return found;
}

test("a linked image survives the editor's store byte for byte (A1)", async () => {
  const engine = await createHeadlessProof();
  for (const markdown of [
    "[![x](i.png)](https://u.com)\n",
    '[![x](i.png "t")](https://u.com "lt")\n',
  ]) {
    const doc = engine.parseMarkdown(markdown);
    const ydoc = new Y.Doc();
    prosemirrorToYXmlFragment(doc, ydoc.getXmlFragment("prosemirror"));
    const back = yXmlFragmentToProseMirrorRootNode(
      ydoc.getXmlFragment("prosemirror"),
      doc.type.schema
    );
    expect(engine.serializeMarkdown(doc)).toBe(markdown);
    expect(engine.serializeMarkdown(back)).toBe(markdown);
  }
});

test("the reader stores a whole-image link as attributes, absent titles as null", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  expect(images(parseMarkdown('[![x](i.png "t")](https://u.com "lt")\n'))).toEqual([
    {
      attrs: { src: "i.png", alt: "x", title: "t", link: "https://u.com", linkTitle: "lt" },
      marks: [],
    },
  ]);
  expect(images(parseMarkdown("![x](i.png)\n"))).toEqual([
    { attrs: { src: "i.png", alt: "x", title: null, link: null, linkTitle: null }, marks: [] },
  ]);
  // A reference-style link and image resolve before the rule.
  const reference = parseMarkdown("[![x][img]][site]\n\n[img]: i.png\n[site]: https://u.com\n");
  expect(images(reference).map((image) => image.attrs.link)).toEqual(["https://u.com"]);
});

test("every other link around an image stays as today", async () => {
  const { parseMarkdown, serializeMarkdown } = await createHeadlessProof();
  for (const markdown of [
    "[a ![x](i) b](u)\n",
    "[ ![x](i) ](u)\n",
    "[**![x](i)**](u)\n",
    "[*![x](i)*](u)\n",
    "[![x](i)]()\n",
    "![[![x](i)](u)](j)\n",
  ]) {
    const found = images(parseMarkdown(markdown));
    expect(found.length).toBeGreaterThan(0);
    for (const image of found) expect(image.attrs.link).toBeNull();
  }
  expect(serializeMarkdown(parseMarkdown("[a ![x](i) b](u)\n"))).toBe("[a ![x](i) b](u)\n");
  // A mark around the link keeps the link.
  const bold = parseMarkdown("**[![x](i)](u)**\n");
  expect(images(bold)).toEqual([
    {
      attrs: { src: "i", alt: "x", title: null, link: "u", linkTitle: null },
      marks: ["strong"],
    },
  ]);
  expect(serializeMarkdown(bold)).toBe("**[![x](i)](u)**\n");
});

test("a linked image inside an Ask span keeps its link through parse and serialize", async () => {
  const { parseMarkdown, serializeMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown(
    '<span data-dispatch="ask" data-id="a1" data-by="human:alice">see [![x](i.png "t")](https://u.com "lt")</span>\n'
  );
  expect(images(doc)).toEqual([
    {
      attrs: { src: "i.png", alt: "x", title: "t", link: "https://u.com", linkTitle: "lt" },
      marks: ["dispatchAsk"],
    },
  ]);
  // The Ask span's own inline writer leaves link and image titles out (the stored attributes
  // keep them).
  expect(serializeMarkdown(doc)).toBe(
    '<span data-dispatch="ask" data-id="a1" data-by="human:alice">see [![x](i.png)](https://u.com)</span>\n'
  );
});

test("the image's DOM carries the link as data attributes and an anchor only when it is safe", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  await withDomWindow((window) => {
    const render = (markdown: string): HTMLElement => {
      const paragraph = parseMarkdown(markdown).firstChild as ProseMirrorNode;
      const image = paragraph.firstChild as ProseMirrorNode;
      return DOMSerializer.fromSchema(image.type.schema).serializeNode(image, {
        document: window.document as unknown as Document,
      }) as HTMLElement;
    };

    const plain = render("![x](i.png)\n");
    expect(plain.nodeName).toBe("IMG");
    expect(plain.hasAttribute("data-link")).toBe(false);

    const safe = render('[![x](i.png "t")](https://u.com "lt")\n');
    expect(safe.nodeName).toBe("A");
    expect(safe.getAttribute("href")).toBe("https://u.com");
    expect(safe.getAttribute("title")).toBe("lt");
    const img = safe.firstChild as HTMLElement;
    expect(img.nodeName).toBe("IMG");
    expect(img.getAttribute("src")).toBe("i.png");
    expect(img.getAttribute("alt")).toBe("x");
    expect(img.getAttribute("title")).toBe("t");
    expect(img.getAttribute("data-link")).toBe("https://u.com");
    expect(img.getAttribute("data-link-title")).toBe("lt");
    expect(img.hasAttribute("link")).toBe(false);
    expect(img.hasAttribute("linkTitle")).toBe(false);
    expect(img.hasAttribute("linktitle")).toBe(false);

    const unsafe = render("[![x](i.png)](javascript:alert(1))\n");
    expect(unsafe.nodeName).toBe("IMG");
    expect(unsafe.getAttribute("data-link")).toBe("javascript:alert(1)");
    expect(unsafe.hasAttribute("link")).toBe(false);

    // A `javascript:` address spelled with a character reference, an escape, mixed case, or a
    // leading or inner tab or line feed is read as the address it names, whose scheme the
    // sanitizer refuses: no anchor.
    for (const [written, link] of [
      ["javascript&#58;window.__pwned=1", "javascript:window.__pwned=1"],
      ["javascript\\:window.__pwned=1", "javascript:window.__pwned=1"],
      ["JaVaScRiPt:window.__pwned=1", "JaVaScRiPt:window.__pwned=1"],
      ["&#x6A;avascript:window.__pwned=1", "javascript:window.__pwned=1"],
      ["&#9;javascript:window.__pwned=1", "\tjavascript:window.__pwned=1"],
      ["java&#10;script:window.__pwned=1", "java\nscript:window.__pwned=1"],
    ] as const) {
      const encoded = render(`[![x](i.png)](${written})\n`);
      expect(encoded.nodeName).toBe("IMG");
      expect(encoded.getAttribute("data-link")).toBe(link);
      expect(encoded.querySelector("a")).toBeNull();
    }

    const dispatch = render("[![x](i.png)](dispatch://KEY-1)\n");
    expect(dispatch.nodeName).toBe("A");
    expect(dispatch.getAttribute("data-dispatch-href")).toBe("dispatch://KEY-1");
    expect((dispatch.firstChild as HTMLElement).getAttribute("data-link")).toBe("dispatch://KEY-1");
  });
});

/** prosemirror-view's own clipboard reader, exported for tests but left out of its typings. */
const { __parseFromClipboard: parseFromClipboard } = proseView as unknown as {
  __parseFromClipboard: (
    view: EditorView,
    text: string,
    html: string,
    plainText: boolean,
    $context: ResolvedPos
  ) => Slice | null;
};

/** What prosemirror-view's paste handler does: parse the clipboard's HTML (if any) into a slice
 *  and hand the event and that slice to every plugin's `handlePaste` — the real clipboard
 *  plugin's, unmodified. happy-dom does not deliver a synthetic paste event to ProseMirror's own
 *  listener, so the test makes the call (as clipboard-paste-recovery.test.ts does). */
function paste(view: EditorView, data: Record<string, string>): void {
  const html = data["text/html"] ?? "";
  const slice = html
    ? parseFromClipboard(view, data["text/plain"] ?? "", html, false, view.state.selection.$from)
    : undefined;
  const event = { clipboardData: { getData: (type: string) => data[type] ?? "" } };
  view.someProp("handlePaste", (handlePaste) =>
    handlePaste(view, event as unknown as ClipboardEvent, slice as never)
  );
}

/** The HTML and text prosemirror-view puts on the clipboard for a copy of the whole document. */
function copyAll(view: EditorView): Record<string, string> {
  const slice = view.state.doc.slice(0, view.state.doc.content.size);
  const serializer =
    view.someProp("clipboardSerializer") ?? DOMSerializer.fromSchema(view.state.schema);
  const wrap = document.createElement("div");
  wrap.appendChild(serializer.serializeFragment(slice.content, { document }));
  const text = view.someProp("clipboardTextSerializer", (serialize) => serialize(slice, view));
  return { "text/html": wrap.innerHTML, "text/plain": text ?? "" };
}

function clear(view: EditorView): void {
  const { state } = view;
  const emptied = state.tr.delete(1, state.doc.content.size - 1);
  view.dispatch(emptied.setSelection(TextSelection.create(emptied.doc, 1)));
}

const LINKED_IMAGE = {
  src: "i.png",
  alt: "x",
  title: "",
  link: "https://u.com",
  linkTitle: "",
};

test("a markdown paste of a linked image holds one link: the attributes, no link mark", async () => {
  await withMarksEditor("", async ({ handle, view }) => {
    paste(view, { "text/plain": "[![x](i.png)](https://u.com)" });
    expect(images(view.state.doc)).toEqual([{ attrs: LINKED_IMAGE, marks: [] }]);
    expect(handle.getMarkdown()).toBe("[![x](i.png)](https://u.com)\n");
  });
});

test("an HTML copy and paste of the editor's own linked image keeps one link", async () => {
  await withMarksEditor("[![x](i.png)](https://u.com)\n", async ({ handle, view }) => {
    const clipboard = copyAll(view);
    expect(clipboard["text/plain"]).toBe("[![x](i.png)](https://u.com)\n");
    clear(view);
    expect(images(view.state.doc)).toEqual([]);
    paste(view, clipboard);
    expect(images(view.state.doc)).toEqual([{ attrs: LINKED_IMAGE, marks: [] }]);
    expect(handle.getMarkdown()).toBe("[![x](i.png)](https://u.com)\n");
  });
});

test("an HTML paste of an anchor around one image reads as a linked image", async () => {
  await withMarksEditor("", async ({ handle, view }) => {
    paste(view, { "text/html": '<a href="https://u.com"><img src="i.png" alt="x"></a>' });
    // An image from elsewhere with no title takes its alt text as title, as Milkdown's rule does.
    expect(images(view.state.doc)).toEqual([{ attrs: { ...LINKED_IMAGE, title: "x" }, marks: [] }]);
    expect(handle.getMarkdown()).toBe('[![x](i.png "x")](https://u.com)\n');
  });
});

test("an HTML paste of a link around text and an image keeps its link mark", async () => {
  await withMarksEditor("", async ({ view }) => {
    // Milkdown's own link rule, which reads this anchor, needs the HTMLElement global.
    withHTMLElementGlobal(() =>
      paste(view, { "text/html": '<a href="https://u.com">see <img src="i.png" alt="x"></a>' })
    );
    expect(images(view.state.doc)).toEqual([
      { attrs: { ...LINKED_IMAGE, title: "x", link: "" }, marks: ["link"] },
    ]);
  });
});
