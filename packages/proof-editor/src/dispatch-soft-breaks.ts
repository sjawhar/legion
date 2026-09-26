// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Normalizes CommonMark soft line breaks to a single space on markdown import.
 *
 * A soft break — a single newline inside a paragraph, as opposed to a hard
 * break formed by two trailing spaces or a backslash before the newline — is
 * defined by CommonMark to render as a space (or nothing) when the paragraph
 * is reflowed. remark-parse instead keeps it as a literal "\n" inside the
 * paragraph's "text" node value, and this library's `.ProseMirror {
 * white-space: break-spaces }` then renders every one of those characters as
 * its own visual line break. Agents that hard-wrap markdown at a fixed
 * column produce many such breaks per paragraph, so an imported document
 * renders far narrower, and with far more lines, than its author intended.
 *
 * This remark plugin walks the parsed mdast tree and replaces every "\n" in
 * a "text" node's value with a single space, leaving "code"/"inlineCode"
 * node values — which are not "text" nodes and carry newlines that are part
 * of the code itself — untouched, and leaving actual hard breaks (their own
 * "break" mdast node, never a "\n" inside a "text" node) untouched. Export
 * (ProseMirror -> markdown) is unaffected: remark-stringify always re-wraps
 * paragraph text itself, so it never emits a literal "\n" inside a "text"
 * node for this transform to see.
 */

type MdastNode = {
  type: string;
  value?: string;
  data?: { isInline?: boolean };
  children?: MdastNode[];
};

/** Milkdown's own `remarkLineBreak` (registered by the commonmark preset, so it runs before
 *  this transform) has already split each soft break out of its text node into a
 *  `break` node tagged `data.isInline`; a real hard break is a `break` without that tag. */
function isSoftBreak(node: MdastNode): boolean {
  return node.type === 'break' && node.data?.isInline === true;
}

function visit(node: MdastNode): void {
  if (node.type === 'code' || node.type === 'inlineCode') return;
  if (node.type === 'text') {
    if (typeof node.value === 'string') node.value = node.value.replace(/\n/g, ' ');
    return;
  }
  if (!node.children) return;
  const joined: MdastNode[] = [];
  for (const child of node.children) {
    if (isSoftBreak(child)) {
      const previous = joined[joined.length - 1];
      if (previous && previous.type === 'text' && typeof previous.value === 'string') {
        previous.value += ' ';
      } else {
        joined.push({ type: 'text', value: ' ' });
      }
      continue;
    }
    visit(child);
    const previous = joined[joined.length - 1];
    if (child.type === 'text' && previous && previous.type === 'text' && typeof previous.value === 'string' && typeof child.value === 'string') {
      previous.value += child.value;
      continue;
    }
    joined.push(child);
  }
  node.children = joined;
}

/** remark transformer — applied on markdown import only (`setMarkdown` and the headless parser),
 *  never on the editor's shared parser, which also serves text/plain paste. */
export function remarkSoftBreakAsSpace() {
  return (tree: MdastNode) => {
    visit(tree);
  };
}
