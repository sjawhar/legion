import { afterEach, beforeEach, expect, test } from "bun:test";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import type { Window as HappyDOMWindow } from "happy-dom";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import { pictureClassName } from "../refs/DispatchPicture";
import { type PictureEditor, pictureView } from "./picture-view";

// Milkdown's `image` node: `src`/`alt`/`title` strings and a `toDOM` that writes them onto an
// `<img>`.
const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    image: {
      atom: true,
      attrs: { src: { default: "" }, alt: { default: "" }, title: { default: "" } },
      group: "inline",
      inline: true,
      toDOM: (node) => ["img", { ...node.attrs }],
    },
  },
});

function picture(src: string, alt = "") {
  return schema.node("image", { alt, src });
}

const noCaret: PictureEditor = { editable: () => true, placeCaretAfter: () => {} };

function wrapper(src: string, alt = ""): HTMLElement {
  const { dom } = pictureView(picture(src, alt), document, noCaret);
  if (!(dom instanceof HTMLElement) || dom.tagName !== "SPAN") {
    throw new Error(`the view's dom is not a wrapper span: ${(dom as Element).outerHTML}`);
  }
  return dom;
}

function pictureOf(dom: HTMLElement): HTMLImageElement {
  const img = dom.querySelector("img");
  if (img === null) throw new Error(`no <img> in ${dom.outerHTML}`);
  return img;
}

function linkOf(dom: HTMLElement): HTMLAnchorElement {
  const link = dom.querySelector("a");
  if (link === null) throw new Error(`no <a> in ${dom.outerHTML}`);
  return link;
}

// The dashboard is served from an origin, where a picture's same-origin bytes route resolves.
// happy-dom's page starts at `about:blank`, where no relative `src` resolves and every picture
// errors as its `src` is set, so these tests put the page on one. The test DOM is happy-dom's
// window (`__tests__/setup.ts` registers it), which TypeScript knows only as the DOM's.
const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

beforeEach(() => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-1/spec");
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

test("an issue's picture at a version draws from the version's bytes route, captioned by its alt", () => {
  const dom = wrapper("dispatch://CORE-1/artifact/shot-png@v1", "shot.png");

  expect(dom.getAttribute("data-dispatch-doc-picture")).toBe(
    "dispatch://CORE-1/artifact/shot-png@v1"
  );
  const img = pictureOf(dom);
  expect(img.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/1");
  expect(img.getAttribute("alt")).toBe("shot.png");
  expect(img.getAttribute("loading")).toBe("lazy");
  expect(img.className).toBe(pictureClassName);
  expect(img.hasAttribute("title")).toBe(false);
  expect(dom.textContent).toBe("");
});

test("a leaf view: no contentDOM, no update, events on the picture kept", () => {
  let caretPlaced = 0;
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/shot-png@v1", "shot.png"),
    document,
    {
      editable: () => true,
      placeCaretAfter: () => {
        caretPlaced += 1;
      },
    }
  );

  expect(view.contentDOM).toBeUndefined();
  expect(view.update).toBeUndefined();
  const img = pictureOf(view.dom as HTMLElement);
  const click = new MouseEvent("mousedown", { bubbles: true, button: 0 });
  img.dispatchEvent(click);
  expect(view.stopEvent?.(click)).toBe(false);
  expect(caretPlaced).toBe(0);
});

test("an empty caption falls back to the artifact's slug", () => {
  const img = pictureOf(wrapper("dispatch://CORE-1/artifact/shot-png@v1", "  "));

  expect(img.getAttribute("alt")).toBe("shot-png");
});

test("a project document's picture draws from the project's bytes route", () => {
  const img = pictureOf(wrapper("dispatch://CORE/artifact/design@v2", "design"));

  expect(img.getAttribute("src")).toBe("/api/v1/projects/CORE/artifacts/design/versions/2");
});

test("a version the browser cannot draw becomes a link to the artifact's page", () => {
  let caretPlaced = 0;
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/notes-md@v1", "notes.md"),
    document,
    {
      editable: () => true,
      placeCaretAfter: () => {
        caretPlaced += 1;
      },
    }
  );
  const dom = view.dom as HTMLElement;

  pictureOf(dom).dispatchEvent(new Event("error"));

  const link = linkOf(dom);
  expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-md?v=1");
  expect(link.getAttribute("data-dispatch-href")).toBe("dispatch://CORE-1/artifact/notes-md@v1");
  expect(link.textContent).toBe("CORE-1 notes-md");
  expect(link.draggable).toBe(false);
  expect(dom.querySelector("img")).toBeNull();
  expect(dom.childNodes.length).toBe(1);
  const click = new MouseEvent("mousedown", { bubbles: true, button: 0 });
  link.dispatchEvent(click);
  expect(view.stopEvent?.(click)).toBe(true);
  // A primary-button press on the link puts the caret beside the picture, so the selection the
  // editor restores when Firefox focuses it is next to the link; a secondary button does not.
  expect(caretPlaced).toBe(1);
  link.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, button: 2 }));
  expect(caretPlaced).toBe(1);
});

