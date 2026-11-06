import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import type { IssueReferenceSummary } from "../../api/types";
import { handleReferenceClick, openReference } from "./reference-click";

/** A document article with one link in it, wired as both editors wire theirs: the article hears
 *  `click` and `auxclick`, and the paths the handler opened are recorded. */
function article(html: string) {
  const opened: string[] = [];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const host = document.createElement("article");
  host.innerHTML = html;
  const open = (event: MouseEvent) =>
    handleReferenceClick(event, queryClient, (path) => opened.push(path));
  host.addEventListener("click", open);
  host.addEventListener("auxclick", open);
  const anchor = host.querySelector("a");
  if (anchor === null) throw new Error(`no <a> in ${html}`);
  return { anchor, opened, queryClient };
}

function click(init: MouseEventInit = {}): MouseEvent {
  return new MouseEvent("click", { bubbles: true, button: 0, cancelable: true, ...init });
}

function auxclick(button = 1): MouseEvent {
  return new MouseEvent("auxclick", { bubbles: true, button, cancelable: true });
}

/** `window.open` held for the row that calls it, so a new tab is a recorded call and no page
 *  is opened; the row restores it. */
function newTabs() {
  return spyOn(window, "open").mockReturnValue(null);
}

// What `@legion/proof-editor`'s serializer emits for a `dispatch://` link mark: the href
// sanitized to "", the real target in `data-dispatch-href`. Following that href would reload
// the page the reader is on.
test("a plain click on a dispatch:// link opens its target inside the app", () => {
  const { anchor, opened } = article(
    '<a href="" data-dispatch-href="dispatch://CORE-1/artifact/design">design</a>'
  );

  const event = click();
  anchor.dispatchEvent(event);

  expect(opened).toEqual(["/issues/CORE-1/artifacts/design"]);
  expect(event.defaultPrevented).toBe(true);
});

test("a dispatch:// href the serializer left alone is read as the target too", () => {
  const { anchor, opened } = article('<a href="dispatch://CORE-1/ask/a1">the ask</a>');

  anchor.dispatchEvent(click());

  expect(opened).toEqual(["/issues/CORE-1/asks/a1"]);
});

// A middle click raises `auxclick` alone and a modified click raises `click`; both would open
// a new tab on the link's own href, which the serializer emptied - the page the reader is on.
// The handler opens the target there instead.
test("a middle click and a modified click open the target in a new tab", () => {
  const { anchor, opened } = article(
    '<a href="" data-dispatch-href="dispatch://CORE-1/artifact/design">design</a>'
  );
  const open = newTabs();

  const middle = auxclick();
  anchor.dispatchEvent(middle);
  for (const modifier of ["altKey", "ctrlKey", "metaKey", "shiftKey"] as const) {
    anchor.dispatchEvent(click({ [modifier]: true }));
  }
  // The secondary button's menu is the browser's, drawn before any click event reaches the
  // page, so the handler leaves that one alone.
  anchor.dispatchEvent(auxclick(2));

  expect(open.mock.calls).toEqual(
    Array.from({ length: 5 }, () => ["/issues/CORE-1/artifacts/design", "_blank", "noopener"])
  );
  expect(middle.defaultPrevented).toBe(true);
  // Nothing opened on the page the reader stays on.
  expect(opened).toEqual([]);
  open.mockRestore();
});

// The picture's fallback link carries the real path (`picture-view.ts`), so those same clicks
// are the browser's own and open it themselves, in the tab or window the reader asked for.
test("a new-tab click on a link that carries its own address is left to the browser", () => {
  const { anchor, opened } = article(
    '<a href="/issues/CORE-1/artifacts/shot-png" data-dispatch-href="dispatch://CORE-1/artifact/shot-png">CORE-1 shot-png</a>'
  );
  const open = newTabs();

  const middle = auxclick();
  anchor.dispatchEvent(middle);
  const modified = click({ ctrlKey: true });
  anchor.dispatchEvent(modified);

  expect(open).not.toHaveBeenCalled();
  expect(middle.defaultPrevented).toBe(false);
  expect(modified.defaultPrevented).toBe(false);
  expect(opened).toEqual([]);
  open.mockRestore();
});

// The picture's fallback link answers its own click (`picture-view.ts`), and the article sees
// that same click again as it bubbles: opening the page twice would leave two history entries.
test("a click something nearer the link already answered is not opened again", () => {
  const { anchor, opened } = article('<a href="" data-dispatch-href="dispatch://CORE-1">one</a>');
  anchor.addEventListener("click", (event) => event.preventDefault());

  anchor.dispatchEvent(click());

  expect(opened).toEqual([]);
});

test("a link to anywhere else, and a click on no link at all, are left where they are", () => {
  const { anchor, opened } = article(
    '<a href="https://example.invalid/page">elsewhere</a><span>words</span>'
  );
  const words = anchor.nextElementSibling;
  if (words === null) throw new Error("the span is missing");

  const link = click();
  anchor.dispatchEvent(link);
  const plain = click();
  words.dispatchEvent(plain);

  expect(opened).toEqual([]);
  expect(link.defaultPrevented).toBe(false);
  expect(plain.defaultPrevented).toBe(false);
});

/** CORE-1's summary, holding the spec under the slug a pinned block of it opens. */
const specSummary: IssueReferenceSummary = {
  documents: [
    {
      height: null,
      id: "artifact-1",
      kind: "doc",
      latest_version: 4,
      mime: null,
      name: "spec.md",
      primary: true,
      slug: "spec-md",
      width: null,
    },
  ],
  key: "CORE-1",
  project: "CORE",
  status: "testing",
  title: "Ship the release",
};

// A block of a spec pinned to a version opens that version under the spec's own slug, which
// only the issue names, so this one path waits on the issue's summary.
test("a pinned block of a spec opens the version under the slug the issue's summary names", async () => {
  const { anchor, opened, queryClient } = article(
    '<a href="" data-dispatch-href="dispatch://CORE-1/spec#b-one@v3">the block</a>'
  );
  queryClient.setQueryData(["issue-summary", "CORE-1"], specSummary);

  const event = click();
  anchor.dispatchEvent(event);

  expect(event.defaultPrevented).toBe(true);
  // The path is known only once the summary read settles, so the navigation is a task later.
  expect(opened).toEqual([]);
  await waitFor(() => expect(opened).toEqual(["/issues/CORE-1/artifacts/spec-md?v=3"]));
});

test("a middle click on a pinned block of a spec opens that same path in a new tab", async () => {
  const { anchor, opened, queryClient } = article(
    '<a href="" data-dispatch-href="dispatch://CORE-1/spec#b-one@v3">the block</a>'
  );
  queryClient.setQueryData(["issue-summary", "CORE-1"], specSummary);
  const open = newTabs();

  const middle = auxclick();
  anchor.dispatchEvent(middle);

  expect(middle.defaultPrevented).toBe(true);
  await waitFor(() =>
    expect(open.mock.calls).toEqual([
      ["/issues/CORE-1/artifacts/spec-md?v=3", "_blank", "noopener"],
    ])
  );
  expect(opened).toEqual([]);
  open.mockRestore();
});

test("openReference opens a route the caller already holds", () => {
  const opened: string[] = [];

  openReference(
    { key: "CORE-1", kind: "artifact", slug: "shot-png", version: 2 },
    new QueryClient(),
    (path) => opened.push(path)
  );

  expect(opened).toEqual(["/issues/CORE-1/artifacts/shot-png?v=2"]);
});
