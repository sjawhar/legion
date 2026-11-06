import { afterEach, beforeEach, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import type { Window as HappyDOMWindow } from "happy-dom";
import { Schema } from "prosemirror-model";
import { pictureClassName } from "../refs/picture-classes";
import type { ReferenceMedia } from "../refs/reference-target";
import type { DispatchReferenceRoute } from "../refs/routes";
import { type PictureLinkOpener, pictureView, type WatchPicture } from "./picture-view";

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
/** A watcher for tests whose source pins a version or names no artifact: it reports nothing, as
 *  the page's does while the summary that would name the type is unread, so a pinned source
 *  draws the address it names as a picture. */
const noWatch: WatchPicture = (_route, onPicture) => {
  onPicture(undefined);
  return () => {};
};

/** What the editor lends the fallback link, with the paths its own click asked for. No
 *  reference these tests draw needs a read to know its path, so the client answers nothing. */
function fakeOpener(editable = false) {
  const opened: string[] = [];
  const opener: PictureLinkOpener = {
    editable,
    navigate: (path) => opened.push(path),
    queryClient: new QueryClient(),
  };
  return { opened, opener };
}

/** A past version's: the view every test below draws in unless it says otherwise. */
const pastVersion = fakeOpener().opener;

/** A watcher the test drives: `report` sends the latest picture to the view, `reportVideo` a
 *  video at that address, and `stopped` counts the view's unsubscribes. */
function fakeWatch(initial?: string) {
  const fake = {
    asked: [] as string[],
    send: (_media: ReferenceMedia | undefined) => {},
    report: (src: string | undefined) =>
      fake.send(src === undefined ? undefined : { kind: "picture", src }),
    reportVideo: (src: string) => fake.send({ kind: "video", src }),
    stopped: 0,
    watch: (
      route: DispatchReferenceRoute,
      onPicture: (media: ReferenceMedia | undefined) => void
    ) => {
      fake.asked.push(route.kind === "artifact" ? `${route.key}/${route.slug}` : route.kind);
      fake.send = onPicture;
      fake.report(initial);
      return () => {
        fake.stopped += 1;
      };
    },
  };
  return fake;
}

function wrapper(src: string, alt = ""): HTMLElement {
  const { dom } = pictureView(picture(src, alt), document, noCaret, noWatch, pastVersion);
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
    },
    noWatch,
    pastVersion
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
    },
    noWatch,
    pastVersion
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

// The live editor's own `Open link` card shows in an editable view, and Dispatch's card
// triggers on `data-dispatch-href`, so the attribute is the one view's and not the other's:
// one card on the link in each.
test("the fallback link carries Dispatch's card in a past version and none of its own in the editor", () => {
  const live = fakeOpener(true);
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/notes-md@v1", "notes.md"),
    document,
    noCaret,
    noWatch,
    live.opener
  );
  const dom = view.dom as HTMLElement;
  pictureOf(dom).dispatchEvent(new Event("error"));
  const link = linkOf(dom);

  expect(link.hasAttribute("data-dispatch-href")).toBe(false);
  // Still a link to the same page, so the keyboard, the browser's menu and a middle click
  // keep working.
  expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-md?v=1");
  expect(link.textContent).toBe("CORE-1 notes-md");
});

test("a plain click on the fallback link opens the artifact inside the app, in either view", () => {
  for (const editable of [true, false]) {
    const host = fakeOpener(editable);
    const view = pictureView(
      picture("dispatch://CORE-1/artifact/notes-md@v1", "notes.md"),
      document,
      noCaret,
      noWatch,
      host.opener
    );
    const dom = view.dom as HTMLElement;
    pictureOf(dom).dispatchEvent(new Event("error"));
    const link = linkOf(dom);

    const plain = new MouseEvent("click", { bubbles: true, button: 0, cancelable: true });
    link.dispatchEvent(plain);
    expect(host.opened).toEqual(["/issues/CORE-1/artifacts/notes-md?v=1"]);
    // The browser's own follow of the href is off: the page never reloads.
    expect(plain.defaultPrevented).toBe(true);

    // A modified click and a middle click are the browser's: a new tab, from the real href.
    const modified = new MouseEvent("click", {
      bubbles: true,
      button: 0,
      cancelable: true,
      metaKey: true,
    });
    link.dispatchEvent(modified);
    link.dispatchEvent(new MouseEvent("auxclick", { bubbles: true, button: 1, cancelable: true }));
    expect(host.opened).toHaveLength(1);
    expect(modified.defaultPrevented).toBe(false);
  }
});