test("an error raised as the source is set is not missed", () => {
  // At `about:blank` a relative `src` fails to parse, and happy-dom raises `error` from the
  // attribute setter itself - the earliest a browser can.
  testWindow.happyDOM.setURL("about:blank");

  const dom = wrapper("dispatch://CORE-1/artifact/notes-md@v1", "notes.md");

  expect(dom.querySelector("img")).toBeNull();
  expect(linkOf(dom).getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-md?v=1");
});

test("a source that pins no version, or names no artifact, is a link at once", () => {
  const cases = [
    ["dispatch://CORE-1/artifact/notes-md", "/issues/CORE-1/artifacts/notes-md", "CORE-1 notes-md"],
    ["dispatch://CORE-1/ask/a1", "/issues/CORE-1/asks/a1", "CORE-1 ask"],
  ] as const;
  for (const [source, href, words] of cases) {
    const dom = wrapper(source, "notes.md");

    expect(dom.getAttribute("data-dispatch-doc-picture")).toBe(source);
    expect(dom.querySelector("img")).toBeNull();
    const link = linkOf(dom);
    expect(link.getAttribute("href")).toBe(href);
    expect(link.getAttribute("data-dispatch-href")).toBe(source);
    expect(link.textContent).toBe(words);
  }
});

test("a picture from elsewhere, or a reference the grammar refuses, is drawn by the schema", () => {
  for (const source of [
    "https://example.invalid/logo.png",
    "logo.png",
    "dispatch://not a reference",
  ]) {
    const view = pictureView(picture(source, "logo"), document, noCaret);
    const dom = view.dom as HTMLElement;

    expect(dom.tagName).toBe("IMG");
    expect(dom.getAttribute("src")).toBe(source);
    expect(dom.getAttribute("alt")).toBe("logo");
    expect(dom.hasAttribute("data-dispatch-doc-picture")).toBe(false);
    expect(view.stopEvent).toBeUndefined();
  }
});

// The editor's own `image` node (`linked-image.ts` in `@legion/proof-editor`), whose `toDOM` draws
// a linked picture in its link's anchor.
const proof = await createHeadlessProof();

function linkedPicture(src: string, link: string, alt = "", linkTitle = ""): ProseMirrorNode {
  const image = proof.schema.nodes.image;
  if (image === undefined) throw new Error("the editor's schema has no image node");
  return image.create({ alt, link, linkTitle, src });
}

/** A view of `node` whose editor is editable while `editable.value` is. */
function linkedView(node: ProseMirrorNode, editable = { value: true }) {
  let caretPlaced = 0;
  const view = pictureView(node, document, {
    editable: () => editable.value,
    placeCaretAfter: () => {
      caretPlaced += 1;
    },
  });
  return { caretPlaced: () => caretPlaced, dom: view.dom as HTMLElement, editable, view };
}

/** Clicks `target` and answers whether the browser's own handling was prevented by the time the
 * click left the view; the click goes no further, so the test's page never follows a link. */
function clickPrevented(target: Element, init: MouseEventInit = {}): boolean {
  let outermost = target;
  while (outermost.parentElement !== null) outermost = outermost.parentElement;
  let prevented: boolean | undefined;
  const keepPage = (event: Event) => {
    prevented = event.defaultPrevented;
    event.preventDefault();
  };
  outermost.addEventListener("click", keepPage);
  target.dispatchEvent(
    new MouseEvent("click", { bubbles: true, button: 0, cancelable: true, ...init })
  );
  outermost.removeEventListener("click", keepPage);
  if (prevented === undefined) throw new Error("the click never left the view");
  return prevented;
}

test("a picture from elsewhere with a link sits in the link's anchor, followed by no click while editable", () => {
  const { dom, editable, view } = linkedView(
    linkedPicture("https://example.invalid/badge.svg", "https://ci.example/build", "build", "Build")
  );

  expect(dom.tagName).toBe("A");
  expect(dom.getAttribute("href")).toBe("https://ci.example/build");
  expect(dom.getAttribute("title")).toBe("Build");
  expect(dom.hasAttribute("data-dispatch-href")).toBe(false);
  const img = pictureOf(dom);
  expect(img.getAttribute("src")).toBe("https://example.invalid/badge.svg");
  expect(img.getAttribute("alt")).toBe("build");
  expect(dom.childNodes.length).toBe(1);
  // Events on it are the editor's, as on any picture: a click selects it.
  expect(view.stopEvent).toBeUndefined();
  // An editable editor's text link is no live link; neither is the picture's on a plain click. A
  // Ctrl/Cmd-click is left unprevented for proof-editor's link plugin, which opens the link and
  // prevents the browser's own: ProseMirror hands the plugin no event already prevented.
  expect(clickPrevented(img)).toBe(true);
  expect(clickPrevented(img, { ctrlKey: true })).toBe(false);
  expect(clickPrevented(img, { metaKey: true })).toBe(false);
  expect(clickPrevented(img, { shiftKey: true })).toBe(true);
  // Read-only, the browser and the plugin take the click, as they take a text link's.
  editable.value = false;
  expect(clickPrevented(img)).toBe(false);
});

test("a Dispatch picture with a link is the link's anchor around the picture, its events the editor's", () => {
  const { caretPlaced, dom, editable, view } = linkedView(
    linkedPicture("dispatch://CORE-1/artifact/shot-png@v1", "https://ci.example/build", "shot.png")
  );

  expect(dom.tagName).toBe("A");
  expect(dom.getAttribute("href")).toBe("https://ci.example/build");
  expect(dom.querySelector("a")).toBeNull();
  const picture = dom.firstElementChild as HTMLElement;
  expect(dom.childNodes.length).toBe(1);
  expect(picture.tagName).toBe("SPAN");
  expect(picture.getAttribute("data-dispatch-doc-picture")).toBe(
    "dispatch://CORE-1/artifact/shot-png@v1"
  );
  const img = pictureOf(picture);
  expect(img.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/1");
  expect(img.getAttribute("alt")).toBe("shot.png");
  expect(img.className).toBe(pictureClassName);
  const press = new MouseEvent("mousedown", { bubbles: true, button: 0 });
  img.dispatchEvent(press);
  expect(view.stopEvent?.(press)).toBe(false);
  expect(caretPlaced()).toBe(0);
  expect(clickPrevented(img)).toBe(true);
  editable.value = false;
  expect(clickPrevented(img)).toBe(false);
});

test("a linked Dispatch picture that cannot be drawn, or pins no version, is its short form in the link", () => {
  const failed = linkedView(
    linkedPicture("dispatch://CORE-1/artifact/notes-md@v1", "https://ci.example/build", "notes.md")
  );
  pictureOf(failed.dom).dispatchEvent(new Event("error"));
  const unpinned = linkedView(
    linkedPicture("dispatch://CORE-1/artifact/notes-md", "https://ci.example/build", "notes.md")
  );

  for (const [{ caretPlaced, dom, view }, source] of [
    [failed, "dispatch://CORE-1/artifact/notes-md@v1"],
    [unpinned, "dispatch://CORE-1/artifact/notes-md"],
  ] as const) {
    expect(dom.tagName).toBe("A");
    expect(dom.getAttribute("href")).toBe("https://ci.example/build");
    // No link to the artifact's page sits in the picture's link.
    expect(dom.querySelector("a")).toBeNull();
    expect(dom.querySelector("img")).toBeNull();
    const picture = dom.firstElementChild;
    expect(picture?.getAttribute("data-dispatch-doc-picture")).toBe(source);
    const words = picture?.firstElementChild;
    if (words === null || words === undefined) throw new Error(`no fallback in ${dom.outerHTML}`);
    expect(words.tagName).toBe("SPAN");
    expect(words.textContent).toBe("CORE-1 notes-md");
    const press = new MouseEvent("mousedown", { bubbles: true, button: 0 });
    words.dispatchEvent(press);
    expect(view.stopEvent?.(press)).toBe(false);
    expect(caretPlaced()).toBe(0);
  }
});

test("a picture linked to a dispatch:// target carries it as a text link does, for the host's handler", () => {
  for (const src of [
    "https://example.invalid/badge.svg",
    "dispatch://CORE-1/artifact/shot-png@v1",
  ]) {
    const { dom } = linkedView(linkedPicture(src, "dispatch://CORE-2", "x"));

    expect(dom.tagName).toBe("A");
    // Milkdown's sanitizer empties the href, as for a `dispatch://` link mark.
    expect(dom.getAttribute("href")).toBe("");
    expect(dom.getAttribute("data-dispatch-href")).toBe("dispatch://CORE-2");
    expect(dom.querySelector("img")).not.toBeNull();
    // Its empty href is no address the link plugin opens, so no click follows it while editable,
    // a Ctrl/Cmd-click included, which would open the current page in a new tab.
    expect(clickPrevented(dom.querySelector("img") as Element, { ctrlKey: true })).toBe(true);
  }
});

test("a picture with an unsafe link draws no anchor", () => {
  const external = linkedView(
    linkedPicture("https://example.invalid/badge.svg", "javascript:window.__pwned=1", "x")
  );
  expect(external.dom.tagName).toBe("IMG");
  expect(external.dom.closest("a")).toBeNull();
  expect(clickPrevented(external.dom)).toBe(false);

  // A Dispatch picture with an unsafe link is drawn as one with none: a link to its own page only
  // when it cannot be drawn.
  const dispatch = linkedView(
    linkedPicture("dispatch://CORE-1/artifact/notes-md@v1", "javascript:window.__pwned=1", "x")
  );
  expect(dispatch.dom.tagName).toBe("SPAN");
  expect(dispatch.dom.querySelector("a")).toBeNull();
  pictureOf(dispatch.dom).dispatchEvent(new Event("error"));
  expect(linkOf(dispatch.dom).getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-md?v=1");
});

test("an unlinked picture of the editor's own schema is drawn as before", () => {
  const external = linkedView(linkedPicture("https://example.invalid/logo.png", "", "logo"));
  expect(external.dom.tagName).toBe("IMG");
  expect(external.dom.getAttribute("src")).toBe("https://example.invalid/logo.png");
  expect(external.view.stopEvent).toBeUndefined();

  const dispatch = linkedView(linkedPicture("dispatch://CORE-1/artifact/shot-png@v1", "", "shot"));
  expect(dispatch.dom.tagName).toBe("SPAN");
  expect(dispatch.dom.getAttribute("data-dispatch-doc-picture")).toBe(
    "dispatch://CORE-1/artifact/shot-png@v1"
  );
  expect(pictureOf(dispatch.dom).getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1"
  );
});
