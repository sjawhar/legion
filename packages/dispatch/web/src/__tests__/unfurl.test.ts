import { describe, expect, it } from "bun:test";

import {
  createReferenceUnfurler,
  findReferences,
  parseReferenceKey,
  referenceFromUrl,
} from "../unfurl";

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

describe("parseReferenceKey", () => {
  it("reads the owner/repo#N keys the linkifier writes", () => {
    expect(parseReferenceKey("acme-org/example.repo_1#12")).toEqual({
      repo: "acme-org/example.repo_1",
      number: 12,
    });
  });

  it("rejects a planted key that would leave the charset or break a selector", () => {
    expect(parseReferenceKey('X"]#1')).toBeNull();
    expect(parseReferenceKey("acme-org/example-repo#0")).toBeNull();
    expect(parseReferenceKey("acme-org/example-repo#12#3")).toBeNull();
    expect(parseReferenceKey("../admin#1")).toBeNull();
    expect(parseReferenceKey("#12")).toBeNull();
  });
});

describe("createReferenceUnfurler", () => {
  it("ignores an anchor whose data-gh-ref a commenter planted: no fetch, no throw", async () => {
    const fetched: string[] = [];
    const unfurl = createReferenceUnfurler(async (ref) => {
      fetched.push(`${ref.repo}#${ref.number}`);
      return "title";
    });
    const planted = { dataset: { ghRef: 'X"]#1' } };
    const root = { querySelectorAll: () => [planted] } as unknown as ParentNode;
    await unfurl(root);
    expect(fetched).toEqual([]);
  });

  it("applies a title once per call however many anchors cite the reference", async () => {
    // bun has no CSS namespace; the selector builder only needs escape(), so
    // install one for this test. The cast names the one global it touches.
    const globals = globalThis as { CSS?: { escape(value: string): string } };
    const cssBefore = globals.CSS;
    globals.CSS = {
      escape: (value: string) => value.replace(/[^A-Za-z0-9_-]/g, (char) => `\\${char}`),
    };
    try {
      const fetched: string[] = [];
      const unfurl = createReferenceUnfurler(async (ref) => {
        fetched.push(`${ref.repo}#${ref.number}`);
        return "Pick a color";
      });
      const applied: string[] = [];
      const ownerDocument = {
        querySelectorAll: (selector: string) => {
          applied.push(selector);
          return [];
        },
      };
      const anchor = () => ({ dataset: { ghRef: "acme-org/example-repo#7" }, ownerDocument });
      const root = {
        querySelectorAll: () => [anchor(), anchor(), anchor()],
      } as unknown as ParentNode;
      await unfurl(root);
      expect(fetched).toEqual(["acme-org/example-repo#7"]);
      expect(applied).toEqual([
        'a.gh-ref[data-gh-ref="acme-org\\/example-repo\\#7"]:not([data-unfurled])',
      ]);
    } finally {
      globals.CSS = cssBefore;
    }
  });
});
