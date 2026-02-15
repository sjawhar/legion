import { describe, expect, it } from "bun:test";
import { getAgentToolRestrictions, isLeafAgent } from "../agent-restrictions";

describe("agent-restrictions", () => {
  describe("getAgentToolRestrictions", () => {
    it("returns correct restrictions for explore agent", () => {
      const restrictions = getAgentToolRestrictions("explore");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for explorer agent", () => {
      const restrictions = getAgentToolRestrictions("explorer");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for librarian agent", () => {
      const restrictions = getAgentToolRestrictions("librarian");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for oracle agent", () => {
      const restrictions = getAgentToolRestrictions("oracle");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for metis agent", () => {
      const restrictions = getAgentToolRestrictions("metis");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for momus agent", () => {
      const restrictions = getAgentToolRestrictions("momus");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for multimodal agent", () => {
      const restrictions = getAgentToolRestrictions("multimodal");
      expect(restrictions).toEqual({
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for multimodal-looker agent", () => {
      const restrictions = getAgentToolRestrictions("multimodal-looker");
      expect(restrictions).toEqual({
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for simplicity-reviewer agent", () => {
      const restrictions = getAgentToolRestrictions("simplicity-reviewer");
      expect(restrictions).toEqual({
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns correct restrictions for executor agent", () => {
      const restrictions = getAgentToolRestrictions("executor");
      expect(restrictions).toEqual({
        background_task: false,
        background_cancel: false,
      });
    });

    it("handles case-insensitive matching", () => {
      const restrictions1 = getAgentToolRestrictions("Explorer");
      const restrictions2 = getAgentToolRestrictions("EXPLORER");
      const restrictions3 = getAgentToolRestrictions("explorer");

      expect(restrictions1).toEqual(restrictions2);
      expect(restrictions2).toEqual(restrictions3);
    });

    it("returns empty object for unknown agents", () => {
      const restrictions = getAgentToolRestrictions("unknown-agent");
      expect(restrictions).toEqual({});
    });

    it("returns empty object for empty string", () => {
      const restrictions = getAgentToolRestrictions("");
      expect(restrictions).toEqual({});
    });
  });

  describe("isLeafAgent", () => {
    it("returns true for explore agent", () => {
      expect(isLeafAgent("explore")).toBe(true);
    });

    it("returns true for explorer agent", () => {
      expect(isLeafAgent("explorer")).toBe(true);
    });

    it("returns true for librarian agent", () => {
      expect(isLeafAgent("librarian")).toBe(true);
    });

    it("returns true for oracle agent", () => {
      expect(isLeafAgent("oracle")).toBe(true);
    });

    it("returns true for metis agent", () => {
      expect(isLeafAgent("metis")).toBe(true);
    });

    it("returns true for momus agent", () => {
      expect(isLeafAgent("momus")).toBe(true);
    });

    it("returns true for multimodal agent", () => {
      expect(isLeafAgent("multimodal")).toBe(true);
    });

    it("returns true for multimodal-looker agent", () => {
      expect(isLeafAgent("multimodal-looker")).toBe(true);
    });

    it("returns true for simplicity-reviewer agent", () => {
      expect(isLeafAgent("simplicity-reviewer")).toBe(true);
    });

    it("returns true for executor agent", () => {
      expect(isLeafAgent("executor")).toBe(true);
    });

    it("returns false for unknown agents", () => {
      expect(isLeafAgent("unknown-agent")).toBe(false);
    });

    it("returns false for empty string", () => {
      expect(isLeafAgent("")).toBe(false);
    });

    it("handles case-insensitive matching", () => {
      expect(isLeafAgent("Explorer")).toBe(true);
      expect(isLeafAgent("EXPLORER")).toBe(true);
      expect(isLeafAgent("explorer")).toBe(true);
    });
  });
});
