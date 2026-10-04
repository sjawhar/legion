import { expect, test } from "bun:test";

import {
  balanceCutMarkdown,
  replyPreviewMarkdown,
  searchSnippetMarkdown,
} from "./markdown-fragment";

// The shapes come from `ts_headline` with search.go's own options (`MaxWords=24, MinWords=12,
// MaxFragments=1`) run on a real Postgres, and from `reply_body`'s 160-rune head
// (`messageReplyPreview` in packages/envoy/internal/dispatch/api/messages.go).

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

test("a reply preview cut at 160 runes inside a code span drops the open backtick", () => {
  const head = "Before the cut `dispatch-deploy.y";
  expect(balanceCutMarkdown(head)).toBe("Before the cut dispatch-deploy.y");
});

test("a reply preview is balanced only when the server cut it: exactly 160 runes long", () => {
  // Under the limit, `HeadRunes` returns the body unchanged: a complete message, whose author's
  // literal `**` is theirs to keep.
  expect(replyPreviewMarkdown("Use ** for bold")).toBe("Use ** for bold");
  // At the limit it was cut (or is exactly 160 runes, which the balancer leaves alone when its
  // marks are balanced): a `**bold` opened before the cut loses the opener.
  const cut = `${"word ".repeat(30)}and then **bold text that the`.slice(0, 160);
  expect(cut.length).toBe(160);
  expect(replyPreviewMarkdown(cut)).toBe(cut.replace("**", ""));
  expect(replyPreviewMarkdown(undefined)).toBe("");
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
