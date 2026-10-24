import { describe, expect, test } from "bun:test";
import { HUMAN_EDIT_DIFF_CUT_NOTE, humanEditDiffBlock, humanEditHead } from "../human-edit";

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
});