test("an error raised as the source is set is not missed", () => {
  // At `about:blank` a relative `src` fails to parse, and happy-dom raises `error` from the
  // attribute setter itself - the earliest a browser can.
  testWindow.happyDOM.setURL("about:blank");

  const dom = wrapper("dispatch://CORE-1/artifact/notes-md@v1", "notes.md");

  expect(dom.querySelector("img")).toBeNull();
  expect(linkOf(dom).getAttribute("href")).toBe("/issues/CORE-1/artifacts/notes-md?v=1");
});

test("a source that names no artifact is a link at once", () => {
  const source = "dispatch://CORE-1/ask/a1";
  const dom = wrapper(source, "notes.md");

  expect(dom.getAttribute("data-dispatch-doc-picture")).toBe(source);
  expect(dom.querySelector("img")).toBeNull();
  const link = linkOf(dom);
  expect(link.getAttribute("href")).toBe("/issues/CORE-1/asks/a1");
  expect(link.getAttribute("data-dispatch-href")).toBe(source);
  expect(link.textContent).toBe("CORE-1 ask");
});

test("a source that pins no version is its link until the latest version is known, then follows it", () => {
  const latest = fakeWatch();
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/shot-png", "shot.png"),
    document,
    noCaret,
    latest.watch,
    pastVersion
  );
  const dom = view.dom as HTMLElement;

  expect(latest.asked).toEqual(["CORE-1/shot-png"]);
  expect(linkOf(dom).getAttribute("href")).toBe("/issues/CORE-1/artifacts/shot-png");

  latest.report("/api/v1/issues/CORE-1/artifacts/shot-png/versions/3");
  const img = pictureOf(dom);
  expect(img.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/3");
  expect(img.getAttribute("alt")).toBe("shot.png");
  expect(dom.querySelector("a")).toBeNull();
  // The picture's events are the editor's again, as a pinned picture's are.
  const press = new MouseEvent("mousedown", { bubbles: true, button: 0 });
  img.dispatchEvent(press);
  expect(view.stopEvent?.(press)).toBe(false);

  // A new version uploaded while the document is open replaces the picture.
  latest.report("/api/v1/issues/CORE-1/artifacts/shot-png/versions/4");
  expect(pictureOf(dom).getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/4"
  );
  // A version stored under a video type is played where a picture is drawn, at the same
  // address, with the same class and no full-size view of its own.
  latest.reportVideo("/api/v1/issues/CORE-1/artifacts/shot-png/versions/5");
  expect(dom.querySelector("img")).toBeNull();
  const player = dom.querySelector("video");
  expect(player?.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/5");
  expect(player?.hasAttribute("controls")).toBe(true);
  expect(player?.getAttribute("preload")).toBe("metadata");
  expect(player?.getAttribute("aria-label")).toBe("shot.png");
  expect(player?.className).toBe(pictureClassName);
  latest.report("/api/v1/issues/CORE-1/artifacts/shot-png/versions/4");
  expect(dom.querySelector("video")).toBeNull();
  expect(pictureOf(dom).getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/4"
  );
  // A target that stops being a picture reads as its link again.
  latest.report(undefined);
  expect(dom.querySelector("img")).toBeNull();
  expect(linkOf(dom).getAttribute("data-dispatch-href")).toBe(
    "dispatch://CORE-1/artifact/shot-png"
  );

  view.destroy?.();
  expect(latest.stopped).toBe(1);
});

test("a source that pins no version draws at once when its latest version is already known", () => {
  const latest = fakeWatch("/api/v1/issues/CORE-1/artifacts/shot-png/versions/2");
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/shot-png", "shot.png"),
    document,
    noCaret,
    latest.watch,
    pastVersion
  );

  expect(pictureOf(view.dom as HTMLElement).getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/2"
  );
});

test("a pinned source whose artifact is a video plays at the address it pins", () => {
  const pinned = fakeWatch();
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/clip-webm@v2", "clip.webm"),
    document,
    noCaret,
    pinned.watch,
    pastVersion
  );
  const dom = view.dom as HTMLElement;
  const pinnedSrc = "/api/v1/issues/CORE-1/artifacts/clip-webm/versions/2";

  // The reference names the version but not its type, so the watch is asked for the type; until
  // it answers, the pinned address is drawn as the picture a pinned source has always drawn.
  expect(pinned.asked).toEqual(["CORE-1/clip-webm"]);
  expect(pictureOf(dom).getAttribute("src")).toBe(pinnedSrc);

  // The report names the artifact's latest version; the address stays the one the source pins.
  pinned.reportVideo("/api/v1/issues/CORE-1/artifacts/clip-webm/versions/9");
  expect(dom.querySelector("img")).toBeNull();
  const player = dom.querySelector("video");
  expect(player?.getAttribute("src")).toBe(pinnedSrc);
  expect(player?.hasAttribute("controls")).toBe(true);
  expect(player?.getAttribute("preload")).toBe("metadata");

  // A report of a target the summary calls neither picture nor video leaves the pinned bytes
  // drawn: a pinned source has always drawn what it names and fallen back only on an error.
  pinned.report(undefined);
  expect(dom.querySelector("video")).toBeNull();
  expect(pictureOf(dom).getAttribute("src")).toBe(pinnedSrc);

  view.destroy?.();
  expect(pinned.stopped).toBe(1);
});

