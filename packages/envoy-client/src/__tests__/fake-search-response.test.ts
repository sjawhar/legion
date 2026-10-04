import { describe, expect, test } from "bun:test";
import { fakeSearchResponse } from "./fake-search-response";

describe("fakeSearchResponse", () => {
  test("defaults to a single complete page sized to the hits", () => {
    const response = fakeSearchResponse([]);
    expect(response).toEqual({
      results: [],
      total: 0,
      reachable: 0,
      limit: 20,
      offset: 0,
      took_ms: 0,
    });
  });

  test("accepts all four paging fields together", () => {
    const response = fakeSearchResponse([], {
      total: 250,
      reachable: 120,
      limit: 2,
      offset: 10,
      took_ms: 4,
    });
    expect(response).toMatchObject({ total: 250, reachable: 120, limit: 2, offset: 10 });
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
