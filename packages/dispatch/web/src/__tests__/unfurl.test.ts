import { describe, expect, it } from "bun:test";

import { findReferences, referenceFromUrl } from "../unfurl";

describe("findReferences", () => {
  it("resolves bare #N against the thread repo and owner/repo#N against its own", () => {
    const text = "See #12 and acme-org/other-repo#7, not #0.";
    expect(
      findReferences(text, "acme-org/example-repo").map((m) => [
        m.ref.repo,
        m.ref.number,
        text.slice(m.index, m.index + m.length),
      ])
    ).toEqual([
      ["acme-org/example-repo", 12, "#12"],
      ["acme-org/other-repo", 7, "acme-org/other-repo#7"],
    ]);
  });

  it("ignores hashes glued to words, headings, and entity-looking sequences", () => {
    expect(findReferences("abc#12 ## Context word#3", "o/r")).toEqual([]);
    expect(findReferences("(#5) [#6] #7,", "o/r").map((m) => m.ref.number)).toEqual([5, 6, 7]);
  });

  it("does not let one reference run into the next", () => {
    expect(findReferences("#5#6 and #7 #8", "o/r").map((m) => m.ref.number)).toEqual([7, 8]);
  });

  it("returns nothing for text without references", () => {
    expect(findReferences("plain prose, a # sign, and #hashtag", "o/r")).toEqual([]);
  });
});

describe("referenceFromUrl", () => {
  it("reads issue and pull URLs, with or without a fragment", () => {
    expect(referenceFromUrl("https://github.com/acme-org/example-repo/issues/12")).toEqual({
      repo: "acme-org/example-repo",
      number: 12,
    });
    expect(
      referenceFromUrl("https://github.com/acme-org/example-repo/pull/9#issuecomment-1")
    ).toEqual({ repo: "acme-org/example-repo", number: 9 });
    expect(referenceFromUrl("https://github.com/acme-org/example-repo/commit/abc")).toBeNull();
    expect(referenceFromUrl("https://example.com/issues/1")).toBeNull();
  });

  it("rejects issue numbers that are not positive integers or have trailing path", () => {
    expect(referenceFromUrl("https://github.com/acme-org/example-repo/issues/0")).toBeNull();
    expect(referenceFromUrl("https://github.com/acme-org/example-repo/issues/12/files")).toBeNull();
    expect(referenceFromUrl("https://github.com/acme-org/example-repo/issues")).toBeNull();
  });

  it("accepts only GitHub's owner and repo charsets, so a hostile URL never reaches a selector", () => {
    expect(referenceFromUrl("https://github.com/acme-org/example.repo_1/issues/3")).toEqual({
      repo: "acme-org/example.repo_1",
      number: 3,
    });
    expect(referenceFromUrl('https://github.com/acme"org/example-repo/issues/1')).toBeNull();
    expect(referenceFromUrl("https://github.com/acme#org/example-repo/issues/1")).toBeNull();
    expect(referenceFromUrl("https://github.com/acme_org/example-repo/issues/1")).toBeNull();
    expect(referenceFromUrl('https://github.com/acme-org/ex"ample/issues/1')).toBeNull();
    expect(referenceFromUrl("https://github.com/acme-org/ex]ample/issues/1")).toBeNull();
  });
});
