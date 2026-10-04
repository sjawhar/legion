/**
 * Resolves CommonMark reference-style links and images on markdown import.
 *
 * `[text][id]`, `[text][]` and `[text]` name a definition written elsewhere in the body,
 * `[id]: url "title"`, and `![alt][id]` does the same for an image. remark parses each into a
 * `linkReference` / `imageReference` node beside a `definition` node, and Milkdown's commonmark
 * preset has no parser for either: the parse throws, the whole body falls back to its literal
 * source, and the throw leaves the shared parser mid-document. People and agents write these
 * links (a bibliography at the end of a message, a generated report), so this transformer
 * replaces each reference whose definition exists with the inline `link` or `image` node the
 * definition describes, as CommonMark renders it, and drops the definitions, which render as
 * nothing. A reference with no definition is literal text as CommonMark reads it: its brackets
 * and label stay as written.
 */

type MdastNode = {
  type: string;
  value?: string;
  identifier?: string;
  label?: string;
  referenceType?: string;
  url?: string;
  title?: string | null;
  alt?: string | null;
  children?: MdastNode[];
};

type Definition = { url: string; title: string | null };

/** CommonMark matches labels case-insensitively after collapsing internal whitespace; remark's
 *  `identifier` is already normalized so, and the same normalization applies to both sides. */
function collectDefinitions(node: MdastNode, into: Map<string, Definition>): void {
  if (node.type === 'definition' && node.identifier !== undefined && node.url !== undefined) {
    if (!into.has(node.identifier)) {
      into.set(node.identifier, { url: node.url, title: node.title ?? null });
    }
    return;
  }
  for (const child of node.children ?? []) collectDefinitions(child, into);
}

/** The text a reference reads as when nothing defines it: its brackets and label, as written. */
function literalReference(node: MdastNode): MdastNode[] {
  const label = node.label ?? node.identifier ?? '';
  const open: MdastNode = { type: 'text', value: node.type === 'imageReference' ? '![' : '[' };
  if (node.type === 'imageReference') {
    return [{ type: 'text', value: `![${node.alt ?? ''}]${referenceSuffix(node, label)}` }];
  }
  const close: MdastNode = { type: 'text', value: `]${referenceSuffix(node, label)}` };
  return [open, ...(node.children ?? []), close];
}

function referenceSuffix(node: MdastNode, label: string): string {
  if (node.referenceType === 'full') return `[${label}]`;
  if (node.referenceType === 'collapsed') return '[]';
  return '';
}

function resolve(node: MdastNode, definitions: Map<string, Definition>): void {
  if (!node.children) return;
  const resolved: MdastNode[] = [];
  for (const child of node.children) {
    if (child.type === 'definition') continue;
    if (child.type === 'linkReference' || child.type === 'imageReference') {
      const definition = child.identifier === undefined ? undefined : definitions.get(child.identifier);
      if (definition === undefined) {
        resolved.push(...literalReference(child));
        continue;
      }
      if (child.type === 'imageReference') {
        resolved.push({ type: 'image', url: definition.url, title: definition.title, alt: child.alt ?? null });
      } else {
        resolve(child, definitions);
        resolved.push({ type: 'link', url: definition.url, title: definition.title, children: child.children ?? [] });
      }
      continue;
    }
    resolve(child, definitions);
    resolved.push(child);
  }
  node.children = resolved;
}

/** remark transformer, applied on markdown import. */
export function remarkResolveReferenceLinks() {
  return (tree: MdastNode) => {
    const definitions = new Map<string, Definition>();
    collectDefinitions(tree, definitions);
    resolve(tree, definitions);
  };
}
