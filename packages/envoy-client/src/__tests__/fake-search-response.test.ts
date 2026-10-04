import { describe, expect, test } from "bun:test";
import type { SearchResult } from "@legion/contracts";
import { fakeSearchResponse } from "./fake-search-response";

function hit(id: string): SearchResult {
  return {
    kind: "document",
    owner: { key: "LEGION-2", kind: "issue", title: "Astrolabe", status: "triage" },
    artifact: { slug: "spec", name: "spec.md" },
    id,
    snippet: "<mark>astrolabe</mark>",
    rank: 1,
    href: `/issues/LEGION-2/spec?q=astrolabe#${id}`,
  };
}

describe("fakeSearchResponse", () => {
  test("defaults to a single complete page sized to the hits", () => {
    const hits = [hit("artifact-1"), hit("artifact-2")];
    const response = fakeSearchResponse(hits);
    expect(response).toEqual({
      results: hits,
      total: 2,
      reachable: 2,
      limit: 20,
      offset: 0,
      took_ms: 0,
    });
  });

  test("accepts all four paging fields together", () => {
    const hits = [hit("artifact-1")];
    const response = fakeSearchResponse(hits, {
      total: 250,
      reachable: 120,
      limit: 2,
      offset: 10,
      took_ms: 4,
    });
    expect(response).toEqual({
      results: hits,
      total: 250,
      reachable: 120,
      limit: 2,
      offset: 10,
      took_ms: 4,
    });
  });

  test("accepts all four paging fields explicitly undefined, the legacy pre-paging shape", () => {
    const response = fakeSearchResponse([], {
      total: undefined,
      reachable: undefined,
      limit: undefined,
      offset: undefined,
    });
    expect(response.total).toBeUndefined();
    expect(response.reachable).toBeUndefined();
    expect(response.limit).toBeUndefined();
    expect(response.offset).toBeUndefined();
  });

  test("throws naming the field left undefined when one paging field is explicitly undefined", () => {
    expect(() => fakeSearchResponse([], { total: undefined })).toThrow(/total/);
  });

  test("throws naming every field left undefined when more than one is explicitly undefined", () => {
    expect(() => fakeSearchResponse([], { total: undefined, reachable: undefined })).toThrow(
      /total, reachable/
    );
  });
});
