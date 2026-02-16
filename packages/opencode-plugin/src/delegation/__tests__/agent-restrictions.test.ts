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

    it("returns default restrictions for unknown agents (fail-closed)", () => {
      const restrictions = getAgentToolRestrictions("unknown-agent");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns default restrictions for empty string (fail-closed)", () => {
      const restrictions = getAgentToolRestrictions("");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
        background_task: false,
        background_cancel: false,
      });
    });

    it("returns empty restrictions for orchestrator (delegator)", () => {
      const restrictions = getAgentToolRestrictions("orchestrator");
      expect(restrictions).toEqual({});
    });

    it("returns write/edit restrictions for conductor (can delegate, cannot edit)", () => {
      const restrictions = getAgentToolRestrictions("conductor");
      expect(restrictions).toEqual({
        write: false,
        edit: false,
      });
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

    it("returns true for unknown agents (fail-closed)", () => {
      expect(isLeafAgent("unknown-agent")).toBe(true);
    });

    it("returns true for empty string (fail-closed)", () => {
      expect(isLeafAgent("")).toBe(true);
    });

    it("returns false for orchestrator in isLeafAgent", () => {
      expect(isLeafAgent("orchestrator")).toBe(false);
    });

    it("returns false for conductor in isLeafAgent (can delegate)", () => {
      expect(isLeafAgent("conductor")).toBe(false);
    });

    it("handles case-insensitive matching", () => {
      expect(isLeafAgent("Explorer")).toBe(true);
      expect(isLeafAgent("EXPLORER")).toBe(true);
      expect(isLeafAgent("explorer")).toBe(true);
    });
  });
});
