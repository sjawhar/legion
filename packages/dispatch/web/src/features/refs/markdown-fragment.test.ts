import { expect, test } from "bun:test";

import { balanceCutMarkdown } from "./markdown-fragment";

// The shapes come from `ts_headline` with search.go's own options (`MaxWords=24, MinWords=12,
// MaxFragments=1`) run on legion540-pg, and from `reply_body`'s 160-rune head.

test("a search snippet cut after a bold span opened loses the stray delimiter, not the words", () => {
  expect(balanceCutMarkdown("stop the staging deploy before the cut and report back** eleven")).toBe(
    "stop the staging deploy before the cut and report back eleven"
  );
  expect(balanceCutMarkdown("twenty anchor** after the mark, then `code")).toBe(
    "twenty anchor after the mark, then code"
  );
});

test("a snippet cut inside a link keeps the link's words", () => {
  expect(balanceCutMarkdown("read it and [link")).toBe("read it and link");
  expect(balanceCutMarkdown("read [the docs](https://example.com/do")).toBe("read the docs");
  expect(balanceCutMarkdown("docs](https://example.com/docs) and then")).toBe("docs and then");
});

test("a fragment whose marks are balanced is left exactly as it is", () => {
  const whole = "**Bold** then `code` and [a link](https://example.com) and ~~old~~ __u__";
  expect(balanceCutMarkdown(whole)).toBe(whole);
  expect(balanceCutMarkdown("")).toBe("");
});

test("a reply preview cut at 160 runes inside a code span drops the open backtick", () => {
  const head = `Before the cut \`dispatch-deploy.y`;
  expect(balanceCutMarkdown(head)).toBe("Before the cut dispatch-deploy.y");
});
