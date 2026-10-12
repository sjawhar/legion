import { afterEach, beforeEach, expect, test } from "bun:test";
import type { Window as HappyDOMWindow } from "happy-dom";
import { Schema } from "prosemirror-model";

import { pictureClassName } from "../refs/DispatchPicture";
import { pictureView } from "./picture-view";

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

const noCaret = () => {};

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
    () => {
      caretPlaced += 1;
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
    () => {
      caretPlaced += 1;
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
