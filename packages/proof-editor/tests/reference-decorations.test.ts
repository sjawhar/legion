import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { EditorReferences, ReferenceCandidate, ReferenceTarget } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

/** A stand-in for the host's reader: a `dispatch://` address, an issue key or a `#id` (`#` and
 *  its whole run of id characters), each keyed by its own text, as the host keys a candidate by
 *  the words the author wrote. */
function readReferences(text: string): ReferenceCandidate[] {
  return Array.from(
    text.matchAll(/dispatch:\/\/[^\s<`]+|\b[A-Z]+-\d+\b|#[A-Za-z0-9_-]+/g),
    (match) => ({
      end: match.index + match[0].length,
      key: match[0],
      start: match.index,
    })
  );
}

const CORE_1: ReferenceTarget = {
  href: "dispatch://CORE-1",
  title: "CORE-1 Fix the thing",
  titleKey: "CORE-1",
};
const Q2: ReferenceTarget = { href: "dispatch://CORE-1/ask/q2-uuid", title: "Which way?" };

/** The host's side of the titles: what it last handed the editor, every text the editor read
 *  through it, and every key set the editor reported. */
interface TestHost {
  readonly references: EditorReferences;
  /** Each block of text the editor read, in order, so a test says what an edit read again. */
  readonly reads: string[];
  readonly reported: string[][];
}

/** A host whose `scan` and `onKeys` keep one identity each, as a host that memoizes them does,
 *  so a fresh set of targets alone never makes the editor read the document again. */
function hostReferences(): TestHost {
  const reads: string[] = [];
  const reported: string[][] = [];
  return {
    references: {
      onKeys: (keys) => void reported.push([...keys]),
      scan: (text) => {
        reads.push(text);
        return readReferences(text);
      },
      targets: new Map(),
    },
    reads,
    reported,
  };
}

/** Hands the editor what the host has resolved, as a host pushing a fresh value with the same
 *  reader does: one `EditorReferences` carries the whole map and replaces the last one. */
function resolve(
  handle: { setReferences(references: EditorReferences | null): void },
  host: TestHost,
  resolved: Record<string, ReferenceTarget | null>
): void {
  handle.setReferences({ ...host.references, targets: new Map(Object.entries(resolved)) });
}

/** Each drawn title as its link target and its words, in document order. */
function titles(view: EditorView): Array<{ href: string | undefined; text: string }> {
  return Array.from(view.dom.querySelectorAll<HTMLElement>("a.dispatch-reference"), (link) => ({
    href: link.dataset.dispatchHref,
    text: link.textContent ?? "",
  }));
}

/** The stored text each drawn title hides, in document order. */
function hidden(view: EditorView): string[] {
  return Array.from(
    view.dom.querySelectorAll(".dispatch-reference-text"),
    (span) => span.textContent ?? ""
  );
}

/** Focuses or blurs the editor as the browser does: ProseMirror offers the event to every
 *  plugin's handler first. happy-dom delivers no synthetic focus event to ProseMirror's listener,
 *  so the test makes the call itself. */
function setFocus(view: EditorView, focused: boolean): void {
  const event = new window.FocusEvent(focused ? "focus" : "blur");
  view.someProp("handleDOMEvents", (handlers) => {
    handlers[focused ? "focus" : "blur"]?.(view, event);
  });
}

function select(view: EditorView, from: number, to = from): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

test("a resolved candidate is drawn as its title and an unresolved one as its stored text", async () => {
  const host = hostReferences();
  await withMarksEditor("See CORE-1, CORE-2 and #q2 here.", ({ handle, view }) => {
    handle.setReferences(host.references);
    expect(host.reported).toEqual([["#q2", "CORE-1", "CORE-2"]]);
    expect(titles(view)).toEqual([]);

    resolve(handle, host, { "#q2": Q2, "CORE-1": CORE_1, "CORE-2": null });

    expect(titles(view)).toEqual([
      { href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" },
      { href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" },
    ]);
    // CORE-2 resolved to nothing, so the words the author wrote still stand.
    expect(hidden(view)).toEqual(["CORE-1", "#q2"]);
    expect(view.dom.textContent).toContain("CORE-2");
    expect(view.dom.querySelector("a.dispatch-reference strong")?.textContent).toBe("CORE-1");
  });
});

test("a title that stands for fewer characters than its candidate hides only those", async () => {
  const host = hostReferences();
  await withMarksEditor("See #q2- now.", ({ handle, view }) => {
    handle.setReferences(host.references);
    // The host's question is q2, so its title stands for `#q2` and the `-` is the author's.
    resolve(handle, host, { "#q2-": { ...Q2, length: 3 } });

    expect(titles(view)).toEqual([{ href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" }]);
    expect(hidden(view)).toEqual(["#q2"]);
    expect(view.dom.textContent).toContain("-");

    // "See #q2- now." opens its paragraph at 0: `#q2` is 5 to 8 and the `-` 8 to 9. A caret
    // after the `-` touches no title; one at the title's end does.
    setFocus(view, true);
    select(view, 9);
    expect(hidden(view)).toEqual(["#q2"]);
    select(view, 8);
    expect(titles(view)).toEqual([]);
  });
});

// What the clamp in `drawnEnd` stops. The host resolves a key from the text the editor reported,
// and its answer arrives a render later; a target naming more characters than the candidate
// holds must hide none of the words beside it, and the caret that reveals the stored text must
// land on the same range the title was drawn over, or the text stays hidden while it is typed in.
test("a title that names more characters than its candidate holds is drawn no further than it", async () => {
  const host = hostReferences();
  await withMarksEditor("See #q2 now.", ({ handle, view }) => {
    handle.setReferences(host.references);
    resolve(handle, host, { "#q2": { ...Q2, length: 9 } });

    expect(titles(view)).toEqual([{ href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" }]);
    expect(hidden(view)).toEqual(["#q2"]);
    expect(view.dom.textContent).toContain("now.");

    // `#q2` is 5 to 8: a caret at its end reveals it, which it cannot do unless the drawn range
    // ends there too.
    setFocus(view, true);
    select(view, 8);
    expect(titles(view)).toEqual([]);
    expect(hidden(view)).toEqual([]);
  });
});

test("the stored text shows while the focused editor's selection touches the reference", async () => {
  const host = hostReferences();
  await withMarksEditor("See CORE-1 here.", ({ handle, view }) => {
    handle.setReferences(host.references);
    resolve(handle, host, { "CORE-1": CORE_1 });
    // "See CORE-1 here." opens its paragraph at 0: its text starts at 1, so CORE-1 is 5 to 11.
    const drawn = [{ href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" }];

    select(view, 7);
    expect(titles(view)).toEqual(drawn);

    setFocus(view, true);
    expect(titles(view)).toEqual([]);
    expect(hidden(view)).toEqual([]);

    for (const caret of [5, 11]) {
      select(view, caret);
      expect({ caret, titles: titles(view) }).toEqual({ caret, titles: [] });
    }
    select(view, 2, 6);
    expect(titles(view)).toEqual([]);

    select(view, 13);
    expect(titles(view)).toEqual(drawn);

    select(view, 7);
    setFocus(view, false);
    expect(titles(view)).toEqual(drawn);
  });
});

test("a link whose text is its own address is drawn with its target's title", async () => {
  const host = hostReferences();
  // The autolink shape a document stores: the link's text is its destination.
  await withMarksEditor(
    "See [dispatch://CORE-1/spec](dispatch://CORE-1/spec) here.",
    ({ handle, view }) => {
      handle.setReferences(host.references);
      expect(host.reported).toEqual([["dispatch://CORE-1/spec"]]);
      const spec: ReferenceTarget = { href: "dispatch://CORE-1/spec", title: "spec.md" };
      resolve(handle, host, { "dispatch://CORE-1/spec": spec });

      expect(titles(view)).toEqual([{ href: "dispatch://CORE-1/spec", text: "spec.md" }]);
      expect(hidden(view)).toEqual(["dispatch://CORE-1/spec"]);
      // The title stands beside the anchor, never inside it: a link in a link is no link.
      expect(view.dom.querySelector("a a")).toBeNull();
    }
  );
});

test("a link whose text its author wrote keeps those words", async () => {
  const host = hostReferences();
  await withMarksEditor("See [the decision](dispatch://CORE-1/spec) here.", ({ handle, view }) => {
    handle.setReferences(host.references);
    expect(host.reported).toEqual([]);
    resolve(handle, host, {
      "dispatch://CORE-1/spec": { href: "dispatch://CORE-1/spec", title: "spec.md" },
      "the decision": { href: "dispatch://CORE-1/spec", title: "spec.md" },
    });

    expect(titles(view)).toEqual([]);
    expect(view.dom.textContent).toContain("the decision");
  });
});

test("code spans, code blocks and a link's own words are never candidates", async () => {
  const host = hostReferences();
  await withMarksEditor(
    ["In `CORE-1` and [my words](dispatch://CORE-2) and #q2.", "", "```", "CORE-3 #q4", "```"].join(
      "\n"
    ),
    ({ handle, view }) => {
      handle.setReferences(host.references);
      expect(host.reported).toEqual([["#q2"]]);

      resolve(handle, host, {
        "#q2": Q2,
        "CORE-1": CORE_1,
        "CORE-2": CORE_1,
        "CORE-3": CORE_1,
      });

      expect(titles(view)).toEqual([{ href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" }]);
      expect(hidden(view)).toEqual(["#q2"]);
      expect(view.dom.textContent).toContain("my words");
      expect(view.dom.textContent).toContain("CORE-1");
      expect(view.dom.textContent).toContain("CORE-3 #q4");
    }
  );
});

test("drawing titles changes neither the document nor its Markdown", async () => {
  const host = hostReferences();
  const markdown = "See CORE-1 and #q2 and dispatch://CORE-1/spec here.";
  await withMarksEditor(markdown, ({ handle, view }) => {
    const { doc } = view.state;
    const stored = handle.getMarkdown();

    handle.setReferences(host.references);
    resolve(handle, host, {
      "#q2": Q2,
      "CORE-1": CORE_1,
      "dispatch://CORE-1/spec": { href: "dispatch://CORE-1/spec", title: "spec.md" },
    });
    setFocus(view, true);
    select(view, 5);
    select(view, 20);
    setFocus(view, false);
    // A click on a title follows the host's link and leaves the document alone.
    const title = view.dom.querySelector<HTMLElement>("a.dispatch-reference");
    title?.dispatchEvent(new window.MouseEvent("mousedown", { bubbles: true, cancelable: true }));
    title?.dispatchEvent(new window.MouseEvent("click", { bubbles: true, cancelable: true }));

    expect(titles(view)).toHaveLength(3);
    expect(view.state.doc).toBe(doc);
    expect(handle.getMarkdown()).toBe(stored);
  });
});

test("an edit re-reads the text it touched and nothing else", async () => {
  const host = hostReferences();
  await withMarksEditor("See CORE-1 here.\n\nSecond paragraph", ({ handle, view }) => {
    handle.setReferences(host.references);
    expect(host.reads).toEqual(["See CORE-1 here.", "Second paragraph"]);
    resolve(handle, host, { "CORE-1": CORE_1, "CORE-2": null });
    // Fresh targets say nothing new about the text, and a selection away from every reference
    // changes nothing at all: neither reads the document again.
    select(view, 14);
    expect(host.reads).toHaveLength(2);

    host.reads.length = 0;
    view.dispatch(view.state.tr.insertText(" CORE-2", 35));
    expect(host.reads).toEqual(["Second paragraph CORE-2"]);
    expect(host.reported.at(-1)).toEqual(["CORE-1", "CORE-2"]);

    // Typing before the reference moves its title along.
    view.dispatch(view.state.tr.insertText("Now ", 1));
    expect(titles(view)).toEqual([{ href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" }]);
    expect(hidden(view)).toEqual(["CORE-1"]);

    // Breaking the key apart makes it no candidate.
    view.dispatch(view.state.tr.insertText(" ", 13));
    expect(host.reported.at(-1)).toEqual(["CORE-2"]);
    expect(titles(view)).toEqual([]);
  });
});

test("the editor's stylesheet draws a title on one line, cut with an ellipsis", async () => {
  const stylesheet = await Bun.file(new URL("../src/lib.css", import.meta.url)).text();
  const rule = /\.dispatch-reference \{([^}]*)\}/.exec(stylesheet)?.[1];
  const hiddenRule = /\.dispatch-reference-text \{([^}]*)\}/.exec(stylesheet)?.[1];

  expect(rule).toBeDefined();
  for (const declaration of ["white-space: nowrap", "overflow: hidden", "text-overflow: ellipsis"])
    expect(rule).toContain(declaration);
  // The stored text is hidden rather than removed: it stays in the document under its title.
  expect(hiddenRule).toContain("display: none");
});