test("destroying the view closes the full-size layer its picture opened", () => {
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/shot-png@v1", "shot.png"),
    document,
    noCaret,
    noWatch,
    pastVersion
  );
  pictureOf(view.dom as HTMLElement).dispatchEvent(
    new MouseEvent("click", { bubbles: true, button: 0, cancelable: true })
  );
  expect(document.querySelector('[data-testid="picture-lightbox"]')).not.toBeNull();

  view.destroy?.();
  expect(document.querySelector('[data-testid="picture-lightbox"]')).toBeNull();
});

test("only the picture that opened the full-size layer closes it", () => {
  const view = (src: string) =>
    pictureView(picture(src, "shot.png"), document, noCaret, noWatch, pastVersion);
  const first = view("dispatch://CORE-1/artifact/shot-png@v1");
  const second = view("dispatch://CORE-1/artifact/shot-png@v2");
  const click = () => new MouseEvent("click", { bubbles: true, button: 0, cancelable: true });
  pictureOf(first.dom as HTMLElement).dispatchEvent(click());
  // A click that reaches the second picture while the first's layer is open opens nothing.
  pictureOf(second.dom as HTMLElement).dispatchEvent(click());
  expect(document.querySelectorAll('[data-testid="picture-lightbox"]')).toHaveLength(1);

  second.destroy?.();
  expect(document.querySelector('[data-testid="picture-lightbox"]')).not.toBeNull();
  first.destroy?.();
  expect(document.querySelector('[data-testid="picture-lightbox"]')).toBeNull();
});

test("a replaced version's late load error leaves the newer picture, and a failed load draws again on its next report", () => {
  const latest = fakeWatch("/api/v1/issues/CORE-1/artifacts/shot-png/versions/3");
  const view = pictureView(
    picture("dispatch://CORE-1/artifact/shot-png", "shot.png"),
    document,
    noCaret,
    latest.watch,
    pastVersion
  );
  const dom = view.dom as HTMLElement;
  const v3 = pictureOf(dom);

  latest.report("/api/v1/issues/CORE-1/artifacts/shot-png/versions/4");
  v3.dispatchEvent(new Event("error"));
  const v4 = pictureOf(dom);
  expect(v4.getAttribute("src")).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/4");

  v4.dispatchEvent(new Event("error"));
  expect(dom.querySelector("img")).toBeNull();
  latest.report("/api/v1/issues/CORE-1/artifacts/shot-png/versions/4");
  expect(pictureOf(dom).getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/4"
  );
});

test("a plain click on a picture opens it at full size; a modified click does not", () => {
  const img = pictureOf(wrapper("dispatch://CORE-1/artifact/shot-png@v1", "shot.png"));

  img.dispatchEvent(new MouseEvent("click", { bubbles: true, button: 0, ctrlKey: true }));
  expect(document.querySelector('[data-testid="picture-lightbox"]')).toBeNull();

  const click = new MouseEvent("click", { bubbles: true, button: 0, cancelable: true });
  img.dispatchEvent(click);
  expect(click.defaultPrevented).toBe(true);
  const lightbox = document.querySelector<HTMLDialogElement>('[data-testid="picture-lightbox"]');
  expect(lightbox?.open).toBe(true);
  expect(lightbox?.getAttribute("aria-label")).toBe("shot.png");
  expect(lightbox?.querySelector("img")?.getAttribute("src")).toBe(
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1"
  );
  // A second click that reaches the picture while it is open opens no second layer.
  img.dispatchEvent(new MouseEvent("click", { bubbles: true, button: 0, cancelable: true }));
  expect(document.querySelectorAll('[data-testid="picture-lightbox"]')).toHaveLength(1);
  // A click anywhere on it closes it, and it leaves the page.
  lightbox?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  expect(document.querySelector('[data-testid="picture-lightbox"]')).toBeNull();
});

test("a picture from elsewhere, or a reference the grammar refuses, is drawn by the schema", () => {
  for (const source of [
    "https://example.invalid/logo.png",
    "logo.png",
    "dispatch://not a reference",
  ]) {
    const view = pictureView(picture(source, "logo"), document, noCaret, noWatch, pastVersion);
    const dom = view.dom as HTMLElement;

    expect(dom.tagName).toBe("IMG");
    expect(dom.getAttribute("src")).toBe(source);
    expect(dom.getAttribute("alt")).toBe("logo");
    expect(dom.hasAttribute("data-dispatch-doc-picture")).toBe(false);
    expect(view.stopEvent).toBeUndefined();
  }
});
