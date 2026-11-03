// Checks the guides' layout; runs on demand (bun test scripts/semantic-line-breaks.guides.test.ts) and is deliberately not in CI: the deployment's rule is an instruction first, and a gate only once a measurement shows one is needed.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fromMarkdown } from "mdast-util-from-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { gfm } from "micromark-extension-gfm";

const SCRIPT = resolve(import.meta.dir, "semantic-line-breaks.ts");
const REPO_ROOT = resolve(import.meta.dir, "..");
/** The root guide, the e2e README and the Kubernetes doc. */
const GUIDES = ["AGENTS.md", "scripts/e2e/README.md", "docs/kubernetes.md"];

/** Runs the script as a contributor does: `bun scripts/semantic-line-breaks.ts [--write] <files>`. */
function run(args: string[]) {
  const result = Bun.spawnSync(["bun", SCRIPT, ...args], { cwd: REPO_ROOT });
  return {
    exitCode: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

test("the Envoy guide and the Dispatch server README keep the layout", () => {
  const { exitCode, stdout, stderr } = run([
    "packages/envoy/AGENTS.md",
    "packages/envoy/cmd/dispatch/README.md",
  ]);
  expect(stdout).toBe("");
  expect(stderr).toBe("");
  expect(exitCode).toBe(0);
});

test("the root guide, the e2e README and the Kubernetes doc keep the layout", () => {
  const { exitCode, stdout, stderr } = run(GUIDES);
  expect(stdout).toBe("");
  expect(stderr).toBe("");
  expect(exitCode).toBe(0);
});

interface MarkdownNode {
  type: string;
  children?: MarkdownNode[];
  position?: { start: { line: number } };
}

test("the root guide, the e2e README and the Kubernetes doc hold no raw HTML block, which the checker skips", () => {
  // A line that starts with a block-level tag such as `<dir>` or `<p>` opens an HTML block even in
  // the middle of a paragraph, as GitHub reads it: every line up to the next blank line is then raw
  // HTML, which R1 never reads and GitHub does not render as the paragraph's text.
  const found: string[] = [];
  for (const file of GUIDES) {
    const tree = fromMarkdown(readFileSync(resolve(REPO_ROOT, file), "utf8"), {
      extensions: [gfm()],
      mdastExtensions: [gfmFromMarkdown()],
    }) as MarkdownNode;
    const visit = (node: MarkdownNode, containsBlocks: boolean) => {
      if (node.type === "html" && containsBlocks) {
        found.push(`${file}:${node.position?.start.line}`);
      }
      const blocks = ["root", "listItem", "blockquote", "footnoteDefinition"].includes(node.type);
      for (const child of node.children ?? []) visit(child, blocks);
    };
    visit(tree, false);
  }
  expect(found).toEqual([]);
});
