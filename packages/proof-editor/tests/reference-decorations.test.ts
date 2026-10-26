import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { EditorReferences, ReferenceCandidate, ReferenceTarget } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

/** A stand-in for the host's grammar: a `dispatch://` address, an issue key or a `#id` (`#` and
 *  its whole run of id characters), each keyed by its own text. */
function scan(text: string): ReferenceCandidate[] {
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

/** The references option, recording each key set the editor reports. */
function recordingReferences(): EditorReferences & { reported: string[][] } {
  const reported: string[][] = [];
  return { onKeys: (keys) => void reported.push([...keys]), reported, scan };
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
  const references = recordingReferences();
  await withMarksEditor(
    "See CORE-1, CORE-2 and #q2 here.",
    ({ handle, view }) => {
      expect(references.reported).toEqual([["#q2", "CORE-1", "CORE-2"]]);
      expect(titles(view)).toEqual([]);

      handle.setReferenceTargets(
        new Map([
          ["CORE-1", CORE_1],
          ["CORE-2", null],
          ["#q2", Q2],
        ])
      );

      expect(titles(view)).toEqual([
        { href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" },
        { href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" },
      ]);
      expect(hidden(view)).toEqual(["CORE-1", "#q2"]);
      expect(view.dom.querySelector("a.dispatch-reference strong")?.textContent).toBe("CORE-1");
    },
    { references }
  );
});

test("a title that stands for fewer characters than its candidate hides only those", async () => {
  await withMarksEditor(
    "See #q2- now.",
    ({ handle, view }) => {
      // The host's question is q2, so its title stands for `#q2` and the `-` is the author's.
      handle.setReferenceTargets(new Map([["#q2-", { ...Q2, length: 3 }]]));

      expect(titles(view)).toEqual([{ href: "dispatch://CORE-1/ask/q2-uuid", text: "Which way?" }]);
      expect(hidden(view)).toEqual(["#q2"]);

      // "See #q2- now." opens its paragraph at 0: `#q2` is 5 to 8 and the `-` 8 to 9. A caret
      // after the `-` touches no title; one at the title's end does.
      setFocus(view, true);
      select(view, 9);
      expect(hidden(view)).toEqual(["#q2"]);
      select(view, 8);
      expect(titles(view)).toEqual([]);
    },
    { references: recordingReferences() }
  );
});

test("code spans, code blocks and a link its author worded are never candidates; a link whose text is its address is", async () => {
  const references = recordingReferences();
  await withMarksEditor(
    [
      "In `CORE-1` and [my words](dispatch://CORE-1) and [dispatch://CORE-1](dispatch://CORE-1).",
      "",
      "```",
      "CORE-1 #q2",
      "```",
    ].join("\n"),
    ({ handle, view }) => {
      expect(references.reported).toEqual([["dispatch://CORE-1"]]);

      handle.setReferenceTargets(
        new Map([
          ["dispatch://CORE-1", CORE_1],
          ["CORE-1", CORE_1],
          ["#q2", Q2],
        ])
      );

      expect(titles(view)).toEqual([{ href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" }]);
      expect(hidden(view)).toEqual(["dispatch://CORE-1"]);
      expect(view.dom.textContent).toContain("my words");
      // The title is drawn beside the link mark, never inside its anchor.
      expect(view.dom.querySelector("a a")).toBeNull();
    },
    { references }
  );
});

test("the stored text shows while the focused editor's selection touches the reference", async () => {
  await withMarksEditor(
    "See CORE-1 here.",
    ({ handle, view }) => {
      handle.setReferenceTargets(new Map([["CORE-1", CORE_1]]));
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
    },
    { references: recordingReferences() }
  );
});

test("drawing titles changes neither the document nor its Markdown", async () => {
  const markdown = "See CORE-1 and [dispatch://CORE-1](dispatch://CORE-1) and #q2.";
  await withMarksEditor(
    markdown,
    ({ handle, view }) => {
      const doc = view.state.doc;
      const stored = handle.getMarkdown();

      handle.setReferenceTargets(
        new Map([
          ["CORE-1", CORE_1],
          ["dispatch://CORE-1", CORE_1],
          ["#q2", Q2],
        ])
      );
      setFocus(view, true);
      select(view, 5);
      select(view, 20);
      setFocus(view, false);

      expect(titles(view)).toHaveLength(3);
      expect(view.state.doc).toBe(doc);
      expect(handle.getMarkdown()).toBe(stored);
    },
    { references: recordingReferences() }
  );
});

test("an edit rescans the text it touched, and a selection away from every reference redraws nothing", async () => {
  const references = recordingReferences();
  await withMarksEditor(
    "See CORE-1 here.\n\nSecond paragraph",
    ({ handle, view }) => {
      handle.setReferenceTargets(
        new Map([
          ["CORE-1", CORE_1],
          ["CORE-2", null],
        ])
      );
      const plugin = view.state.plugins.find((candidate) =>
        String(candidate.spec.key).startsWith("dispatch-reference-decorations")
      );
      const decorations = () => plugin?.props.decorations?.call(plugin, view.state);
      const before = decorations();
      select(view, 14);
      expect(decorations()).toBe(before);

      view.dispatch(view.state.tr.insertText(" CORE-2", 35));
      expect(references.reported.at(-1)).toEqual(["CORE-1", "CORE-2"]);
      // Typing before the reference moves its title along.
      view.dispatch(view.state.tr.insertText("Now ", 1));
      expect(titles(view)).toEqual([{ href: "dispatch://CORE-1", text: "CORE-1 Fix the thing" }]);
      expect(hidden(view)).toEqual(["CORE-1"]);

      // Breaking the key apart makes it no candidate.
      view.dispatch(view.state.tr.insertText(" ", 13));
      expect(references.reported.at(-1)).toEqual(["CORE-2"]);
      expect(titles(view)).toEqual([]);
    },
    { references }
  );
});
