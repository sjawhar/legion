import { expect, test } from "bun:test";
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

test("a middle click, a modified click and a selection drag keep the browser's own meaning", () => {
  const { anchor, opened } = article('<a href="" data-dispatch-href="dispatch://CORE-1">one</a>');

  // A middle click raises `auxclick` alone, which the article hears for exactly this reason:
  // the handler leaves it, and the browser opens the link's own href in a new tab.
  const middle = new MouseEvent("auxclick", { bubbles: true, button: 1, cancelable: true });
  anchor.dispatchEvent(middle);
  for (const modifier of ["altKey", "ctrlKey", "metaKey", "shiftKey"] as const) {
    anchor.dispatchEvent(click({ [modifier]: true }));
  }

  expect(opened).toEqual([]);
  expect(middle.defaultPrevented).toBe(false);
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

// A block of a spec pinned to a version opens that version under the spec's own slug, which
// only the issue names, so this one path waits on the issue's summary.
test("a pinned block of a spec opens the version under the slug the issue's summary names", async () => {
  const { anchor, opened, queryClient } = article(
    '<a href="" data-dispatch-href="dispatch://CORE-1/spec#b-one@v3">the block</a>'
  );
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "artifact-1",
        kind: "doc",
        latest_version: 4,
        mime: null,
        name: "spec.md",
        primary: true,
        slug: "spec-md",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "testing",
    title: "Ship the release",
  };
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);

  const event = click();
  anchor.dispatchEvent(event);

  expect(event.defaultPrevented).toBe(true);
  // The path is known only once the summary read settles, so the navigation is a task later.
  expect(opened).toEqual([]);
  await waitFor(() => expect(opened).toEqual(["/issues/CORE-1/artifacts/spec-md?v=3"]));
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
