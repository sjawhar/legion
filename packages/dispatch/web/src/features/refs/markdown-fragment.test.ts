import { expect, test } from "bun:test";

import { balanceCutMarkdown, markdownHead, searchSnippetMarkdown } from "./markdown-fragment";

// The shapes come from `ts_headline` with search.go's own options (`MaxWords=24, MinWords=12,
// MaxFragments=1`) run on a real Postgres.

test("a snippet cut after a bold span opened loses only the delimiter the cut left open", () => {
  // `ts_headline` on `… twenty **anchor** after the mark …` with a 24-word window: the window
  // starts after the opener and ends after the closer, so only `**` after `anchor` survives.
  expect(balanceCutMarkdown("twenty anchor** after the mark twentyone")).toBe(
    "twenty anchor after the mark twentyone"
  );
  expect(balanceCutMarkdown("stop the deploy before the cut and report back** eleven")).toBe(
    "stop the deploy before the cut and report back eleven"
  );
  // The same for single emphasis, which CommonMark also leaves literal when unmatched.
  expect(balanceCutMarkdown("check this thing* before you report back")).toBe(
    "check this thing before you report back"
  );
  expect(balanceCutMarkdown("Please check *this thing before you report")).toBe(
    "Please check this thing before you report"
  );
});

test("a delimiter the author typed on purpose stays: one that CommonMark would read as literal", () => {
  // Not at either edge of the text, surrounded by spaces: CommonMark cannot open emphasis with
  // `** ` (a left-flanking run needs a non-space after it), so this is literal text, not a cut.
  expect(balanceCutMarkdown("Use ** for bold")).toBe("Use ** for bold");
  expect(balanceCutMarkdown("press the ` key then")).toBe("press the ` key then");
  expect(balanceCutMarkdown("2 * 3 is 6")).toBe("2 * 3 is 6");
  expect(balanceCutMarkdown("snake_case and __init__")).toBe("snake_case and __init__");
});

test("a tilde run pairs only with a closer its own length, as GFM's strikethrough does", () => {
  // remark-gfm reads `~~a~` as plain text, since a `~~` opener pairs only with a `~~` closer.
  // Leaving `~a~` would be strikethrough: formatting over text the author never struck out.
  expect(balanceCutMarkdown("keep ~~a~ here")).toBe("keep a here");
  // A run of three or more is literal text to GFM (the third marker ends the sequence), so the
  // `~~` closer is the only delimiter here, and the cut is what took its opener.
  expect(balanceCutMarkdown("x ~~~a~~ y")).toBe("x ~~~a y");
  // Runs that do pair are the author's, at either length.
  expect(balanceCutMarkdown("a ~~struck~~ and ~one~ here")).toBe("a ~~struck~~ and ~one~ here");
  // A cut that took the closer leaves the opener to drop, as it does for `**`.
  expect(balanceCutMarkdown("report ~~struck text the cut")).toBe("report struck text the cut");
});

test("delimiters inside a code span are code, never counted", () => {
  expect(balanceCutMarkdown("`code with ** inside` and **bold**")).toBe(
    "`code with ** inside` and **bold**"
  );
  expect(balanceCutMarkdown("call `f(**kwargs)` with")).toBe("call `f(**kwargs)` with");
  expect(balanceCutMarkdown("``a ` tick`` stays")).toBe("``a ` tick`` stays");
});

test("a snippet cut inside a link keeps the link's words", () => {
  expect(balanceCutMarkdown("read it and [link")).toBe("read it and link");
  expect(balanceCutMarkdown("read [the docs](https://example.com/do")).toBe("read the docs");
  expect(balanceCutMarkdown("docs](https://example.com/docs) and then")).toBe("docs and then");
});

test("a fragment whose marks are balanced is left exactly as it is", () => {
  const whole = "**Bold** then `code` and [a link](https://example.com) and ~~old~~ __u__ *i* _j_";
  expect(balanceCutMarkdown(whole)).toBe(whole);
  expect(balanceCutMarkdown("")).toBe("");
});

test("a fragment cut inside a code span drops the open backtick", () => {
  const head = "Before the cut `dispatch-deploy.y";
  expect(balanceCutMarkdown(head)).toBe("Before the cut dispatch-deploy.y");
});

test("a search snippet is balanced only when the headline window cut it: exactly 24 words", () => {
  // `ts_headline` returns a short body whole (measured: `Use ** for bold and anchor here` →
  // `bold and <<anchor>> here`, 4 words): the author's `**` is theirs.
  expect(searchSnippetMarkdown("bold and ** anchor here")).toBe("bold and ** anchor here");
  // At the window (measured: 24 words around the match, the mark's opener left outside it).
  const window = `${"word ".repeat(21)}anchor** after the`;
  expect(window.split(" ").length).toBe(24);
  expect(searchSnippetMarkdown(window)).toBe(window.replace("**", ""));
});

test("a body's head is the body when it fits, else cut at a word and closed with an ellipsis", () => {
  expect(markdownHead("Keep **both** frames.", 21)).toBe("Keep **both** frames.");
  expect(markdownHead("Keep **both** frames.", 20)).toBe("Keep **both**…");
  // A whitespace exactly at the limit keeps the whole limit.
  expect(markdownHead("abc def ghi", 7)).toBe("abc def…");
  // A line break is a word boundary too, and the cut drops the whitespace before it.
  expect(markdownHead("first line  \nsecond line", 14)).toBe("first line…");
});

test("a cut inside a mark drops the delimiter it left open", () => {
  expect(markdownHead("Keep **both frames** level", 15)).toBe("Keep both…");
  expect(markdownHead("Run `make deploy-all` now", 16)).toBe("Run make…");
});

test("a head with no whitespace in the limit's second half is cut at the limit", () => {
  expect(markdownHead("see https://example.com/a/very/long/path", 20)).toBe(
    "see https://example.…"
  );
  // Never inside a surrogate pair: the astral character goes whole.
  expect(markdownHead(`${"a".repeat(9)}😀b`, 10)).toBe(`${"a".repeat(9)}…`);
});
