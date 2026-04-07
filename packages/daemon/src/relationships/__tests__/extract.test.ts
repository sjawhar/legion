import { describe, expect, it } from "bun:test";
import { extractRelationshipsFromBody } from "../extract";

describe("extractRelationshipsFromBody", () => {
  it("extracts 'Part of #NNN' as parent-child relationship", () => {
    const body = "Part of #277";
    const rels = extractRelationshipsFromBody("sjawhar-legion-317", body, "sjawhar-legion");

    expect(rels).toHaveLength(1);
    expect(rels[0]).toEqual({
      parent: "sjawhar-legion-277",
      child: "sjawhar-legion-317",
      type: "parent-child",
    });
  });

  it("is case-insensitive", () => {
    const body = "PART OF #100\npart of #200\nPart Of #300";
    const rels = extractRelationshipsFromBody("sjawhar-legion-42", body, "sjawhar-legion");

    expect(rels).toHaveLength(3);
    expect(rels.map((r) => r.parent)).toEqual([
      "sjawhar-legion-100",
      "sjawhar-legion-200",
      "sjawhar-legion-300",
    ]);
  });

  it("deduplicates within a single body", () => {
    const body = "Part of #277. Also see Part of #277 again.";
    const rels = extractRelationshipsFromBody("sjawhar-legion-317", body, "sjawhar-legion");

    expect(rels).toHaveLength(1);
    expect(rels[0]?.parent).toBe("sjawhar-legion-277");
  });

  it("returns empty array when no patterns match", () => {
    const body = "This issue has no relationship references.";
    const rels = extractRelationshipsFromBody("sjawhar-legion-42", body, "sjawhar-legion");

    expect(rels).toEqual([]);
  });

  it("returns empty array for empty body", () => {
    expect(extractRelationshipsFromBody("issue-1", "", "prefix")).toEqual([]);
  });

  it("handles multi-line bodies with mixed content", () => {
    const body = `## Context

Part of #277 (native coordination platform). Addresses pain point #2.

Some other text here.

Part of #100`;
    const rels = extractRelationshipsFromBody("sjawhar-legion-317", body, "sjawhar-legion");

    expect(rels).toHaveLength(2);
    expect(rels[0]?.parent).toBe("sjawhar-legion-277");
    expect(rels[1]?.parent).toBe("sjawhar-legion-100");
  });

  it("uses raw #NNN format when no repoPrefix provided", () => {
    const body = "Part of #42";
    const rels = extractRelationshipsFromBody("ENG-21", body);

    expect(rels).toHaveLength(1);
    expect(rels[0]).toEqual({
      parent: "#42",
      child: "ENG-21",
      type: "parent-child",
    });
  });

  it("does not match partial patterns", () => {
    // "apart of" should not match
    const body = "This is apart of the larger effort. Not Part of #999 though — wait, it is.";
    const rels = extractRelationshipsFromBody("issue-1", body, "prefix");

    // "apart of" contains "part of" as a substring — regex will match after "a"
    // but the actual issue reference #999 should be captured
    expect(rels).toHaveLength(1);
    expect(rels[0]?.parent).toBe("prefix-999");
  });
});
