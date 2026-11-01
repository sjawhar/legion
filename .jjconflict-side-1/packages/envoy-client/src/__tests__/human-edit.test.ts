import { describe, expect, test } from "bun:test";
import {
  HUMAN_EDIT_DIFF_CUT_NOTE,
  HUMAN_EDIT_DIFF_MAX,
  humanEditDiffBlock,
  humanEditHead,
} from "../human-edit";

describe("a person's edit as the card shows it", () => {
  test("the head names the document, the two versions and each editor", () => {
    expect(
      humanEditHead({
        name: "spec.md",
        from_version: 4,
        to_version: 7,
        editors: ["alice@example.com", "bob@example.com"],
      })
    ).toBe("spec.md v4 to v7, edited by alice@example.com, bob@example.com");
  });

  test("a diff that holds a fence of its own gets a longer fence, so the block stays closed", () => {
    const diff = "--- previous\n+++ current\n@@ -1 +1 @@\n-```sh\n+````sh\n";
    const block = humanEditDiffBlock(diff, false);
    expect(block).toBe(`\`\`\`\`\`diff\n${diff}\`\`\`\`\``);
  });

  test("a diff with no final line end still closes its fence on a line of its own", () => {
    expect(humanEditDiffBlock("-a\n+b", false)).toBe("```diff\n-a\n+b\n```");
  });

  test("an empty diff shows no block, and the cut note alone when Dispatch cut it", () => {
    expect(humanEditDiffBlock("", false)).toBe("");
    expect(humanEditDiffBlock(undefined, true)).toBe(HUMAN_EDIT_DIFF_CUT_NOTE);
  });

  test("a diff longer than the server's own limit is cut at a line end, and says so", () => {
    const line = `+${"a".repeat(79)}\n`;
    const long = line.repeat(400);
    expect(long.length).toBeGreaterThan(HUMAN_EDIT_DIFF_MAX * 7);
    const block = humanEditDiffBlock(long, false);
    expect(block.endsWith(`\`\`\`\n${HUMAN_EDIT_DIFF_CUT_NOTE}`)).toBe(true);
    const shown = block.slice("```diff\n".length, block.indexOf("\n```\n") + 1);
    expect(new TextEncoder().encode(shown).length).toBeLessThanOrEqual(HUMAN_EDIT_DIFF_MAX);
    expect(long.startsWith(shown)).toBe(true);
    expect(shown.endsWith("\n")).toBe(true);
    expect(new TextEncoder().encode(shown + line).length).toBeGreaterThan(HUMAN_EDIT_DIFF_MAX);
  });

  test("a diff within the limit is shown whole, with no cut note", () => {
    const diff = "--- previous\n+++ current\n@@ -1 +1 @@\n-before\n+after\n";
    expect(humanEditDiffBlock(diff, false)).toBe(`\`\`\`diff\n${diff}\`\`\``);
  });
});
