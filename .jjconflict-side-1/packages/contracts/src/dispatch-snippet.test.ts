import { expect, test } from "bun:test";
import { snippetSegments, snippetText } from "./dispatch-snippet";

test("snippetSegments splits marks and decodes escaped entities", () => {
  expect(
    snippetSegments("Use the &lt;b&gt;<mark>astrolabe</mark>&lt;/b&gt; &amp; sextant")
  ).toEqual([
    { text: "Use the <b>", mark: false },
    { text: "astrolabe", mark: true },
    { text: "</b> & sextant", mark: false },
  ]);
  expect(snippetSegments("")).toEqual([]);
  expect(snippetSegments("&#39;quoted&#34; &lt;mark&gt;not a mark&lt;/mark&gt;")).toEqual([
    { text: `'quoted" <mark>not a mark</mark>`, mark: false },
  ]);
});

test("snippetSegments preserves an unmatched opening marker as literal text", () => {
  expect(snippetSegments("alpha <mark>beta gamma")).toEqual([
    { text: "alpha <mark>beta gamma", mark: false },
  ]);
});

test("snippetSegments preserves an unmatched closing marker as literal text", () => {
  expect(snippetSegments("alpha beta</mark> gamma")).toEqual([
    { text: "alpha beta</mark> gamma", mark: false },
  ]);
});

test("snippetSegments preserves nested markers as literal text", () => {
  expect(snippetSegments("alpha <mark>beta <mark>gamma</mark> delta</mark> omega")).toEqual([
    { text: "alpha <mark>beta <mark>gamma</mark> delta</mark> omega", mark: false },
  ]);
});

test("snippetText renders marks as bold", () => {
  expect(snippetText("the <mark>astrolabe</mark> here")).toBe("the **astrolabe** here");
});
